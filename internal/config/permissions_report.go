package config

import (
	"fmt"
	"slices"
	"strings"
)

// PermissionsReport is what the agent may do without asking, as Markdown
// for the TUI's /permissions, which shows it as Claude Code's
// /permissions does: the permission mode, the allow and deny rules, each
// with where it comes from (a config key, or the --allowedTools and
// --disallowedTools flags, which o records), and the directories the
// session works in. skipRequests is whether every request is granted
// (yolo, Claude Code's bypassPermissions).
func PermissionsReport(cfg *Config, o *RuntimeOverrides, workingDir string, skipRequests bool) string {
	if o == nil {
		o = &RuntimeOverrides{}
	}
	perms := cfg.Permissions
	if perms == nil {
		perms = &Permissions{}
	}
	var sb strings.Builder
	sb.WriteString("## Permissions\n\n")
	mode, meaning := PermissionModeDefault, "anything the rules below do not allow is asked about"
	switch {
	case skipRequests:
		mode, meaning = PermissionModeBypass, "everything runs without asking (yolo); deny rules still stop what they match"
	case o.AcceptEdits:
		mode, meaning = PermissionModeAcceptEdits, "edits of files in the project run without asking; anything else the rules do not allow is asked about"
	case o.DontAsk:
		mode, meaning = PermissionModeDontAsk, "anything the rules below do not allow is denied instead of asked about"
	}
	fmt.Fprintf(&sb, "**Mode:** %s: %s.\n", mode, meaning)

	var allow, deny [][2]string
	for _, rule := range perms.Allow {
		allow = append(allow, [2]string{rule, ruleSource(rule, o.AllowRules, "permissions.allow", "--allowedTools")})
	}
	for _, tool := range perms.AllowedTools {
		allow = append(allow, [2]string{tool, toolSource(tool, o.AllowedTools, "permissions.allowed_tools", "--allowedTools")})
	}
	for _, rule := range perms.Deny {
		deny = append(deny, [2]string{rule, ruleSource(rule, o.DenyRules, "permissions.deny", "--disallowedTools")})
	}
	if cfg.Options != nil {
		for _, tool := range cfg.Options.DisabledTools {
			deny = append(deny, [2]string{tool, toolSource(tool, o.DisabledTools, "options.disabled_tools", "--disallowedTools")})
		}
	}
	rulesTable(&sb, "Allow", allow)
	rulesTable(&sb, "Deny", deny)

	sb.WriteString("\n### Workspace\n\n| Directory | |\n|-----------|---|\n")
	fmt.Fprintf(&sb, "| %s | working directory |\n", markdownCell(workingDir))
	for _, dir := range o.AddedDirs {
		fmt.Fprintf(&sb, "| %s | --add-dir or /add-dir |\n", markdownCell(dir))
	}
	sb.WriteString("\nRead-only commands (ls, cat, grep, git status and the like) run without asking when what they read lies in the project, as in Claude Code.\n")
	return sb.String()
}

// rulesTable writes the rules under title, each with where it comes from,
// or says there are none.
func rulesTable(sb *strings.Builder, title string, rules [][2]string) {
	fmt.Fprintf(sb, "\n### %s\n\n", title)
	if len(rules) == 0 {
		sb.WriteString("None.\n")
		return
	}
	sb.WriteString("| Rule | From |\n|------|------|\n")
	for _, r := range rules {
		fmt.Fprintf(sb, "| %s | %s |\n", markdownCell(r[0]), r[1])
	}
}

// ruleSource is where a Bash(pattern) rule or a tool's name in an allow or
// deny list comes from: flag when it was given on the command line, whose
// Bash patterns are flagPatterns, key otherwise.
func ruleSource(rule string, flagPatterns []string, key, flag string) string {
	if pattern, ok := BashRulePattern(rule); ok && slices.Contains(flagPatterns, pattern) {
		return flag
	}
	return key
}

// toolSource is where a tool's name in allowed_tools or disabled_tools
// comes from: flag when it was given on the command line, key otherwise.
func toolSource(tool string, flagTools []string, key, flag string) string {
	if slices.Contains(flagTools, tool) {
		return flag
	}
	return key
}
