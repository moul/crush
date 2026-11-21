package instructions

import (
	"strings"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	for _, c := range []struct {
		name  string
		text  string
		body  string
		paths []string
	}{
		{"none", "# Rules\ntext\n", "# Rules\ntext\n", nil},
		{"four dashes are not three", "----\npaths: a\n---\nx", "----\npaths: a\n---\nx", nil},
		{"empty", "---\n---\nbody", "body", nil},
		{"a list", "---\npaths:\n  - \"src/api/**/*.ts\"\n  - lib/**\n---\n\n# API\n", "# API\n", []string{"src/api/**/*.ts", "lib/**"}},
		{"a list at the field's own depth", "---\npaths:\n- a\n- b\n---\nx", "x", []string{"a", "b"}},
		{"one string of several", "---\npaths: \"a/**, b/*.{x,y}\"\n---\nbody", "body", []string{"a/**", "b/*.{x,y}"}},
		{"one plain string of several", "---\npaths: a, b ,c\n---\nx", "x", []string{"a", "b", "c"}},
		{"a list on one line", "---\npaths: [\"a\", 'b', c]\n---\nx", "x", []string{"a", "b", "c"}},
		{"a list on one line with a comma to spare", "---\npaths: [a, b, ]\n---\nx", "x", []string{"a", "b"}},
		{"an empty list", "---\npaths: []\n---\nbody", "body", nil},
		{"no value", "---\npaths:\n---\nbody", "body", nil},
		{"a plain word", "---\npaths: src/**\n---\nx", "x", []string{"src/**"}},
		{"a plain word that opens with a star", "---\npaths: **/*.ts\n---\nx", "x", []string{"**/*.ts"}},
		{"list items that open with a star", "---\npaths:\n  - *.go\n  - **/*.mod\n---\nx", "x", []string{"*.go", "**/*.mod"}},
		{"comments", "---\n# about\npaths:\n  # which files\n  - a # the first\n  - \"b # not a comment\"\n---\nx", "x", []string{"a", "b # not a comment"}},
		{"a comment where the value would be", "---\npaths: # which files\n  - \"src/api/**/*.ts\"\n---\nx", "x", []string{"src/api/**/*.ts"}},
		{"a comment where the value would be, over a list at the field's own depth", "---\npaths:  # which files\n- a\n- b\n---\nx", "x", []string{"a", "b"}},
		{"a comment and no value at all", "---\npaths: # none yet\n---\nx", "x", nil},
		{"a comment where another field's value would be", "---\ndescription: # about\npaths: a\n---\nx", "x", []string{"a"}},
		{"a comment after a list on one line", "---\npaths: [a, b] # note\n---\nx", "x", []string{"a", "b"}},
		{"a comment after a tab", "---\npaths: a\t# note\n---\nx", "x", []string{"a"}},
		{"spaces inside a list on one line", "---\npaths: [ a ,\t'b' ]\n---\nx", "x", []string{"a", "b"}},
		{"single quotes", "---\npaths:\n  - 'it''s'\n  - 'a\\[b'\n---\nx", "x", []string{"it's", "a\\[b"}},
		{"double quotes", "---\npaths: \"a\\\\b \\\"c\\\"\"\n---\nx", "x", []string{"a\\b \"c\""}},
		{"a quoted field name", "---\n\"paths\": a\n---\nx", "x", []string{"a"}},
		{"spaces after the dashes", "--- \npaths: a\n---  \nx", "x", []string{"a"}},
		{"blank lines after it are dropped", "---\npaths: a\n---\n\n\n\nx\n", "x\n", []string{"a"}},
		{"other fields are skipped whatever they hold", "---\ndescription: \"unclosed\nalwaysApply: false\nglobs: [a, [b\nnested:\n  deeper:\n    - x: |\n        text\n  key: {a: b\npaths:\n  - p\ntail: [\n---\nx", "x", []string{"p"}},
		{"only other fields", "---\ndescription: hello\n---\nx", "x", nil},
		{"windows line ends are gone by now, but a closing line with no newline works", "---\npaths: a\n---", "", []string{"a"}},
	} {
		body, paths, problem := splitFrontmatter(c.text)
		if problem != "" {
			t.Errorf("%s: problem %q", c.name, problem)
			continue
		}
		if body != c.body {
			t.Errorf("%s: body %q, want %q", c.name, body, c.body)
		}
		same(t, c.name, paths, c.paths)
	}
}

func TestSplitFrontmatterProblems(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		says string // what the problem must mention
	}{
		{"never closed", "---\npaths:\n  - a\n", "never closed"},
		{"prose", "---\njust some words\n---\nx", "line 2"},
		{"indented with no field", "---\n  paths: a\n---\nx", "line 2 is indented"},
		{"a list item with no field", "---\n- a\n---\nx", "line 2"},
		{"a list never closed", "---\ndescription: x\npaths: [a, b\n---\nx", "line 3"},
		{"a list with an empty item", "---\npaths: [a, , b]\n---\nx", "empty item"},
		{"a list in a list", "---\npaths: [a, [b]]\n---\nx", "in quotes"},
		{"a bare brace in a list on one line", "---\npaths: [*.{ts,tsx}]\n---\nx", "in quotes"},
		{"text after a list", "---\npaths: [a] b\n---\nx", "after the closing ]"},
		{"a block of text", "---\npaths: |\n  a\n---\nx", "line 2"},
		{"a folded block with nothing under it", "---\npaths: >\n---\nx", "opens with \">\""},
		{"a quote never closed", "---\npaths: \"a\n---\nx", "never closed"},
		{"a backslash in double quotes", "---\npaths: \"a\\[b\"\n---\nx", "single quotes"},
		{"text after a quote", "---\npaths: \"a\" b\n---\nx", "after the closing quote"},
		{"items that do not line up", "---\npaths:\n  - a\n   - b\n---\nx", "line 4"},
		{"something that is not an item", "---\npaths:\n  - a\n  b\n---\nx", "line 4 is not a list item"},
		{"an empty item", "---\npaths:\n  -\n---\nx", "line 3 is an empty list item"},
		{"a map in the list", "---\npaths:\n  - key: v\n---\nx", "line 3"},
		{"a list in the list", "---\npaths:\n  - - a\n---\nx", "line 3"},
		{"given twice", "---\npaths: a\npaths: b\n---\nx", "line 3 gives paths a second time"},
		{"a tag", "---\npaths: !!str a\n---\nx", "opens with \"!\""},
		{"an anchor", "---\npaths:\n  - &a x\n---\nx", "line 3"},
		{"a value and a list", "---\npaths: a\n  - b\n---\nx", "line 2"},
		{"a map in a list at the top", "---\n- key: v\n---\nx", "line 2"},
		{"text after a quoted item", "---\npaths:\n  - \"a\" b\n---\nx", "line 3: there is text after the closing quote"},
		{"a quote never closed inside a list on one line", "---\npaths: [\"a, b]\n---\nx", "never closed"},
		{"a tag inside a list on one line", "---\npaths: [a, !b]\n---\nx", "opens with \"!\""},
	} {
		body, paths, problem := splitFrontmatter(c.text)
		if problem == "" {
			t.Errorf("%s: no problem; paths %q", c.name, paths)
			continue
		}
		if !strings.Contains(problem, c.says) {
			t.Errorf("%s: problem %q does not mention %q", c.name, problem, c.says)
		}
		// Nothing is lost: the caller gets the whole text back.
		if body != c.text || paths != nil {
			t.Errorf("%s: body %q, paths %q", c.name, body, paths)
		}
	}
}

func TestSplitCommas(t *testing.T) {
	for in, want := range map[string][]string{
		"":                            nil,
		"a":                           {"a"},
		" a , b,,c ,":                 {"a", "b", "c"},
		"*.{ts,tsx}, lib/{a,{b,c}}/*": {"*.{ts,tsx}", "lib/{a,{b,c}}/*"},
		`a\,b, c`:                     {`a\,b`, "c"},
		"a}, b":                       {"a}", "b"},
	} {
		same(t, in, splitCommas(in), want)
	}
}
