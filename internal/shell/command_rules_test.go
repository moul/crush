package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A Bash deny rule matches the commands Claude Code's documentation says
// it does (code.claude.com/docs/en/permissions, "Wildcard patterns" and
// "Wrappers"), and not the ones it says it does not.
func TestCommandRuleBlocker(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pattern     string
		matches     []string
		doesntMatch []string
	}{
		{"npm run build", []string{"npm run build"}, []string{"npm run build --watch", "npm run"}},
		{"npm run *", []string{"npm run build", "npm run test --watch", "npm run"}, []string{"npm install"}},
		{"git log * main", []string{"git log --oneline main", "git log -5 main", "git log --output=out main"}, []string{"git log main", "git push origin main"}},
		{"git * main", []string{"git merge main", "git push origin main", "git -c core.fsmonitor=x diff main"}, []string{"git log"}},
		{"* --version", []string{"node --version"}, []string{"node -v"}},
		{"ls *", []string{"ls -la", "ls"}, []string{"lsof"}},
		{"ls*", []string{"ls -la", "lsof"}, nil},
		{"* --help *", []string{"npm --help x"}, []string{"npm --help"}},
		{"ls:*", []string{"ls -la", "ls"}, []string{"lsof"}},
		{"git:* push", nil, []string{"git push"}},
		// Wrappers come off, as Claude Code strips them; and xargs with
		// its options too, which Claude Code matches as xargs itself, as
		// the command it runs is the one a deny rule is about.
		{"git push *", []string{
			"git push origin main", "timeout 30 git push", "timeout -s KILL 5 git push", "nice -n 10 git push",
			"nohup git push", "stdbuf -o L git push", "command git push", "builtin git push", "xargs git push",
			"xargs -n1 git push", "xargs -n 1 git push", "xargs -I {} git push {}",
		}, []string{"git -C . push origin main", "/usr/bin/git push", "command -v git push", "gitpush"}},
	} {
		block := CommandRuleBlocker(tc.pattern)
		for _, command := range tc.matches {
			require.Truef(t, block(strings.Fields(command)), "Bash(%s) should match %q", tc.pattern, command)
		}
		for _, command := range tc.doesntMatch {
			require.Falsef(t, block(strings.Fields(command)), "Bash(%s) should not match %q", tc.pattern, command)
		}
	}
}

// The shell refuses a command a deny rule matches wherever it runs: after
// another command, in a pipeline, a subshell, a loop or a command
// substitution, past an assignment, and says what it refused; other
// commands run.
func TestRunRefusesACommandADenyRuleMatches(t *testing.T) {
	t.Parallel()

	blocks := []BlockFunc{CommandRuleBlocker("touch *")}
	for _, command := range []string{
		"touch denied",
		"true && touch denied",
		"echo x | touch denied",
		"(touch denied)",
		"for f in a; do touch denied; done",
		`echo "$(touch denied)"`,
		"FOO=bar touch denied",
		"timeout 5 touch denied",
	} {
		dir := t.TempDir()
		var stdout, stderr strings.Builder
		said := stderr.String
		if err := Run(t.Context(), RunOptions{Command: command, Cwd: dir, Stdout: &stdout, Stderr: &stderr, BlockFuncs: blocks}); err != nil {
			said = func() string { return err.Error() + stderr.String() }
		}
		require.Containsf(t, said(), "not allowed for security reasons", "%s", command)
		require.Containsf(t, said(), "touch denied", "%s: the refusal names the command", command)
		require.NoFileExistsf(t, dir+"/denied", "%s", command)
	}

	var stdout strings.Builder
	require.NoError(t, Run(t.Context(), RunOptions{Command: "echo allowed", Cwd: t.TempDir(), Stdout: &stdout, BlockFuncs: blocks}))
	require.Equal(t, "allowed\n", stdout.String())
}

// An allow rule matches a command as Claude Code's do: with the wrappers
// Claude Code strips taken off, xargs only without options, the trailing
// " *" also matching the bare command, and only a rule with no "*"
// approving an exec wrapper or a find that runs, deletes or writes.
func TestCommandRuleAllows(t *testing.T) {
	t.Parallel()

	for pattern, cases := range map[string]map[string]bool{
		"npm test *": {
			"npm test":                       true,
			"npm test --watch":               true,
			"timeout 30 npm test":            true,
			"timeout -s KILL 30 npm test":    true,
			"nice -n 5 nohup npm test":       true,
			"command npm test":               true,
			"stdbuf -o0 npm test --coverage": true,
			"npm install":                    false,
			"npm testing":                    false,
			"xargs npm test":                 true,
			"xargs -n1 npm test":             false,
			"timeout 30 npm install":         false,
		},
		"ls:*": {"ls": true, "ls -la": true, "lsof": false},
		"git log * main": {
			"git log --oneline main": true,
			"git log main":           false,
		},
		"find *": {
			"find . -name x":        true,
			"find . -exec rm {} ;":  false,
			"find . -delete":        false,
			"find . -fprint out":    false,
			"find -files0-from f .": false,
		},
		"find . -delete":  {"find . -delete": true, "find . -delete -x": false},
		"watch *":         {"watch ls": false},
		"flock /tmp/l ls": {"flock /tmp/l ls": true},
	} {
		rule := NewCommandRule(pattern)
		for command, want := range cases {
			require.Equal(t, want, rule.Allows(strings.Fields(command)), "%s / %s", pattern, command)
		}
	}
}

// A wrapper option that takes a value but has none left after it, as
// when the variable holding the value was unset, is skipped like any
// other option instead of reading past the end of the command.
func TestCommandRuleWrapperOptionWithoutItsValue(t *testing.T) {
	t.Parallel()

	block := CommandRuleBlocker("git push *")
	for _, command := range []string{"timeout -s", "timeout -k", "nice -n", "stdbuf -o", "xargs -n"} {
		args := strings.Fields(command)
		require.NotPanicsf(t, func() { block(args) }, "deny rule against %q", command)
		require.NotPanicsf(t, func() { AllowForm(args) }, "allow form of %q", command)
	}
}

// The shell runs a command whose wrapper lost its option value to an
// unset variable, and lets the wrapper itself report the mistake,
// rather than refusing the whole line with an internal error.
func TestRunAWrapperMissingItsOptionValue(t *testing.T) {
	t.Parallel()

	blocks := []BlockFunc{CommandRuleBlocker("touch *")}
	var stdout, stderr strings.Builder
	err := Run(t.Context(), RunOptions{
		Command: "nice -n $NICENESS", Cwd: t.TempDir(),
		Stdout: &stdout, Stderr: &stderr, BlockFuncs: blocks,
	})
	said := stderr.String()
	if err != nil {
		said += err.Error()
	}
	require.NotContains(t, said, "panic", "the command reaches nice: %s", said)
}
