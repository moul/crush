package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// The coordinator applies --system-prompt and --append-system-prompt (in
// RuntimeOverrides) when it builds its top-level agents: the coder's own
// instructions give way to the user's, its environment and the project's
// GNOSH.md stay, and the appended text ends the coder's and the plan
// agent's prompts.
func TestNewCoordinatorUsesTheSystemPromptFlags(t *testing.T) {
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
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "GNOSH.md"), []byte("Run the linter before every commit."), 0o644))

	prompts := func(replace, appended string) (coder, plan string) {
		cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
		require.NoError(t, err)
		cfg.SetupAgents()
		cfg.Overrides().SystemPrompt = replace
		cfg.Overrides().AppendSystemPrompt = appended
		c, err := NewCoordinator(t.Context(), CoordinatorOptions{
			Config:      cfg,
			Sessions:    env.sessions,
			Messages:    env.messages,
			Permissions: env.permissions,
			History:     env.history,
			FileTracker: env.filetracker,
		})
		require.NoError(t, err)
		coord := c.(*coordinator)
		require.NoError(t, coord.readyWg.Wait())
		// The <env> block goes to the conversation, not the system prompt
		// (prompt.SplitEnv); put it back where it stood, to read the whole.
		whole := func(a *sessionAgent) string {
			return strings.Replace(a.systemPrompt.Get(), envPointerText, a.environment.Get(), 1)
		}
		return whole(coord.agents[config.AgentCoder].(*sessionAgent)), whole(coord.agents[config.AgentPlan].(*sessionAgent))
	}

	coder, plan := prompts("", "")
	require.True(t, strings.HasPrefix(coder, "You are Gnosh, a powerful AI Assistant"), "without the flags the built-in instructions are used")
	require.Contains(t, coder, "Run the linter before every commit.")
	plainPlan := plan
	require.True(t, strings.HasPrefix(plan, "You are Gnosh in plan mode"), "the plan agent's own instructions come first")
	require.Contains(t, plan, "\n<env>\nWorking directory: "+env.workingDir, "the plan agent knows where it is, as the coder does")
	require.Contains(t, plan, "Run the linter before every commit.", "the plan agent plans with the project's GNOSH.md, as Claude Code's plan mode has CLAUDE.md")

	coder, plan = prompts("You answer only in haiku.", "End every answer with DONE.")
	require.True(t, strings.HasPrefix(coder, "You answer only in haiku.\n\n<env>\nWorking directory: "+env.workingDir), "the user's instructions come first, then the environment: %.120q", coder)
	require.NotContains(t, coder, "You are Gnosh, a powerful AI Assistant", "--system-prompt replaces the built-in instructions")
	require.Contains(t, coder, "Run the linter before every commit.", "the project's GNOSH.md is kept, as Claude Code keeps CLAUDE.md")
	require.True(t, strings.HasSuffix(coder, "\n\nEnd every answer with DONE."), "--append-system-prompt ends the coder's prompt")
	require.Equal(t, plainPlan+"\n\nEnd every answer with DONE.", plan, "the plan agent keeps its own instructions and gets the appended text")
}

// --system-prompt keeps the built-in template from its <env> line on, so
// the line must be there.
func TestBuiltinCoderTemplateHasAnEnvLine(t *testing.T) {
	require.True(t, strings.HasPrefix(builtinCoderContext(), "<env>\n"))
	require.Contains(t, builtinCoderContext(), "{{range .ContextFiles}}")
}

// --permission-mode plan (RuntimeOverrides.MainAgent) starts the session
// with the plan agent, in the TUI and in gnosh run alike.
func TestNewCoordinatorStartsWithThePlanAgent(t *testing.T) {
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
	for _, start := range []string{"", config.AgentPlan} {
		cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
		require.NoError(t, err)
		cfg.SetupAgents()
		cfg.Overrides().MainAgent = start
		c, err := NewCoordinator(t.Context(), CoordinatorOptions{
			Config:      cfg,
			Sessions:    env.sessions,
			Messages:    env.messages,
			Permissions: env.permissions,
			History:     env.history,
			FileTracker: env.filetracker,
		})
		require.NoError(t, err)
		coord := c.(*coordinator)
		agent, name := coord.activeAgent()
		want := config.AgentCoder
		if start == config.AgentPlan {
			want = config.AgentPlan
		}
		require.Equal(t, want, name)
		require.Same(t, coord.agents[want], agent)
		// Either agent may send the session's first prompt; what its
		// SessionStart hooks know of the session is one record for both.
		require.Same(t, coord.agents[config.AgentCoder].(*sessionAgent).sessionStarts,
			coord.agents[config.AgentPlan].(*sessionAgent).sessionStarts)
	}
}

// Directories added with --add-dir are named in the coder's environment,
// so the model knows it may work there, as Claude Code's prompt names them.
func TestCoderPromptNamesTheAddedDirs(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	p, err := coderPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	plain, err := p.Build(t.Context(), "", "", store)
	require.NoError(t, err)
	require.NotContains(t, plain, "Additional working directories")

	store.Overrides().AddedDirs = []string{"/src/gno", "/src/bubblerumble"}
	withDirs, err := p.Build(t.Context(), "", "", store)
	require.NoError(t, err)
	require.Contains(t, withDirs, "Working directory: ")
	require.Contains(t, withDirs, "\nAdditional working directories: /src/gno, /src/bubblerumble\nIs directory a git repo:")
}

// The task sub-agent's environment names them too, as Claude Code's
// environment does for every agent.
func TestTaskPromptNamesTheAddedDirs(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	p, err := taskPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	plain, err := p.Build(t.Context(), "", "", store)
	require.NoError(t, err)
	require.NotContains(t, plain, "Additional working directories")

	store.Overrides().AddedDirs = []string{"/src/gno", "/src/bubblerumble"}
	withDirs, err := p.Build(t.Context(), "", "", store)
	require.NoError(t, err)
	require.Contains(t, withDirs, "\nAdditional working directories: /src/gno, /src/bubblerumble\nIs directory a git repo:")
}

// The task sub-agent is told what Claude Code tells its Explore agent: a
// read-only search specialist that searches in parallel and reports what it
// found, where it was told that one-word answers were best; and the agent
// tool says what to use it for and asks for the breadth of the search.
func TestTaskAgentIsAnExploreAgent(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	p, err := taskPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	text, err := p.Build(t.Context(), "", "", store)
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(text, "You are a file search specialist for Gnosh."), text)
	for _, want := range []string{
		"=== CRITICAL: READ-ONLY MODE - NO FILE MODIFICATIONS ===",
		"Wherever possible you should try to spawn multiple parallel tool calls for grepping and reading files",
		"Complete the user's search request efficiently and report your findings clearly.",
		"Any file paths you return in your final report MUST be absolute.",
		"\n<env>\nWorking directory: ",
	} {
		require.Contains(t, text, want)
	}
	require.NotContains(t, text, "One word answers")
	require.Contains(t, agentToolDescription, `When calling, specify search breadth: "quick" for a single targeted lookup`)
	require.Contains(t, agentToolDescription, "Do NOT use it for code review")
}

// After /add-dir, the coder's prompt is built again and names the
// directory, as --add-dir's does from the start, and so are the prompts of
// the sub-agents its agent tool runs.
func TestRebuildSystemPromptsNamesAnAddedDir(t *testing.T) {
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
	})
	require.NoError(t, err)
	coord := c.(*coordinator)
	require.NoError(t, coord.readyWg.Wait())
	// The working directories are in the <env> block, which the agents send
	// as the first user message (prompt.SplitEnv).
	coder := func() string { return coord.agents[config.AgentCoder].(*sessionAgent).environment.Get() }
	task := func() string {
		coord.agentMu.RLock()
		defer coord.agentMu.RUnlock()
		return coord.taskAgents[config.AgentCoder].agent.(*sessionAgent).environment.Get()
	}
	require.NotContains(t, coder(), "Additional working directories")
	require.Contains(t, task(), "Working directory: ")
	require.NotContains(t, task(), "Additional working directories")

	cfg.AddDir("/src/gno")
	require.NotContains(t, coder(), "Additional working directories", "built again only when asked")
	require.NoError(t, coord.RebuildSystemPrompts(t.Context()))
	require.Contains(t, coder(), "\nAdditional working directories: /src/gno\n")
	require.Contains(t, task(), "\nAdditional working directories: /src/gno\n")
	planAgent := coord.agents[config.AgentPlan].(*sessionAgent)
	require.Contains(t, planAgent.environment.Get(), "\nAdditional working directories: /src/gno\n")
	require.True(t, strings.HasPrefix(planAgent.systemPrompt.Get(), "You are Gnosh in plan mode"))
	require.NotContains(t, planAgent.systemPrompt.Get(), "Working directory: ", "the system prompt is the same in every session")
}

// envPointerText is the line prompt.SplitEnv leaves where <env> was.
var envPointerText = func() string {
	rest, _ := prompt.SplitEnv("<env>x</env>")
	return rest
}()
