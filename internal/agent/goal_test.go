package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/gnovm"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// judgeModel is a goal check that answers the verdicts given, one per
// check and the last over again, and counts the checks; during, when
// set, runs as each check is made.
type judgeModel struct {
	fakeLanguageModel
	verdicts []string
	checks   atomic.Int64
	during   func()
}

func (m *judgeModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	n := int(m.checks.Add(1))
	if m.during != nil {
		m.during()
	}
	answer := m.verdicts[min(n, len(m.verdicts))-1]
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: answer})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

// goalRun is a session with a goal bound in an in-process mem, and an
// agent to run its turns.
type goalRun struct {
	stopHookRun
	goals *goalBindings
	t     *testing.T
}

// newGoalRun makes a session whose goal is condition, bound in mem (none
// for ""), and an agent whose model is main and whose goal check is
// judge. Run with run.
func newGoalRun(t *testing.T, condition string, main fantasy.LanguageModel, turns *atomic.Int64, judge fantasy.LanguageModel, opts ...func(*SessionAgentOptions)) goalRun {
	t.Helper()
	env := testEnv(t)
	notifier := &recordingNotifier{}
	vms := gnovm.NewLazy(gnovm.ClientOptions{Options: gnovm.Options{Dir: t.TempDir()}, InProcess: true})
	t.Cleanup(vms.Close)
	goals := &goalBindings{sessions: env.sessions, gnovms: vms, project: env.workingDir}
	model := func(llm fantasy.LanguageModel) Model {
		return Model{Model: llm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
	}
	o := SessionAgentOptions{
		LargeModel:           model(main),
		SmallModel:           model(judge),
		SystemPrompt:         "test",
		IsYolo:               true,
		DisableAutoSummarize: true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Cfg:                  config.NewTestStore(&config.Config{}),
		Notify:               notifier,
		Goals:                goals,
	}
	for _, opt := range opts {
		opt(&o)
	}
	a := NewSessionAgent(o).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "goal turn")
	require.NoError(t, err)
	// A prompt before, so Run makes no title: that would ask the judge.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier prompt"}},
	})
	require.NoError(t, err)
	if condition != "" {
		change, err := goals.set(t.Context(), sess.ID, condition)
		require.NoError(t, err)
		require.True(t, change.Had)
		require.Contains(t, change.Directive, "Goal `"+change.Goal.Name+"`")
	}
	return goalRun{stopHookRun{agent: a, sessionID: sess.ID, notifier: notifier, turns: turns}, goals, t}
}

// run runs a turn, prompted with prompt.
func (r goalRun) run(prompt string) {
	r.t.Helper()
	_, err := r.agent.Run(r.t.Context(), SessionAgentCall{SessionID: r.sessionID, RunID: "run-1", Prompt: prompt})
	require.NoError(r.t, err)
}

// mem runs code in mem as the session's agent does.
func (r goalRun) mem(code string) string {
	r.t.Helper()
	client, err := r.goals.client()
	require.NoError(r.t, err)
	res, err := client.Run(context.Background(), gnovm.RunRequest{Project: r.goals.project, Session: r.sessionID, Name: gnovm.MemVM, Code: code, Goal: r.goals.boundName(context.Background(), r.sessionID)})
	require.NoError(r.t, err, code)
	require.NoError(r.t, res.Err, code)
	return res.Output
}

// memUnbound runs code in mem with no /goal bound, as the session was
// before /goal: a todo it adds at the root is top-level.
func (r goalRun) memUnbound(code string) string {
	r.t.Helper()
	client, err := r.goals.client()
	require.NoError(r.t, err)
	res, err := client.Run(context.Background(), gnovm.RunRequest{Project: r.goals.project, Session: r.sessionID, Name: gnovm.MemVM, Code: code})
	require.NoError(r.t, err, code)
	require.NoError(r.t, res.Err, code)
	return res.Output
}

// goal is the session's goal, and whether it is set.
func (r goalRun) goal() (Goal, bool) {
	return r.goals.get(context.Background(), r.sessionID)
}

// notices are the notifications of type kind the run sent.
func (r goalRun) notices(kind notify.Type) []string {
	r.notifier.mu.Lock()
	defer r.notifier.mu.Unlock()
	var out []string
	for _, n := range r.notifier.sent {
		if n.Type == kind {
			out = append(out, n.Message)
		}
	}
	return out
}

// hiddenPrompts are the hidden prompts the session was sent: the goal's
// continuations.
func (r goalRun) hiddenPrompts() []string {
	msgs, err := r.agent.messages.List(r.t.Context(), r.sessionID)
	require.NoError(r.t, err)
	var out []string
	for _, msg := range msgs {
		if msg.Role == message.User && msg.Content().Hidden {
			out = append(out, msg.Content().Text)
		}
	}
	return out
}

// The feature, end to end: mem drives the turn, with the goal's next step
// as a hidden prompt; once the agent closes the goal, the check asks
// whether it holds; not met, the goal is reopened and the agent goes back
// to it; met, it is cleared, and the user told.
func TestGoalDrivesTheTurnFromMem(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		switch turns.Load() {
		case 1:
			r.mem(`mem.Todo("fix the parser")`)
			return "Planned."
		case 2:
			r.mem(`mem.Done("tests-pass/fix-parser", "fixed"); mem.Done("tests-pass", "go test ./... → ok")`)
			return "Done."
		default:
			r.mem(`mem.Done("tests-pass", "go test ./... → ok, all of it")`)
			return "Done now."
		}
	}}
	judge := &judgeModel{verdicts: []string{
		`{"ok": false, "reason": "the tests still fail"}`,
		`<think>they ran</think> Verdict: {"ok": true, "reason": "the test run passes"}`,
	}}
	r = newGoalRun(t, "the tests pass", main, &turns, judge)
	r.run("make the tests pass")

	require.Equal(t, int64(3), turns.Load(), "a turn for the next step, one after the check found it not met")
	require.Equal(t, int64(2), judge.checks.Load(), "a check each time the agent closed the goal")
	requireQueueEmpty(t, r.stopHookRun)
	_, set := r.goal()
	require.False(t, set, "a goal met is cleared")
	require.Equal(t, []string{"Goal met: the tests pass (the test run passes)"}, r.notices(notify.TypeGoalMet))
	require.Equal(t, []string{"The check found the goal not met: the tests still fail. It is reopened, and the agent goes back to it"}, r.notices(notify.TypeWarning))
	prompts := r.hiddenPrompts()
	require.Len(t, prompts, 2)
	require.Contains(t, prompts[0], "Its next todo is `tests-pass/fix-parser` \"fix the parser\"")
	require.Contains(t, prompts[1], "The check found the goal `tests-pass` not met: the tests still fail")
	require.Contains(t, r.mem(`mem.Show("tests-pass")`), "the check found it not met: the tests still fail")
}

// Every goal is judged before it counts as done, not only /goal's: a goal
// the agent set itself and closed is checked as the turn ends; not met, it
// is reopened and the agent sent back to it; met, the user is told.
func TestEveryGoalClosedIsJudged(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		switch turns.Load() {
		case 1:
			r.mem(`mem.Todo("fix the login loop").Todo("set SameSite on the cookie")`)
			r.mem(`mem.Done("fix-login-loop/set-samesite-cookie", "SameSite=Lax"); mem.Done("fix-login-loop", "fixed in auth.go")`)
			return "Fixed."
		case 2:
			r.mem(`mem.Done("fix-login-loop", "go test ./auth → ok")`)
			return "Fixed, and the test passes."
		default:
			return "You're welcome."
		}
	}}
	judge := &judgeModel{verdicts: []string{
		`{"ok": false, "reason": "no test was run"}`,
		`{"ok": true, "reason": "the auth tests pass"}`,
	}}
	r = newGoalRun(t, "", main, &turns, judge)
	r.run("users get bounced back to the login page")

	require.Equal(t, int64(2), turns.Load(), "the turn, then one back to the goal the check found not met")
	require.Equal(t, int64(2), judge.checks.Load(), "a check each time the agent closed the goal, none for its step")
	requireQueueEmpty(t, r.stopHookRun)
	prompts := r.hiddenPrompts()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "The check found the goal `fix-login-loop` not met: no test was run")
	require.Contains(t, prompts[0], "The goal: fix the login loop")
	require.Contains(t, r.mem(`mem.Show("fix-login-loop")`), "the check found it not met: no test was run")
	require.Equal(t, []string{"Goal met: fix the login loop (the auth tests pass)"}, r.notices(notify.TypeGoalMet))
	require.Equal(t, []string{"The check found the goal `fix-login-loop` not met: no test was run. It is reopened, and the agent goes back to it"}, r.notices(notify.TypeWarning))
	require.Contains(t, prompts[0], "ask them with `mem.Ask(\"fix-login-loop\", \"…\")` instead. Either way, tell the user what the check found.")

	// The next turn closes nothing: the goal closed before it is not
	// judged again.
	r.run("thanks")
	require.Equal(t, int64(2), judge.checks.Load())
}

// A goal the check keeps finding not met: every stopHookReflectEvery
// continuations the agent is also asked whether there is a real reason to
// stop, with no cap; when it says so, the turn ends, the goal open in mem.
func TestAJudgedGoalReflectsEveryTenContinuations(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(last string) string {
		if strings.Contains(last, "Step back") {
			return "It cannot pass: the staging server is down.\n" + StopMarker
		}
		if turns.Load() == 1 {
			r.mem(`mem.Todo("deploy to staging")`)
		}
		r.mem(`mem.Done("deploy-staging", "deployed")`)
		return "Deployed."
	}}
	judge := &judgeModel{verdicts: []string{`{"ok": false, "reason": "the deploy failed"}`}}
	r = newGoalRun(t, "", main, &turns, judge)
	r.run("deploy to staging")

	require.Equal(t, int64(stopHookReflectEvery+1), turns.Load(), "the turn, then one each time the check found the goal not met")
	require.Equal(t, int64(stopHookReflectEvery), judge.checks.Load())
	warnings := r.notices(notify.TypeWarning)
	require.Len(t, warnings, stopHookReflectEvery+1, "a notice each time the check reopened it, then why the agent stopped")
	require.Equal(t, "The check found the goal `deploy-staging` not met: the deploy failed. It is reopened, and the agent goes back to it", warnings[0])
	require.Equal(t, "The agent judged it should stop working on the goal `deploy-staging`: It cannot pass: the staging server is down. It stays open in mem", warnings[stopHookReflectEvery])
	var j struct {
		Goals []struct {
			Status string `json:"status"`
		} `json:"goals"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(r.mem(`print(mem.JSON("deploy-staging"))`))), &j))
	require.Len(t, j.Goals, 1)
	require.Empty(t, j.Goals[0].Status, "reopened, and left open")
}

// A check that cannot be made leaves the goal closed, and says why.
func TestAJudgedGoalWithNoVerdictStaysClosed(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		r.mem(`mem.Todo("tidy the README"); mem.Done("tidy-readme", "tidied")`)
		return "Tidied."
	}}
	r = newGoalRun(t, "", main, &turns, &judgeModel{verdicts: []string{"maybe"}})
	r.run("tidy the README")

	require.Equal(t, int64(1), turns.Load())
	require.Len(t, r.notifier.warnings(), 1)
	require.Equal(t, "The goal `tidy-readme` could not be checked, so it stays closed unchecked: its answer was not a verdict: maybe", r.notifier.warnings()[0].Message)
}

// With a /goal set, a goal of the agent's own closed in the same turn is
// judged too, and the bound goal only once, against the user's condition.
// A bound session adds its todos under its goal, so the agent's own
// top-level one is from before the /goal was set.
func TestAJudgedGoalBesideTheBoundOne(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		r.mem(`mem.Done("update-changelog", "CHANGELOG.md has 2.0")`)
		r.mem(`mem.Done("tests-pass", "go test ./... → ok")`)
		return "Done."
	}}
	judge := &judgeModel{verdicts: []string{`{"ok": true, "reason": "it shows so"}`}}
	r = newGoalRun(t, "the tests pass", main, &turns, judge)
	r.memUnbound(`mem.Todo("update the changelog")`)
	r.run("make the tests pass, and update the changelog")

	require.Equal(t, int64(1), turns.Load())
	require.Equal(t, int64(2), judge.checks.Load(), "the agent's goal, then the bound one")
	require.Equal(t, []string{"Goal met: update the changelog (it shows so)", "Goal met: the tests pass (it shows so)"}, r.notices(notify.TypeGoalMet))
	_, set := r.goal()
	require.False(t, set)
}

// fullContextModel answers each call in turn with replies, the first one
// reporting a context nearly full, and runs before, if set, ahead of each.
type fullContextModel struct {
	fakeLanguageModel
	mu      sync.Mutex
	calls   int
	replies []string
	before  func(call int)
}

func (m *fullContextModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	reply := m.replies[min(n, len(m.replies))-1]
	m.mu.Unlock()
	if m.before != nil {
		m.before(n)
	}
	usage := fantasy.Usage{InputTokens: 1_000, OutputTokens: 10}
	if n == 1 {
		usage.InputTokens = 190_000
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: reply})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: usage})
	}, nil
}

// A turn that fills the context and ends in an automatic summary goes on
// toward its goal after the summary.
func TestGoalGoesOnAfterAnAutomaticSummary(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &fullContextModel{replies: []string{"Working.", "the summary", "Closed."}}
	main.before = func(call int) {
		if call == 3 {
			r.mem(`mem.Done("tests-pass", "go test → ok")`)
		}
	}
	judge := &judgeModel{verdicts: []string{`{"ok": true, "reason": "the test run passes"}`}}
	r = newGoalRun(t, "the tests pass", main, &turns, judge, func(o *SessionAgentOptions) { o.DisableAutoSummarize = false })
	r.run("make the tests pass")

	require.Equal(t, 3, main.calls, "the turn, its summary, and the goal's next turn")
	sess, err := r.agent.sessions.Get(t.Context(), r.sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, sess.SummaryMessageID, "summarized")
	require.Equal(t, []string{"Goal met: the tests pass (the test run passes)"}, r.notices(notify.TypeGoalMet))
}

// A goal that waits on the user ends the turn, still set, and says why.
func TestGoalWaitingOnTheUserEndsTheTurn(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		r.mem(`mem.Ask("tests-pass", "which test runner do you use?")`)
		return "Asked."
	}}
	r = newGoalRun(t, "the tests pass", main, &turns, &judgeModel{verdicts: []string{`{"ok": true}`}})
	r.run("make the tests pass")

	require.Equal(t, int64(1), turns.Load())
	_, set := r.goal()
	require.True(t, set)
	require.Len(t, r.notifier.warnings(), 1)
	require.Contains(t, r.notifier.warnings()[0].Message, "`tests-pass/which-test-runner-do` \"which test runner do you use?\" is for the user. It stays set")
}

// No cap: every stopHookReflectEvery continuations in a row the agent is
// asked whether there is a real reason to stop; when it judges there is,
// the turn ends there, the goal still set, and the user is told why.
func TestGoalReflectsEveryTenContinuations(t *testing.T) {
	var turns atomic.Int64
	main := &promptAwareModel{turns: &turns, answer: func(last string) string {
		if strings.Contains(last, "Step back") {
			return "I cannot: the test server is down.\n" + StopMarker
		}
		return "Working."
	}}
	judge := &judgeModel{verdicts: []string{`{"ok": true}`}}
	r := newGoalRun(t, "the tests pass", main, &turns, judge)
	r.run("make the tests pass")

	require.Equal(t, int64(stopHookReflectEvery+1), turns.Load(), "the turn, a continuation each, the last a checkpoint")
	require.Zero(t, judge.checks.Load(), "never closed, never checked")
	_, set := r.goal()
	require.True(t, set, "the goal stays set")
	require.Len(t, r.notifier.warnings(), 1)
	require.Equal(t, "The agent judged it should stop working toward the goal: I cannot: the test server is down. The goal stays set (/goal clear clears it)", r.notifier.warnings()[0].Message)
}

// A check that answers no verdict ends the turn, the goal still set, and
// says why.
func TestGoalCheckWithNoVerdictEndsTheTurn(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		r.mem(`mem.Done("tests-pass", "go test → ok")`)
		return "Done."
	}}
	r = newGoalRun(t, "the tests pass", main, &turns, &judgeModel{verdicts: []string{"maybe"}})
	r.run("make the tests pass")

	require.Equal(t, int64(1), turns.Load())
	goal, set := r.goal()
	require.True(t, set)
	require.Zero(t, goal.Checks)
	require.Len(t, r.notifier.warnings(), 1)
	require.Equal(t, "The goal check could not be made, so the turn ended with the goal still set: its answer was not a verdict: maybe", r.notifier.warnings()[0].Message)
}

// A goal set anew while the old one was checked is left as set: the
// check's verdict was about the old one.
func TestGoalSetAnewWhileCheckedIsLeft(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		r.mem(`mem.Done("tests-pass", "go test → ok")`)
		return "Done."
	}}
	judge := &judgeModel{verdicts: []string{`{"ok": false, "reason": "the tests still fail"}`}}
	judge.during = func() {
		_, err := r.goals.set(context.Background(), r.sessionID, "the docs build")
		require.NoError(t, err)
	}
	r = newGoalRun(t, "the tests pass", main, &turns, judge)
	r.run("make the tests pass")

	require.Equal(t, int64(1), turns.Load())
	goal, set := r.goal()
	require.True(t, set)
	require.Equal(t, "the docs build", goal.Condition)
	require.Equal(t, "docs-build", goal.Name)
}

// The stop marker counts only at a checkpoint: one the agent writes
// elsewhere does not end a turn its goal goes on with.
func TestGoalIgnoresAStopMarkerOutsideACheckpoint(t *testing.T) {
	var turns atomic.Int64
	var r goalRun
	main := &promptAwareModel{turns: &turns, answer: func(string) string {
		if turns.Load() == 2 {
			r.mem(`mem.Done("tests-pass", "go test → ok")`)
		}
		return "Done, I think.\n" + StopMarker
	}}
	r = newGoalRun(t, "the tests pass", main, &turns, &judgeModel{verdicts: []string{`{"ok": true, "reason": "the test run passes"}`}})
	r.run("make the tests pass")

	require.Equal(t, int64(2), turns.Load())
	require.Equal(t, []string{"Goal met: the tests pass (the test run passes)"}, r.notices(notify.TypeGoalMet))
	require.Empty(t, r.notifier.warnings())
}

// The verdict is the JSON object in the check's answer, past any thinking
// and around any words; an answer without one, or without ok, is none.
func TestParseGoalCheck(t *testing.T) {
	t.Parallel()

	for answer, want := range map[string]bool{
		`{"ok": true, "reason": "passes"}`:                                              true,
		`{"ok":false,"reason":"passes"}`:                                                false,
		"<think>is it {\"ok\": false}?</think>\n{\"ok\": true, \"reason\": \"passes\"}": true,
		"Verdict:\n```json\n{\"ok\": true, \"reason\": \"passes\"}\n```":                true,
	} {
		met, reason, err := parseGoalCheck(answer)
		require.NoError(t, err, answer)
		require.Equal(t, want, met, answer)
		require.Equal(t, "passes", reason, answer)
	}
	for _, answer := range []string{"", "maybe", `{"reason": "no ok"}`, `{"ok": "yes"}`, `}{`} {
		_, _, err := parseGoalCheck(answer)
		require.Error(t, err, answer)
	}

	// A goal met on the user's word says so in gnosh's words, once.
	for answer, want := range map[string]string{
		`{"ok": true, "basis": "user", "reason": "Lin signed off on 1.3.0 this morning"}`:                       "on the user's word: Lin signed off on 1.3.0 this morning",
		`{"ok": true, "basis": "user", "reason": "On the user's word: they did it on their laptop"}`:            "on the user's word: they did it on their laptop",
		`{"ok": true, "basis": "conversation", "reason": "go test ./... passes"}`:                               "go test ./... passes",
		`{"ok": false, "basis": "user", "reason": "the user said one step is done; the tag is not pushed yet"}`: "the user said one step is done; the tag is not pushed yet",
	} {
		_, reason, err := parseGoalCheck(answer)
		require.NoError(t, err, answer)
		require.Equal(t, want, reason, answer)
	}
}

// The check is shown the end of the conversation: the prompts typed (not
// the hidden ones), what the agent said and called, and what its tools
// returned, each cut short, and the whole cut to its end.
func TestGoalTranscript(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "make the tests pass"}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "A goal is now set", Hidden: true}}},
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "Running them."},
			message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test ./..."}`},
		}},
		{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "c1", Name: "bash", Content: "ok  \tpkg\t0.1s"}}},
	}
	require.Equal(t, "User: make the tests pass\n\nAgent: Running them.\n\nAgent called bash: {\"command\":\"go test ./...\"}\n\nbash returned: ok  \tpkg\t0.1s", goalTranscript(msgs))

	long := []message.Message{{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{Name: "view", Content: strings.Repeat("x", 3_000)}}}}
	require.Equal(t, "view returned: "+strings.Repeat("x", 2_000)+" … (1000 more characters)", goalTranscript(long))
	var many []message.Message
	for range 20 {
		many = append(many, message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("y", 3_000)}}})
	}
	got := goalTranscript(many)
	require.Equal(t, goalTranscriptLimit+1, len([]rune(got)), "cut to its end")
	require.True(t, strings.HasPrefix(got, "…"))
	require.True(t, strings.HasSuffix(got, strings.Repeat("y", 100)))
}

// The coordinator keeps the sessions' goals in their rows, bound in mem:
// set, read, set anew in place of the one before, and cleared, which
// drops the goal binding added.
func TestCoordinatorGoals(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	vms := gnovm.NewLazy(gnovm.ClientOptions{Options: gnovm.Options{Dir: t.TempDir()}, InProcess: true})
	t.Cleanup(vms.Close)
	c := &coordinator{goals: &goalBindings{sessions: env.sessions, gnovms: vms, project: env.workingDir}}
	sess, err := env.sessions.Create(t.Context(), "goals")
	require.NoError(t, err)
	ctx := t.Context()

	_, ok := c.Goal(ctx, sess.ID)
	require.False(t, ok)
	_, err = c.SetGoal(ctx, sess.ID, "the tests pass")
	require.NoError(t, err)
	change, err := c.SetGoal(ctx, sess.ID, "the docs build")
	require.NoError(t, err)
	require.Equal(t, Goal{Condition: "the docs build", Name: "docs-build", Title: "the docs build"}, change.Goal, "in place of the one before")
	goal, ok := c.Goal(ctx, sess.ID)
	require.True(t, ok)
	require.Equal(t, change.Goal, goal)
	stored, err := env.sessions.Goal(ctx, sess.ID)
	require.NoError(t, err)
	require.Contains(t, stored, `"name":"docs-build"`)

	cleared, err := c.ClearGoal(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, cleared.Had)
	require.Equal(t, "the docs build", cleared.Goal.Condition)
	require.Equal(t, "/goal is cleared, and the goal `docs-build` it added is dropped", cleared.Notice)
	_, ok = c.Goal(ctx, sess.ID)
	require.False(t, ok)
	none, err := c.ClearGoal(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, none.Had)
}

// Every agent the coordinator builds reads the goals the coordinator
// keeps: the coder's and the plan agent's share them.
func TestCoordinatorAgentsShareItsGoals(t *testing.T) {
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

	for _, name := range []string{config.AgentCoder, config.AgentPlan} {
		require.Same(t, coord.goals, coord.agents[name].(*sessionAgent).goals, name)
	}
}
