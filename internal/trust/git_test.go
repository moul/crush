package trust

import (
	"path/filepath"
	"strings"
	"testing"
)

// git ends a section's name at any white space and reads a quoted
// subsection after it (config.c, get_base_var), so [filter<tab>"lfs"]
// names filter.lfs, whose clean filter git runs on a status. GitPrograms
// must find it however the settings file spells the header.
func TestGitSectionNameEndsAtAnyWhiteSpace(t *testing.T) {
	t.Parallel()
	const config = "[filter\t\"lfs\"]\n\tclean = ./evil.sh\n"
	settings, ok := parseGitConfig(config)
	switch {
	case !ok:
		t.Fatal("a section header with a tab before its subsection must be read")
	case len(settings) != 1 || settings[0].name() != "filter.lfs.clean":
		t.Fatalf("settings: %+v", settings)
	case !runsProgram(settings[0]):
		t.Fatalf("%s names a program git runs", settings[0].name())
	}
	dir := repo(t, filepath.Join(root(t), "lfs"), config)
	got := GitPrograms(resolve(t, dir, ""))
	if len(got) != 1 || !strings.Contains(got[0].Text, "git filter.lfs.clean = ") {
		t.Errorf("GitPrograms: %+v", got)
	}
}
