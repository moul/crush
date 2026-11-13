package shell

import (
	"regexp"
	"strings"
)

// CommandRuleBlocker returns a BlockFunc for one of Claude Code's Bash deny
// rules: pattern is what Bash(...) holds, such as "git push *". As in
// Claude Code, the rule matches a command's text, its words joined by
// spaces, with "*" standing for any text, spaces included; a trailing
// " *" that is the rule's only wildcard also matches the bare command
// ("git log *" matches "git log"), and a trailing ":*" is the same as
// " *". A command matches too with the wrappers Claude Code strips taken
// off it first: timeout, nice, nohup, stdbuf, command, builtin, noglob
// and xargs, so "timeout 30 git push" is a "git push" (Claude Code takes
// xargs off only when it has no options; see unwrapCommand). The shell
// checks every command it runs, so a rule also stops a command in a
// pipeline, a subshell, a loop or a command substitution, as Claude
// Code's deny rules do; like them it does not see into sh -c or a program
// named by its path.
func CommandRuleBlocker(pattern string) BlockFunc {
	re := commandRuleRegexp(pattern)
	return func(args []string) bool {
		for _, words := range commandForms(args) {
			if re.MatchString(strings.Join(words, " ")) {
				return true
			}
		}
		return false
	}
}

// commandRuleRegexp is the regular expression a Bash rule's pattern
// matches whole command texts with.
func commandRuleRegexp(pattern string) *regexp.Regexp {
	if base, ok := strings.CutSuffix(pattern, ":*"); ok {
		pattern = base + " *"
	}
	if base, ok := strings.CutSuffix(pattern, " *"); ok && !strings.Contains(base, "*") {
		return regexp.MustCompile(`(?s)^` + regexp.QuoteMeta(base) + `(?: .*)?$`)
	}
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile(`(?s)^` + strings.Join(parts, ".*") + `$`)
}

// commandForms is args, and args with each wrapper Claude Code strips
// before matching taken off in turn: the command a wrapper runs is the
// one a rule is about.
func commandForms(args []string) [][]string {
	forms := [][]string{args}
	for {
		inner := unwrapCommand(args)
		if inner == nil {
			return forms
		}
		forms = append(forms, inner)
		args = inner
	}
}

// unwrapCommand is the command a wrapper in args runs, or nil when args
// is no wrapper Claude Code strips, or runs nothing.
func unwrapCommand(args []string) []string {
	if len(args) < 2 {
		return nil
	}
	rest := args[1:]
	switch args[0] {
	case "nohup", "builtin", "noglob", "time":
		return rest
	case "command":
		// command -v and -V look a command up; they run nothing.
		if rest[0] == "-v" || rest[0] == "-V" {
			return nil
		}
		return skipOptions(rest, nil)
	case "xargs":
		// Claude Code strips only a bare xargs and matches one with
		// options as xargs itself, so its Bash(git push *) does not stop
		// "xargs -n1 git push". The command xargs runs is the one after
		// its options either way, and a rule here only refuses, so the
		// options are skipped: refusing more is the safe side.
		return skipOptions(rest, xargsValued)
	case "nice":
		return skipOptions(rest, map[string]bool{"-n": true, "--adjustment": true})
	case "stdbuf":
		return skipOptions(rest, map[string]bool{"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true})
	case "timeout":
		rest = skipOptions(rest, map[string]bool{"-s": true, "-k": true, "--signal": true, "--kill-after": true})
		if len(rest) < 2 {
			return nil
		}
		// The duration.
		return rest[1:]
	}
	return nil
}

// xargsValued are xargs's options that take the next argument as their
// value.
var xargsValued = map[string]bool{
	"-n": true, "-L": true, "-P": true, "-s": true, "-I": true, "-d": true, "-E": true, "-a": true,
	"--max-args": true, "--max-lines": true, "--max-procs": true, "--max-chars": true,
	"--replace": true, "--delimiter": true, "--eof": true, "--arg-file": true,
}

// skipOptions is args past its leading options, each of valued taking
// the next argument as its value unless it is written "--name=value" or
// with the value attached; nil when nothing is left.
func skipOptions(args []string, valued map[string]bool) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		skip := 1
		if valued[args[0]] {
			// The value is the next argument, unless the option is
			// the last one, as when the variable holding its value
			// was unset.
			skip = 2
		}
		args = args[min(skip, len(args)):]
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

// CommandRule is one of Claude Code's Bash allow rules: Bash(pattern),
// read as CommandRuleBlocker reads a deny rule.
type CommandRule struct {
	re *regexp.Regexp
	// exact is set for a rule with no "*": only it may approve an exec
	// wrapper or a find that runs, deletes or writes.
	exact bool
}

// NewCommandRule compiles an allow rule's pattern, what Bash(...) holds.
func NewCommandRule(pattern string) CommandRule {
	return CommandRule{re: commandRuleRegexp(pattern), exact: !strings.Contains(pattern, "*")}
}

// Allows reports whether the rule lets the command args run without
// asking, as Claude Code's allow rules do: the rule must match the
// command with the wrappers Claude Code strips taken off (xargs only when
// it has no options, so a rule for grep does not approve "xargs -n1 grep
// ..."), and only a rule with no "*" approves an exec wrapper (watch,
// setsid, ionice, flock) or a find that runs, deletes or writes, whose
// arguments a prefix cannot vouch for. Where a deny rule matches any of a
// command's forms, an allow rule must match the one that runs.
func (r CommandRule) Allows(args []string) bool {
	args = AllowForm(args)
	if len(args) == 0 {
		return false
	}
	if !r.exact && RunsWhatItIsGiven(args) {
		return false
	}
	return r.re.MatchString(strings.Join(args, " "))
}

// AllowForm is the command args runs, as Claude Code's allow rules see
// it: with the wrappers it strips taken off, xargs only when it has no
// options.
func AllowForm(args []string) []string {
	for {
		inner := unwrapCommand(args)
		if inner == nil || (args[0] == "xargs" && strings.HasPrefix(args[1], "-")) {
			return args
		}
		args = inner
	}
}

// RunsWhatItIsGiven reports whether args is an exec wrapper, which runs
// the command it is given, or a find with an action that runs a command,
// deletes or writes files, or reads its paths from a file.
func RunsWhatItIsGiven(args []string) bool {
	switch args[0] {
	case "watch", "setsid", "ionice", "flock":
		return true
	case "find":
		for _, arg := range args[1:] {
			switch arg {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-rm", "-fprint", "-fprint0", "-fprintf", "-fls", "-files0-from":
				return true
			}
		}
	}
	return false
}
