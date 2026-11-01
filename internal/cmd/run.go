package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/herdr"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.
The prompt can be provided as arguments or piped from stdin.`,
	Example: `
# Run a simple prompt
gnosh run "Guess my 5 favorite Pokémon"

# Pipe input from stdin
curl https://charm.land | gnosh run "Summarize this website"

# Read from a file
gnosh run "What is this code doing?" <<< prrr.go

# Redirect output to a file
gnosh run "Generate a hot README for this project" > MY_HOT_README.md

# Run in quiet mode (hide the spinner)
gnosh run --quiet "Generate a README for this project"

# Run in verbose mode (show logs)
gnosh run --verbose "Generate a README for this project"

# Use a specific reasoning effort
# Levels depend on the model, unsupported values are rejected
# with the accepted values listed
gnosh run --reasoning-effort high "What is the meaning of life?"

# Continue a previous session
gnosh run --session {session-id} "Follow up on your last response"

# Continue the most recent session
gnosh run --continue "Follow up on your last response"

  `,
	RunE: stopsOnSignal(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		var (
			quiet, _           = cmd.Flags().GetBool("quiet")
			verbose, _         = cmd.Flags().GetBool("verbose")
			largeModel, _      = cmd.Flags().GetString("model")
			smallModel, _      = cmd.Flags().GetString("small-model")
			reasoningEffort, _ = cmd.Flags().GetString("reasoning-effort")
			useLast, _         = cmd.Flags().GetBool("continue")
			fork, _            = cmd.Flags().GetBool("fork-session")
			name               = nameFlag(cmd)
			goal, _            = cmd.Flags().GetString("goal")
		)

		// sessionID is read from --session or --resume; --resume also
		// takes a session's name.
		sessionID := sessionFlag(cmd)
		if sessionID == pickSession {
			return errors.New("a session ID or name must follow -r in gnosh run; `gnosh -r` on its own lists the sessions to pick from")
		}
		if err := forkAlone(cmd); err != nil {
			return err
		}
		resume, _ := cmd.Flags().GetString("resume")

		prompt := strings.Join(args, " ")

		prompt, err := MaybePrependStdin(prompt)
		if err != nil {
			slog.Error("Failed to read from stdin", "error", err)
			return err
		}

		if prompt == "" {
			return fmt.Errorf("no prompt provided")
		}
		if goal = strings.TrimSpace(goal); utf8.RuneCountInString(goal) > agent.GoalConditionLimit {
			return fmt.Errorf("--goal is limited to %d characters", agent.GoalConditionLimit)
		}

		outputFormat, _ := cmd.Flags().GetString("output-format")
		output, err := newRunOutput(outputFormat, os.Stdout)
		if err != nil {
			return err
		}

		if output.stream != nil && useClientServer() {
			return fmt.Errorf("--output-format stream-json is not supported with %s=1 yet; use text or json", "GNOSH_CLIENT_SERVER")
		}

		event.SetNonInteractive(true)

		switch {
		case sessionID != "":
			event.SetContinueBySessionID(true)
		case useLast:
			event.SetContinueLastSession(true)
		}

		if useClientServer() {
			if safeModeOn(cmd) {
				return errSafeModeServer
			}
			c, ws, cleanup, err := connectToServer(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			event.AppInitialized()
			// gnosh run never asks: an untrusted folder's settings stay
			// out, and it says so.
			noteUntrusted(cmd, scanUntrusted(cmd))

			if !ws.Config.IsConfigured() {
				return noProviderError(ws.Config)
			}

			clientWs := workspace.NewClientWorkspace(c, *ws)
			if err := clientWs.InitCoderAgentNonInteractive(ctx); err != nil {
				return fmt.Errorf("failed to initialize agent: %w", err)
			}

			if sessionID != "" {
				id, err := clientSessionID(ctx, c, ws.ID, sessionID, resume != "")
				if err != nil {
					return err
				}
				sessionID = id
			}

			if verbose {
				slog.SetDefault(slog.New(log.New(os.Stderr)))
			}

			if enabled, ok := config.ParseThinkingCommand(prompt); ok {
				return setThinking(clientWs, enabled, cmd.OutOrStdout())
			}
			hideSpinner, traceTools := runOutputFlags(quiet, verbose)
			notice, err := clientWs.MemNotice(ctx)
			printMemNotice(cmd.ErrOrStderr(), notice, err)
			return output.finish(runNonInteractive(ctx, c, ws, prompt, largeModel, smallModel, reasoningEffort, hideSpinner, traceTools, sessionID, useLast, fork, name, goal, output))
		}

		ws, cleanup, err := setupLocalWorkspace(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		event.AppInitialized()
		// gnosh run never asks: an untrusted folder's settings stay out,
		// and it says so.
		noteUntrusted(cmd, ws.Config().HeldProject)

		if !ws.Config().IsConfigured() {
			return noProviderError(ws.Config())
		}

		if verbose {
			slog.SetDefault(slog.New(log.New(os.Stderr)))
		}

		appWs := ws.(*workspace.AppWorkspace)

		if sessionID != "" {
			id, err := localSessionID(ctx, appWs.App().Sessions, sessionID, resume != "")
			if err != nil {
				return err
			}
			sessionID = id
		}

		if enabled, ok := config.ParseThinkingCommand(prompt); ok {
			return setThinking(ws, enabled, cmd.OutOrStdout())
		}
		hideSpinner, traceTools := runOutputFlags(quiet, verbose)
		printMemNotice(cmd.ErrOrStderr(), appWs.App().MemNotice(), nil)
		permissionMode := config.PermissionModeBypass // run accepts its session's permission requests
		if mode, _ := permissionModeFlag(cmd); mode == config.PermissionModePlan {
			permissionMode = config.PermissionModePlan
		}
		return output.finish(appWs.App().RunNonInteractive(ctx, output.w, prompt, largeModel, smallModel, reasoningEffort, hideSpinner, traceTools, sessionID, useLast, fork, name, goal, func(id string) {
			output.started(id, func() streamInit {
				// Asked for then, not now: the run replaces the coordinator
				// with one without the interactive-only tools.
				coordinator := appWs.App().AgentCoordinator
				return streamInit{
					SessionID:      id,
					CWD:            appWs.WorkingDir(),
					Model:          coordinator.Model().ModelCfg.Model,
					PermissionMode: permissionMode,
					Tools:          coordinator.ToolNames(),
					MCPServers:     mcpServerStatuses(),
				}
			})
		}, output.message))
	}),
}

// printMemNotice says on stderr, as a run starts, what in mem is due and
// what waits on the user (notice, one line): the model, given another task,
// may not say it.
func printMemNotice(w io.Writer, notice string, err error) {
	if err == nil && notice != "" {
		_, _ = fmt.Fprintln(w, notice)
	}
}

// runOutputFlags maps run's --quiet and --verbose to what a run shows on
// stderr. Either hides the spinner: --verbose writes logs and a line per
// tool call to stderr, where the spinner would draw over them.
func runOutputFlags(quiet, verbose bool) (hideSpinner, traceTools bool) {
	return quiet || verbose, verbose
}

func init() {
	addRunFlags(runCmd)
}

// addRunFlags adds gnosh run's flags to cmd.
func addRunFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	cmd.Flags().BoolP("verbose", "v", false, "Show logs and a line per tool call on stderr")
	cmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	cmd.Flags().String("small-model", "", "Small model to use. If not provided, uses the default small model for the provider")
	cmd.Flags().String("reasoning-effort", "", "Reasoning effort for the model (e.g. low, medium, high). Levels depend on the model; unsupported values are rejected with the accepted values listed")
	cmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	cmd.Flags().StringP("resume", "r", "", "Same as --session, by Claude Code's name for it, and also by a session's name")
	cmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	cmd.MarkFlagsMutuallyExclusive("session", "resume", "continue")
	cmd.Flags().Bool("fork-session", false, forkSessionUsage)
	cmd.Flags().StringP("name", "n", "", nameUsage)
	cmd.Flags().String("goal", "", "Keep working until this holds, as /goal does: the goal is kept in mem, checked once the agent closes it, and the run exits non-zero while it is still set")
	addSafeModeFlag(cmd)
	addTrustFolderFlag(cmd)
	cmd.Flags().String("output-format", "text", "How the answer is printed: text, as it streams; json, one result object at the end; or stream-json, a JSON line per message and the result at the end (as Claude Code's -p --output-format)")
	// The yolo flags are accepted so that Claude Code's
	// `-p --dangerously-skip-permissions` works, and change nothing: run
	// already accepts its own session's permission requests, and keeps
	// the flags from the workspace (see workspaceYolo).
	addYoloFlags(cmd)
	addToolFlags(cmd)
	addSystemPromptFlags(cmd)
	addPermissionModeFlag(cmd)
	addAddDirFlag(cmd)
	addMCPConfigFlags(cmd)
	cmd.Flags().Lookup("yolo").Usage = "Accepted for Claude Code's sake; changes nothing, as gnosh run already accepts its session's permission requests"
	cmd.Flags().Lookup("dangerously-skip-permissions").Usage = "Same as --yolo"
}

// traceToolCalls traces the tool calls and results a message carries:
// the calls an assistant message made, and the results that come back in
// a tool message of their own. sub indents a sub-agent's calls.
func (s *runStream) traceToolCalls(msg proto.Message, sub bool) {
	switch msg.Role {
	case proto.Tool:
		for _, result := range msg.ToolResults() {
			s.trace.Result(result.ToolCallID, result.IsError, result.Metadata)
		}
	case proto.Assistant:
		for _, call := range msg.ToolCalls() {
			if !call.Finished {
				continue
			}
			if sub {
				s.trace.SubCall(call.ID, call.Name, toolCallSummary(call))
			} else {
				s.trace.Call(call.ID, call.Name, toolCallSummary(call))
			}
		}
	}
}

// subSessionCheck returns a check, asking the server once per session,
// of whether a session was made under rootID by a sub-agent of it or of
// one of its sub-agents. It looks at most eight levels up, so a cycle in
// bad data cannot keep it walking.
func subSessionCheck(ctx context.Context, c *client.Client, workspaceID, rootID string) func(string) bool {
	known := map[string]bool{}
	return func(id string) bool {
		if sub, ok := known[id]; ok {
			return sub
		}
		sub := false
		for at, level := id, 0; level < 8; level++ {
			sess, err := c.GetSession(ctx, workspaceID, at)
			if err != nil || sess.ParentSessionID == "" {
				break
			}
			if sess.ParentSessionID == rootID {
				sub = true
				break
			}
			at = sess.ParentSessionID
		}
		known[id] = sub
		return sub
	}
}

// ours reports whether an agent event belongs to this run. Async events
// from an unrelated workspace run share the channel, so a foreign failure
// must not abort us:
//   - if the event carries a RunID, it is the authoritative correlator: it
//     must match our run exactly, otherwise it belongs to a different
//     request.
//   - if the event carries no RunID (an older server, or a notification
//     about the session such as a warning), it must carry our
//     SessionID.
func (s *runStream) ours(ev proto.AgentEvent) bool {
	if ev.RunID != "" {
		return ev.RunID == s.runID
	}
	return ev.SessionID != "" && ev.SessionID == s.sessionID
}

// runNonInteractive executes the agent via the server and streams output
// to stdout.
func runNonInteractive(
	ctx context.Context,
	c *client.Client,
	ws *proto.Workspace,
	prompt, largeModel, smallModel, reasoningEffort string,
	hideSpinner, traceTools bool,
	continueSessionID string,
	useLast, fork bool,
	name, goal string,
	output *runOutput,
) error {
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if largeModel != "" || smallModel != "" {
		if err := overrideModels(ctx, c, ws, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	// The reasoning effort applies to the model that will actually run.
	// On a continued session without an explicit model override, the
	// model is resolved later from the session's last assistant message,
	// so the override is applied after that restore instead.
	deferredEffort := (continueSessionID != "" || useLast) && largeModel == "" && smallModel == ""
	if reasoningEffort != "" && !deferredEffort {
		if err := overrideReasoningEffort(ctx, c, ws.ID, reasoningEffort); err != nil {
			return err
		}
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = ws.Config.Options.Progress == nil || *ws.Config.Options.Progress

	if !hideSpinner && stderrTTY {
		t := common.ThemeStylesFromConfig(ws.Config)

		spinner = format.NewSpinner(ctx, cancel, anim.Settings{
			Size:        10,
			Label:       "Generating",
			GradColorA:  t.WorkingGradFromColor,
			GradColorB:  t.WorkingGradToColor,
			CycleColors: true,
		})
		spinner.Start()
	}

	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for the agent to become ready (MCP init, etc).
	if err := waitForAgent(ctx, c, ws.ID); err != nil {
		stopSpinner()
		return fmt.Errorf("agent not ready: %w", err)
	}

	// Force-update agent models so MCP tools are loaded.
	if err := c.UpdateAgent(ctx, ws.ID); err != nil {
		slog.Warn("Failed to update agent", "error", err)
	}

	defer stopSpinner()

	sess, err := resolveSession(ctx, c, ws.ID, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to resolve session: %w", err)
	}
	switch {
	case fork:
		if sess, err = c.BranchSession(ctx, ws.ID, sess.ID, name); err != nil {
			return fmt.Errorf("failed to fork the session: %w", err)
		}
	case name != "":
		// The server has no rename of its own yet.
		sess.Title = name
		if sess, err = c.SaveSession(ctx, ws.ID, *sess); err != nil {
			return fmt.Errorf("failed to name the session: %w", err)
		}
	}
	output.sessionID = sess.ID
	// --goal binds the session's goal in mem before the prompt is sent,
	// which carries the directive the binding gives.
	if goal != "" {
		status, err := c.SetSessionGoal(ctx, ws.ID, sess.ID, goal)
		if err != nil {
			return fmt.Errorf("failed to set the goal: %w", err)
		}
		prompt = strings.TrimSpace(prompt + "\n\n" + status.Directive)
	}
	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
		// If no explicit model override was requested, restore the
		// model/provider from the last assistant message in the
		// session, provided it is still available.
		if largeModel == "" && smallModel == "" {
			if err := restoreModelFromSession(ctx, c, ws, sess.ID); err != nil {
				slog.Warn("Failed to restore model from session", "error", err)
			}
		}
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	if reasoningEffort != "" && deferredEffort {
		if err := overrideReasoningEffort(ctx, c, ws.ID, reasoningEffort); err != nil {
			return err
		}
	}

	events, err := c.SubscribeEvents(ctx, ws.ID)
	if err != nil {
		return fmt.Errorf("failed to subscribe to events: %w", err)
	}

	// Mint a per-call RunID so we can correlate the terminal
	// RunComplete with *this* SendMessage even if the session was
	// busy and another turn finished first. Without it the stream
	// loop would exit on whichever RunComplete arrived first for
	// the same session and drop the queued prompt's output.
	runID := uuid.New().String()
	if err := c.SendMessage(ctx, ws.ID, sess.ID, runID, "", prompt); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	stream := &runStream{
		sessionID: sess.ID,
		runID:     runID,
		out:       output.w,
		errOut:    os.Stderr,
		trace:     newToolTrace(traceTools),
		isSub:     subSessionCheck(ctx, c, ws.ID, sess.ID),
		read:      make(map[string]int),
	}

	// Start herdr integration when running inside a herdr pane.
	hc := herdr.Init()
	hc.SetSessionID(sess.ID)
	defer hc.Close()

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}()

	for {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case ev, ok := <-events:
			if !ok {
				stopSpinner()
				return nil
			}

			// Forward events to herdr if running inside a herdr pane.
			if hev := herdr.Translate(ev); hev != nil {
				hc.HandleEvent(hev)
			}

			done, err := stream.handle(ev, stopSpinner)
			if err != nil {
				return err
			}
			if done {
				return runGoalOutcome(ctx, c, ws.ID, sess.ID, goal)
			}

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

// runGoalOutcome ends a client run with --goal: it fails while the goal
// is still set. (Whether a goal no longer set was met or cleared, the
// warning printed above says: the server sends no event for a goal met.)
func runGoalOutcome(ctx context.Context, c *client.Client, workspaceID, sessionID, goal string) error {
	if goal == "" {
		return nil
	}
	g, err := c.SessionGoal(ctx, workspaceID, sessionID)
	switch {
	case err != nil:
		return fmt.Errorf("the goal could not be read: %w", err)
	case g != nil:
		return fmt.Errorf("the goal is still set: %s (`%s` in mem); last check: %s", g.Condition, g.Name, cmp.Or(g.LastCheck, "none yet"))
	}
	_, _ = fmt.Fprintf(os.Stderr, "goal no longer set: %s\n", goal)
	return nil
}

// runStream tracks the per-message stdout cursor and the
// reconciliation state used by [runNonInteractive] to translate
// streaming SSE events into a final, complete stdout for `crush run`.
// It is split out so the state machine can be exercised in unit tests
// without spinning up the full server/client harness.
//
// runID, when non-empty, is the authoritative correlator for the
// terminal RunComplete event: the stream suppresses live message
// events and only exits on a RunComplete whose RunID matches, so a
// turn that finishes first on the same session (e.g. when our prompt
// was queued behind a busy session) cannot contaminate stdout or
// terminate us prematurely. When empty (older servers, tests that
// don't supply one) the stream falls back to SessionID-only matching
// and live message streaming, which is still correct for the
// single-turn case.
type runStream struct {
	sessionID string
	runID     string
	out       io.Writer
	// errOut receives warnings; nil drops them.
	errOut io.Writer
	// trace prints the --verbose tool trace; nil when not tracing.
	trace *format.ToolTrace
	// isSub reports whether a session was made under the run's, by a
	// sub-agent, whose tool calls the trace shows too; nil shows none.
	isSub   func(sessionID string) bool
	read    map[string]int
	printed bool
}

// handle processes one SSE event. Returns done=true when the run
// loop should exit (RunComplete observed); returns an error only
// when the agent run failed (not on context cancel — that path is
// handled by the caller's select). stopSpinner is called on the
// first observable assistant output and on completion; passing nil
// is safe for tests.
func (s *runStream) handle(ev any, stopSpinner func()) (done bool, err error) {
	stop := func() {
		if stopSpinner != nil {
			stopSpinner()
		}
	}
	switch e := ev.(type) {
	case pubsub.Event[proto.Message]:
		msg := e.Payload
		if msg.SessionID != s.sessionID {
			// A sub-agent works in a session of its own, under the run's:
			// trace its tool calls too, indented.
			if s.trace != nil && s.isSub != nil && s.isSub(msg.SessionID) {
				s.traceToolCalls(msg, true)
			}
			return false, nil
		}
		s.traceToolCalls(msg, false)
		if msg.Role != proto.Assistant {
			return false, nil
		}
		if len(msg.Parts) == 0 {
			return false, nil
		}
		if s.runID != "" {
			return false, nil
		}
		stop()

		content := msg.Content().String()
		readBytes := s.read[msg.ID]
		if len(content) < readBytes {
			slog.Error("Non-interactive: message content shorter than read bytes",
				"message_length", len(content), "read_bytes", readBytes)
			return false, fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
		}

		part := content[readBytes:]
		if readBytes == 0 {
			part = strings.TrimLeft(part, " \t")
		}
		if s.printed || strings.TrimSpace(part) != "" {
			s.printed = true
			fmt.Fprint(s.out, part)
		}
		s.read[msg.ID] = len(content)
		return false, nil

	case pubsub.Event[proto.RunComplete]:
		// RunComplete is the authoritative end-of-run signal. We
		// exit on it instead of guessing from message finish parts,
		// which fire on every tool-call step too and were the
		// source of the regression where `crush run` exited
		// mid-turn on finish.reason == tool_use.
		//
		// Correlation:
		//   - if we minted a RunID for this SendMessage, only the
		//     event whose RunID matches is ours; any other turn
		//     finishing first on the same session (busy-session
		//     queue path) must be ignored.
		//   - if we have no RunID (older server, tests), fall back
		//     to SessionID matching.
		if s.runID != "" {
			if e.Payload.RunID != s.runID {
				return false, nil
			}
		} else if e.Payload.SessionID != s.sessionID {
			return false, nil
		}
		stop()
		if e.Payload.Error != "" && !e.Payload.Cancelled {
			return true, fmt.Errorf("agent run failed: %s", e.Payload.Error)
		}
		// Reconcile stdout against the authoritative final
		// assistant text carried in the event. The pubsub fan-in
		// does not serialize publishes across upstream brokers, so
		// the final message event may not have reached this loop
		// yet; the embedded Text field is the backstop that
		// guarantees the full final text always appears on stdout.
		if e.Payload.MessageID != "" {
			full := e.Payload.Text
			readBytes := s.read[e.Payload.MessageID]
			if readBytes < len(full) {
				tail := full[readBytes:]
				if readBytes == 0 {
					tail = strings.TrimLeft(tail, " \t")
				}
				if s.printed || strings.TrimSpace(tail) != "" {
					s.printed = true
					fmt.Fprint(s.out, tail)
				}
			}
		}
		return true, nil

	case pubsub.Event[proto.AgentEvent]:
		if e.Payload.Error == nil || !s.ours(e.Payload) {
			return false, nil
		}
		// A warning, or a goal the judge found met, carries its text in
		// Error too, but the turn goes on or ends normally: print it rather
		// than fail the run, as a goal met once did ("agent error: Goal
		// met: …").
		switch e.Payload.Type {
		case proto.AgentEventTypeWarning, proto.AgentEventTypeGoalMet, proto.AgentEventTypeFramePopped:
			if s.errOut != nil {
				stop()
				prefix := "warning: "
				if e.Payload.Type != proto.AgentEventTypeWarning {
					prefix = ""
				}
				_, _ = fmt.Fprintf(s.errOut, "%s%s\n", prefix, e.Payload.Error)
			}
			return false, nil
		}
		stop()
		return true, fmt.Errorf("agent error: %w", e.Payload.Error)
	}
	return false, nil
}

// toolCallSummary renders a short, single-line summary of a tool
// call's input for the non-interactive --verbose trace. The shared
// field precedence and truncation live in [message.ToolCall.Summary];
// this wrapper adapts it to the proto package's call type.
func toolCallSummary(call proto.ToolCall) string {
	return message.ToolCall{ID: call.ID, Name: call.Name, Input: call.Input}.Summary()
}

// newToolTrace returns the --verbose tool trace, written to stderr, or nil
// when the run is not traced.
func newToolTrace(enabled bool) *format.ToolTrace {
	if !enabled {
		return nil
	}
	return format.NewToolTrace(os.Stderr)
}

// waitForAgent polls GetAgentInfo until the agent is ready, with a
// timeout.
func waitForAgent(ctx context.Context, c *client.Client, wsID string) error {
	timeout := time.After(30 * time.Second)
	for {
		info, err := c.GetAgentInfo(ctx, wsID)
		if err == nil && info.IsReady {
			return nil
		}
		select {
		case <-timeout:
			if err != nil {
				return fmt.Errorf("timeout waiting for agent: %w", err)
			}
			return fmt.Errorf("timeout waiting for agent readiness")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// overrideModels resolves model strings and updates the workspace
// configuration via the server.
func overrideModels(
	ctx context.Context,
	c *client.Client,
	ws *proto.Workspace,
	largeModel, smallModel string,
) error {
	cfg, err := c.GetConfig(ctx, ws.ID)
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	providers := cfg.Providers.Copy()

	largeMatches, smallMatches := findModelMatches(providers, largeModel, smallModel)

	var largeProviderID string

	if largeModel != "" {
		found, err := validateModelMatches(largeMatches, largeModel, "large")
		if err != nil {
			return err
		}
		largeProviderID = found.provider
		slog.Info("Overriding large model", "provider", found.provider, "model", found.modelID)
		if err := c.UpdatePreferredModel(ctx, ws.ID, config.ScopeWorkspace, config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}); err != nil {
			return fmt.Errorf("failed to set large model: %w", err)
		}
	}

	switch {
	case smallModel != "":
		found, err := validateModelMatches(smallMatches, smallModel, "small")
		if err != nil {
			return err
		}
		slog.Info("Overriding small model", "provider", found.provider, "model", found.modelID)
		if err := c.UpdatePreferredModel(ctx, ws.ID, config.ScopeWorkspace, config.SelectedModelTypeSmall, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}); err != nil {
			return fmt.Errorf("failed to set small model: %w", err)
		}

	case largeModel != "":
		sm, err := c.GetDefaultSmallModel(ctx, ws.ID, largeProviderID)
		if err != nil {
			slog.Warn("Failed to get default small model", "error", err)
		} else if sm != nil {
			if err := c.UpdatePreferredModel(ctx, ws.ID, config.ScopeWorkspace, config.SelectedModelTypeSmall, *sm); err != nil {
				return fmt.Errorf("failed to set small model: %w", err)
			}
		}
	}

	return c.UpdateAgent(ctx, ws.ID)
}

// restoreModelFromSession reads the last assistant message in the
// session and, if it used a different provider/model than the current
// config, updates the preferred model on the server provided the
// provider/model is still available. This ensures that continuing a
// session uses the same model that produced the last response.
func restoreModelFromSession(ctx context.Context, c *client.Client, ws *proto.Workspace, sessionID string) error {
	msgs, err := c.ListMessages(ctx, ws.ID, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	var lastAssistant *proto.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == proto.Assistant && !msgs[i].IsSummaryMessage {
			lastAssistant = &msgs[i]
			break
		}
	}
	if lastAssistant == nil || lastAssistant.Provider == "" || lastAssistant.Model == "" {
		return nil
	}

	cfg := ws.Config
	currentLarge := cfg.Models[config.SelectedModelTypeLarge]
	if currentLarge.Provider == lastAssistant.Provider && currentLarge.Model == lastAssistant.Model {
		return nil
	}

	if !cfg.IsModelAvailable(lastAssistant.Provider, lastAssistant.Model) {
		slog.Debug("Skipping model restoration: provider/model not available",
			"provider", lastAssistant.Provider,
			"model", lastAssistant.Model)
		return nil
	}

	selectedModel := config.SelectedModel{
		Provider: lastAssistant.Provider,
		Model:    lastAssistant.Model,
	}
	if err := c.UpdatePreferredModel(ctx, ws.ID, config.ScopeWorkspace, config.SelectedModelTypeLarge, selectedModel); err != nil {
		return fmt.Errorf("failed to set large model: %w", err)
	}

	if _, ok := cfg.Models[config.SelectedModelTypeSmall]; !ok {
		sm, err := c.GetDefaultSmallModel(ctx, ws.ID, lastAssistant.Provider)
		if err != nil {
			slog.Warn("Failed to get default small model", "error", err)
		} else if sm != nil {
			if err := c.UpdatePreferredModel(ctx, ws.ID, config.ScopeWorkspace, config.SelectedModelTypeSmall, *sm); err != nil {
				slog.Warn("Failed to set small model during session restore", "error", err)
			}
		}
	}

	return c.UpdateAgent(ctx, ws.ID)
}

// overrideReasoningEffort validates the requested reasoning effort against
// the large model in effect for this run (which may have been overridden by
// --model or restored from a continued session) and applies it on the
// server.
func overrideReasoningEffort(ctx context.Context, c *client.Client, wsID, reasoningEffort string) error {
	cfg, err := c.GetConfig(ctx, wsID)
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

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
	if err := c.UpdatePreferredModel(ctx, wsID, config.ScopeWorkspace, config.SelectedModelTypeLarge, selected); err != nil {
		return fmt.Errorf("failed to set reasoning effort: %w", err)
	}
	return c.UpdateAgent(ctx, wsID)
}

type modelMatch struct {
	provider string
	modelID  string
}

// findModelMatches searches providers for matching large/small model
// strings.
func findModelMatches(providers map[string]config.ProviderConfig, largeModel, smallModel string) ([]modelMatch, []modelMatch) {
	largeFilter, largeID := parseModelString(largeModel)
	smallFilter, smallID := parseModelString(smallModel)

	var largeMatches, smallMatches []modelMatch
	for name, provider := range providers {
		if provider.Disable {
			continue
		}
		for _, m := range provider.Models {
			if matchesModel(largeID, largeFilter, m.ID, name) {
				largeMatches = append(largeMatches, modelMatch{provider: name, modelID: m.ID})
			}
			if matchesModel(smallID, smallFilter, m.ID, name) {
				smallMatches = append(smallMatches, modelMatch{provider: name, modelID: m.ID})
			}
		}
	}
	return largeMatches, smallMatches
}

// parseModelString splits "provider/model" into (provider, model) or
// ("", model).
func parseModelString(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}

// matchesModel returns true if the model ID matches the filter
// criteria.
func matchesModel(wantID, wantProvider, modelID, providerName string) bool {
	if wantID == "" {
		return false
	}
	if wantProvider != "" && wantProvider != providerName {
		return false
	}
	return strings.EqualFold(modelID, wantID)
}

// validateModelMatches ensures exactly one match exists.
func validateModelMatches(matches []modelMatch, modelID, label string) (modelMatch, error) {
	switch {
	case len(matches) == 0:
		return modelMatch{}, fmt.Errorf("%s model %q not found", label, modelID)
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.provider
		}
		return modelMatch{}, fmt.Errorf(
			"%s model: model %q found in multiple providers: %s. Please specify provider using 'provider/model' format",
			label, modelID, strings.Join(names, ", "),
		)
	}
	return matches[0], nil
}

// resolveSession returns the session to use for a non-interactive run.
// If continueSessionID is set it fetches that session; if useLast is set it
// returns the most recently updated top-level session; otherwise it creates a
// new one.
func resolveSession(ctx context.Context, c *client.Client, wsID, continueSessionID string, useLast bool) (*proto.Session, error) {
	switch {
	case continueSessionID != "":
		sess, err := c.GetSession(ctx, wsID, continueSessionID)
		if err != nil {
			return nil, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return nil, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		sessions, err := c.ListSessions(ctx, wsID)
		if err != nil || len(sessions) == 0 {
			return nil, fmt.Errorf("no sessions found to continue")
		}
		last := sessions[0]
		for _, s := range sessions[1:] {
			if s.UpdatedAt > last.UpdatedAt && s.ParentSessionID == "" {
				last = s
			}
		}
		return &last, nil

	default:
		return c.CreateSession(ctx, wsID, session.RunTitle)
	}
}

// resolveSessionByID resolves a session ID that may be a full UUID or a hash
// prefix returned by crush session list.
func resolveSessionByID(ctx context.Context, c *client.Client, wsID, id string) (*proto.Session, error) {
	if sess, err := c.GetSession(ctx, wsID, id); err == nil {
		return sess, nil
	}

	sessions, err := c.ListSessions(ctx, wsID)
	if err != nil {
		return nil, err
	}

	var matches []proto.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("session %q not found", id)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("session ID %q is ambiguous (%d matches)", id, len(matches))
	}
}

// clientSessionID is the ID of the session --session or --resume names in
// a run through the server: by ID or hash ID, and with byName (--resume)
// also by name, as namedSession takes one.
func clientSessionID(ctx context.Context, c *client.Client, wsID, ref string, byName bool) (string, error) {
	sess, err := resolveSessionByID(ctx, c, wsID, ref)
	if err == nil {
		return sess.ID, nil
	}
	if !byName {
		return "", err
	}
	listed, listErr := c.ListSessions(ctx, wsID)
	if listErr != nil {
		return "", listErr
	}
	sessions := make([]session.Session, len(listed))
	for i, s := range listed {
		sessions[i] = session.Session{ID: s.ID, Title: s.Title, UpdatedAt: s.UpdatedAt}
	}
	named, err := namedSession(sessions, ref, err)
	return named.ID, err
}

// localSessionID is clientSessionID for a run in this process.
func localSessionID(ctx context.Context, svc session.Service, ref string, byName bool) (string, error) {
	sess, err := resolveSessionID(ctx, svc, ref)
	if err == nil {
		return sess.ID, nil
	}
	if !byName {
		return "", err
	}
	sessions, listErr := svc.List(ctx)
	if listErr != nil {
		return "", listErr
	}
	named, err := namedSession(sessions, ref, err)
	return named.ID, err
}

// namedSession is the session `gnosh run -r <name>` names when name is no
// session's ID (idErr says why), as Claude Code's print mode takes a
// title: the one session with that name, ignoring case. Several are an
// error listing their IDs, as Claude Code's is; none is idErr.
func namedSession(sessions []session.Session, name string, idErr error) (session.Session, error) {
	named := session.Named(sessions, name)
	switch len(named) {
	case 0:
		return session.Session{}, idErr
	case 1:
		return named[0], nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--resume %q matches %d sessions. Pass one of these session IDs to disambiguate:", strings.TrimSpace(name), len(named))
	for _, s := range named {
		fmt.Fprintf(&sb, "\n  %s  (updated %s)", s.ID, time.Unix(s.UpdatedAt, 0).UTC().Format(time.RFC3339))
	}
	return session.Session{}, errors.New(sb.String())
}

// setThinking moves the current model to one end of its own reasoning axis,
// as a /think or /no_think line asks, and says so instead of sending the
// line to the model. /model and the arrows reach the levels in between.
func setThinking(ws workspace.Workspace, enabled bool, out io.Writer) error {
	cfg := ws.Config()
	selected := cfg.Models[config.SelectedModelTypeLarge]
	model := cfg.GetModel(selected.Provider, selected.Model)
	if model == nil {
		return fmt.Errorf("no large model is selected")
	}
	kind, levels := config.ReasoningAxis(*model)
	if kind == config.ReasoningNone {
		_, err := fmt.Fprintf(out, "%s has no thinking switch.\n", cmp.Or(model.Name, model.ID))
		return err
	}
	level := config.ThinkingLevel(enabled, kind, levels)
	if err := ws.SetConfigField(config.ScopeGlobal, config.ReasoningField(selected.Provider, selected.Model), level); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "%s set to %q.\n", cmp.Or(model.Name, model.ID), level)
	return err
}

// runOutput is where gnosh run writes its answer, by --output-format:
// streamed as it comes for text, or collected and printed as one result
// object at the end for json, as Claude Code's -p --output-format json
// does, for scripts that read the answer and the session to resume;
// stream-json adds a line per message before it (see streamJSON).
type runOutput struct {
	json      bool          // json or stream-json: a result object at the end
	stream    *streamJSON   // stream-json: a line per message as it comes
	w         io.Writer     // what the run writes the answer to
	answer    *bytes.Buffer // json: the answer, collected
	stdout    io.Writer     // json: where the result object goes
	start     time.Time
	sessionID string // set once the run knows its session
	// final is the text of the run's last assistant message with any, the
	// json result as Claude Code gives it: the answer, not the narration
	// of each step before it ("Let me look.").
	final      string
	finalID    string         // the message final came from
	assistants map[string]int // each assistant message's place in the run
	// init says what stream-json's init line says, asked for when the line
	// is written; nil once it is, or before the session is known.
	init func() streamInit
}

func newRunOutput(format string, stdout io.Writer) (*runOutput, error) {
	switch format {
	case "", "text":
		return &runOutput{w: stdout, stdout: stdout, start: time.Now()}, nil
	case "json":
		answer := &bytes.Buffer{}
		return &runOutput{json: true, w: answer, answer: answer, stdout: stdout, start: time.Now()}, nil
	case "stream-json":
		answer := &bytes.Buffer{}
		return &runOutput{json: true, stream: newStreamJSON(stdout), w: answer, answer: answer, stdout: stdout, start: time.Now()}, nil
	default:
		return nil, fmt.Errorf("--output-format %q: expected text, json or stream-json", format)
	}
}

// started records the run's session, once it is known. stream-json's
// init line waits for the run's first message, or its end: a resumed
// session's model is restored after its session is known, and the line
// must name the model the run uses.
func (o *runOutput) started(sessionID string, init func() streamInit) {
	o.sessionID = sessionID
	o.init = init
}

// writeInit writes stream-json's init line, once the session is known.
func (o *runOutput) writeInit() {
	if o.stream == nil || o.init == nil {
		return
	}
	o.stream.start(o.init())
	o.init = nil
}

// message takes each update of the run's own messages.
func (o *runOutput) message(msg message.Message) {
	o.writeInit()
	if o.stream != nil {
		o.stream.message(msg)
	}
	// Only the run's own messages: not a sub-agent's, nor a summary.
	if msg.Role != message.Assistant || msg.IsSummaryMessage || (o.sessionID != "" && msg.SessionID != o.sessionID) {
		return
	}
	if o.assistants == nil {
		o.assistants = map[string]int{}
	}
	if _, ok := o.assistants[msg.ID]; !ok {
		o.assistants[msg.ID] = len(o.assistants)
	}
	// A late update of an earlier message does not take the answer back.
	text := strings.TrimSpace(msg.Content().Text)
	if text != "" && (o.finalID == "" || o.assistants[msg.ID] >= o.assistants[o.finalID]) {
		o.final, o.finalID = text, msg.ID
	}
}

// finish ends the run's output and passes runErr on. In json, it prints
// the result object, a failed run included, so a script always gets one.
func (o *runOutput) finish(runErr error) error {
	if !o.json {
		return runErr
	}
	o.writeInit()
	if o.stream != nil {
		o.stream.flush()
	}
	res := struct {
		Type       string `json:"type"`
		Subtype    string `json:"subtype"`
		IsError    bool   `json:"is_error"`
		Result     string `json:"result"`
		SessionID  string `json:"session_id,omitempty"`
		DurationMS int64  `json:"duration_ms"`
		NumTurns   int    `json:"num_turns"`
	}{
		Type:       "result",
		Subtype:    "success",
		Result:     cmp.Or(o.final, strings.TrimSpace(o.answer.String())),
		SessionID:  o.sessionID,
		DurationMS: time.Since(o.start).Milliseconds(),
		// The run's own assistant messages, as Claude Code's json and
		// stream-json results count them; json left it out.
		NumTurns: len(o.assistants),
	}
	if runErr != nil {
		res.Subtype = "error_during_execution"
		res.IsError = true
		res.Result = runErr.Error()
	}
	if o.stream != nil {
		if o.stream.err != nil {
			return errors.Join(runErr, o.stream.err)
		}
	}
	if err := json.NewEncoder(o.stdout).Encode(res); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

// noProviderError says why gnosh run has no provider to use. When the
// config names providers that loading had to leave out, such as one whose
// model server did not answer, it says which and why; "no providers
// configured" sent a user whose server was down to set one up again.
func noProviderError(cfg *config.Config) error {
	if why := cfg.SkippedProvidersSummary(); why != "" {
		return fmt.Errorf("no configured provider can be used: %s", why)
	}
	return fmt.Errorf("no providers configured - please run '%s' to set up a provider interactively", version.Command)
}
