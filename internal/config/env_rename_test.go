package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// Path settings read from the environment answer to their GNOSH_ names and
// to the KLASH_ and CRUSH_ names from before the renames, the newer name
// winning.
func TestPathSettingsAnswerToEveryName(t *testing.T) {
	inDir := func(d string) string { return filepath.Join(d, appName+".json") }
	asIs := func(d string) string { return d }
	for _, tc := range []struct {
		setting string
		got     func() string
		want    func(dir string) string
	}{
		{"GLOBAL_CONFIG", GlobalConfig, inDir},
		{"GLOBAL_DATA", GlobalConfigData, inDir},
		{"CACHE_DIR", GlobalCacheDir, asIs},
		{"SKILLS_DIR", func() string { return GlobalSkillsDirs()[0] }, asIs},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			oldest, old, newer := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("CRUSH_"+tc.setting, oldest)
			require.Equal(t, tc.want(oldest), tc.got(), "the first name still works")
			t.Setenv("KLASH_"+tc.setting, old)
			require.Equal(t, tc.want(old), tc.got(), "the name before this one wins over it")
			t.Setenv("GNOSH_"+tc.setting, newer)
			require.Equal(t, tc.want(newer), tc.got(), "the new name wins")
		})
	}
}

// A prefixed variable stands in for the plain one while providers are
// configured; GNOSH_ wins over KLASH_, KLASH_ over CRUSH_, and the plain
// value comes back after.
func TestPushPopCrushEnvPrefersGnosh(t *testing.T) {
	t.Setenv("PUSHPOP_TEST_KEY", "plain")
	t.Setenv("CRUSH_PUSHPOP_TEST_KEY", "old")
	restore := PushPopCrushEnv()
	require.Equal(t, "old", os.Getenv("PUSHPOP_TEST_KEY"))
	restore()
	require.Equal(t, "plain", os.Getenv("PUSHPOP_TEST_KEY"))

	t.Setenv("KLASH_PUSHPOP_TEST_KEY", "older")
	restore = PushPopCrushEnv()
	require.Equal(t, "older", os.Getenv("PUSHPOP_TEST_KEY"), "KLASH_ wins over CRUSH_")
	restore()

	t.Setenv("GNOSH_PUSHPOP_TEST_KEY", "new")
	restore = PushPopCrushEnv()
	require.Equal(t, "new", os.Getenv("PUSHPOP_TEST_KEY"), "GNOSH_ wins over CRUSH_")
	restore()
	require.Equal(t, "plain", os.Getenv("PUSHPOP_TEST_KEY"))
}

// Directories and config files take the name Gnosh, but where only one from
// before the renames exists, klash's or crush's, that one is used, so an
// existing setup and its sessions keep working without being moved. Once
// the new one exists, it wins.
func TestPathsPreferGnoshAndFallBackToOlderNames(t *testing.T) {
	mkdir := func(t *testing.T, parts ...string) string {
		t.Helper()
		p := filepath.Join(parts...)
		require.NoError(t, os.MkdirAll(p, 0o755))
		return p
	}
	touch := func(t *testing.T, parts ...string) string {
		t.Helper()
		p := filepath.Join(parts...)
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
		return p
	}

	t.Run("global config", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("CRUSH_GLOBAL_CONFIG", "") // set by TestMain for isolation
		require.Equal(t, filepath.Join(xdg, "gnosh", "gnosh.json"), GlobalConfig(), "a fresh setup gets the new name")

		mkdir(t, xdg, "crush")
		require.Equal(t, filepath.Join(xdg, "crush", "gnosh.json"), GlobalConfig(), "an existing crush directory is used")
		old := touch(t, xdg, "crush", "crush.json")
		require.Equal(t, old, GlobalConfig(), "and the crush.json in it")
		require.Equal(t, filepath.Join(xdg, "crush", "skills"), GlobalSkillsDirs()[0])

		// A gnosh directory made for something else must not hide the
		// crush one's config, hooks and skills.
		mkdir(t, xdg, "gnosh", "commands")
		require.Equal(t, old, GlobalConfig(), "a gnosh directory without a config does not take over")
		require.Equal(t, filepath.Join(xdg, "crush", "skills"), GlobalSkillsDirs()[0])

		newer := touch(t, xdg, "gnosh", "gnosh.json")
		require.Equal(t, newer, GlobalConfig(), "once the gnosh directory holds a config, it wins")
		require.Equal(t, filepath.Join(xdg, "gnosh", "skills"), GlobalSkillsDirs()[0])
	})

	t.Run("global config from the klash days", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("CRUSH_GLOBAL_CONFIG", "") // set by TestMain for isolation
		mkdir(t, xdg, "crush")
		touch(t, xdg, "crush", "crush.json")
		mkdir(t, xdg, "klash")
		klash := touch(t, xdg, "klash", "klash.json")
		require.Equal(t, klash, GlobalConfig(), "klash's config wins over crush's")
		require.Equal(t, filepath.Join(xdg, "klash", "skills"), GlobalSkillsDirs()[0])
	})

	t.Run("global config from a shell config alone", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("CRUSH_GLOBAL_CONFIG", "") // set by TestMain for isolation
		mkdir(t, xdg, "crush")
		touch(t, xdg, "crush", "crushrc")
		mkdir(t, xdg, "gnosh", "templates")
		require.Equal(t, filepath.Join(xdg, "crush", "gnosh.json"), GlobalConfig(),
			"a crush directory with only a crushrc is in use")
	})

	t.Run("global data", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		t.Setenv("CRUSH_GLOBAL_DATA", "") // set by TestMain for isolation
		require.Equal(t, filepath.Join(xdg, "gnosh", "gnosh.json"), GlobalConfigData())
		mkdir(t, xdg, "crush")
		old := touch(t, xdg, "crush", "crush.json")
		require.Equal(t, old, GlobalConfigData(), "an existing setup's data directory is used")
		mkdir(t, xdg, "gnosh", "logs")
		require.Equal(t, old, GlobalConfigData(), "its OAuth tokens are not hidden by a bare gnosh directory")
	})

	t.Run("cache", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CACHE_HOME", xdg)
		require.Equal(t, filepath.Join(xdg, "gnosh"), GlobalCacheDir())
		old := mkdir(t, xdg, "crush")
		require.Equal(t, old, GlobalCacheDir())
		klash := mkdir(t, xdg, "klash")
		require.Equal(t, klash, GlobalCacheDir(), "klash's before crush's")
	})

	t.Run("project data directory", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			exists []string
			want   string
		}{
			{"fresh project", nil, ".gnosh"},
			{"project from before the rename", []string{".crush"}, ".crush"},
			{"both", []string{".crush", ".gnosh"}, ".gnosh"},
			{"project from the klash days", []string{".klash"}, ".klash"},
			{"klash's and crush's", []string{".crush", ".klash"}, ".klash"},
			{"all three", []string{".crush", ".klash", ".gnosh"}, ".gnosh"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				workingDir := t.TempDir()
				for _, d := range tc.exists {
					mkdir(t, workingDir, d)
				}
				cfg := &Config{}
				cfg.setDefaults(workingDir, "")
				require.Equal(t, filepath.Join(workingDir, tc.want), cfg.Options.DataDirectory)
			})
		}
	})

	t.Run("project config files", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		cwd := t.TempDir()
		old := touch(t, cwd, "crush.json")
		newer := touch(t, cwd, "gnosh.json")
		paths := lookupConfigs(cwd)
		require.Contains(t, paths, old, "a crush.json is still read")
		require.Contains(t, paths, newer)
		require.Greater(t, slices.Index(paths, newer), slices.Index(paths, old), "gnosh.json is merged later, so it wins")
	})
}

// A log directory's log is gnosh.log, or the klash.log or crush.log from
// before the renames where only that one exists.
func TestLogFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.Equal(t, filepath.Join(dir, "gnosh.log"), LogFile(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crush.log"), nil, 0o644))
	require.Equal(t, filepath.Join(dir, "crush.log"), LogFile(dir), "an existing log keeps growing")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "klash.log"), nil, 0o644))
	require.Equal(t, filepath.Join(dir, "klash.log"), LogFile(dir), "klash's before crush's")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gnosh.log"), nil, 0o644))
	require.Equal(t, filepath.Join(dir, "gnosh.log"), LogFile(dir))
}

// A plain variable that was not set before is unset again after, not left
// behind empty for every command, hook and MCP server gnosh starts.
func TestPushPopCrushEnvUnsetsWhatWasUnset(t *testing.T) {
	t.Setenv("GNOSH_PUSHPOP_UNSET_KEY", "new")
	t.Cleanup(func() { os.Unsetenv("PUSHPOP_UNSET_KEY") })
	restore := PushPopCrushEnv()
	require.Equal(t, "new", os.Getenv("PUSHPOP_UNSET_KEY"))
	restore()
	_, set := os.LookupEnv("PUSHPOP_UNSET_KEY")
	require.False(t, set, "PUSHPOP_UNSET_KEY was not set before")
}
