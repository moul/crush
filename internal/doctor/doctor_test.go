package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

func doctorConfig() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("qwen", config.ProviderConfig{ID: "qwen", BaseURL: "http://127.0.0.1:8080/v1", Models: []catwalk.Model{{ID: "qwen3.8-27b"}}})
	return &config.Config{
		Providers: providers,
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "qwen", Model: "qwen3.8-27b"},
			config.SelectedModelTypeSmall: {Provider: "qwen", Model: "qwen3.8-27b"},
		},
		Hooks:   map[string][]config.HookConfig{config.EventStop: {{Command: "true"}}, config.EventUserPromptSubmit: {{Command: "a"}, {Command: "b"}}},
		LSP:     config.LSPs{"gopls": {Command: "gopls"}, "off": {Command: "nope", Disabled: true}},
		Options: &config.Options{DataDirectory: "/tmp/proj/.gnosh"},
	}
}

func found(name string) (string, error) { return "/usr/bin/" + name, nil }

// foundAllBut finds every program except missing.
func foundAllBut(missing string) func(string) (string, error) {
	return func(name string) (string, error) {
		if name == missing {
			return "", exec.ErrNotFound
		}
		return found(name)
	}
}

// A hook that starts with a bare program name has it looked up, since a
// program that is not installed makes the hook fail closed every time it
// fires. The shell's own keywords and builtins, Gnosh's jq, and commands
// that start with a path, an assignment or other shell syntax are not.
func TestHookCommandProblem(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ruff is not installed", hookCommandProblem("ruff check --fix .", foundAllBut("ruff")))
	require.Empty(t, hookCommandProblem("ruff check .", found))
	require.Empty(t, hookCommandProblem("", found))
	// Each of these starts with a word that is not on the PATH, and is
	// left alone for what that word is.
	for _, command := range []string{
		"cd sub && ruff check .",    // a shell builtin
		"echo hi",                   // a shell builtin
		"jq -r .tool_input.command", // the hook shell's own builtin
		"if true; then ruff; fi",    // a shell keyword
		"[[ -f x ]] && ruff",        // shell syntax
		"FIX=1 ruff",                // an assignment
		"./hooks/ruff.sh",           // a path, which hookScriptProblem checks
		`"ruff" check`,              // quoted
	} {
		first := strings.Fields(command)[0]
		require.Empty(t, hookCommandProblem(command, foundAllBut(first)), command)
	}
}

// A setup that can run a turn reports each part as fine.
func TestDoctorReportOnAWorkingSetup(t *testing.T) {
	t.Parallel()

	lines, ok := Report(config.NewTestStore(doctorConfig()), found)
	report := strings.Join(lines, "\n")
	require.True(t, ok, report)
	require.Contains(t, report, "✓ provider qwen: 1 models (http://127.0.0.1:8080/v1)")
	require.Contains(t, report, "✓ large model: qwen/qwen3.8-27b")
	require.Contains(t, report, "✓ hooks: UserPromptSubmit 2, Stop 1")
	require.Contains(t, report, "✓ ripgrep: /usr/bin/rg")
	require.Contains(t, report, "✓ lsp gopls: /usr/bin/gopls")
	require.NotContains(t, report, "nope", "a disabled LSP is not checked")
	require.NotContains(t, report, "✗")
}

// A chosen model no provider offers means no turn can run; a missing
// ripgrep or LSP binary is worth knowing but does not.
func TestDoctorReportSaysWhatIsWrong(t *testing.T) {
	t.Parallel()

	cfg := doctorConfig()
	cfg.Models[config.SelectedModelTypeLarge] = config.SelectedModel{Provider: "qwen", Model: "gone"}
	missing := func(name string) (string, error) { return "", errors.New("not found") }

	lines, ok := Report(config.NewTestStore(cfg), missing)
	report := strings.Join(lines, "\n")
	require.False(t, ok)
	require.Contains(t, report, "✗ large model: qwen/gone is not offered by an available provider")
	require.Contains(t, report, "! ripgrep (rg) not found")
	require.Contains(t, report, "! lsp gopls: gopls not found")

	empty := doctorConfig()
	empty.Providers = csync.NewMap[string, config.ProviderConfig]()
	lines, ok = Report(config.NewTestStore(empty), found)
	require.False(t, ok)
	require.Contains(t, strings.Join(lines, "\n"), "✗ providers: none configured")
}

// A provider loading left out is named with why, as from the AI box's log
// when its gpt-oss server was down: until then it only vanished from the
// model list. Other providers still run a turn.
func TestDoctorReportSaysWhyAProviderWasLeftOut(t *testing.T) {
	t.Parallel()

	cfg := doctorConfig()
	cfg.SkipProviderForTest("gptoss", "its model server did not answer (connection refused)")
	lines, ok := Report(config.NewTestStore(cfg), found)
	require.True(t, ok)
	require.Contains(t, strings.Join(lines, "\n"), "✗ provider gptoss: its model server did not answer (connection refused)")
}

// A hook that runs a script by path is checked: one that is missing, is a
// directory or cannot be run fails every time it fires. Bare names may be
// the hook shell's own builtins and are left alone.
func TestHookScriptProblem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.sh"), []byte("#!/bin/sh\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "hooks"), 0o755))

	require.Equal(t, "./missing.sh does not exist", hookScriptProblem("./missing.sh --flag", dir))
	require.Equal(t, "./plain.sh is not executable", hookScriptProblem("./plain.sh", dir))
	require.Equal(t, "./hooks is a directory", hookScriptProblem("./hooks", dir))
	require.Empty(t, hookScriptProblem("./ok.sh arg", dir))
	require.Empty(t, hookScriptProblem(filepath.Join(dir, "ok.sh"), "/elsewhere"))
	require.Empty(t, hookScriptProblem("bash ./missing.sh", dir), "the shell runs it; bash is a bare name")
	require.Empty(t, hookScriptProblem(`echo '{"decision":"allow"}'`, dir))
	require.Empty(t, hookScriptProblem("$HOME/bin/x", dir), "a variable is the shell's to expand")

	// Claude Code's commands name their scripts by the project directory,
	// often quoted. They were skipped for the $, and a missing one fails
	// its hook each time, which lets the call go ahead.
	require.Equal(t, "${CLAUDE_PROJECT_DIR}/missing.sh does not exist", hookScriptProblem(`"${CLAUDE_PROJECT_DIR}/missing.sh"`, dir))
	require.Equal(t, "$GNOSH_PROJECT_DIR/missing.sh does not exist", hookScriptProblem("$GNOSH_PROJECT_DIR/missing.sh arg", dir))
	require.Empty(t, hookScriptProblem(`"${CLAUDE_PROJECT_DIR}/ok.sh"`, dir))

	// Claude Code's documentation quotes the variable alone and leaves
	// the rest of the path after it.
	require.Equal(t, "$CLAUDE_PROJECT_DIR/missing.sh does not exist", hookScriptProblem(`"$CLAUDE_PROJECT_DIR"/missing.sh`, dir))
	require.Equal(t, "$CLAUDE_PROJECT_DIR/plain.sh is not executable", hookScriptProblem(`"$CLAUDE_PROJECT_DIR"/plain.sh arg`, dir))
	require.Empty(t, hookScriptProblem(`"$CLAUDE_PROJECT_DIR"/ok.sh`, dir))
	require.Empty(t, hookScriptProblem(`"$CLAUDE_PROJECT_DIR/missing`, dir), "an unclosed quote is the shell's to report")
}

// The report names the hook whose script is missing, and what that
// costs: Gnosh fails closed, so where a hook can block, a hook that cannot
// run blocks every time, an error; where none can, it is a warning.
func TestDoctorReportNamesABrokenHook(t *testing.T) {
	t.Parallel()

	cfg := doctorConfig()
	missing := filepath.Join(t.TempDir(), "gate.sh")
	cfg.Hooks = map[string][]config.HookConfig{config.EventStop: {{Name: "gate", Command: missing}}}
	lines, ok := Report(config.NewTestStore(cfg), found)
	require.False(t, ok, "a gate that cannot run holds every turn")
	require.Contains(t, strings.Join(lines, "\n"), `✗ hook Stop "gate": `+missing+" does not exist, so every turn is held at its end")

	cfg = doctorConfig()
	cfg.Hooks = map[string][]config.HookConfig{config.EventPreToolUse: {{Name: "lint", Command: "ruff check ."}}}
	lines, ok = Report(config.NewTestStore(cfg), foundAllBut("ruff"))
	require.False(t, ok, "a PreToolUse hook whose program is missing blocks every call it matches")
	require.Contains(t, strings.Join(lines, "\n"), `✗ hook PreToolUse "lint": ruff is not installed, so every tool call it matches is blocked`)

	cfg = doctorConfig()
	cfg.Hooks = map[string][]config.HookConfig{config.EventSessionStart: {{Name: "memsync", Command: missing}}}
	lines, ok = Report(config.NewTestStore(cfg), found)
	require.True(t, ok, "a SessionStart hook cannot block, so a turn still runs")
	require.Contains(t, strings.Join(lines, "\n"), `! hook SessionStart "memsync": `+missing+" does not exist, so it is skipped, with a warning, each time it fires")
}

// /doctor shows the report as gnosh doctor prints it, a line per check
// with its text kept as written (Markdown would read the asterisks and
// underscores of rules and paths as emphasis), then whether a turn can
// run here.
func TestMarkdown(t *testing.T) {
	t.Parallel()

	lines := []string{"✓ gnosh v1", `✗ hook PreToolUse "Bash(*)": ./my_gate.sh does not exist, so every tool call it matches is blocked`}
	require.Equal(t, "## Doctor\n\n- ✓ gnosh v1\n"+
		"- ✗ hook PreToolUse \"Bash(\\*)\": ./my\\_gate.sh does not exist, so every tool call it matches is blocked\n"+
		"\nGnosh could not run a turn here: see the lines marked ✗.\n", Markdown(lines, false))
	require.True(t, strings.HasSuffix(Markdown(lines[:1], true), "\n\nA turn can run here.\n"))
	require.Equal(t, `\\ \`+"`"+` \* \_ \[ \] \< \> \|`, markdownEscaper.Replace("\\ ` * _ [ ] < > |"))
}
