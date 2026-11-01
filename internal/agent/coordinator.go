package agent

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	promptpkg "github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/gnovm"
	gnovmtools "github.com/charmbracelet/crush/internal/gnovm/tools"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	openaioauth "github.com/charmbracelet/crush/internal/oauth/openai"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

// Coordinator errors.
var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errPlanAgentNotConfigured          = errors.New("plan agent not configured")
	errMainAgentNotFound               = errors.New("main agent not found")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errLargeModelNotSelected           = errors.New("large model not selected")
	errSmallModelNotSelected           = errors.New("small model not selected")
	errLargeModelProviderNotConfigured = errors.New("large model provider not configured")
	errSmallModelProviderNotConfigured = errors.New("small model provider not configured")
	errLargeModelNotFound              = errors.New("large model not found in provider config")
	errSmallModelNotFound              = errors.New("small model not found in provider config")
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
	"gpt-5.6-luna":  true,
	"gpt-5.6-terra": true,
	"gpt-5.6-sol":   true,
	"gpt-6-astra":   true,
	"gpt-6-luna":    true,
	"gpt-6-sol":     true,
	"gpt-6.1-sol":   true,
	"grok-4.5":      true,
	"grok-4.6":      true,
	"grok-4.7":      true,
}

// OpenCode models that use the Anthropic Messages API instead of Chat
// Completions. Which endpoint serves each model differs per provider, see
// https://opencode.ai/docs/zen and https://opencode.ai/docs/go.
func isOpenCodeMessagesModel(providerID, modelID string) bool {
	switch providerID {
	case string(catwalk.InferenceProviderOpenCodeGo):
		return strings.HasPrefix(modelID, "minimax-") ||
			strings.HasPrefix(modelID, "qwen3.6-") ||
			strings.HasPrefix(modelID, "qwen3.7-") ||
			strings.HasPrefix(modelID, "qwen3.8-")
	case string(catwalk.InferenceProviderOpenCodeZen):
		return strings.HasPrefix(modelID, "claude-") ||
			strings.HasPrefix(modelID, "qwen3.5-") ||
			strings.HasPrefix(modelID, "qwen3.6-") ||
			strings.HasPrefix(modelID, "qwen3.7-") ||
			strings.HasPrefix(modelID, "qwen3.8-")
	}
	return false
}

// OpenCode models that use the OpenAI Responses API instead of Chat
// Completions. See https://opencode.ai/docs/zen and https://opencode.ai/docs/go.
func isOpenCodeResponsesModel(modelID string) bool {
	return strings.HasPrefix(modelID, "gpt-") ||
		strings.HasPrefix(modelID, "grok-") ||
		strings.HasPrefix(modelID, "muse-spark-")
}

type Coordinator interface {
	SetMainAgent(agentName string) error
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the backend.runAgent /
	// Coordinator / sessionAgent.Run layers: it reaches
	// sessionAgent.Run as SessionAgentCall.Accepted, where it is
	// consumed under dispatchMu once the accepted -> (cancel-on-entry |
	// queued | active) transition is chosen.
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	// ListSettledMessages reads a session's transcript, first settling any
	// tool calls a dead process left without a reply.
	ListSettledMessages(ctx context.Context, sessionID string) ([]message.Message, error)
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	PopQueuedPrompts(sessionID string) []message.PoppedPrompt
	ClearQueue(sessionID string)
	// Summarize summarizes the session, with instructions the text given
	// after /compact ("" for none).
	Summarize(ctx context.Context, sessionID, instructions string) error
	Model() Model
	UpdateModels(ctx context.Context) error
	// ToolInfos builds the coder agent's tools and returns what the model is
	// told about each one (name, description, parameters): the part of every
	// request that is not the prompt template. Behind `crush prompt tools`.
	ToolInfos(ctx context.Context) ([]fantasy.ToolInfo, error)
	// ToolNames names the tools the main agent offers the model now:
	// the coder's or, in plan mode, the plan agent's, MCP tools included.
	ToolNames() []string
	GenerateTitle(ctx context.Context, sessionID, prompt string)
	// ContextUsage estimates what fills a session's context window, as
	// Claude Code's /context does ("" for a new session).
	ContextUsage(ctx context.Context, sessionID string) (ContextUsage, error)
	// GenerateSessionName names a session from the end of its
	// conversation, as Claude Code's /rename does with no name, and
	// returns the name.
	GenerateSessionName(ctx context.Context, sessionID string) (string, error)
	// SetGoal binds the session's goal to a goal in mem (see Goal), in
	// place of any it had, and says what starts the agent on it; Goal says
	// what it is; ClearGoal unbinds it, and says what clearing it did.
	SetGoal(ctx context.Context, sessionID, condition string) (GoalChange, error)
	Goal(ctx context.Context, sessionID string) (Goal, bool)
	ClearGoal(ctx context.Context, sessionID string) (GoalChange, error)
	// Rewound forgets what the session was given in the messages a
	// /rewind cut, removed, as Claude Code forgets what the cut
	// conversation read: the context files of the subdirectories it was
	// given there, which it is given again when it next touches a file
	// in one.
	Rewound(sessionID string, removed []string)
	// Branched records that the session was made as a copy of another to
	// go on in, by /branch or --fork-session, so the first prompt to it
	// runs the SessionStart hooks with source "fork", as Claude Code's do.
	Branched(sessionID string)
	// RebuildSystemPrompts builds the system prompts of the coder and plan
	// agents and of their task sub-agents again from the config as it now
	// is, as after /add-dir adds a directory their environments name.
	RebuildSystemPrompts(ctx context.Context) error
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	questions   question.Service
	history     history.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	interactive bool
	// memErased and memRules are CoordinatorOptions'.
	memErased func(ctx context.Context, sessionID string, e gnovm.MemErasure)
	memRules  func(ctx context.Context, sessionID string, rules []gnovm.MemRule)
	// gnovms is the App's hold on the GnoVM machines; nil gives no gnovm
	// tools.
	gnovms    *gnovm.Lazy
	artifacts tools.ArtifactPublisher
	// named is the files each session has been told changed since it read
	// them, shared across buildTools calls so a coordinator rebuild does
	// not reset the dedup state.
	named *namedChanges
	// jobs tells a session when a background job its agent started ends.
	jobs *jobWatcher

	// agentMu guards mainAgent and mainAgentName: SetMainAgent runs on
	// HTTP handler goroutines while runs, cancels, and probes read the
	// current agent from their own goroutines.
	agentMu       sync.RWMutex
	mainAgent     SessionAgent
	mainAgentName string
	agents        map[string]SessionAgent
	// taskAgents are the sub-agents of the agent tools, by the agent whose
	// tool runs each, with their prompts (agentTool). Guarded by agentMu.
	taskAgents map[string]promptedAgent

	// sessionStarts is what the SessionStart hooks of the agents the
	// coordinator builds know of the sessions the process started: one
	// for all of them, so a session started in code mode counts as
	// started in plan mode, and a summary made in either fires "compact"
	// for the next prompt in either. Sub-agents fire no SessionStart
	// hooks, so sharing it with them changes nothing.
	sessionStarts *sessionStartState

	// instructionsLoaded is what the InstructionsLoaded hooks of the
	// agents the coordinator builds know of the sessions the process has
	// sent context files to: one for all of them, as sessionStarts is.
	instructionsLoaded *instructionsLoadedState

	// goals are the sessions' /goal bindings, which every agent the
	// coordinator makes reads as its turns end.
	goals *goalBindings

	// frames are the sessions' stacks of frames in mem (FRAMES.md), which
	// gnovm_run reads and tells, and every agent cuts its context by.
	frames *frameStacks

	// subdirectoryGiven records the subdirectories' context files each
	// session has been given since its last summary, with the message
	// whose tool call they were given with ("" when not known); see
	// subdirectoryContext and Rewound. Nil, nothing is given.
	subdirectoryMu    sync.Mutex
	subdirectoryGiven map[string]string

	// viewed keeps the file reads each session's context holds, so that
	// view gives an unchanged file's lines once; nil keeps none.
	viewed *viewedFiles

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	readyWg errgroup.Group
}

// CoordinatorOptions holds the dependencies for NewCoordinator. Using a
// struct keeps the constructor self-documenting and avoids a long
// positional parameter list.
type CoordinatorOptions struct {
	Config      *config.ConfigStore
	Sessions    session.Service
	Messages    message.Service
	Permissions permission.Service
	Questions   question.Service
	History     history.Service
	FileTracker filetracker.Service
	LSPManager  *lsp.Manager
	Notify      pubsub.Publisher[notify.Notification]
	RunComplete pubsub.Publisher[notify.RunComplete]
	Skills      *skills.Manager
	Interactive bool
	// GnoVMs is the App's hold on the GnoVM machines (internal/gnovm),
	// or nil for none.
	GnoVMs *gnovm.Lazy
	// MemErased takes what a session's mem call erased out of gnosh's own
	// copies of mem's text (app.memErased); nil leaves them.
	MemErased func(ctx context.Context, sessionID string, e gnovm.MemErasure)
	// MemRules tells the user of the rules a session's mem call set,
	// changed or removed; nil tells no one.
	MemRules func(ctx context.Context, sessionID string, rules []gnovm.MemRule)
	// Artifacts serves the deliverables the agent records with the
	// artifact tool, so the user can open one from the machine they are
	// at. Nil records them as paths.
	Artifacts tools.ArtifactPublisher
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var allSkills, activeSkills []*skills.Skill
	if opts.Skills != nil {
		allSkills = opts.Skills.AllSkills()
		activeSkills = opts.Skills.ActiveSkills()
	} else {
		allSkills, activeSkills = discoverSkills(opts.Config)
	}
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                opts.Config,
		sessions:           opts.Sessions,
		messages:           opts.Messages,
		permissions:        opts.Permissions,
		questions:          opts.Questions,
		history:            opts.History,
		filetracker:        opts.FileTracker,
		lspManager:         opts.LSPManager,
		notify:             opts.Notify,
		runComplete:        opts.RunComplete,
		gnovms:             opts.GnoVMs,
		memErased:          opts.MemErased,
		memRules:           opts.MemRules,
		agents:             make(map[string]SessionAgent),
		sessionStarts:      newSessionStartState(),
		instructionsLoaded: newInstructionsLoadedState(),
		goals:              &goalBindings{sessions: opts.Sessions, gnovms: opts.GnoVMs, project: opts.Config.WorkingDir()},
		allSkills:          allSkills,
		activeSkills:       activeSkills,
		skillTracker:       skillTracker,
		interactive:        opts.Interactive,
		artifacts:          opts.Artifacts,
		named:              newNamedChanges(),

		subdirectoryGiven: map[string]string{},
		viewed:            newViewedFiles(opts.Sessions),
		frames:            newFrameStacks(opts.Sessions, opts.Messages, opts.FileTracker, opts.Config.WorkingDir()),
	}
	c.frames.forgetGiven = c.forgetGiven
	c.frames.notify = opts.Notify
	c.jobs = &jobWatcher{run: func(ctx context.Context, sessionID, prompt string) error {
		_, err := c.Run(ctx, sessionID, prompt)
		return err
	}}

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	coderSystemPrompt, err := c.coderSystemPrompt()
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, coderSystemPrompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.agents[config.AgentCoder] = agent

	planCfg, ok := c.cfg.Config().Agents[config.AgentPlan]
	if !ok {
		return nil, errPlanAgentNotConfigured
	}

	planSystemPrompt, err := c.planSystemPrompt()
	if err != nil {
		return nil, err
	}

	planAgent, err := c.buildAgent(ctx, planSystemPrompt, planCfg, false)
	if err != nil {
		return nil, err
	}
	c.agents[config.AgentPlan] = planAgent

	c.mainAgent = agent
	c.mainAgentName = config.AgentCoder
	if c.cfg.Overrides().MainAgent == config.AgentPlan {
		c.mainAgent = planAgent
		c.mainAgentName = config.AgentPlan
	}
	return c, nil
}

// onDemandContextLimit is how many characters of a context file, given on
// demand for a subdirectory, a session gets: 40,000, the per-file limit
// Claude Code applies in a 200K-token context window. Files loaded at the
// start of a session are not cut.
const onDemandContextLimit = 40000

// holdTurn holds a session's turn lock while it is busy, so peer sessions
// working the same plan in mem see it working (gnovm's Lazy.HoldTurn).
func (c *coordinator) holdTurn(ctx context.Context, sessionID string) func() {
	if c.gnovms == nil {
		return func() {}
	}
	return c.gnovms.HoldTurn(ctx, sessionID)
}

// memBlock is how a main agent tells a session what the project's mem
// holds, as it starts and after each summary; a sub-agent's sessions are
// not told.
func (c *coordinator) memBlock(isSubAgent bool) func(sessionID string) string {
	if c.gnovms == nil || isSubAgent {
		return nil
	}
	return func(sessionID string) string { return c.gnovms.MemBlock(c.cfg.WorkingDir(), sessionID) }
}

// memChanged is how a main agent shows a session the user comes back to
// what the project's mem holds, when it changed since; a sub-agent's
// sessions are not shown.
func (c *coordinator) memChanged(isSubAgent bool) func(sessionID string, since time.Time) string {
	if c.gnovms == nil || isSubAgent {
		return nil
	}
	return func(sessionID string, since time.Time) string {
		return c.gnovms.MemChanged(c.cfg.WorkingDir(), sessionID, since)
	}
}

// memRecall is how a main agent gives a prompt the user typed, or a
// channel's message, what the project's mem holds close to it in meaning;
// a sub-agent's prompts get none. memSummarized goes with it.
func (c *coordinator) memRecall(isSubAgent bool) func(ctx context.Context, sessionID, prompt string) string {
	if c.gnovms == nil || isSubAgent {
		return nil
	}
	return func(ctx context.Context, sessionID, prompt string) string {
		return c.gnovms.MemRecall(ctx, c.cfg.WorkingDir(), sessionID, prompt)
	}
}

func (c *coordinator) memSummarized(isSubAgent bool) func(sessionID string) {
	if c.gnovms == nil || isSubAgent {
		return nil
	}
	return c.gnovms.MemSummarized
}

// subdirectoryContext returns, for a file a session views, writes or
// edits, the instructions in the context files of the subdirectories
// between the working directory and the file that the session has not
// been given since its last summary, as Claude Code loads a
// subdirectory's CLAUDE.md once it reads, writes or edits a file there.
// A file over onDemandContextLimit characters is given cut, with a note
// to read the rest with the view tool. Started at the root of Jae's gno
// checkout, Gnosh never read misc/gnoe2e's own AGENTS.md. A call with no
// session gives nothing: what is given is recorded per session. A
// relative filePath, as lsp_replace_symbol passes on as the model wrote
// it, is taken from the working directory.

func (c *coordinator) subdirectoryContext(ctx context.Context, sessionID, filePath string) string {
	if c.subdirectoryGiven == nil || sessionID == "" {
		return ""
	}
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(c.cfg.WorkingDir(), filePath)
	}
	files := prompt.SubdirectoryContextFiles(c.cfg.Config().Options.ContextPaths, c.cfg, filePath)
	if len(files) == 0 {
		return ""
	}
	// A summary leaves behind what the session was given before it.
	var summary string
	isSubAgentSession := false
	if sess, err := c.sessions.Get(ctx, sessionID); err == nil {
		summary = sess.SummaryMessageID
		isSubAgentSession = sess.ParentSessionID != ""
	}
	var b strings.Builder
	var newFiles []promptContextFile
	c.subdirectoryMu.Lock()
	for _, f := range files {
		key := sessionID + "\x00" + summary + "\x00" + f.Path
		if _, given := c.subdirectoryGiven[key]; given {
			continue
		}
		c.subdirectoryGiven[key] = tools.GetMessageFromContext(ctx)
		newFiles = append(newFiles, promptContextFile{path: f.Path, parent: f.Parent})
		fmt.Fprintf(&b, "<file path=\"%s\">\n%s\n</file>\n", f.Path, onDemandContextFile(f))
	}
	c.subdirectoryMu.Unlock()
	// Fire the InstructionsLoaded hooks for the new files, as in Claude
	// Code: load_reason "nested_traversal", trigger_file_path the file
	// the tool touched. They run asynchronously. Sub-agent sessions do
	// not fire the hooks, as the session-start path does not.
	if len(newFiles) > 0 && !isSubAgentSession {
		fireInstructionsLoadedHooks(c.cfg, sessionID, newFiles, hooks.InstructionsLoadedNestedTraversal, filePath)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n<project_context>\nThis file's directory has context files of its own. Follow their instructions for the files there:\n" + b.String() + "</project_context>\n"
}

// memGoals shows the goal a mem call changed as the session's task list
// (gnovmtools.SessionTodos), with the frame the session is in named on
// the step in progress (frameStacks.decorateTodos).
func (c *coordinator) memGoals(ctx context.Context, sessionID string, goals []gnovm.MemGoal) {
	if sessionID == "" || len(goals) == 0 {
		return
	}
	todos := c.frames.decorateTodos(ctx, sessionID, gnovmtools.MemTodos(goals[len(goals)-1], sessionID))
	if _, err := c.sessions.SetTodos(ctx, sessionID, todos); err != nil {
		slog.Warn("mem's goal could not be shown as the session's todos", "error", err)
	}
}

// viewedFiles is what keeps the file reads view gives, or nil, never a
// nil *viewedFiles in the interface.
func (c *coordinator) viewedFiles() tools.ViewedFiles {
	if c.viewed == nil {
		return nil
	}
	return c.viewed
}

// SetGoal binds the session's goal to a goal in mem, in place of any it
// had.
func (c *coordinator) SetGoal(ctx context.Context, sessionID, condition string) (GoalChange, error) {
	return c.goals.set(ctx, sessionID, condition)
}

// Goal is the session's goal, and false when it has none.
func (c *coordinator) Goal(ctx context.Context, sessionID string) (Goal, bool) {
	return c.goals.get(ctx, sessionID)
}

// ClearGoal unbinds the session's goal, and says what that did.
func (c *coordinator) ClearGoal(ctx context.Context, sessionID string) (GoalChange, error) {
	return c.goals.clear(ctx, sessionID)
}

// compactPoint is the running token total at which a session on a model
// with context window window compacts, as /context reports it: the
// variables and the auto_compact_window option read (see compactAt), or
// 0 when auto-summarize is off.
func (c *coordinator) compactPoint(window int64) int64 {
	options := c.cfg.Config().Options
	if options.DisableAutoSummarize {
		return 0
	}
	return compactAt(window, options.AutoCompactWindow, true)
}

// Rewound forgets the subdirectories' context files the session was
// given in the messages a rewind cut, and those given it with no message
// known: it is given them again when it next touches a file there. So
// too the file reads view gave it there.
func (c *coordinator) Rewound(sessionID string, removed []string) {
	c.forgetGiven(sessionID, removed)
	// The session's stack of frames is derived again from what remains.
	c.frames.forget(sessionID)
}

// forgetGiven forgets what the session was given in the messages removed,
// a rewind's or a popped frame's: the file reads view gave it there, and
// the subdirectories' context files, with those given it with no message
// known.
func (c *coordinator) forgetGiven(sessionID string, removed []string) {
	cut := make(map[string]bool, len(removed))
	for _, id := range removed {
		cut[id] = true
	}
	c.viewed.forget(sessionID, cut)
	prefix := sessionID + "\x00"
	c.subdirectoryMu.Lock()
	defer c.subdirectoryMu.Unlock()
	for key, messageID := range c.subdirectoryGiven {
		if strings.HasPrefix(key, prefix) && (messageID == "" || cut[messageID]) {
			delete(c.subdirectoryGiven, key)
		}
	}
}

// Branched marks the session a fork for the SessionStart hooks of its
// first prompt.
func (c *coordinator) Branched(sessionID string) {
	c.sessionStarts.markForked(sessionID)
}

// onDemandContextFile cuts a context file over the on-demand limit to
// it, naming the file and telling the session to read the rest with the
// view tool, as Claude Code does. The limit is in characters, not bytes:
// a multi-byte UTF-8 character counts as one.
func onDemandContextFile(f prompt.ContextFile) string {
	runes := []rune(f.Content)
	if len(runes) <= onDemandContextLimit {
		return f.Content
	}
	return string(runes[:onDemandContextLimit]) + fmt.Sprintf("\n\n(%s is over %d characters. You have only been given its start; read the rest with the view tool.)", f.Path, onDemandContextLimit)
}

// permissionMode is a session's permission mode as Claude Code names it
// to hooks, for any agent but the plan agent: bypassPermissions when its
// permission requests are all granted (yolo, or a gnosh run's session and
// its sub-agents'), acceptEdits or dontAsk under that --permission-mode,
// else default.
func (c *coordinator) permissionMode(sessionID string) string {
	switch {
	case c.permissions != nil && (c.permissions.SkipRequests() || c.permissions.IsSessionAutoApproved(sessionID)):
		return config.PermissionModeBypass
	case c.cfg != nil && c.cfg.Overrides().AcceptEdits:
		return config.PermissionModeAcceptEdits
	case c.cfg != nil && c.cfg.Overrides().DontAsk:
		return config.PermissionModeDontAsk
	default:
		return config.PermissionModeDefault
	}
}

// activeAgent returns the coordinator's current main agent and its config
// name as one snapshot. Callers use the snapshot for the whole operation,
// so a SetMainAgent racing mid-flight never splits a run, model refresh, or
// summarize across two agents.
func (c *coordinator) activeAgent() (SessionAgent, string) {
	c.agentMu.RLock()
	defer c.agentMu.RUnlock()
	return c.mainAgent, c.mainAgentName
}

// currentAgent returns the current main agent.
func (c *coordinator) currentAgent() SessionAgent {
	agent, _ := c.activeAgent()
	return agent
}

func (c *coordinator) SetMainAgent(agentName string) error {
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	agent, ok := c.agents[agentName]
	if !ok {
		return fmt.Errorf("%w: %s", errMainAgentNotFound, agentName)
	}
	c.mainAgent = agent
	c.mainAgentName = agentName
	return nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, accept, sessionID, prompt, attachments...)
}

// run is the shared implementation behind Run and RunAccepted. When
// accept is non-nil it is threaded onto the SessionAgentCall as
// Accepted so sessionAgent.Run can consume the accept reservation under
// dispatchMu; when nil (the in-process/local path) no accept tracking
// applies.
func (c *coordinator) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// MCP servers connect asynchronously (see mcp.Initialize).
	//
	// Interactive runs never wait for that to finish: the tool list below
	// is built from whatever is registered right now, servers still
	// connecting are simply absent from this run's palette, and they are
	// picked up by later runs once they register and publish
	// EventToolsListChanged. Blocking here froze the TUI for the duration
	// of the slowest server's connect timeout whenever a prompt was sent
	// before initialization finished — most visibly on the first message.
	//
	// Non-interactive runs get a single shot at the tool palette, so they
	// do wait for initialization to settle — but bounded by InitWaitBudget
	// rather than each server's connect timeout, so a server wedged
	// mid-handshake cannot stall a headless run for minutes. Past the
	// budget the turn proceeds without the stragglers; their tools simply
	// stay absent from this run.
	if !c.interactive {
		if err := mcp.WaitForInitBudget(ctx, mcp.InitWaitBudget); err != nil {
			return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
		}
	}

	// refresh models before each run. Snapshot the agent first: the run,
	// its model settings, and the model refresh below must all target the
	// same agent even if SetMainAgent swaps the main agent mid-flight.
	agent, agentName := c.activeAgent()
	if err := c.updateAgentModels(ctx, agent, agentName); err != nil {
		return nil, fmt.Errorf("failed to update models: %w", err)
	}

	model := agent.Model()
	maxTokens := runMaxTokens(model)

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
	applyReasoning(mergedOptions, model.CatwalkCfg, model.ModelCfg.Provider, providerCfg, c.cfg.Config())

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	}

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `crush run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the HTTP boundary in backend.SendMessage) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	channel := ChannelFromContext(ctx)
	c.syncSessionChannel(ctx, sessionID, channel)
	run := func() (*fantasy.AgentResult, error) {
		return agent.Run(ctx, SessionAgentCall{
			SessionID:         sessionID,
			RunID:             runID,
			Channel:           channel,
			Prompt:            prompt,
			HiddenUserMessage: message.HiddenUserMessage(ctx),
			NoticeUserMessage: message.NoticeUserMessage(ctx),
			Attachments:       attachments,
			MaxOutputTokens:   maxTokens,
			ProviderOptions:   mergedOptions,
			Temperature:       temp,
			TopP:              topP,
			TopK:              callTopK(providerCfg, topK),
			FrequencyPenalty:  freqPenalty,
			PresencePenalty:   presPenalty,
			OnComplete:        onComplete,
			Accepted:          accept,
			OnAuthRefresh:     c.makeAuthRefreshCallback(providerCfg),
		})
	}
	beforeLoaded := c.skillTracker.LoadedNames()
	result, originalErr := run()
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate. AWS SSO is
	// handled transparently inside OnAuthRefresh, so it needs no post-run
	// notification here.
	if originalErr != nil && isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	if hasLatest && c.runComplete != nil {
		c.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, latest)
		// Signal to the dispatcher (backend.runAgent) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	return result, originalErr
}

// syncSessionChannel reconciles the session's persisted channel binding with
// the origin of the turn about to run. A channel-originated turn (re)binds
// the session to that channel — the newest push wins — and a local turn
// clears a stale binding, since the session is no longer channel-driven once
// the user takes it over directly. This is the binding's whole lifecycle:
// it is only ever a reflection of the most recent turn's origin, and the
// column is dropped with the session row when the session is deleted, so
// there is no separate state to reap.
//
// Failures are logged and the turn proceeds: the binding is provenance for
// reply routing, not a precondition for running.
func (c *coordinator) syncSessionChannel(ctx context.Context, sessionID, channel string) {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		// A missing session is expected (it may be created later in the
		// run), but a real database failure would otherwise be silent.
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("Failed to load session for channel binding sync",
				"session", sessionID, "channel", channel, "error", err)
		}
		return
	}
	if sess.Channel == channel {
		return
	}
	if _, err := c.sessions.SetChannel(ctx, sessionID, channel); err != nil {
		slog.Warn("Failed to sync session channel binding",
			"session", sessionID, "channel", channel, "error", err)
	}
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

// providerOptions is getProviderOptions plus how hard this model thinks.
func (c *coordinator) providerOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	opts := getProviderOptions(model, providerCfg)
	applyReasoning(opts, model.CatwalkCfg, model.ModelCfg.Provider, providerCfg, c.cfg.Config())
	return opts
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		}

	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink && model.ModelCfg.Think:
				mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}

	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}

	case google.Name:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning {
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_budget":  2000,
					"include_thoughts": true,
				}
			} else {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   reasoningEffort,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}

	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
				// MiniMax models use the "thinking" parameter instead of
				// "reasoning_effort". Other models on these providers still
				// use the standard field.
				if !strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
					mergedOptions["reasoning_effort"] = reasoningEffort
				}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}

		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if model.ModelCfg.Think || reasoningEffort != "" {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}

		case string(catwalk.InferenceProviderFireworks):
			// NOTE: Fireworks break if we set both `reasoning_effort` and `thinking`.
			if reasoningEffort == "" {
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}

		case string(catwalk.InferenceProviderBaseten):
			extraBody["chat_template_args"] = map[string]any{
				"enable_thinking": model.ModelCfg.Think || reasoningEffort != "" && reasoningEffort != "none",
			}

		case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
			// MiniMax M3 uses the "thinking" parameter to control reasoning.
			// "reasoning_split" must be true so thinking content is returned
			// in the "reasoning_content" field instead of inline in "content".
			if strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
				if model.CatwalkCfg.CanReason && (model.ModelCfg.Think || reasoningEffort != "") {
					extraBody["thinking"] = map[string]any{"type": "adaptive"}
					extraBody["reasoning_split"] = true
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}

		case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
			if model.CatwalkCfg.CanReason && !shouldSetEffort {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}

	default:
		// Known custom providers (litellm, llamacpp, lmstudio, ollama,
		// omlx) are openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			// Set "top_k" under "extra_body", as it is not part of the OpenAI protocol
			// and will be explicitly omitted by Fantasy downstream.
			topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
			if topK != nil {
				extraBody, hasExtraBody := mergedOptions["extra_body"].(map[string]any)
				if !hasExtraBody {
					extraBody = make(map[string]any)
					mergedOptions["extra_body"] = extraBody
				}
				if _, hasTopK := extraBody["top_k"]; !hasTopK {
					extraBody["top_k"] = *topK
				}
			}

			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && shouldSetEffort {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}

			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			} else {
				if topK != nil {
					slog.Warn(
						"Failed to parse provider_options, falling back to top_k only",
						"provider", providerCfg.ID,
						"error", err,
					)

					fallbackMergeOptions := map[string]any{
						"extra_body": map[string]any{"top_k": *topK},
					}
					parsed, err := openaicompat.ParseOptions(fallbackMergeOptions)
					if err == nil {
						options[openaicompat.Name] = parsed
					} else {
						slog.Warn(
							"Failed to parse fallback provider options, this should never happen",
							"provider", providerCfg.ID,
							"error", err,
						)
					}
				}
			}
		}
	}

	return options
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}

	largeProviderCfg, _ := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	result := NewSessionAgent(SessionAgentOptions{
		LargeModel:            large,
		SmallModel:            small,
		SystemPromptPrefix:    largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:          "",
		IsSubAgent:            isSubAgent,
		AgentType:             agent.ID,
		PermissionMode:        c.permissionMode,
		SessionStarts:         c.sessionStarts,
		InstructionsLoaded:    c.instructionsLoaded,
		Goals:                 c.goals,
		Frames:                c.frames,
		MemBlock:              c.memBlock(isSubAgent),
		MemChanged:            c.memChanged(isSubAgent),
		MemRecall:             c.memRecall(isSubAgent),
		MemSummarized:         c.memSummarized(isSubAgent),
		HoldTurn:              c.holdTurn,
		DisableAutoSummarize:  c.cfg.Config().Options.DisableAutoSummarize,
		DisableIdleCompaction: c.cfg.Config().Options.DisableIdleCompaction,
		IsYolo:                c.permissions.SkipRequests(),
		Sessions:              c.sessions,
		Messages:              c.messages,
		Cfg:                   c.cfg,
		FileTracker:           c.filetracker,
		Tools:                 nil,
		Notify:                c.notify,
		RunComplete:           c.runComplete,
	})

	// The readiness goroutines below perform one-time setup — building the
	// system prompt and the initial tool list — whose results the
	// coordinator needs for its whole lifetime, so they must survive the
	// caller's context being canceled. Several entry points build an agent
	// from a short-lived HTTP request context: the server's
	// InitAgent/UpdateAgent handlers, and UpdateModels -> buildTools ->
	// agentTool -> buildAgent for the sub-agent. The tool-list build reads
	// the MCP registry as it stands; servers still connecting are picked up
	// by later runs. WithoutCancel drops cancellation while keeping context
	// values; the work is local and always completes.
	initCtx := context.WithoutCancel(ctx)

	c.readyWg.Go(func() error {
		systemPrompt, err := prompt.Build(initCtx, large.Model.Provider(), large.Model.Model(), c.cfg)
		if err != nil {
			return err
		}
		systemPrompt, env := promptpkg.SplitEnv(systemPrompt)
		result.SetSystemPrompt(systemPrompt)
		result.SetEnvironment(env)
		return nil
	})

	c.readyWg.Go(func() error {
		tools, err := c.buildTools(initCtx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	return result, nil
}

// ToolInfos implements Coordinator.
func (c *coordinator) ToolInfos(ctx context.Context) ([]fantasy.ToolInfo, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errors.New("coder agent not configured")
	}
	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return nil, err
	}
	infos := make([]fantasy.ToolInfo, 0, len(tools))
	for _, t := range tools {
		infos = append(infos, t.Info())
	}
	return infos, nil
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	var allTools []fantasy.AgentTool

	// Get the model name for the agent
	modelID := ""
	if modelCfg, ok := c.cfg.Config().Models[agent.Model]; ok {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	logFile := config.LogFile(filepath.Join(c.cfg.Config().Options.DataDirectory, "logs"))

	// One namedChanges per coordinator, shared across every tool and
	// every buildTools call: a file the session read, changed by anything
	// other than the agent's own edit, multiedit or write, is named at
	// the end of the result of the next tool call after it changed,
	// whatever the tool. Sharing the named state is what makes a file
	// told once and not told again after the agent reads it, and a
	// coordinator rebuild does not reset the dedup state.
	wrap := func(tool fantasy.AgentTool) fantasy.AgentTool {
		return &changedReadFiles{AgentTool: tool, files: c.filetracker, workingDir: c.cfg.WorkingDir(), named: c.named}
	}

	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, wrap(agentTool))
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, wrap(agenticFetchTool))
	}

	allTools = append(
		allTools,
		&changedReadFiles{
			AgentTool: tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.DataDirectory, c.cfg.Config().Options.Attribution, modelID, c.cfg.Config().Options.GetMaxBackgroundJobs(), c.cfg.Config().Permissions.BashDenyPatterns(), c.cfg.Config().Permissions.BashAllowPatterns(), c.backgroundWatcher(isSubAgent), c.cfg.Config().Hosts, c.cfg.Config().HeldProject.GitRuns()),

			files:      c.filetracker,
			workingDir: c.cfg.WorkingDir(),
			named:      c.named,
		},
		wrap(tools.NewGnoshInfoTool(c.cfg, c.lspManager, c.allSkills, c.activeSkills, c.skillTracker)),
		wrap(tools.NewGnoshLogsTool(logFile)),
		wrap(tools.NewJobOutputTool(c.cfg.WorkingDir(), c.cfg.Config().Options.DataDirectory)),
		wrap(tools.NewJobKillTool(c.cfg.WorkingDir())),
		wrap(tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil)),
		wrap(tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.subdirectoryContext, c.cfg.WorkingDir())),
		wrap(tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.subdirectoryContext, c.cfg.WorkingDir())),
		wrap(tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil)),
		wrap(tools.NewGlobTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Glob)),
		wrap(tools.NewGrepTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep)),
		wrap(tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls)),
		wrap(tools.NewSourcegraphTool(nil)),
		wrap(tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.skillTracker, c.subdirectoryContext, c.viewedFiles(), c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...)),
		wrap(tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.subdirectoryContext, c.cfg.WorkingDir())),
	)

	// The question tool, the gnokey and x402_fetch tools that ask the user
	// for a key's password, and the artifact tool that surfaces deliverables
	// in the TUI sidebar all need an interactive user; sub-agents have none.
	if !isSubAgent && c.interactive {
		allTools = append(allTools,
			wrap(tools.NewQuestionTool(c.questions)),
			wrap(tools.NewGnokeyTool(c.permissions, c.questions, c.cfg.WorkingDir())),
			wrap(tools.NewX402FetchTool(c.permissions, c.questions, c.cfg.WorkingDir(), nil)),
			wrap(tools.NewArtifactTool(c.artifacts, c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Hosts)),
		)
	}

	// The GnoVM machines' tools (internal/gnovm/tools). mem, reached through
	// gnovm_run, is the one task list: the goal a call to it changes is
	// what the session's todos show.
	if c.gnovms != nil {
		allTools = append(allTools,
			wrap(gnovmtools.NewRunTool(c.gnovms, c.cfg.WorkingDir(), gnovmtools.OnMemGoals(c.memGoals), gnovmtools.BoundGoal(c.goals.boundName), gnovmtools.OnMemErased(c.memErased), gnovmtools.OnMemRules(c.memRules),
				gnovmtools.MemStack(c.frames.stack), gnovmtools.MemFresh(c.frames.fresh), gnovmtools.OnMemFrames(c.frames.events), gnovmtools.Namer(c.nameFor), gnovmtools.SubAgent(isSubAgent))),
			wrap(gnovmtools.NewNewTool(c.gnovms, c.cfg.WorkingDir())),
			wrap(gnovmtools.NewListTool(c.gnovms, c.cfg.WorkingDir())),
			wrap(gnovmtools.NewDropTool(c.gnovms, c.cfg.WorkingDir(), c.permissions)),
		)
	}

	// Add LSP tools if user has configured LSPs or auto_lsp is enabled (nil or true).
	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		allTools = append(
			allTools,
			wrap(tools.NewDiagnosticsTool(c.lspManager)),
			wrap(tools.NewReferencesTool(c.lspManager)),
			wrap(tools.NewLSPRestartTool(c.lspManager)),
			wrap(tools.NewSymbolsTool(c.lspManager)),
			wrap(tools.NewDefinitionTool(c.lspManager)),
			wrap(tools.NewCallHierarchyTool(c.lspManager)),
			wrap(tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker)),
			wrap(tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker, c.subdirectoryContext)),
		)
	}

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			wrap(tools.NewListMCPResourcesTool(c.cfg, c.permissions)),
			wrap(tools.NewReadMCPResourceTool(c.cfg, c.permissions)),
		)
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, wrap(tool))
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, wrap(tool))
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// A sub-agent's tool calls go past the hooks around tool calls too, as
	// in Claude Code: a PreToolUse hook that keeps the agent from reading
	// a file must keep a sub-agent it starts from reading it. Their hooks
	// see whose calls they are from the context runSubAgent gives the
	// sub-agent (hooks.WithSubAgent).
	filteredTools = wrapToolsWithHooks(filteredTools, c.buildToolHooks())

	return applyUserTools(filteredTools), nil
}

// buildToolHooks builds the runners for the hooks configured around tool
// calls, for every agent's tools.
func (c *coordinator) buildToolHooks() toolHooks {
	th := toolHooks{files: c.filetracker, workingDir: c.cfg.WorkingDir(), permissions: c.permissions, interactive: c.interactive, dontAsk: c.cfg.Overrides().DontAsk}
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		th.pre = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	if postToolHooks := c.cfg.Config().Hooks[hooks.EventPostToolUse]; len(postToolHooks) > 0 {
		th.post = hooks.NewRunner(postToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	if failureHooks := c.cfg.Config().Hooks[hooks.EventPostToolUseFailure]; len(failureHooks) > 0 {
		th.postFailure = hooks.NewRunner(failureHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	if requestHooks := c.cfg.Config().Hooks[hooks.EventPermissionRequest]; len(requestHooks) > 0 {
		th.permissionRequest = hooks.NewRunner(requestHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	return th
}

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	largeModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	largeProviderCfg, ok := c.cfg.Config().Providers.Get(largeModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}

	largeProvider, err := c.buildProvider(largeProviderCfg, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := c.cfg.Config().Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}

	smallProvider, err := c.buildProvider(smallProviderCfg, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	// Resolve the selected model from the provider's whole catalog: an
	// API-key model, a ChatGPT subscription model, or a Grok one. The
	// subscription catalogs live outside Models, so looking only there
	// strands a Grok/ChatGPT model that catwalk doesn't also publish.
	largeCatwalkModel := c.cfg.Config().GetModel(largeModelCfg.Provider, largeModelCfg.Model)
	smallCatwalkModel := c.cfg.Config().GetModel(smallModelCfg.Provider, smallModelCfg.Model)

	if largeCatwalkModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}

	if smallCatwalkModel == nil {
		return Model{}, Model{}, errSmallModelNotFound
	}

	largeModelID := largeModelCfg.Model
	smallModelID := smallModelCfg.Model

	if largeModelCfg.Provider == openrouter.Name && isExactoSupported(largeModelID) {
		largeModelID += ":exacto"
	}

	if smallModelCfg.Provider == openrouter.Name && isExactoSupported(smallModelID) {
		smallModelID += ":exacto"
	}

	largeModel, err := largeProvider.LanguageModel(ctx, largeModelID)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallModel, err := smallProvider.LanguageModel(ctx, smallModelID)
	if err != nil {
		return Model{}, Model{}, err
	}

	// Bound each request with the configured timeout so unreachable or hung
	// providers fail instead of blocking a session forever. The wrapper is
	// applied per request, so retries get a fresh budget each attempt.
	requestTimeout := c.cfg.Config().Options.GetRequestTimeout()
	largeModel = newRequestTimeoutModel(largeModel, requestTimeout)
	smallModel = newRequestTimeoutModel(smallModel, requestTimeout)

	// Ride out a model server that is restarting, outermost so each
	// reconnect attempt gets a fresh request timeout; see reconnectingModel.
	largeModel = newReconnectingModel(largeModel)
	smallModel = newReconnectingModel(smallModel)

	// Hyper completions no longer report the hypercredit balance, so wrap
	// the Hyper models to fetch it from /v1/credits on every request.
	if largeModelCfg.Provider == hyper.Name {
		largeModel = newHyperCreditsModel(largeModel, c.hyperAPIKey)
	}
	if smallModelCfg.Provider == hyper.Name {
		smallModel = newHyperCreditsModel(smallModel, c.hyperAPIKey)
	}

	large := Model{
		Model:      largeModel,
		CatwalkCfg: *largeCatwalkModel,
		ModelCfg:   largeModelCfg,
		FlatRate:   largeProviderCfg.FlatRate,
	}
	small := Model{
		Model:      smallModel,
		CatwalkCfg: *smallCatwalkModel,
		ModelCfg:   smallModelCfg,
		FlatRate:   smallProviderCfg.FlatRate,
	}

	return large, small, nil
}

// hyperAPIKey resolves the Hyper API key from the live config, so an
// OAuth token refreshed after the models were built is picked up by the
// next credits fetch.
func (c *coordinator) hyperAPIKey() string {
	return config.ResolveHyperAPIKey(c.cfg.Config())
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []anthropic.Option

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	opts = append(opts, anthropic.WithHTTPClient(c.modelHTTPClient(baseURL)))
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, token *oauth.Token) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	httpClient := c.modelHTTPClient(baseURL)
	if token != nil {
		// ChatGPT OAuth: requests go through the Codex backend, which
		// expects account headers and rejects some request fields, so
		// they pass through the Codex transport.
		httpClient.Transport = &openaioauth.Transport{
			Base:  httpClient.Transport,
			Token: token,
		}
	}
	opts = append(opts, openai.WithHTTPClient(httpClient))
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)

	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(isOpenCodeResponsesModel),
		)

	case hyper.Name:
		// Hyper may route requests through a Prism model; capture the
		// router headers so the UI can show which model answered.
		opts = append(
			opts,
			openaicompat.WithLanguageModelOptions(
				openai.WithLanguageModelHeaderFunc(hyper.HeaderFunc),
			),
		)
	}
	if httpClient == nil {
		httpClient = c.modelHTTPClient(baseURL)
	}
	opts = append(opts, openaicompat.WithHTTPClient(httpClient))

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		opts = append(opts, bedrock.WithRegion("us-east-1"))
	}

	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if isOpenCodeMessagesModel(providerCfg.ID, model.Model) {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
		}
	}

	switch providerCfg.Type {
	case openai.Name:
		// A ChatGPT login is the provider's single credential: every
		// request goes through the Codex backend with the OAuth token.
		token := providerCfg.OAuthToken
		if token != nil {
			baseURL = openaioauth.CodexBaseURL
			apiKey = token.AccessToken
			headers["originator"] = "crush"
			if token.AccountID != "" {
				headers["chatgpt-account-id"] = token.AccountID
			}
		}
		return c.buildOpenaiProvider(baseURL, apiKey, headers, token)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers, providerCfg.ID)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-crush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	default:
		// Known custom providers (litellm, llamacpp, lmstudio, ollama,
		// omlx) are openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.currentAgent().BeginAccepted(sessionID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent().Cancel(sessionID)
}

// CancelAll stops every turn, as gnosh shuts down; from then on a
// background job that ends starts no turn.
func (c *coordinator) CancelAll() {
	if c.jobs != nil {
		c.jobs.closing.Store(true)
	}
	c.currentAgent().CancelAll()
}

// backgroundWatcher is what the bash tool tells of a command that
// becomes a background job: the job watcher, for the main agent of an
// interactive gnosh, which goes on after a turn ends. A sub-agent's turn
// is over by the time its job ends, and `gnosh run` exits with its turn.
func (c *coordinator) backgroundWatcher(isSubAgent bool) tools.BackgroundWatcher {
	if isSubAgent || !c.interactive || c.jobs == nil {
		return nil
	}
	return c.jobs.watch
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent().ClearQueue(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent().IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent().IsSessionBusy(sessionID)
}

func (c *coordinator) ListSettledMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return c.currentAgent().ListSettledMessages(ctx, sessionID)
}

func (c *coordinator) ToolNames() []string {
	return c.currentAgent().ToolNames()
}

func (c *coordinator) Model() Model {
	return c.currentAgent().Model()
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	// A ChatGPT or Grok login without its model catalog — the fetch at
	// login failed, or the credentials predate it — would leave the
	// models dialog's subscription section empty. Fill it in lazily; the
	// guards make this a no-op once a catalog exists.
	c.cfg.RefetchOpenAIChatGPTModels(ctx)
	c.cfg.RefetchGrokModels(ctx)

	agent, name := c.activeAgent()
	return c.updateAgentModels(ctx, agent, name)
}

// updateAgentModels rebuilds the model and tool configuration for the
// given agent from the current config.
func (c *coordinator) updateAgentModels(ctx context.Context, agent SessionAgent, name string) error {
	// build the models again so we make sure we get the latest config
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	agent.SetModels(large, small)

	agentCfg, ok := c.cfg.Config().Agents[name]
	if !ok {
		return fmt.Errorf("%w: %s", errMainAgentNotFound, name)
	}

	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}
	agent.SetTools(tools)
	return nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent().QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent().QueuedPromptsList(sessionID)
}

func (c *coordinator) PopQueuedPrompts(sessionID string) []message.PoppedPrompt {
	return c.currentAgent().PopQueuedPrompts(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID, instructions string) error {
	agent := c.currentAgent()
	providerCfg, ok := c.cfg.Config().Providers.Get(agent.Model().ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	}

	// Auth failures during summarize flow through fantasy's OnAuthRefresh,
	// the same path used by regular turns.
	return agent.Summarize(ctx, sessionID, instructions, c.providerOptions(agent.Model(), providerCfg), c.makeAuthRefreshCallback(providerCfg), preCompactTriggerManual)
}

// RebuildSystemPrompts implements Coordinator, each agent's for the model
// it is on. A sub-agent the agent tool ran after /add-dir was not told of
// the directory, though its tools could reach it.
func (c *coordinator) RebuildSystemPrompts(ctx context.Context) error {
	c.agentMu.RLock()
	coder, plan := c.agents[config.AgentCoder], c.agents[config.AgentPlan]
	taskAgents := slices.Collect(maps.Values(c.taskAgents))
	c.agentMu.RUnlock()
	coderP, err := c.coderSystemPrompt()
	if err != nil {
		return err
	}
	planP, err := c.planSystemPrompt()
	if err != nil {
		return err
	}
	for _, a := range append([]promptedAgent{{agent: coder, prompt: coderP}, {agent: plan, prompt: planP}}, taskAgents...) {
		if a.agent == nil || a.agent.Model().Model == nil {
			continue
		}
		model := a.agent.Model().Model
		systemPrompt, err := a.prompt.Build(ctx, model.Provider(), model.Model(), c.cfg)
		if err != nil {
			return err
		}
		systemPrompt, env := promptpkg.SplitEnv(systemPrompt)
		a.agent.SetSystemPrompt(systemPrompt)
		a.agent.SetEnvironment(env)
	}
	return nil
}

// promptedAgent is an agent and the prompt its system prompt is built
// from.
type promptedAgent struct {
	agent  SessionAgent
	prompt *prompt.Prompt
}

// planSystemPrompt is the plan agent's prompt, with --append-system-prompt
// at its end.
func (c *coordinator) planSystemPrompt() (*prompt.Prompt, error) {
	return planPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()), prompt.WithTextAfter(c.cfg.Overrides().AppendSystemPrompt))
}

// coderSystemPrompt is the coder agent's prompt: --system-prompt replaces
// the coder's instructions, and --append-system-prompt goes at the end of
// both top-level agents' prompts; sub-agents keep their own.
func (c *coordinator) coderSystemPrompt() (*prompt.Prompt, error) {
	overrides := c.cfg.Overrides()
	appended := prompt.WithTextAfter(overrides.AppendSystemPrompt)
	interactive := prompt.WithInteractive(c.interactive)
	if overrides.SystemPrompt != "" {
		return coderPromptReplacing(overrides.SystemPrompt, prompt.WithWorkingDir(c.cfg.WorkingDir()), interactive, appended)
	}
	return coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()), interactive, appended)
}

// ContextUsage estimates what fills sessionID's context window, as Claude
// Code's /context does: the coder agent's system prompt and tools as its
// requests carry them, on the current model, and the session's
// conversation since its last summary ("" for a new session, which has
// none).
func (c *coordinator) ContextUsage(ctx context.Context, sessionID string) (ContextUsage, error) {
	agent := c.currentAgent()
	if agent == nil {
		return ContextUsage{}, errors.New("the agent is not ready")
	}
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return ContextUsage{}, errCoderAgentNotConfigured
	}
	model := agent.Model()
	provider, modelID := model.Model.Provider(), model.Model.Model()
	p, err := c.coderSystemPrompt()
	if err != nil {
		return ContextUsage{}, err
	}
	data, err := p.Data(ctx, provider, modelID, c.cfg)
	if err != nil {
		return ContextUsage{}, err
	}
	systemPrompt, err := p.Build(ctx, provider, modelID, c.cfg)
	if err != nil {
		return ContextUsage{}, err
	}
	agentTools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return ContextUsage{}, err
	}
	var msgs []message.Message
	if sessionID != "" {
		sess, err := c.sessions.Get(ctx, sessionID)
		if err != nil {
			return ContextUsage{}, err
		}
		if msgs, err = c.messages.ListFromSummary(ctx, sessionID, sess.SummaryMessageID); err != nil {
			return ContextUsage{}, err
		}
	}
	window := model.CatwalkCfg.ContextWindow
	at := c.compactPoint(window)
	return contextUsage(contextUsageInput{
		model:        modelID,
		window:       window,
		systemPrompt: systemPrompt + mcpInstructions(),
		data:         data,
		tools:        agentTools,
		messages:     msgs,
		compactAt:    at,
	}), nil
}

// nameFor is the names the small model gives the texts one call keeps in
// mem without naming them (gnovmtools.Namer); nil when there is no agent or
// no answer, and mem names them from their words.
func (c *coordinator) nameFor(ctx context.Context, sessionID string, texts []string) map[string]string {
	agent := c.currentAgent()
	if agent == nil {
		return nil
	}
	names, err := agent.NamesFor(ctx, sessionID, texts)
	if err != nil {
		slog.Debug("No names from the small model; mem names them from the words", "error", err)
	}
	return names
}

// GenerateTitle generates a session title using the current agent.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	agent := c.currentAgent()
	if agent == nil {
		return
	}
	agent.GenerateTitle(ctx, sessionID, prompt)
}

// GenerateSessionName names a session from its conversation using the
// current agent.
func (c *coordinator) GenerateSessionName(ctx context.Context, sessionID string) (string, error) {
	agent := c.currentAgent()
	if agent == nil {
		return "", errors.New("the agent is not ready")
	}
	return agent.GenerateSessionName(ctx, sessionID)
}

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, providerCfg)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, and for Bedrock providers whose AWS SSO session
// has expired, it triggers interactive re-authentication and blocks until the
// user completes it (or the context is cancelled).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			// If the refresh token was revoked, trigger interactive
			// re-auth and wait for the user to complete it.
			var exchangeErr *oauth.TokenExchangeError
			if c.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
				})
				return c.waitForInteractiveReauth(ctx, providerCfg.ID)
			}
			return err
		}
		return nil
	case providerCfg.AWSAuthRefresh != "":
		return c.refreshAWSCredentials(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return nil
	}
}

// errNoInteractiveAuth is returned by an OnAuthRefresh callback when a
// provider needs interactive re-authentication but no notifier is available
// to drive it (e.g. headless runs). Returning it surfaces the original auth
// error rather than retrying.
var errNoInteractiveAuth = errors.New("interactive authentication unavailable")

// waitForInteractiveReauth blocks until interactive re-authentication for the
// provider completes (signalled via SignalAuthComplete) or the context is
// cancelled, then rebuilds models so the next attempt picks up fresh
// credentials. Returns nil when the caller should retry.
func (c *coordinator) waitForInteractiveReauth(ctx context.Context, providerID string) error {
	// Use a detached context with a generous timeout so the wait survives
	// agent run cancellation. The user needs time to complete browser-based
	// authentication.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := c.cfg.WaitForTokenChange(waitCtx, providerID); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	// If the original context was cancelled during the wait, fantasy's retry
	// would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	// Rebuild models so ModelProvider picks up the fresh credentials.
	if updateErr := c.UpdateModels(waitCtx); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "error", updateErr)
		return updateErr
	}
	slog.Info("Models updated, returning nil to retry", "provider", providerID)
	return nil
}

// isUnauthorized reports whether err is an HTTP 401 from a provider.
func isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// makeAuthRefreshCallback returns an OnAuthRefresh callback for fantasy that
// delegates to the coordinator's existing credential refresh logic. Returns
// nil if no refresh mechanism is configured for the provider.
func (c *coordinator) makeAuthRefreshCallback(providerCfg config.ProviderConfig) func(context.Context, *fantasy.ProviderError) error {
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		return c.retryAfterUnauthorized(ctx, providerCfg)
	}
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// AgentType is the sub-agent's type as its hooks see it, agent_type:
	// task for the agent tool's, agentic_fetch for that tool's.
	AgentType string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
}

// callTopK returns topK for use on fantasy.Call.TopK, suppressing it for
// known custom providers: getProviderOptions already carries top_k for
// them via extra_body, and passing it here too makes Fantasy emit a
// spurious "top_k unsupported" warning for every turn.
func callTopK(providerCfg config.ProviderConfig, topK *int64) *int64 {
	if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return nil
	}
	return topK
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	// Create sub-session
	agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	session, err := c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
	}

	// A sub-agent works on its parent's behalf, so when the parent's
	// session is auto-approved, as a non-interactive run's is, the
	// sub-agent's is too. Otherwise its first permission request waits
	// for an answer nobody is there to give: in a gnosh run on the AI box
	// a sub-agent hung that way, reading a file in the Go module cache.
	if c.permissions != nil && c.permissions.IsSessionAutoApproved(params.SessionID) {
		c.permissions.AutoApproveSession(session.ID)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration
	model := params.Agent.Model()
	maxTokens := runMaxTokens(model)

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	// The sub-agent's tool calls are its own, and their hooks say so. A
	// hook's approval of the call that started it, or its question about
	// it, is not theirs: their calls can carry the same ID.
	ctx = hooks.WithSubAgent(ctx, params.SessionID, params.AgentType)
	ctx = permission.WithoutHookAnswers(ctx)

	// What the user's SubagentStart hooks add goes to the sub-agent with
	// its first prompt, as Claude Code gives it.
	startContext := c.runSubagentStartHooks(ctx, params.SessionID, session.ID, params.AgentType)

	// Run the agent
	run := func() (*fantasy.AgentResult, error) {
		return params.Agent.Run(ctx, SessionAgentCall{
			SessionID:         session.ID,
			Prompt:            params.Prompt,
			promptHookContext: startContext,
			MaxOutputTokens:   maxTokens,
			ProviderOptions:   c.providerOptions(model, providerCfg),
			Temperature:       model.ModelCfg.Temperature,
			TopP:              model.ModelCfg.TopP,
			TopK:              callTopK(providerCfg, model.ModelCfg.TopK),
			FrequencyPenalty:  model.ModelCfg.FrequencyPenalty,
			PresencePenalty:   model.ModelCfg.PresencePenalty,
			NonInteractive:    true,
			OnAuthRefresh:     c.makeAuthRefreshCallback(providerCfg),
		})
	}
	result, err := run()
	// Notify only if still unauthorized after retry. AWS SSO is handled
	// transparently inside OnAuthRefresh, so it needs no post-run notice.
	if err != nil && isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", err)), nil
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	if err := c.updateParentSessionCost(ctx, session.ID, params.SessionID); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", session.ID,
			"parent_session", params.SessionID,
			"error", err,
		)
	}

	// A tool result that ended the sub-agent's turn, a hook's halt or a
	// permission the user denied, ends the parent's too. It is the user's
	// word on the turn: the parent went on before, given only the
	// sub-agent's last text, and could try by other means what was just
	// refused.
	if reason, halted := subAgentHalt(result); halted {
		resp := fantasy.NewTextErrorResponse("The sub-agent was stopped: " + reason)
		resp.StopTurn = true
		return resp, nil
	}
	output := subAgentOutput(result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

// subAgentOutput is the sub-agent's answer as the parent reads it, without
// the stop marker it ends on when it overrules its SubagentStop hooks.
func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return StripStopMarker(result.Response.Content.Text())
}

// subAgentHalt reports whether a tool result ended the sub-agent's turn,
// and what it said.
func subAgentHalt(result *fantasy.AgentResult) (string, bool) {
	if result == nil || len(result.Steps) == 0 {
		return "", false
	}
	for _, tr := range result.Steps[len(result.Steps)-1].Content.ToolResults() {
		if !tr.StopTurn {
			continue
		}
		switch out := tr.Result.(type) {
		case fantasy.ToolResultOutputContentText:
			return out.Text, true
		case fantasy.ToolResultOutputContentError:
			if out.Error != nil {
				return out.Error.Error(), true
			}
		}
		return "no reason given", true
	}
	return "", false
}

// updateParentSessionCost accumulates the cost from a child session to its parent session.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}
	// Only the cost is added: saving the whole parent session put back
	// what it had read, over the parent's own steps' usage and title.
	if err := c.sessions.AddCost(ctx, parentSessionID, childSession.Cost); err != nil {
		return fmt.Errorf("add to parent session cost: %w", err)
	}
	return nil
}

// discoverSkills is a thin fallback wrapper used only when no
// skills.Manager has been threaded through to the coordinator. All
// production call sites (backend.CreateWorkspace, setupLocalWorkspace)
// run discovery in advance and pass the results via the manager;
// reaching this path means a caller bypassed both. It deliberately does
// NOT publish to the package-level broker — there are no subscribers in
// that case, so doing so would be misleading without delivering the
// snapshot anywhere useful.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		Resolver:       resolver,
	})
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start. Builtin vs user counts are derived from the
// SkillState.Path — builtin states use the "builtin/" embed prefix.
func logDiscoveryStats(
	states []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	var builtinOK, builtinErr, userOK, userErr int
	for _, s := range states {
		isBuiltin := strings.HasPrefix(s.Path, "builtin/")
		switch {
		case isBuiltin && s.State == skills.StateNormal:
			builtinOK++
		case isBuiltin && s.State == skills.StateError:
			builtinErr++
		case !isBuiltin && s.State == skills.StateNormal:
			userOK++
		case !isBuiltin && s.State == skills.StateError:
			userErr++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", builtinOK,
		"builtin_errors", builtinErr,
		"user_ok", userOK,
		"user_errors", userErr,
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}

// runMaxTokens is how many tokens a turn's reply may run to:
// CLAUDE_CODE_MAX_OUTPUT_TOKENS, as in Claude Code, when it is a positive
// number, or else the selected model's max_tokens, or else its catalog
// default. A run and a sub-agent's alike; the title and summary requests
// keep their own limits.
func runMaxTokens(model Model) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS")), 10, 64); err == nil && n > 0 {
		return n
	}
	if model.ModelCfg.MaxTokens != 0 {
		return model.ModelCfg.MaxTokens
	}
	return model.CatwalkCfg.DefaultMaxTokens
}
