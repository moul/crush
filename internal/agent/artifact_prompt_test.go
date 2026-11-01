package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// The coder is asked to record what it produced only when there is a
// sidebar to show it in: the artifact tool is given to an interactive
// session alone, and a prompt that asked a `gnosh run` for it would send
// it after a tool it was never given.
func TestTheCoderIsAskedForArtifactsOnlyWhenInteractive(t *testing.T) {
	env := testEnv(t)
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(t.TempDir(), "crush.json"))
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(crushJSON), 0o644))

	coderPromptFor := func(interactive bool) string {
		cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
		require.NoError(t, err)
		cfg.SetupAgents()
		c, err := NewCoordinator(t.Context(), CoordinatorOptions{
			Config:      cfg,
			Sessions:    env.sessions,
			Messages:    env.messages,
			Permissions: env.permissions,
			History:     env.history,
			FileTracker: env.filetracker,
			Interactive: interactive,
		})
		require.NoError(t, err)
		coord := c.(*coordinator)
		require.NoError(t, coord.readyWg.Wait())
		return coord.agents[config.AgentCoder].(*sessionAgent).systemPrompt.Get()
	}

	interactive := coderPromptFor(true)
	require.Contains(t, interactive, "<artifacts>")
	require.Contains(t, interactive, "`artifact` tool",
		"the section has to name the tool it is asking for")
	require.NotContains(t, interactive, "{{", "the section is a template, not text")

	require.NotContains(t, coderPromptFor(false), "<artifacts>",
		"a run with no sidebar has no artifact tool to be asked for")
}

// The coder's tools match what it is told: the artifact tool is there
// for an interactive session and not for a run, which is what the
// prompt's gate rests on.
func TestTheArtifactToolFollowsTheSameGate(t *testing.T) {
	env := testEnv(t)
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(t.TempDir(), "crush.json"))
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(crushJSON), 0o644))

	has := func(interactive bool) bool {
		cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
		require.NoError(t, err)
		cfg.SetupAgents()
		c, err := NewCoordinator(t.Context(), CoordinatorOptions{
			Config:      cfg,
			Sessions:    env.sessions,
			Messages:    env.messages,
			Permissions: env.permissions,
			History:     env.history,
			FileTracker: env.filetracker,
			Interactive: interactive,
		})
		require.NoError(t, err)
		coord := c.(*coordinator)
		require.NoError(t, coord.readyWg.Wait())
		infos, err := coord.ToolInfos(t.Context())
		require.NoError(t, err)
		for _, info := range infos {
			if strings.EqualFold(info.Name, "artifact") {
				return true
			}
		}
		return false
	}

	require.True(t, has(true), "an interactive session records artifacts")
	require.False(t, has(false), "a run has no sidebar to record them in")
}

// fakePublisher stands in for the artifact server, to see that the
// coordinator hands the tool the one it was given.
type fakePublisher struct{ dir string }

func (f fakePublisher) Register(path, host string) (string, error) {
	return "http://192.0.2.9:7777/served/" + filepath.Base(path), nil
}

func (f fakePublisher) CacheDir() string { return f.dir }

// The coordinator gives the artifact tool the server that serves what it
// records. Without it every artifact would quietly be a path again, and
// no test of the tool on its own would notice.
func TestTheCoordinatorGivesTheToolItsPublisher(t *testing.T) {
	env := testEnv(t)
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(t.TempDir(), "crush.json"))
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(crushJSON), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "cut.mp4"), []byte("x"), 0o644))

	cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
	require.NoError(t, err)
	cfg.SetupAgents()
	c, err := NewCoordinator(t.Context(), CoordinatorOptions{
		Config:      cfg,
		Sessions:    env.sessions,
		Messages:    env.messages,
		Permissions: env.permissions,
		History:     env.history,
		FileTracker: env.filetracker,
		Interactive: true,
		Artifacts:   fakePublisher{dir: t.TempDir()},
	})
	require.NoError(t, err)
	coord := c.(*coordinator)
	require.NoError(t, coord.readyWg.Wait())

	built, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentCoder], false)
	require.NoError(t, err)
	var tool fantasy.AgentTool
	for _, candidate := range built {
		if candidate.Info().Name == "artifact" {
			tool = candidate
		}
	}
	require.NotNil(t, tool, "the interactive toolset has the artifact tool")

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  "artifact",
		Input: `{"label":"the cut","target":"cut.mp4"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "http://192.0.2.9:7777/served/cut.mp4",
		"the tool answers with the URL from the server the coordinator was given")
}
