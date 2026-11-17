package memjoin

import (
	"strings"
	"testing"

	"github.com/gnosh-ai/gnosh/internal/app/apptest"
	"github.com/gnosh-ai/gnosh/internal/prompt"
)

// How a model's answer becomes a name (integration-contract.md section
// 5.5).
func TestCleanName(t *testing.T) {
	for line, want := range map[string]string{
		"cache-key-fix":                              "cache-key-fix",
		"  Cache-Key-Fix.  ":                         "cache-key-fix",
		"`flaky-test-fix`":                           "flaky-test-fix",
		`"port flag rename"`:                         "port-flag-rename",
		"1. rename-code":                             "rename-code",
		"2) rename tests":                            "rename-tests",
		"- readme-update":                            "readme-update",
		"* readme_update!":                           "readme-update",
		"one two three four five six":                "one-two-three-four",
		"tls-cert-renewal-december-due":              "tls-cert-renewal-december",
		"extraordinarily-long-words-everywhere-here": "extraordinarily-long-words",
		"3-way-merge":                                "",
		"!!!":                                        "",
		"":                                           "",
		"Name: flaky-fix":                            "name-flaky-fix",
		"fix":                                        "fix",
	} {
		if got := cleanName(line); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", line, got, want)
		}
		if got := cleanName(line); len(got) > maxNameBytes {
			t.Errorf("cleanName(%q) = %q, which is longer than a name may be", line, got)
		}
	}
}

func TestReadNames(t *testing.T) {
	if got := readNames("flaky-fix\nand some more words", 1); len(got) != 1 || got[0] != "flaky-fix" {
		t.Errorf("one name is the first line: %q", got)
	}
	got := readNames("1. rename-code\n\n2. rename-tests\n- docs-update\n", 3)
	if strings.Join(got, " ") != "rename-code rename-tests docs-update" {
		t.Errorf("several names, one a line: %q", got)
	}
}

// Names are given to the texts they fit: a model that answers in another
// order does not swap a task's name with its todo's, a name that shares
// no word with its text is dropped, and no name is given twice.
func TestMatchNames(t *testing.T) {
	texts := []string{"rename the project", "rename it in the code", "rename it in the tests"}
	for _, c := range []struct {
		what  string
		names []string
		want  string
	}{
		{"in order", []string{"project-rename", "code-rename", "tests-rename"}, "project-rename code-rename tests-rename"},
		{"in another order", []string{"tests-rename", "project-rename", "code-rename"}, "project-rename code-rename tests-rename"},
		{"one shares no word with any text", []string{"project-rename", "banana-split", "tests-rename"}, "project-rename  tests-rename"},
		{"too few", []string{"project-rename"}, "project-rename  "},
		{"one given twice", []string{"rename", "rename", "tests-rename"}, "rename  tests-rename"},
		{"a word that begins another", []string{"proj-rename", "coding-rename", "test-rename"}, "proj-rename coding-rename test-rename"},
	} {
		if got := strings.Join(matchNames(texts, c.names), " "); got != c.want {
			t.Errorf("%s: matchNames = %q, want %q", c.what, got, c.want)
		}
	}
	if got := matchNames([]string{"find why TestFlaky fails"}, []string{"flaky-cause"}); got[0] != "" {
		t.Errorf("a short word does not stand for a longer one: %q", got)
	}
	if got := matchNames([]string{"find why TestFlaky fails"}, []string{"testflaky-cause"}); got[0] != "testflaky-cause" {
		t.Errorf("a name that shares a word with its text is kept: %q", got)
	}
}

// The namer, end to end: the texts a call keeps without naming them are
// named by the model in one request, mem takes the names, and the same
// text keeps its name for as long as the session lasts.
func TestNamerNamesTheTextsOfACall(t *testing.T) {
	w := newWorld(t,
		reply(memCall("c1", `mem.Todo("rename the project", "rename it in the code", "rename it in the tests").Push()`)),
		// Refused by mem (the name is a task, not a name), and sent
		// again with one more text: the texts named before keep their
		// names, and only the new one is asked about.
		reply(memCall("c2", "mem.Think(\"the module path is in go.mod\")\nmem.Todo(\"rename the project\")")),
		says("Planned."),
	)
	var asked []string
	w.script.onSide = func(kind string, r apptest.Request) *apptest.Reply {
		if kind != sideNamer {
			return nil
		}
		prompt := lastUser(t, r)
		asked = append(asked, prompt)
		if strings.HasPrefix(prompt, "1. ") {
			// The names come back in another order than the texts went.
			return &apptest.Reply{Text: "1. tests-rename\n2. project-rename\n3. code-rename"}
		}
		return &apptest.Reply{Text: "`module-path-note`"}
	}
	r := w.run("-y", "-p", "plan the rename")
	r.wantCode(t, 0)
	reqs := w.script.requests()

	has(t, "the plan's receipt", lastResult(t, reqs[1]),
		"added `project-rename` \"rename the project\"",
		"added `project-rename/code-rename` \"rename it in the code\"",
		"added `project-rename/tests-rename` \"rename it in the tests\"")
	if len(asked) != 2 {
		t.Fatalf("the namer was asked %d times, want twice: %q", len(asked), asked)
	}
	if want := prompt.Builtin().Namer([]string{"rename the project", "rename it in the code", "rename it in the tests"}).Prompt; asked[0] != want {
		t.Errorf("the namer's first request:\n%s\nwant:\n%s", asked[0], want)
	}
	if asked[1] != "the module path is in go.mod" {
		t.Errorf("the namer's second request asks about %q, want the one new text alone", asked[1])
	}
	sides := w.script.sides(sideNamer)
	if got := sides[0].JSON["max_tokens"]; got != float64(48) {
		t.Errorf("the namer was given room for %v tokens for three names, want 48", got)
	}
	if got := sides[1].JSON["max_tokens"]; got != float64(24) {
		t.Errorf("the namer was given room for %v tokens for one name, want 24", got)
	}

	// What mem was handed: every name by its text, the same both times.
	sent := w.memRequests()
	last := sent[len(sent)-1]
	if last.Names["rename the project"] != "project-rename" || last.Names["the module path is in go.mod"] != "module-path-note" {
		t.Errorf("the names handed to mem with the second call: %v", last.Names)
	}
	if first := sent[0].Names; first["rename it in the code"] != "code-rename" || first["rename it in the tests"] != "tests-rename" || len(first) != 3 {
		t.Errorf("the names handed to mem with the first call: %v", first)
	}
}

// Every order of the names is set against the same texts, so each text is
// split into words once and not once per order. Seven texts are the most
// matchNames tries every order of, and 5,040 orders of seven names are
// 35,280 fits: splitting the texts inside fit makes the work of naming one
// plan's todos grow with the factorial.
func TestMatchNamesSplitsEachTextOnce(t *testing.T) {
	texts := []string{
		"write the bus journal reader",
		"add gas and storage to each entry",
		"wire the reader into the status line",
		"document the journal format",
		"test the reader against a fixture",
		"handle a journal that was truncated",
		"report a journal the node no longer keeps",
	}
	names := []string{
		"write-bus-journal-reader", "add-gas-storage-entry", "wire-reader-status-line",
		"document-journal-format", "test-reader-fixture", "handle-truncated-journal",
		"report-journal-gone",
	}
	if len(texts) != maxMatched || len(names) != maxMatched {
		t.Fatalf("the case wants %d texts and names", maxMatched)
	}
	allocs := testing.AllocsPerRun(1, func() { matchNames(texts, names) })
	if allocs > 2000 {
		t.Errorf("matchNames allocated %.0f times for %d texts; it should split each text once, not once per order of the names", allocs, len(texts))
	}
}
