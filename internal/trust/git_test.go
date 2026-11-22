package trust

import (
	"path/filepath"
	"strings"
	"testing"
)

// git holds a subsection off its section's name with any blank its parser
// passes over, not a space alone.
func TestParseGitConfigBlankBeforeSubsection(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{" ", "  ", "\t", " \t ", "\r"} {
		text := "[filter" + blank + "\"x\"]\n\tsmudge = ./evil.sh\n"
		settings, ok := parseGitConfig(text)
		if !ok {
			t.Fatalf("%q: a settings file git reads must be read", text)
		}
		if len(settings) != 1 {
			t.Fatalf("%q: %d settings, want 1", text, len(settings))
		}
		s := settings[0]
		if s.name() != "filter.x.smudge" {
			t.Errorf("%q: the setting is %q, want \"filter.x.smudge\"", text, s.name())
		}
		if !runsProgram(s) {
			t.Errorf("%q: %s = %s names a program git runs, and is not reported", text, s.name(), s.value)
		}
	}
}

// A repository whose settings hold a program under such a subsection is
// listed like any other.
func TestGitProgramsBlankBeforeSubsection(t *testing.T) {
	t.Parallel()
	dir := repo(t, filepath.Join(root(t), "tabbed"), "[filter\t\"x\"]\n\tsmudge = ./evil.sh\n")
	got := GitPrograms(resolve(t, dir, ""))
	if len(got) != 1 || got[0].Kind != KindGitProgram || !strings.Contains(got[0].Text, "git filter.x.smudge = ") {
		t.Errorf("a filter held off its section by a tab: %+v", got)
	}
}
