package tools

import (
	"slices"

	"github.com/charmbracelet/crush/internal/shell"
	"mvdan.cc/sh/v3/syntax"
)

// allowedCommand reports whether command may run without asking under
// the Bash allow rules of the permissions (Claude Code's
// permissions.allow, such as Bash(git diff *)): as in Claude Code, each
// command in it must be read-only or match a rule on its own, so a rule
// for git diff does not approve "git diff && rm -rf ~". The command line
// is read as readOnlyCommand reads one, and asked about wherever that
// asks: a substitution, an assignment, a background job, a redirect to a
// file, a line it cannot parse.
func allowedCommand(command string, dirs workDirs, rules []shell.CommandRule) bool {
	if len(rules) == 0 {
		return false
	}
	allowed := func(call *syntax.CallExpr) bool { return allowedCall(call, rules) }
	return eachCallIn(command, dirs, func(call *syntax.CallExpr) bool {
		return readOnlyCall(call) || allowed(call)
	}) && readsProject(command, dirs, allowed)
}

// allowedCall reports whether one of rules lets call run (see
// shell.CommandRule.Allows), when its words run nothing and the program
// it runs, past any wrapper, is named in plain text.
func allowedCall(call *syntax.CallExpr, rules []shell.CommandRule) bool {
	if len(call.Assigns) > 0 || len(call.Args) == 0 {
		// Claude Code's allow rules match past an assignment of a few
		// variables it knows to be safe only.
		return false
	}
	args := make([]string, len(call.Args))
	unsure := make([]bool, len(call.Args))
	for i, word := range call.Args {
		text, uncertain, ok := plainWord(word)
		if !ok {
			return false
		}
		args[i], unsure[i] = text, uncertain
	}
	form := shell.AllowForm(args)
	if len(form) == 0 || unsure[len(args)-len(form)] {
		return false
	}
	// A glob or an expansion could put an action such as -delete in a
	// find, which a rule with a "*" approves only without one.
	if form[0] == "find" && slices.Contains(unsure, true) {
		return false
	}
	return slices.ContainsFunc(rules, func(rule shell.CommandRule) bool { return rule.Allows(args) })
}

// commandRules compiles the patterns of the Bash allow rules.
func commandRules(patterns []string) []shell.CommandRule {
	rules := make([]shell.CommandRule, len(patterns))
	for i, pattern := range patterns {
		rules[i] = shell.NewCommandRule(pattern)
	}
	return rules
}
