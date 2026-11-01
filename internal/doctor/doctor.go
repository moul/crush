// Package doctor checks the setup Gnosh runs with in a directory, as
// Claude Code's doctor does: for gnosh doctor, and /doctor in the TUI.
package doctor

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/version"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Report checks the setup store holds and returns a line per check
// ("✓" fine, "!" works but worth knowing, "✗" a turn could not run), and
// whether a turn could run. lookPath finds a program, as exec.LookPath.
func Report(store *config.ConfigStore, lookPath func(string) (string, error)) (lines []string, ok bool) {
	ok = true
	add := func(mark, format string, args ...any) {
		lines = append(lines, mark+" "+fmt.Sprintf(format, args...))
	}
	cfg := store.Config()

	add("✓", "gnosh %s", version.Version)

	if paths := store.LoadedPaths(); len(paths) > 0 {
		add("✓", "config: %s", strings.Join(paths, ", "))
	} else {
		add("!", "config: no config files, defaults only")
	}
	switch trust := store.ProjectTrust(); {
	case cfg.HeldProject.Holds():
		add("!", "project settings: off, %s is not trusted (%s); %s turns them on",
			config.ShowPath(cfg.HeldProject.Folder), cfg.HeldProject.Brief(), config.TrustCommand(cfg.HeldProject.Folder))
	case trust.Folder != "" && trust.Session:
		add("✓", "project settings: %s is trusted for this session", config.ShowPath(trust.Folder))
	case trust.Folder != "" && trust.Trusted:
		add("✓", "project settings: %s is trusted", config.ShowPath(trust.Folder))
	}

	enabled := cfg.EnabledProviders()
	slices.SortFunc(enabled, func(a, b config.ProviderConfig) int { return strings.Compare(a.ID, b.ID) })
	for _, p := range enabled {
		add("✓", "provider %s: %d models (%s)", p.ID, len(p.Models), p.BaseURL)
	}
	skipped := cfg.SkippedProviders()
	for _, id := range slices.Sorted(maps.Keys(skipped)) {
		add("✗", "provider %s: %s", id, skipped[id])
	}
	if len(enabled) == 0 {
		ok = false
		if len(skipped) == 0 {
			add("✗", "providers: none configured; run '%s' to set one up", version.Command)
		}
	}

	for _, t := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		selected, chosen := cfg.Models[t]
		switch {
		case !chosen:
			add("✗", "%s model: none chosen", t)
			ok = false
		case cfg.GetModelByType(t) == nil:
			add("✗", "%s model: %s/%s is not offered by an available provider", t, selected.Provider, selected.Model)
			ok = false
		default:
			add("✓", "%s model: %s/%s", t, selected.Provider, selected.Model)
		}
	}

	var hookCounts []string
	for _, event := range config.KnownHookEvents() {
		if n := len(cfg.Hooks[event]); n > 0 {
			hookCounts = append(hookCounts, fmt.Sprintf("%s %d", event, n))
		}
	}
	if len(hookCounts) > 0 {
		add("✓", "hooks: %s", strings.Join(hookCounts, ", "))
	} else {
		add("✓", "hooks: none")
	}
	for _, event := range config.KnownHookEvents() {
		for _, h := range cfg.Hooks[event] {
			problem := hookScriptProblem(h.Command, store.WorkingDir())
			if problem == "" {
				problem = hookCommandProblem(h.Command, lookPath)
			}
			if problem != "" {
				mark, cost := missingHookCost(event)
				add(mark, "hook %s %q: %s, so %s", event, h.DisplayName(), problem, cost)
				if mark == "✗" {
					ok = false
				}
			}
		}
	}

	if path, err := lookPath("rg"); err == nil {
		add("✓", "ripgrep: %s", path)
	} else {
		add("!", "ripgrep (rg) not found: grep and glob fall back to slower searches")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.LSP)) {
		lsp := cfg.LSP[name]
		if lsp.Disabled || lsp.Command == "" {
			continue
		}
		if path, err := lookPath(lsp.Command); err == nil {
			add("✓", "lsp %s: %s", name, path)
		} else {
			add("!", "lsp %s: %s not found, so it does not start", name, lsp.Command)
		}
	}

	if cfg.Options != nil && cfg.Options.DataDirectory != "" {
		add("✓", "data: %s", cfg.Options.DataDirectory)
	}
	return lines, ok
}

// missingHookCost says what a hook that cannot run costs at event, and
// how Report marks it. Gnosh fails closed, so where a hook can block it
// blocks every time it fires, and a turn cannot do its work: an error.
// Where none can, it is a warning each time.
func missingHookCost(event string) (mark, cost string) {
	switch event {
	case config.EventPreToolUse:
		return "✗", "every tool call it matches is blocked"
	case config.EventUserPromptSubmit:
		return "✗", "every prompt is blocked"
	case config.EventStop:
		return "✗", "every turn is held at its end"
	case config.EventSubagentStop:
		return "✗", "every sub-agent is held at its end"
	case config.EventPostToolUse, config.EventPostToolUseFailure:
		return "!", "every call it matches ends with a note that it could not run"
	case config.EventPermissionRequest:
		return "!", "every dialog it matches is put to you, with a note that it could not run"
	default:
		return "!", "it is skipped, with a warning, each time it fires"
	}
}

// commandName is a name a command can be run by: no path, no quoting, no
// assignment, no shell syntax.
var commandName = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// hookCommandProblem says that the program a hook command starts with is
// not installed, when it starts with a bare name ("ruff check", "npx
// prettier"): a hook whose program is missing exits 127, which Gnosh
// counts as a hook that could not run. Shell keywords and builtins, the
// hook shell's own builtins (jq), and a command that starts any other way
// (a path, which hookScriptProblem checks, an assignment or a quote) are
// left alone.
func hookCommandProblem(command string, lookPath func(string) (string, error)) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	name := fields[0]
	if !commandName.MatchString(name) || syntax.IsKeyword(name) || interp.IsBuiltin(name) || shell.IsBuiltin(name) {
		return ""
	}
	if _, err := lookPath(name); err != nil {
		return name + " is not installed"
	}
	return ""
}

// hookScriptProblem says what is wrong with the script a hook command
// runs, when the command starts with a path to one (./hooks/check.sh,
// ~/bin/gate, Claude Code's "${CLAUDE_PROJECT_DIR}/.claude/hooks/x.sh"):
// it does not exist, is a directory, or is not executable. Hooks run in
// dir, the project, which is what the project-directory variables hold.
// A command that starts with a bare name is left alone, since the hook
// shell has builtins of its own, and so is one that needs any other
// variable, the shell's to expand.
func hookScriptProblem(command, dir string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	script := fields[0]
	if len(script) > 1 && script[0] == '"' && script[len(script)-1] == '"' {
		script = script[1 : len(script)-1]
	}
	other := false
	expanded := os.Expand(script, func(name string) string {
		switch name {
		case "CLAUDE_PROJECT_DIR", "GNOSH_PROJECT_DIR", "KLASH_PROJECT_DIR", "CRUSH_PROJECT_DIR":
			return dir
		}
		other = true
		return ""
	})
	if other || !strings.Contains(expanded, "/") || strings.ContainsAny(expanded, "$`\"'") {
		return ""
	}
	path := home.Long(expanded)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return script + " does not exist"
	case info.IsDir():
		return script + " is a directory"
	case info.Mode()&0o111 == 0:
		return script + " is not executable"
	}
	return ""
}

// Markdown is the report as /doctor shows it in the TUI: a line per check,
// as gnosh doctor prints them, and last whether a turn can run here.
func Markdown(lines []string, ok bool) string {
	var sb strings.Builder
	sb.WriteString("## Doctor\n\n")
	for _, line := range lines {
		sb.WriteString("- " + markdownEscaper.Replace(line) + "\n")
	}
	if ok {
		sb.WriteString("\nA turn can run here.\n")
	} else {
		sb.WriteString("\nGnosh could not run a turn here: see the lines marked ✗.\n")
	}
	return sb.String()
}

// markdownEscaper keeps a check's text as written, in Markdown: paths,
// commands and errors are full of characters Markdown reads as emphasis,
// code, links, HTML or tables.
var markdownEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "|", `\|`)
