package skills

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuiltinGnoSkill checks the gno skill is embedded with the reference
// files its SKILL.md points at, under the paths it names.
func TestBuiltinGnoSkill(t *testing.T) {
	t.Parallel()
	var gno *Skill
	for _, s := range DiscoverBuiltin() {
		if s.Name == "gno" {
			gno = s
		}
	}
	require.NotNil(t, gno, "gno builtin skill not found")
	require.Equal(t, BuiltinPrefix+"gno/SKILL.md", gno.SkillFilePath)
	require.Contains(t, gno.Description, "gno.land")
	refs := []string{
		"security-review.md",
		"toolchain.md",
		"cost.md",
		"testing.md",
		"go-differences.md",
	}
	for _, ref := range refs {
		location := BuiltinPrefix + "gno/references/" + ref
		require.Contains(t, gno.Instructions, location, "SKILL.md must point at %s", ref)
		content, err := BuiltinFS().ReadFile("builtin/" + strings.TrimPrefix(location, BuiltinPrefix))
		require.NoError(t, err)
		require.NotEmpty(t, content)
	}
}
