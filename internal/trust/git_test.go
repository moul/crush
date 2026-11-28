package trust

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseGitConfigTabInSectionHeader(t *testing.T) {
	t.Parallel()

	// Git accepts a tab, as well as a space, between a section's name and
	// its quoted subsection.
	settings, ok := parseGitConfig("[filter\t\"lfs\"]\n\tclean = evil\n")
	require.True(t, ok)
	require.Len(t, settings, 1)
	require.Equal(t, "filter.lfs.clean", settings[0].name())
	require.True(t, runsProgram(settings[0]))
}
