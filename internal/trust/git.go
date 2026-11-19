package trust

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// gitSetting is one "key = value" of a git config file.
type gitSetting struct {
	// section and key are in lower case, as git compares them; sub is the
	// subsection as written.
	section, sub, key string
	value             string
	line              int
}

// name is the setting's full name, as git config prints it.
func (s gitSetting) name() string {
	if s.sub != "" {
		return s.section + "." + s.sub + "." + s.key
	}
	return s.section + "." + s.key
}

// parseGitConfig reads the text of a git config file. It understands
// sections, subsections, comments, quoted values and lines joined by a
// backslash. ok is false when a line fits none of those, and then the
// caller assumes the worst rather than guess what git would make of it.
func parseGitConfig(text string) (settings []gitSetting, ok bool) {
	text = strings.TrimPrefix(text, string(rune(0xFEFF)))
	section, sub := "", ""
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		num := i + 1
		line := strings.TrimRight(lines[i], "\r")
		for joinsNext(line) && i+1 < len(lines) {
			i++
			line = line[:len(line)-1] + strings.TrimRight(lines[i], "\r")
		}
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := closingBracket(line)
			if end < 0 {
				return nil, false
			}
			head := strings.TrimSpace(line[1:end])
			name, rest, quoted := strings.Cut(head, " ")
			if !quoted {
				// The older spelling: [section.subsection].
				name, rest, _ = strings.Cut(head, ".")
			} else {
				rest = strings.TrimSpace(rest)
				if len(rest) < 2 || rest[0] != '"' || rest[len(rest)-1] != '"' {
					return nil, false
				}
				rest = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(rest[1 : len(rest)-1])
			}
			if name == "" {
				return nil, false
			}
			section, sub = strings.ToLower(name), rest
			line = strings.TrimSpace(line[end+1:])
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
		}
		if section == "" {
			return nil, false
		}
		key, value, has := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || strings.ContainsAny(key, " \t\"") {
			if !has {
				// "key" alone, possibly with a comment after it.
				key, _, _ = strings.Cut(key, " ")
			}
			if key == "" || strings.ContainsAny(key, " \t\"") {
				return nil, false
			}
		}
		if !has {
			value = "true"
		}
		settings = append(settings, gitSetting{section: section, sub: sub, key: key, value: gitValue(value), line: num})
	}
	return settings, true
}

// joinsNext reports whether a line goes on into the one after it: git
// reads a backslash and a line break in a value as nothing, so the value
// carries on. Only a backslash that stands for itself does that. One that
// is escaped ("x\\") is a backslash in the value and ends the line, and
// one in a comment is part of the comment, which ends at the line break.
func joinsNext(line string) bool {
	quoted := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\':
			if i+1 == len(line) {
				return true
			}
			i++
		case c == '"':
			quoted = !quoted
		case (c == '#' || c == ';') && !quoted:
			return false
		}
	}
	return false
}

// closingBracket finds the ] that ends a section header, passing over one
// inside a quoted subsection.
func closingBracket(line string) int {
	quoted := false
	for i := 1; i < len(line); i++ {
		switch {
		case line[i] == '\\' && quoted:
			i++
		case line[i] == '"':
			quoted = !quoted
		case line[i] == ']' && !quoted:
			return i
		}
	}
	return -1
}

// gitValue reads a value as git does: outer spaces dropped, a comment
// outside quotes cut off, quotes removed, and the common escapes undone.
func gitValue(raw string) string {
	var b strings.Builder
	quoted := false
	raw = strings.TrimSpace(raw)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw):
			i++
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(raw[i])
			}
		case c == '"':
			quoted = !quoted
		case (c == '#' || c == ';') && !quoted:
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// gitTruth reports whether a value is one of git's words for true or
// false.
func gitTruth(v string) bool {
	switch strings.ToLower(v) {
	case "", "true", "false", "yes", "no", "on", "off", "1", "0":
		return true
	}
	return false
}

// runsProgram reports whether a git setting names a program that git
// would run, some of them when a repository is merely looked at: a
// file-system monitor, content filters, diff and merge drivers, a
// signature checker, a custom ssh or proxy command, a credential helper,
// custom upload and receive programs, a pager, an editor, another hooks
// folder, or a shell alias. A setting that pulls in another settings file
// counts too, since what that file names cannot be told from here.
func runsProgram(s gitSetting) bool {
	in := func(keys ...string) bool { return slices.Contains(keys, s.key) }
	switch s.section {
	case "core":
		if s.key == "fsmonitor" {
			return !gitTruth(s.value)
		}
		return in("hookspath", "sshcommand", "gitproxy", "pager", "editor", "askpass", "alternaterefscommand")
	case "filter":
		return in("clean", "smudge", "process")
	case "diff":
		return in("textconv", "command", "external")
	case "merge":
		return in("driver")
	case "gpg":
		return in("program")
	case "credential":
		return in("helper")
	case "remote":
		return in("uploadpack", "receivepack", "vcs")
	case "pager":
		return !gitTruth(s.value)
	case "alias":
		return strings.HasPrefix(s.value, "!")
	case "sequence":
		return in("editor")
	case "interactive":
		return in("difffilter")
	case "uploadpack":
		return in("packobjectshook")
	case "include", "includeif":
		return in("path")
	}
	return false
}

// readTriggeredHooks are the git hooks that fire when a repository is
// merely looked at. Writing the index fires post-index-change, and a
// status writes the index.
var readTriggeredHooks = []string{"post-index-change"}

// GitPrograms lists what a repository's own git settings name that git
// would run as the user: the settings themselves, and a hook that fires
// on a mere look. It reads the repository's settings files and nothing
// else; git is not run. The result is empty outside a repository and for
// a repository whose settings name no program.
//
// A repository handed over with its .git folder (a download or a copy, not
// a clone) can carry such settings. While the folder is not trusted and
// this list is not empty, the agent is given no git status, the agent's
// own git commands ask first, and the trust question shows the list.
func GitPrograms(f Folder) []Holding {
	if f.GitDir == "" {
		return nil
	}
	var out []Holding
	files := []string{filepath.Join(f.CommonDir, "config"), filepath.Join(f.CommonDir, "config.worktree")}
	for _, more := range []string{filepath.Join(f.GitDir, "config"), filepath.Join(f.GitDir, "config.worktree")} {
		if !slices.Contains(files, more) {
			files = append(files, more)
		}
	}
	for _, path := range files {
		text, err := readSmall(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			out = append(out, Holding{Kind: KindGitProgram, Text: "git settings that could not be read, and so may name programs", File: path})
			continue
		}
		settings, ok := parseGitConfig(text)
		if !ok {
			out = append(out, Holding{Kind: KindGitProgram, Text: "git settings that could not be understood, and so may name programs", File: path})
			continue
		}
		for _, s := range settings {
			if runsProgram(s) {
				out = append(out, Holding{Kind: KindGitProgram, Text: fmt.Sprintf("git %s = %s", s.name(), s.value), File: path, Line: s.line})
			}
		}
	}
	for _, name := range readTriggeredHooks {
		path := filepath.Join(f.CommonDir, "hooks", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			out = append(out, Holding{Kind: KindGitProgram, Text: "git hook " + name + ", which runs when the repository is looked at", File: path})
		}
	}
	return out
}
