package instructions

import (
	"fmt"
	"strings"
)

// This file reads the frontmatter of a rule file: the lines between a "---"
// on the first line and the next "---". Only one field matters, "paths",
// which lists the path patterns the rule applies to:
//
//	---
//	paths:
//	  - "src/api/**/*.ts"
//	  - lib/**
//	---
//
// or on one line, as a list or as one comma-separated string:
//
//	paths: ["src/**/*.ts", "lib/**"]
//	paths: "src/**/*.ts, lib/**"
//
// The reader understands exactly that much YAML: top-level "key: value"
// lines, a value that is a plain, 'single-quoted' or "double-quoted" word, a
// one-line [list] of such words, or a block of "- item" lines. Other fields
// are skipped whatever they hold. Anything else in or around "paths" is
// refused with a reason, and the caller then loads the file whole and
// unscoped, so that a rule is never dropped for a slip in its header.

// splitFrontmatter takes the frontmatter off a rule's text. It returns the
// text that follows it and the path patterns it lists. When there is no
// frontmatter, body is the whole text. When there is one that cannot be
// read, problem says why, and body is the whole text.
func splitFrontmatter(text string) (body string, paths []string, problem string) {
	first, rest, _ := strings.Cut(text, "\n")
	if strings.TrimRight(first, " \t") != "---" {
		return text, nil, ""
	}
	var lines []string
	closed := false
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		if strings.TrimRight(line, " \t") == "---" {
			closed = true
			break
		}
		lines = append(lines, line)
	}
	if !closed {
		return text, nil, "it opens with --- on line 1 and is never closed by another ---"
	}
	paths, problem = frontmatterPaths(lines)
	if problem != "" {
		return text, nil, problem
	}
	return strings.TrimLeft(rest, "\n"), paths, ""
}

// frontmatterPaths reads the lines of a frontmatter block and returns the
// patterns of its "paths" field. Line numbers in problems count from the
// top of the file, where the opening "---" is line 1.
func frontmatterPaths(lines []string) (paths []string, problem string) {
	seen := false
	for i := 0; i < len(lines); {
		line := lines[i]
		n := i + 2
		t := strings.TrimSpace(line)
		if t == "" || t[0] == '#' {
			i++
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, fmt.Sprintf("line %d is indented but belongs to no field", n)
		}
		key, val, ok := cutKey(line)
		if !ok {
			return nil, fmt.Sprintf("line %d is not of the form \"name: value\"", n)
		}
		// The lines that belong to this field: those indented under it,
		// and, when the field's own line holds no value, list items
		// written at its own depth.
		j := i + 1
		for j < len(lines) {
			l := lines[j]
			lt := strings.TrimSpace(l)
			own := lt == "" || lt[0] == '#' || l[0] == ' ' || l[0] == '\t'
			if !own && val == "" && (lt == "-" || strings.HasPrefix(lt, "- ")) {
				own = true
			}
			if !own {
				break
			}
			j++
		}
		if key == "paths" {
			if seen {
				return nil, fmt.Sprintf("line %d gives paths a second time", n)
			}
			seen = true
			if paths, problem = pathsValue(val, lines[i+1:j], n); problem != "" {
				return nil, problem
			}
		}
		i = j
	}
	return paths, ""
}

// cutKey splits a "name: value" line. The colon must end the line or be
// followed by a space. The name may be quoted. What follows the colon is a
// comment when it opens with "#", since the colon is followed by a space,
// and then the field has no value on its own line.
func cutKey(line string) (key, val string, ok bool) {
	at := -1
	for i := 0; i < len(line); i++ {
		if line[i] == ':' && (i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t') {
			at = i
			break
		}
	}
	if at <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:at])
	if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
		key = key[1 : len(key)-1]
	}
	if key == "" || strings.HasPrefix(key, "- ") || key == "-" {
		return "", "", false
	}
	val = strings.TrimSpace(line[at+1:])
	if strings.HasPrefix(val, "#") {
		val = ""
	}
	return key, val, true
}

// pathsValue reads the value of the "paths" field: val is what follows the
// colon on the field's own line, block the lines under it, n the number of
// the field's line.
func pathsValue(val string, block []string, n int) ([]string, string) {
	if val != "" {
		for _, l := range block {
			if t := strings.TrimSpace(l); t != "" && t[0] != '#' {
				return nil, fmt.Sprintf("line %d: the value of paths goes on over more lines in a way that is not read here", n)
			}
		}
		switch val[0] {
		case '[':
			return flowList(val, n)
		case '"', '\'':
			s, rest, problem := quoted(val, n)
			if problem != "" {
				return nil, problem
			}
			if rest = strings.TrimSpace(rest); rest != "" && rest[0] != '#' {
				return nil, fmt.Sprintf("line %d: there is text after the closing quote", n)
			}
			return splitCommas(s), ""
		}
		s, problem := plainWord(val, n)
		if problem != "" {
			return nil, problem
		}
		return splitCommas(s), ""
	}

	var paths []string
	indent := -1
	for k, l := range block {
		t := strings.TrimSpace(l)
		if t == "" || t[0] == '#' {
			continue
		}
		ln := n + 1 + k
		if t != "-" && !strings.HasPrefix(t, "- ") {
			return nil, fmt.Sprintf("line %d is not a list item of the form \"- pattern\"", ln)
		}
		depth := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 {
			indent = depth
		} else if depth != indent {
			return nil, fmt.Sprintf("line %d: the list items do not line up", ln)
		}
		item := strings.TrimSpace(t[1:])
		if item == "" {
			return nil, fmt.Sprintf("line %d is an empty list item", ln)
		}
		var s, problem string
		if item[0] == '"' || item[0] == '\'' {
			var rest string
			s, rest, problem = quoted(item, ln)
			if rest = strings.TrimSpace(rest); problem == "" && rest != "" && rest[0] != '#' {
				problem = fmt.Sprintf("line %d: there is text after the closing quote", ln)
			}
		} else {
			s, problem = plainWord(item, ln)
		}
		if problem != "" {
			return nil, problem
		}
		if s != "" {
			paths = append(paths, s)
		}
	}
	return paths, ""
}

// plainWord reads an unquoted value. A trailing comment is taken off. YAML
// gives some opening characters a meaning this reader does not carry out
// (a block of text, an anchor, a tag, a nested list or map), so a value
// that opens with one is refused rather than read as a pattern. An opening
// "*" is read as the pattern it plainly is.
func plainWord(s string, n int) (string, string) {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "\t#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if strings.IndexByte("|>&!%@`{[", s[0]) >= 0 {
		return "", fmt.Sprintf("line %d: a value that opens with %q is not read here; put the pattern in quotes", n, s[:1])
	}
	if s == "-" || s == "?" || strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "? ") {
		return "", fmt.Sprintf("line %d holds a list or a map inside a list, which is not read here", n)
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") {
		return "", fmt.Sprintf("line %d reads as a nested \"name: value\"; put the pattern in quotes", n)
	}
	return s, ""
}

// quoted reads the quoted word that s opens with and returns what it says
// and the text that follows the closing quote. Inside single quotes, two
// quotes stand for one. Inside double quotes, a backslash may come before a
// backslash or a double quote and nowhere else, since YAML would read other
// pairs as special characters.
func quoted(s string, n int) (word, rest, problem string) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			b.WriteByte('\'')
			i++
		case c == q:
			return b.String(), s[i+1:], ""
		case c == '\\' && q == '"':
			if i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '"') {
				b.WriteByte(s[i+1])
				i++
				continue
			}
			return "", "", fmt.Sprintf("line %d: a backslash inside double quotes must come before \\ or \"; use single quotes for a pattern that holds one", n)
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Sprintf("line %d: a quote is opened and never closed", n)
}

// flowList reads a list written on one line: [a, "b", 'c'].
func flowList(val string, n int) ([]string, string) {
	var items []string
	i := 1
	for {
		for i < len(val) && (val[i] == ' ' || val[i] == '\t') {
			i++
		}
		if i >= len(val) {
			return nil, fmt.Sprintf("line %d: the list is opened with [ and never closed", n)
		}
		if val[i] == ']' {
			i++
			break
		}
		var item string
		if val[i] == '"' || val[i] == '\'' {
			s, rest, problem := quoted(val[i:], n)
			if problem != "" {
				return nil, problem
			}
			item, i = s, len(val)-len(rest)
		} else {
			j := i
			for j < len(val) && val[j] != ',' && val[j] != ']' {
				if strings.IndexByte("[{}", val[j]) >= 0 {
					return nil, fmt.Sprintf("line %d: inside [ ] a pattern that holds a bracket or a brace must be in quotes", n)
				}
				j++
			}
			s, problem := plainWord(val[i:j], n)
			if problem != "" {
				return nil, problem
			}
			if s == "" {
				return nil, fmt.Sprintf("line %d: the list holds an empty item", n)
			}
			item, i = s, j
		}
		if item != "" {
			items = append(items, item)
		}
		for i < len(val) && (val[i] == ' ' || val[i] == '\t') {
			i++
		}
		switch {
		case i < len(val) && val[i] == ',':
			i++
		case i < len(val) && val[i] == ']':
		default:
			return nil, fmt.Sprintf("line %d: after an item in the list there should be , or ]", n)
		}
	}
	if rest := strings.TrimSpace(val[i:]); rest != "" && rest[0] != '#' {
		return nil, fmt.Sprintf("line %d: there is text after the closing ]", n)
	}
	return items, ""
}

// splitCommas splits one string of comma-separated patterns. A comma inside
// braces belongs to its pattern ("*.{ts,tsx}"), as does one after a
// backslash.
func splitCommas(s string) []string {
	var out []string
	depth, start := 0, 0
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				add(s[start:i])
				start = i + 1
			}
		}
	}
	add(s[start:])
	return out
}
