// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/artifacts"
	"github.com/charmbracelet/crush/internal/clipboard"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/gnovm"
	"github.com/charmbracelet/crush/internal/herdr"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/update"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// UpdateAvailableMsg is sent when a new version is available.
type UpdateAvailableMsg struct {
	CurrentVersion string
	LatestVersion  string
	IsDevelopment  bool
}

type App struct {
	// gnovms is this App's hold on the GnoVM machines (internal/gnovm):
	// made with the coder agent, released in Shutdown.
	gnovms *gnovm.Lazy

	Sessions    session.Service
	Messages    message.Service
	History     history.Service
	Permissions permission.Service
	Questions   question.Service
	FileTracker filetracker.Service

	AgentCoordinator agent.Coordinator

	LSPManager *lsp.Manager

	// artifacts serves the deliverables the agent records with the
	// artifact tool, so the user can open one from the machine they are
	// sitting at. Nil when nothing is served (a non-interactive run, or
	// artifact_server "off"), and then an artifact is a path.
	artifacts *artifacts.Server

	Skills *skills.Manager

	config *config.ConfigStore

	serviceEventsWG *sync.WaitGroup
	eventsCtx       context.Context
	events          *pubsub.Broker[tea.Msg]
	tuiWG           *sync.WaitGroup

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
	// runCompletions is the authoritative per-run completion signal,
	// emitted once per top-level agent turn after all message
	// updates have been flushed. Bridged into app.events so SSE
	// subscribers (notably `crush run` in client/server mode) can
	// drive their exit on a deterministic, payload-bearing event
	// instead of guessing from message finish parts.
	runCompletions *pubsub.Broker[notify.RunComplete]

	// herdrClient reports agent state to herdr when running inside
	// a herdr-managed pane. Nil when not in a herdr environment.
	herdrClient *herdr.Client

	// currentSessionID is the session the user is in, reported through
	// ReportCurrentSession and ended with SessionEnd hooks when it
	// changes or the process exits (see sessionEndReason and
	// endSession). Guarded by sessionEndMu.
	currentSessionID string
	sessionEndMu     sync.Mutex
	// sessionEndHooks is the set of in-flight SessionEnd hook goroutines;
	// each entry is closed when its hook has finished, so
	// waitForSessionEndHooks can wait on the channels rather than on a
	// WaitGroup, which can return before the shell's buffered output is
	// visible to a reader on another goroutine.
	sessionEndHooks []chan struct{}

	// nonInteractive is set for gnosh run, whose exit ends the open
	// session with reason "other" instead of the TUI's
	// "prompt_input_exit".
	nonInteractive bool
	// stoppedBySignal is set when SIGINT or SIGTERM stopped gnosh, whose
	// exit then ends the open session with reason "other" too.
	stoppedBySignal bool
}

// permissionOptions is what the workspace's permission service is made
// with: yolo, the allowed tools, the directories added with --add-dir,
// --permission-mode acceptEdits, and the session env files' directory,
// which no allowed tool may change unasked.
func permissionOptions(store *config.ConfigStore) permission.Options {
	opts := permission.Options{
		WorkingDir:  store.WorkingDir(),
		Skip:        store.Overrides().SkipPermissionRequests,
		AddedDirs:   store.Overrides().AddedDirs,
		AcceptEdits: store.Overrides().AcceptEdits,
		DontAsk:     store.Overrides().DontAsk,
		// What a session env file holds runs ahead of every bash command.
		ProtectedPaths: []string{config.SessionEnvDir()},
	}
	if cfg := store.Config(); cfg.Permissions != nil {
		opts.AllowedTools = cfg.Permissions.AllowedTools
	}
	return opts
}

// New initializes a new application instance. skillsMgr carries the
// per-workspace skill discovery results computed by the caller; the
// caller is responsible for constructing it (typically via
// skills.NewManager + skills.DiscoverFromConfig).
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	files := history.NewService(q, conn)
	cfg := store.Config()

	app := &App{
		Sessions:    sessions,
		Messages:    messages,
		History:     files,
		Permissions: permission.New(permissionOptions(store)),
		Questions:   question.NewService(),
		FileTracker: filetracker.NewService(q),
		LSPManager:  lsp.NewManager(store),
		Skills:      skillsMgr,

		globalCtx: ctx,

		config: store,

		events:             pubsub.NewBroker[tea.Msg](),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		runCompletions:     pubsub.NewBroker[notify.RunComplete](),
	}

	app.setupEvents()

	// Initialize clipboard support. This is best-effort; if it fails
	// (e.g., headless environment), clipboard operations will return nil.
	if err := clipboard.Init(); err != nil {
		slog.Warn("Clipboard initialization failed", "error", err)
	}

	// Check for updates in the background.
	go app.checkForUpdates(ctx)

	// Arm initialization synchronously before launching it so WaitForInit
	// blocks for the in-flight init instead of racing the goroutine and
	// returning before any MCP tools register.
	mcp.ArmInit()
	// Config-disabled servers the user enabled via Toggle MCPs have a
	// repository-scoped enabled override; force-start them so the toggle
	// survives restarts. A read failure only means the override is skipped.
	// Where the database is the project's own and the folder is not
	// trusted, a downloaded repository could have written them: none is
	// started until the folder is trusted (the trust question lists them).
	var forceStart []string
	if store.DatabaseHeld() {
		slog.Info("The folder is not trusted, so the MCP servers its sessions database turns on stay off")
	} else if enabled, err := app.Sessions.MCPServersEnabled(ctx); err != nil {
		slog.Warn("Failed to list enabled MCP overrides; config-disabled servers stay disabled", "error", err)
	} else {
		for _, name := range enabled {
			if m, ok := store.Config().MCP[name]; ok && m.Disabled {
				forceStart = append(forceStart, name)
			}
		}
	}
	// Servers disabled for this repository via Toggle MCPs are not started.
	localDisabled, err := app.Sessions.MCPDisabledServers(ctx)
	if err != nil {
		slog.Warn("Failed to list disabled MCP overrides; starting them anyway", "error", err)
	}
	go mcp.Initialize(ctx, app.Permissions, store, forceStart, localDisabled)

	// Start herdr integration when running inside a herdr pane.
	app.herdrClient = herdr.Init()
	herdr.BridgeLocal(ctx, app.herdrClient, herdr.BridgeSources{
		PermRequests:      app.Permissions,
		PermNotifications: app.Permissions,
		RunCompletions:    app.runCompletions,
		Messages:          app.Messages,
	})

	// Release the shared database connection on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	dataDir := cfg.Options.DataDirectory
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(context.Context) error { return db.Release(dataDir) },
		func(ctx context.Context) error { return mcp.Close(ctx) },
	)

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			updateLSPState(name, lsp.StateUnstarted, nil, nil, 0)
			return
		}
		client.SetDiagnosticsCallback(updateLSPDiagnostics)
		updateLSPState(name, client.GetServerState(), nil, client, 0)
	})

	// TrackConfigured must run after SetCallback so the callback is already
	// installed when configured-but-not-yet-started LSPs are announced.
	go app.LSPManager.TrackConfigured(ctx)

	return app, nil
}

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[tea.Msg] {
	return app.events.Subscribe(ctx)
}

// SendEvent publishes a message to all event subscribers.
func (app *App) SendEvent(msg tea.Msg) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// RunCompletions returns the broker for the authoritative per-run
// terminal RunComplete events. The dispatcher (backend.runAgent) uses
// it to emit a reliable terminal event when a run fails before the
// coordinator could publish one of its own.
func (app *App) RunCompletions() *pubsub.Broker[notify.RunComplete] {
	return app.runCompletions
}

// ReportCurrentSession tells herdr which session the user is now
// viewing so it can persist a resumable reference for the pane. Safe
// to call when not running inside a herdr pane; the underlying client
// is nil-safe. Call this whenever the active session changes (load,
// new, or select).
//
// It also ends the session the user leaves, with SessionEnd hooks: a
// report of "" ends it with reason "clear", as the TUI reports the
// landing screen after a New Session, and a report of another session
// ends it with reason "other". A session ends once: reporting the
// same session again, or ending it and then exiting, does not end it
// again. A session that was never reported open does not end at all,
// which is what keeps the new, still empty session from ending.
func (app *App) ReportCurrentSession(sessionID string) {
	if sessionID != "" {
		app.endSessionIfLeft(sessionEndReasonOther, sessionID)
	} else if prev := app.takeCurrentSessionID(); prev != "" {
		app.endSession(prev, sessionEndReasonClear)
	}
	app.herdrClient.SetSessionID(sessionID)
}

// sessionEndReason names why a session ended, as the SessionEnd payload
// and its matcher carry it.
type sessionEndReason string

// sessionEndReasonOther covers a session ending any way this one does not
// name: the user switched to another session, or gnosh run exited with it
// open.
const sessionEndReasonOther sessionEndReason = hooks.SessionEndReasonOther

// sessionEndReasonClear is the user's New Session: the TUI reports ""
// after a session.
const sessionEndReasonClear sessionEndReason = hooks.SessionEndReasonClear

// sessionEndReasonPromptInputExit is the TUI exiting with a session open.
const sessionEndReasonPromptInputExit sessionEndReason = hooks.SessionEndReasonPromptInputExit

// takeCurrentSessionID swaps the reported session out for "" and returns
// it; "" when nothing was reported. The swap is what makes a session end
// once: a second end finds no current session and ends nothing.
func (app *App) takeCurrentSessionID() string {
	app.sessionEndMu.Lock()
	defer app.sessionEndMu.Unlock()
	sid := app.currentSessionID
	app.currentSessionID = ""
	return sid
}

// endSessionIfLeft ends the session the user leaves for id, with
// reason. Reporting the same session again leaves it to itself: the
// user is still in it, so it does not end. The swap happens under
// sessionEndMu so two reports in a race end at most one session.
func (app *App) endSessionIfLeft(reason sessionEndReason, id string) {
	app.sessionEndMu.Lock()
	prev := app.currentSessionID
	app.currentSessionID = id
	app.sessionEndMu.Unlock()
	if prev == "" || prev == id {
		return
	}
	app.endSession(prev, reason)
}

// endSession runs id's SessionEnd hooks with reason, when hooks are
// configured. The hooks run asynchronously so that a session switch or a
// new session does not block on them; Shutdown waits for them to finish
// before the process exits. Each end of a session runs them: one the user
// left, reopened and left again ends twice, even while the first end's
// hook is still running. That a session does not end twice for one
// leaving is the callers' doing: each takes the current session out
// under sessionEndMu before ending it.
func (app *App) endSession(id string, reason sessionEndReason) {
	// The hooks are read from the config and run in a goroutine that may
	// outlive the caller; snapshot them here, in the caller, so a config
	// reload that swaps the Config pointer while the hook is starting
	// cannot race the goroutine's read.
	cfgs := app.config.Config().Hooks[hooks.EventSessionEnd]
	if len(cfgs) == 0 {
		return
	}
	done := make(chan struct{})
	app.sessionEndMu.Lock()
	app.sessionEndHooks = append(app.sessionEndHooks, done)
	// The hook runs under the process's context without its
	// cancellation, its own timeout (default 1.5s, capped 60s) its
	// bound: SIGINT cancels that context, and gnosh ending on SIGINT is
	// an end the hook is there for, as Claude Code runs SessionEnd on
	// SIGINT and SIGTERM alike. Under the cancelled context it never ran.
	// It is not a child of the Shutdown's 5-second budget, because the
	// budget belongs to the process exit, not to the hook: a hook at its
	// own cap may run a little past the budget, which is what keeps the
	// hook's timeout the effective bound for it.
	ctx := context.WithoutCancel(app.globalCtx)
	app.sessionEndMu.Unlock()
	go func() {
		app.runSessionEndHooks(ctx, cfgs, id, reason)
		// Close the channel only after the hook has finished.
		close(done)
	}()
}

// waitForSessionEndHooks blocks until every SessionEnd hook started by
// endSession has finished, or until bound is done, whichever is first.
// The wait is on the channels endSession records, which close after the
// hook's goroutine has returned, which is after shell.Run has returned
// and the shell's buffered output has been written; a bare
// WaitGroup.Wait returns as soon as the goroutine's deferred Done runs,
// which can be before that, so the channel is what makes the wait
// reliable.
//
// The snapshot-and-replace is atomic under the lock, so a hook that
// ends between the replace and the wait below lands in a fresh slice
// and is drained by a later wait, not lost: endSession appends only to
// the current slice, and the replace happens before any wait, so the
// hook's channel is always in exactly one slice that some wait drains.
//
// bound, when non-nil, is the deadline the wait is released at: the
// Shutdown's 5-second budget, so a slow hook does not hold the exit
// open. The wait is released at the deadline even if the hook is still
// running: the hook's context is not the budget (it is the process's
// context, so the hook's own timeout is what bounds the hook itself),
// so a hook at its cap may finish a little after the exit, but the exit
// is not held up for it.
func (app *App) waitForSessionEndHooks(bound context.Context) {
	app.sessionEndMu.Lock()
	hooks := app.sessionEndHooks
	app.sessionEndHooks = nil
	app.sessionEndMu.Unlock()
	for _, done := range hooks {
		if bound != nil {
			select {
			case <-done:
			case <-bound.Done():
			}
		} else {
			<-done
		}
	}
}

// runSessionEndHooks runs the SessionEnd hooks for the session that just
// ended, with the reason it ended. As in Claude Code a hook cannot stop
// the exit and what it prints is ignored; a hook that fails is logged
// (runOne already warns about it). Their default timeout is Claude
// Code's 1.5 seconds, so exiting Gnosh is not held up for long.
func (app *App) runSessionEndHooks(ctx context.Context, cfgs []config.HookConfig, id string, reason sessionEndReason) {
	workingDir := app.config.WorkingDir()
	if workingDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			slog.Warn("SessionEnd hooks are configured but no working directory is available; skipping them",
				"session_id", id,
				"error", err,
			)
			return
		}
		workingDir = wd
	}
	runner := hooks.NewRunner(cfgs, workingDir, workingDir)
	if _, err := runner.RunSessionEnd(ctx, id, string(reason)); err != nil {
		slog.Warn("SessionEnd hook execution error", "session_id", id, "reason", string(reason), "error", err)
	}
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}

// RunNonInteractive runs the application in non-interactive mode with the
// given prompt, printing to stdout.
// RunNonInteractive runs one prompt without the TUI, writing the answer to
// output. onSession, when set, is told the session the run uses. With
// fork, for Claude Code's --fork-session, the run goes on in a copy of the
// session it continues, which is left as it was. A name, for Claude
// Code's --name, names the session the run goes on in, as /rename does.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt, largeModel, smallModel, reasoningEffort string, hideSpinner, traceTools bool, continueSessionID string, useLast, fork bool, name, goal string, onSession func(sessionID string), onMessage func(message.Message)) error {
	slog.Info("Running in non-interactive mode")

	// Re-initialize the coder agent without interactive-only tools. The
	// reinitialization sets nonInteractive, which Shutdown reads to pick
	// the SessionEnd reason for the open session.
	if err := app.InitCoderAgentNonInteractive(ctx); err != nil {
		return fmt.Errorf("failed to reinitialize agent for non-interactive mode: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if largeModel != "" || smallModel != "" {
		if err := app.OverrideModels(ctx, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	// The reasoning effort applies to the model that will actually run.
	// On a continued session without an explicit model override, the
	// model is resolved later from the session's last assistant message,
	// so the override is applied after that restore instead.
	deferredEffort := (continueSessionID != "" || useLast) && largeModel == "" && smallModel == ""
	if reasoningEffort != "" && !deferredEffort {
		if err := app.overrideReasoningEffort(ctx, reasoningEffort); err != nil {
			return err
		}
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		activeTheme := ""
		if cfg := app.config.Config(); cfg != nil && cfg.Options != nil && cfg.Options.TUI != nil {
			activeTheme = cfg.Options.TUI.ActiveTheme
		}
		t := styles.ThemeFromConfig(activeTheme)

		spinner = format.NewSpinner(ctx, cancel, anim.Settings{
			Size:        10,
			Label:       "Generating",
			GradColorA:  t.WorkingGradFromColor,
			GradColorB:  t.WorkingGradToColor,
			CycleColors: true,
		})
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Non-interactive runs get a single shot at the tool palette, so wait for
	// MCP initialization to settle before reading MCP tools. The coordinator
	// waits again for the same reason (it is the gate the client/server path
	// goes through); doing it here too surfaces the failure before we create a
	// session, and lets the UpdateModels below see every MCP tool. The wait is
	// bounded so a server wedged mid-handshake cannot stall the one-shot run
	// for its full connect timeout; past the budget the run proceeds without
	// the unfinished servers' tools.
	if err := mcp.WaitForInitBudget(ctx, mcp.InitWaitBudget); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// force update of agent models before running so mcp tools are loaded
	app.AgentCoordinator.UpdateModels(ctx)

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}
	// After InitCoderAgentNonInteractive, so the coordinator that runs the
	// prompt is the one told the copy is a fork. Named before the prompt,
	// a new session keeps its name: only an untitled one is titled.
	switch {
	case fork:
		if sess, err = app.BranchSession(ctx, sess.ID, name); err != nil {
			return fmt.Errorf("failed to fork the session: %w", err)
		}
	case name != "":
		if err := app.Sessions.Rename(ctx, sess.ID, name); err != nil {
			return fmt.Errorf("failed to name the session: %w", err)
		}
	}
	// --goal binds the session's goal in mem before the prompt is
	// composed, and the prompt carries the directive that binding gives.
	if goal != "" {
		change, err := app.SetSessionGoal(ctx, sess.ID, goal)
		if err != nil {
			return fmt.Errorf("failed to set the goal: %w", err)
		}
		prompt = strings.TrimSpace(prompt + "\n\n" + change.Directive)
	}
	if onSession != nil {
		onSession(sess.ID)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
		// If no explicit model override was requested, restore the
		// model/provider from the last assistant message in the
		// session, provided it is still available.
		if largeModel == "" && smallModel == "" {
			if err := app.restoreModelFromSession(ctx, sess.ID); err != nil {
				slog.Warn("Failed to restore model from session", "error", err)
			}
		}
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	if reasoningEffort != "" && deferredEffort {
		if err := app.overrideReasoningEffort(ctx, reasoningEffort); err != nil {
			return err
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session, but under --permission-mode dontAsk, which denies them.
	if !app.config.Overrides().DontAsk {
		app.Permissions.AutoApproveSession(sess.ID)
	}

	// Report session identity to herdr.
	app.ReportCurrentSession(sess.ID)

	type response struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan response, 1)

	// Warnings (a Stop hook that failed to run, an agent that stopped
	// over a Stop hook's objection, a goal that could not be checked or
	// was reopened, a rule a session's mem call set) and goals found met
	// are notifications only the TUI shows. A run prints them on stderr
	// instead of losing them.
	notifications := app.agentNotifications.Subscribe(ctx)
	met := false
	warn := func(n notify.Notification) {
		met = met || n.Type == notify.TypeGoalMet && n.SessionID == sess.ID
		if line, ok := warningLine(n, sess.ID); ok {
			stopSpinner()
			_, _ = fmt.Fprint(os.Stderr, line)
		}
	}

	go func(ctx context.Context, sessionID, prompt string) {
		result, err := app.AgentCoordinator.Run(ctx, sess.ID, prompt)
		if err != nil {
			// Run returns the turn's error, whenever in the turn it came;
			// it is reported in runError.
			done <- response{err: err}
			return
		}
		done <- response{
			result: result,
		}
	}(ctx, sess.ID, prompt)

	messageEvents := app.Messages.Subscribe(ctx)
	answer := &answerPrinter{w: output, readBytes: map[string]int{}}
	var trace *format.ToolTrace
	if traceTools {
		trace = format.NewToolTrace(os.Stderr)
	}
	subSessions := map[string]bool{}
	isSubSession := func(id string) bool {
		sub, known := subSessions[id]
		if !known {
			sub = app.descendsFrom(ctx, id, sess.ID)
			subSessions[id] = sub
		}
		return sub
	}

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}

		// Always print a newline at the end. If output is a TTY this will
		// prevent the prompt from overwriting the last line of output.
		_, _ = fmt.Fprintln(output)
	}()

	// handle takes one message event: it traces the run's tool calls and
	// its sub-agents', hands the run's own messages to onMessage, and
	// prints the new text of its assistant messages.
	handle := func(msg message.Message) error {
		if msg.SessionID != sess.ID {
			// A sub-agent works in a session of its own, under the
			// run's. Its tool calls are traced too, indented, so a long
			// stretch of delegated work does not leave the trace silent.
			if trace != nil && isSubSession(msg.SessionID) {
				traceToolCalls(trace, msg, true)
			}
			return nil
		}
		traceToolCalls(trace, msg, false)
		if onMessage != nil {
			onMessage(msg)
		}
		if msg.Role != message.Assistant {
			return nil
		}

		if len(msg.Parts) == 0 {
			return nil
		}

		stopSpinner()
		return answer.print(msg)
	}

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			stopSpinner()
			// The turn's last message updates are published before Run
			// returns, so they can still be waiting: select picks among
			// ready cases at random, and returning first could drop the end
			// of the answer, and a stream-json run its last message.
			if err := drainMessageEvents(messageEvents, handle); err != nil {
				return err
			}
			// A Stop hook's warning is published just before the turn
			// ends, so it may still be waiting here.
			for drained := false; !drained; {
				select {
				case event := <-notifications:
					warn(event.Payload)
				default:
					drained = true
				}
			}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) || errors.Is(result.err, agent.ErrRequestCancelled) {
					slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
					return nil
				}
				return runError(result.err)
			}
			if goal == "" {
				return nil
			}
			g, set, err := app.SessionGoal(ctx, sess.ID)
			return GoalOutcome(os.Stderr, goal, met, g, set, err)

		case event := <-messageEvents:
			if err := handle(event.Payload); err != nil {
				return err
			}

		case event := <-notifications:
			warn(event.Payload)

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

// traceToolCalls traces the tool calls and results a message carries:
// the calls an assistant message made, and the results that come back in
// a tool message of their own. sub indents a sub-agent's calls.
func traceToolCalls(trace *format.ToolTrace, msg message.Message, sub bool) {
	switch msg.Role {
	case message.Tool:
		for _, result := range msg.ToolResults() {
			trace.Result(result.ToolCallID, result.IsError, result.Metadata)
		}
	case message.Assistant:
		for _, call := range msg.ToolCalls() {
			if !call.Finished {
				continue
			}
			if sub {
				trace.SubCall(call.ID, call.Name, call.Summary())
			} else {
				trace.Call(call.ID, call.Name, call.Summary())
			}
		}
	}
}

// answerPrinter writes the text of a run's assistant messages as it
// streams in, each new part once, with a blank line between one message
// and the next: they used to run together ("Let me look.There are two
// files.").
type answerPrinter struct {
	w         io.Writer
	readBytes map[string]int // how much of each message's text is written
	printed   bool
	lastID    string // the message last written to
}

func (p *answerPrinter) print(msg message.Message) error {
	content := msg.Content().String()
	readBytes := p.readBytes[msg.ID]
	if len(content) < readBytes {
		slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
		return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
	}
	part := content[readBytes:]
	p.readBytes[msg.ID] = len(content)
	// Trim leading whitespace. Sometimes the LLM includes leading
	// formatting and indentation, which we don't want here.
	if readBytes == 0 {
		part = strings.TrimLeft(part, " \t")
	}
	// Ignore initial whitespace-only messages.
	if part == "" || (!p.printed && strings.TrimSpace(part) == "") {
		return nil
	}
	if p.printed && msg.ID != p.lastID {
		fmt.Fprint(p.w, "\n\n")
	}
	p.printed = true
	p.lastID = msg.ID
	fmt.Fprint(p.w, part)
	return nil
}

// drainMessageEvents hands handle every message event already waiting in
// events, without waiting for more.
func drainMessageEvents(events <-chan pubsub.Event[message.Message], handle func(message.Message) error) error {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if err := handle(event.Payload); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// descendsFrom reports whether session id was made under ancestorID, by
// a sub-agent of it or of one of its sub-agents. It looks at most eight
// levels up, so a cycle in bad data cannot keep it walking.
func (app *App) descendsFrom(ctx context.Context, id, ancestorID string) bool {
	for range 8 {
		s, err := app.Sessions.Get(ctx, id)
		if err != nil || s.ParentSessionID == "" {
			return false
		}
		if s.ParentSessionID == ancestorID {
			return true
		}
		id = s.ParentSessionID
	}
	return false
}

// warningLine is the stderr line a non-interactive run prints for a
// notification of the run's own session: a warning, or a goal the judge
// found met, which only the TUI showed.
func warningLine(n notify.Notification, sessionID string) (string, bool) {
	if n.SessionID != sessionID || n.Message == "" {
		return "", false
	}
	switch n.Type {
	case notify.TypeWarning:
		return "warning: " + n.Message + "\n", true
	case notify.TypeGoalMet, notify.TypeFramePopped:
		return n.Message + "\n", true
	}
	return "", false
}

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// restoreModelFromSession reads the last assistant message in the
// session and, if it used a different provider/model than the current
// config, overrides the preferred model in-memory (non-persistent)
// provided the provider/model is still available. This ensures that
// continuing a session uses the same model that produced the last
// response.
func (app *App) restoreModelFromSession(ctx context.Context, sessionID string) error {
	lastMsg, err := app.Messages.GetLastAssistantMessage(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("failed to get last assistant message: %w", err)
	}
	if lastMsg.Provider == "" || lastMsg.Model == "" {
		return nil
	}

	cfg := app.config.Config()
	currentLarge := cfg.Models[config.SelectedModelTypeLarge]
	if currentLarge.Provider == lastMsg.Provider && currentLarge.Model == lastMsg.Model {
		return nil
	}

	if !cfg.IsModelAvailable(lastMsg.Provider, lastMsg.Model) {
		slog.Debug("Skipping model restoration: provider/model not available",
			"provider", lastMsg.Provider,
			"model", lastMsg.Model)
		return nil
	}

	app.config.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: lastMsg.Provider,
		Model:    lastMsg.Model,
	})
	if _, ok := cfg.Models[config.SelectedModelTypeSmall]; !ok {
		smallModel := app.GetDefaultSmallModel(lastMsg.Provider)
		app.config.OverridePreferredModel(config.SelectedModelTypeSmall, smallModel)
	}
	if err := app.AgentCoordinator.UpdateModels(ctx); err != nil {
		return fmt.Errorf("failed to update agent models: %w", err)
	}
	slog.Info("Restored model from session",
		"provider", lastMsg.Provider,
		"model", lastMsg.Model)
	return nil
}

// OverrideModels parses the model strings and overrides the model
// configurations in memory, not saved, for gnosh run's --model and
// --small-model and the TUI's --model, then rebuilds the agent.
// Format: "model-name" (searches all providers) or "provider/model-name".
// Model matching is case-insensitive.
// If largeModel is provided but smallModel is not, the small model defaults to
// the provider's default small model.
func (app *App) OverrideModels(ctx context.Context, largeModel, smallModel string) error {
	providers := app.config.Config().Providers.Copy()

	largeMatches, smallMatches, err := findModels(providers, largeModel, smallModel)
	if err != nil {
		return err
	}

	var largeProviderID string

	// Override large model.
	if largeModel != "" {
		found, err := validateMatches(largeMatches, largeModel, "large")
		if err != nil {
			return err
		}
		largeProviderID = found.provider
		slog.Info("Overriding large model", "provider", found.provider, "model", found.modelID)
		app.config.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		})
	}

	// Override small model.
	switch {
	case smallModel != "":
		found, err := validateMatches(smallMatches, smallModel, "small")
		if err != nil {
			return err
		}
		slog.Info("Overriding small model", "provider", found.provider, "model", found.modelID)
		app.config.OverridePreferredModel(config.SelectedModelTypeSmall, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		})

	case largeModel != "":
		// No small model specified, but large model was - use provider's default.
		smallCfg := app.GetDefaultSmallModel(largeProviderID)
		app.config.OverridePreferredModel(config.SelectedModelTypeSmall, smallCfg)
	}

	return app.AgentCoordinator.UpdateModels(ctx)
}

// overrideReasoningEffort validates the requested reasoning effort against
// the large model in effect for this run (which may have been overridden by
// --model or restored from a continued session) and applies it as an
// in-memory override.
func (app *App) overrideReasoningEffort(ctx context.Context, reasoningEffort string) error {
	cfg := app.config.Config()
	selected, ok := cfg.Models[config.SelectedModelTypeLarge]
	if !ok {
		return fmt.Errorf("no large model selected; set one with the --model flag or 'model large'")
	}
	if err := cfg.ValidateReasoningEffort(selected.Provider, selected.Model, reasoningEffort); err != nil {
		return err
	}
	selected.ReasoningEffort = reasoningEffort
	slog.Info("Overriding reasoning effort for non-interactive run",
		"provider", selected.Provider,
		"model", selected.Model,
		"reasoning_effort", reasoningEffort)
	app.config.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	return app.AgentCoordinator.UpdateModels(ctx)
}

// GetDefaultSmallModel returns the default small model for the given
// provider. Falls back to the large model if no default is found.
func (app *App) GetDefaultSmallModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	largeModelCfg := cfg.Models[config.SelectedModelTypeLarge]

	// Find the provider in the known providers list to get its default small model.
	knownProviders, _ := config.Providers(cfg)
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the large model as small.
	if knownProvider == nil {
		slog.Warn("Using large model as small model for unknown provider", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	defaultSmallModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultSmallModelID)
	if model == nil {
		slog.Warn("Default small model not found, using large model", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	// A subscription-authenticated provider only serves the models its
	// subscription grants, so the default small model must come from that
	// catalog as well. This applies to ChatGPT-authenticated OpenAI and
	// Grok-authenticated xAI alike.
	if largeModelCfg.Provider == providerID {
		var pick func(config.ProviderConfig) *catwalk.Model
		switch providerID {
		case string(catwalk.InferenceProviderOpenAI):
			pick = chatGPTSmallModel
		case string(catwalk.InferenceProviderXAI):
			pick = grokSmallModel
		}
		if pick != nil {
			if pc, ok := cfg.Providers.Get(providerID); ok && pc.OAuthToken != nil {
				if small := pick(pc); small != nil {
					return config.SelectedModel{
						Provider:        providerID,
						Model:           small.ID,
						MaxTokens:       small.DefaultMaxTokens,
						ReasoningEffort: small.DefaultReasoningEffort,
					}
				}
				return largeModelCfg
			}
		}
	}

	slog.Info("Using provider default small model", "provider", providerID, "model", defaultSmallModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultSmallModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
}

// chatGPTSmallModel picks a lightweight model from the ChatGPT catalog,
// preferring a "mini" variant and falling back to the last entry (the
// catalog lists heavier models first). Returns nil when the catalog is
// empty.
func chatGPTSmallModel(pc config.ProviderConfig) *catwalk.Model {
	for i := range pc.ChatGPTModels {
		if strings.Contains(pc.ChatGPTModels[i].ID, "mini") {
			return &pc.ChatGPTModels[i]
		}
	}
	if len(pc.ChatGPTModels) > 0 {
		return &pc.ChatGPTModels[len(pc.ChatGPTModels)-1]
	}
	return nil
}

// grokSmallModel picks a lightweight model from the Grok catalog,
// preferring a "fast" or "mini" variant and falling back to the last
// entry (the catalog lists heavier models first). Returns nil when the
// catalog is empty.
func grokSmallModel(pc config.ProviderConfig) *catwalk.Model {
	for _, marker := range []string{"fast", "mini"} {
		for i := range pc.GrokModels {
			if strings.Contains(pc.GrokModels[i].ID, marker) {
				return &pc.GrokModels[i]
			}
		}
	}
	if len(pc.GrokModels) > 0 {
		return &pc.GrokModels[len(pc.GrokModels)-1]
	}
	return nil
}

func (app *App) setupEvents() {
	ctx, cancel := context.WithCancel(app.globalCtx)
	app.eventsCtx = ctx
	app.subscribe(ctx, "sessions", app.Sessions.Subscribe)
	app.subscribe(ctx, "messages", app.Messages.Subscribe)
	app.subscribeMustDeliver(ctx, "permissions", app.Permissions.Subscribe)
	app.subscribeMustDeliver(ctx, "permissions-notifications", app.Permissions.SubscribeNotifications)
	app.subscribeMustDeliver(ctx, "question-batches", app.Questions.Subscribe)
	app.subscribeMustDeliver(ctx, "question-notifications", app.Questions.SubscribeNotifications)
	app.subscribe(ctx, "history", app.History.Subscribe)
	app.subscribe(ctx, "agent-notifications", app.agentNotifications.Subscribe)
	app.subscribeMustDeliver(ctx, "run-completions", app.runCompletions.Subscribe)
	app.subscribe(ctx, "mcp", mcp.SubscribeEvents)
	app.subscribe(ctx, "mcp-channels", app.subscribeScopedChannelEvents)
	app.subscribe(ctx, "lsp", SubscribeLSPEvents)
	if app.Skills != nil {
		app.subscribe(ctx, "skills", app.Skills.SubscribeEvents)
	}
	cleanupFunc := func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	}
	app.cleanupFuncs = append(app.cleanupFuncs, cleanupFunc)
}

// subscribeScopedChannelEvents forwards channel message events for servers
// this workspace both declares in its MCP config and opted in (via
// --channels or channel_enabled; see mcp.ChannelOptIn).
// The MCP broker is process-global and channel events carry no workspace
// identity, so this per-app scoping is what keeps another workspace's channel
// messages out of this app's event stream (see mcp.SubscribeChannelEvents).
// The scoped events feed the TUI's in-process injection and, in server mode,
// the SSE stream to attached clients.
func (app *App) subscribeScopedChannelEvents(ctx context.Context) <-chan pubsub.Event[mcp.Event] {
	raw := mcp.SubscribeChannelEvents(ctx)
	scoped := make(chan pubsub.Event[mcp.Event], 64)
	go func() {
		defer close(scoped)
		for ev := range raw {
			mcpCfg, declared := app.config.Config().MCP[ev.Payload.Name]
			if !declared {
				continue
			}
			if !mcp.ChannelOptIn(mcpCfg, app.config.Overrides().EnabledChannels, ev.Payload.Name) {
				continue
			}
			select {
			case scoped <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return scoped
}

// subscribe fans a service's event stream into the shared app.events
// broker on app.serviceEventsWG, re-publishing each upstream event as a
// tea.Msg. The goroutine exits when ctx is cancelled or the upstream
// channel closes. It is a generic method (Go 1.27) so it can live in the
// App namespace while still inferring the upstream event type T.
func (app *App) subscribe[T any](
	ctx context.Context,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
) {
	app.serviceEventsWG.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				app.events.Publish(pubsub.UpdatedEvent, tea.Msg(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

// subscribeMustDeliver is the bounded-blocking fan-in variant of
// [App.subscribe]: it re-publishes upstream events onto the shared
// app.events broker using PublishMustDeliver instead of Publish. Use
// this for terminal events that subscribers cannot tolerate losing —
// notably RunComplete, which is the authoritative end-of-run signal
// for `crush run`. A lossy fan-in here can drop the only terminal
// event and hang non-interactive clients waiting on it.
func (app *App) subscribeMustDeliver[T any](
	ctx context.Context,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
) {
	app.serviceEventsWG.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				app.events.PublishMustDeliver(ctx, pubsub.UpdatedEvent, tea.Msg(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

// setNonInteractive records whether the next agent initialization is for
// a gnosh run (true) or the TUI (false). The flag is what Shutdown reads
// to pick the SessionEnd reason for the open session: "other" for a run,
// "prompt_input_exit" for the TUI.
func (app *App) setNonInteractive(nonInteractive bool) {
	app.sessionEndMu.Lock()
	app.nonInteractive = nonInteractive
	app.sessionEndMu.Unlock()
}

// StoppedBySignal makes the session open when gnosh exits end with
// reason "other", as Claude Code's does when SIGINT or SIGTERM stops it:
// "prompt_input_exit" says the user left from the prompt.
func (app *App) StoppedBySignal() {
	app.sessionEndMu.Lock()
	app.stoppedBySignal = true
	app.sessionEndMu.Unlock()
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	// The interactive agent is for the TUI, which exits with reason
	// prompt_input_exit; reset the flag a gnosh run may have set so a
	// TUI session that follows a run on the same App is not mislabeled.
	app.setNonInteractive(false)
	return app.initCoderAgent(ctx, true)
}

// InitCoderAgentNonInteractive initializes the coder agent without
// interactive-only tools (e.g. question).
func (app *App) InitCoderAgentNonInteractive(ctx context.Context) error {
	app.setNonInteractive(true)
	return app.initCoderAgent(ctx, false)
}

// setArtifactServer makes or drops the artifact server to match how the
// coder agent is being initialized. Both directions matter: app.New
// initializes the agent interactively, and a `gnosh run` only then
// re-initializes it without the interactive tools, so a server made for
// that first init has to go. Otherwise every headless run holds the
// artifact port open for a tool its agent is never given, and the next
// interactive session, finding the port taken, falls back to a random
// one and its URLs no longer outlive it.
func (app *App) setArtifactServer(interactive bool, opts *config.Options) {
	if !interactive {
		if app.artifacts != nil {
			_ = app.artifacts.Close()
			app.artifacts = nil
		}
		return
	}
	if app.artifacts != nil {
		return
	}
	app.artifacts = artifactServerFor(interactive, opts)
	if app.artifacts != nil {
		app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
			return app.artifacts.Close()
		})
	}
}

// artifactServerFor serves the deliverables the agent records, for a
// session that has a sidebar to show them in. A run has none and serves
// nothing: its agent is given no artifact tool either.
func artifactServerFor(interactive bool, opts *config.Options) *artifacts.Server {
	if !interactive {
		return nil
	}
	return artifacts.Start(opts.ArtifactServer, opts.DataDirectory)
}

func (app *App) initCoderAgent(ctx context.Context, interactive bool) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	if app.gnovms == nil {
		app.gnovms = gnovm.NewLazy(gnovm.ClientOptions{Options: gnovm.Options{
			Dir:        GnoVMDir(),
			Embeddings: app.config.Config().Options.MemEmbeddingsURL,
		}})
		hooks.SetMemStatusPath(app.gnovms.MemStatusPath)
	}
	app.setArtifactServer(interactive, app.config.Config().Options)
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(ctx, agent.CoordinatorOptions{
		Config:      app.config,
		Sessions:    app.Sessions,
		Messages:    app.Messages,
		Permissions: app.Permissions,
		Questions:   app.Questions,
		History:     app.History,
		FileTracker: app.FileTracker,
		LSPManager:  app.LSPManager,
		Notify:      app.agentNotifications,
		RunComplete: app.runCompletions,
		Skills:      app.Skills,
		Interactive: interactive,
		GnoVMs:      app.gnovms,
		MemErased:   app.memErasedNotice,
		MemRules:    app.memRulesNotice,
		Artifacts:   app.artifacts,
	})
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}

// Subscribe sends events to the TUI as tea.Msgs.
func (app *App) Subscribe(program *tea.Program) {
	defer log.RecoverPanic("app.Subscribe", func() {
		slog.Info("TUI subscription panic: attempting graceful shutdown")
		program.Quit()
	})

	app.tuiWG.Add(1)
	tuiCtx, tuiCancel := context.WithCancel(app.globalCtx)
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		slog.Debug("Cancelling TUI message handler")
		tuiCancel()
		app.tuiWG.Wait()
		return nil
	})
	defer app.tuiWG.Done()

	events := app.events.Subscribe(tuiCtx)
	for {
		select {
		case <-tuiCtx.Done():
			slog.Debug("TUI message handler shutting down")
			return
		case ev, ok := <-events:
			if !ok {
				slog.Debug("TUI message channel closed")
				return
			}
			program.Send(ev.Payload)
		}
	}
}

// Shutdown performs a graceful shutdown of the application.
func (app *App) Shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	// Shared shutdown context for all timeout-bounded cleanup, created
	// first so the SessionEnd wait below is inside it, not ahead of it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The process exits with the reported session open: end it, with the
	// reason it is exiting. "prompt_input_exit" for the TUI, "other" for
	// gnosh run, which is not in a prompt input loop. The reason is read
	// under sessionEndMu, the same lock setNonInteractive writes it
	// under, so a TUI reinitializing its agent concurrently with the
	// exit cannot read a half-updated flag. A session already ended —
	// switched away from, say — does not end again.
	app.sessionEndMu.Lock()
	reason := sessionEndReasonPromptInputExit
	if app.nonInteractive || app.stoppedBySignal {
		reason = sessionEndReasonOther
	}
	app.sessionEndMu.Unlock()
	if id := app.takeCurrentSessionID(); id != "" {
		app.endSession(id, reason)
	}
	// The hooks run asynchronously; wait for them inside the shutdown
	// budget, so the process does not exit before they have run, but a
	// slow hook does not hold the exit past the budget: at the deadline
	// the wait is released, and the hook goes on under its own timeout.
	app.waitForSessionEndHooks(shutdownCtx)

	// First, cancel all agents and wait for them to finish. This must
	// complete before closing the DB so agents can finish writing their
	// state.
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
	}

	// Drain any debounced message updates before the DB-close cleanup
	// runs in the parallel block below. message.Service buffers
	// streaming deltas (see internal/message/message.go) and we must
	// land them while the connection is still open.
	if app.Messages != nil {
		if err := app.Messages.FlushAll(shutdownCtx); err != nil {
			slog.Error("Failed to flush pending message updates on shutdown", "error", err)
		}
	}
	// Release this App's hold on the GnoVM machines: the last App in the
	// process stops the worker and deletes its memory-only machines.
	if app.gnovms != nil {
		app.gnovms.Close()
	}

	// Now run remaining cleanup tasks in parallel.
	var wg sync.WaitGroup

	// Send exit event
	wg.Go(func() {
		event.AppExited()
	})

	// Kill all background shells.
	wg.Go(func() {
		shell.GetBackgroundShellManager().KillAll(shutdownCtx)
	})

	// Close herdr client to stop its background writer.
	app.herdrClient.Close()

	// Shutdown all LSP clients.
	wg.Go(func() {
		app.LSPManager.KillAll(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
				}
			})
		}
	}
	wg.Wait()
}

// updateCheckEnabled reports whether to ask GitHub for a newer release. The
// check reads the configured update_repo's latest release, which says nothing about
// this fork, and a source build's version reads as a prerelease, so it
// would report an update on every start. It runs only when asked for with
// GNOSH_DISABLE_UPDATE_CHECK=0.
func updateCheckEnabled() bool {
	disable, ok := env.LookupBool("GNOSH_DISABLE_UPDATE_CHECK")
	return ok && !disable
}

// checkForUpdates checks for available updates.
func (app *App) checkForUpdates(ctx context.Context) {
	if !updateCheckEnabled() {
		return
	}
	// The fork is not on GitHub and Charm's repo is never a default, so the
	// check runs only against the repo the user configured (update_repo).
	repo := app.config.Config().Options.UpdateCheckRepo()
	if repo == "" {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := update.Check(checkCtx, version.Version, update.NewGitHub(repo))
	if err != nil || !info.Available() {
		return
	}
	app.events.Publish(pubsub.UpdatedEvent, UpdateAvailableMsg{
		CurrentVersion: info.Current,
		LatestVersion:  info.Latest,
		IsDevelopment:  info.IsDevelopment(),
	})
}

// runError is what a non-interactive run reports for the error its turn
// ended with. A prompt a UserPromptSubmit hook blocked is reported with
// the hook's reason as it stands: the user's own hook refused it, nothing
// failed. Anything else failed somewhere in the turn, which is all the
// error can say: it used to read "failed to start agent processing
// stream" also for a model error halfway through.
func runError(err error) error {
	if errors.Is(err, agent.ErrPromptBlocked) {
		return err
	}
	return fmt.Errorf("agent processing failed: %w", err)
}
