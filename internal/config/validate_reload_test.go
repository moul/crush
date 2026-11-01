package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// isolatedConfig writes crush.json into a fresh working directory with
// HOME and the XDG dirs pointed at a temp tree, so the developer's own
// global config can't leak into the result.
func isolatedConfig(t *testing.T, cfgJSON string) (workDir, dataDir, configPath string) {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))

	workDir = t.TempDir()
	dataDir = t.TempDir()
	configPath = filepath.Join(workDir, "crush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(cfgJSON), 0o600))
	return workDir, dataDir, configPath
}

// Both validators have to run at the load seam.
func TestLoad_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfgJSON string
		wantErr string
	}{
		{
			name:    "unknown hook event",
			cfgJSON: `{"hooks": {"pre_tool_use": [{"command": "true"}]}}`,
			wantErr: "invalid hook configuration",
		},
		{
			name:    "matcher on stop hook",
			cfgJSON: `{"hooks": {"Stop": [{"command": "true", "matcher": "^bash$"}]}}`,
			wantErr: "invalid hook configuration",
		},
		{
			name:    "unknown model type",
			cfgJSON: `{"models": {"medium": {"provider": "openai", "model": "gpt-4o"}}}`,
			wantErr: "invalid model configuration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir, dataDir, _ := isolatedConfig(t, tt.cfgJSON)
			_, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
			require.Error(t, err, "a config Crush can't honour must not load")
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// ...and at the reload seam, or a config that could never have been
// loaded from cold could still be installed into a running session by
// editing the file while Crush is up.
func TestReloadFromDisk_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		badJSON string
	}{
		{"unknown hook event", `{"hooks": {"banana": [{"command": "true"}]}}`},
		{"unknown model type", `{"models": {"medium": {"provider": "openai", "model": "gpt-4o"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goodJSON := `{"hooks": {"PreToolUse": [{"command": "exit 0"}]}}`
			workDir, dataDir, configPath := isolatedConfig(t, goodJSON)

			store, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
			require.NoError(t, err)
			require.Len(t, store.Config().Hooks["PreToolUse"], 1)

			// Someone edits the file badly while Crush is running.
			require.NoError(t, os.WriteFile(configPath, []byte(tt.badJSON), 0o600))

			err = store.ReloadFromDisk(context.Background())
			require.Error(t, err, "a reload must refuse what a cold load would refuse")
			require.Contains(t, err.Error(), "config reload rejected")

			// The running session keeps the config it had.
			require.Len(t, store.Config().Hooks["PreToolUse"], 1,
				"a rejected reload must leave the live config untouched")
		})
	}
}

// A config that is fine must survive both seams unchanged.
func TestLoadAndReload_AcceptValidConfig(t *testing.T) {
	cfgJSON := `{
        "hooks": {
            "PreToolUse": [{"matcher": "^bash$", "command": "exit 0"}],
            "Stop": [{"command": "exit 0"}]
        }
    }`
	workDir, dataDir, _ := isolatedConfig(t, cfgJSON)

	store, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
	require.NoError(t, err)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Len(t, store.Config().Hooks["PreToolUse"], 1)
	require.Len(t, store.Config().Hooks["Stop"], 1)
}

// Validate is the single seam both paths call; every check must be
// reachable through it.
func TestValidate_CoversEveryCheck(t *testing.T) {
	t.Parallel()

	badHooks := &config.Config{
		Hooks: map[string][]config.HookConfig{"nope": {{Command: "true"}}},
	}
	require.ErrorContains(t, badHooks.Validate(), "invalid hook configuration")

	badModels := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			"medium": {Provider: "openai", Model: "gpt-4o"},
		},
	}
	require.ErrorContains(t, badModels.Validate(), "invalid model configuration")

	require.NoError(t, (&config.Config{}).Validate())
}
