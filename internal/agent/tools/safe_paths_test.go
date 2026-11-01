package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// A read-only command runs without asking only when what it reads lies in
// the project, as Claude Code asks before its read-only commands read
// outside the working directory: cat of a key in the home directory, or
// of a link in the project that leads to one, asks; so do the options
// that make a command follow the links it meets, an input redirect from
// outside, and a word whose text the shell would expand to something else.
func TestReadOnlyCommandsReadTheProjectOnly(t *testing.T) {
	t.Parallel()
	project, outside, _ := linkedProject(t)
	require.NoError(t, os.Mkdir(filepath.Join(project, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(project, "sub", "b.txt"), []byte("b\n"), 0o644))
	dirs := workDirs{start: project, project: project}

	for _, command := range []string{
		"cat a.txt", "cat " + filepath.Join(project, "a.txt"), "cat alias.txt", "cat sub/b.txt", "cat sub/*.txt",
		"head -n 5 a.txt", "tail -n +3 a.txt", "wc -l a.txt", "stat a.txt", "ls", "ls -la", "ls sub", "du -sh sub",
		"grep -rn plain .", "grep -r plain", "grep plain a.txt sub/b.txt", "grep --include=*.txt -r plain .",
		"diff a.txt sub/b.txt", "cat -- a.txt", "cat - < a.txt", "cat < a.txt", "ls missing/new",
		"cd sub && cat ../a.txt", "cd sub && cat b.txt", "cd sub && cat ../a.txt | wc -l",
		"git log --oneline -5", "git show HEAD:a.txt",
		"git diff main...feature", "git log origin/main..HEAD", "git blame a.txt", "git diff --no-index a.txt sub/b.txt",
		// Commands that read no file they are given take any word.
		"echo /etc/passwd", "echo ~/.ssh/id_ed25519", "which go", "printenv HOME", "pwd",
	} {
		require.Truef(t, readOnlyCommand(command, dirs), "%q reads the project only", command)
	}

	for _, command := range []string{
		"cat /etc/passwd", "cat ~/.ssh/id_ed25519", "cat ~", "cat ~root/.ssh/id_rsa", "cat " + filepath.Join(outside, "id_ed25519"),
		"cat notes.txt", "cat docs/id_ed25519", "cat docs/*", "cat *.txt", "head -c 100 notes.txt", "wc notes.txt",
		"ls docs", "ls " + outside, "ls ..", "ls -L", "ls -RL .", "ls --dereference", "du -L .", "stat ../",
		"grep -R plain .", "grep --dereference-recursive plain .", "grep SECRET notes.txt", "grep -f/etc/passwd a.txt",
		"grep --file=/etc/passwd a.txt", "grep -e x /etc/hosts", "diff -r . sub", "diff -ru . sub", "diff --recursive . sub",
		"diff a.txt /etc/passwd", "cat -- ../x", "cat < /etc/passwd", "cat < notes.txt", "wc < ~/.ssh/id_ed25519",
		"cd sub && cat ../../x", "cd sub && cat ../notes.txt", "cat $HOME/.ssh/id_ed25519", `cat "$HOME/x"`,
		// After ; or || a cd may not have happened; after ! it did not.
		"cd sub; cat ../a.txt", "cd sub || cat ../a.txt", "! cd sub && cat ../a.txt",
		"cat {a.txt,/etc/passwd}", "git diff /dev/null /etc/passwd", "git blame --contents=/etc/passwd a.txt",
		"git blame --contents /etc/passwd a.txt", "git log {--output=/tmp/x,-1}", "git log -1 {--output,-p}",
	} {
		require.Falsef(t, readOnlyCommand(command, dirs), "%q reads outside the project", command)
	}

	// A line that starts outside the project reads outside it.
	elsewhere := workDirs{start: outside, project: project}
	require.False(t, readOnlyCommand("cat id_ed25519", elsewhere))
	require.True(t, readOnlyCommand("echo hi", elsewhere))

	// Allow rules let a read-only command read only the project too,
	// unless a rule names the command itself.
	rules := commandRules([]string{"npm test *"})
	require.True(t, allowedCommand("cat a.txt && npm test", dirs, rules))
	require.False(t, allowedCommand("cat /etc/passwd && npm test", dirs, rules))
	require.False(t, allowedCommand("npm test < /etc/passwd", dirs, rules))
	require.True(t, allowedCommand("cat /etc/passwd", dirs, commandRules([]string{"cat *"})), "the user allowed cat")
}

// The shell expands a brace pattern into words of its own, so one of them
// can be an option the check refuses: git log {--output=x,-1} writes x.
func TestBraceExpansionMakesWordsOfItsOwn(t *testing.T) {
	t.Parallel()
	file, err := syntax.NewParser().Parse(strings.NewReader("git log {--output=/tmp/x,-1}"), "")
	require.NoError(t, err)
	fields, err := expand.Fields(&expand.Config{}, file.Stmts[0].Cmd.(*syntax.CallExpr).Args...)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "log", "--output=/tmp/x", "-1"}, fields)
	require.False(t, readOnlyCommand("git log {--output=/tmp/x,-1}", workDirs{start: t.TempDir(), project: t.TempDir()}))
}

// In a project whose own git config would have git run programs, in a
// folder not trusted, git asks however read-only the command, and the
// question says why.
func TestGitAsksWhereTheRepositoryRunsPrograms(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	dirs := workDirs{start: project, project: project, gitPrograms: []string{"core.fsmonitor=./x.sh"}}
	for _, command := range []string{"git status", "git log --oneline", "ls && git diff", "git log | head"} {
		require.False(t, readOnlyCommand(command, dirs), command)
	}
	require.True(t, readOnlyCommand("ls", dirs))
	require.True(t, readOnlyCommand("cat a.txt", dirs))
	require.Contains(t, bashQuestion(BashParams{Command: "git status"}, dirs.gitPrograms).Note, "core.fsmonitor=./x.sh")
	require.Contains(t, bashQuestion(BashParams{Command: "cd sub && git log"}, dirs.gitPrograms).Note, "not trusted")
	require.Empty(t, bashQuestion(BashParams{Command: "ls"}, dirs.gitPrograms).Note)
	require.Empty(t, bashQuestion(BashParams{Command: "echo digit"}, dirs.gitPrograms).Note)
	require.Empty(t, bashQuestion(BashParams{Command: "git status"}, nil).Note)
}
