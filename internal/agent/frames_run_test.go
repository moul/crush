package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/gnovm"
	gnovmtools "github.com/charmbracelet/crush/internal/gnovm/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// The scenarios of FRAMES.md section 9 that are the agent's: the realm is
// stood in for by fakeMem, which answers each input with the text and the
// frames the realm would, and the model by a script of tool calls.

// framesModel streams one scripted step per request and keeps each
// request's messages, for what the model was sent after a pop.
type framesModel struct {
	fakeLanguageModel
	mu      sync.Mutex
	steps   [][]fantasy.StreamPart
	prompts []fantasy.Prompt
}

func (m *framesModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompts = append(m.prompts, slices.Clone(call.Prompt))
	parts := m.steps[min(len(m.prompts)-1, len(m.steps)-1)]
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

// sent is the messages of request i, as texts reads them.
func (m *framesModel) sent(i int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return texts(m.prompts[i])
}

func (m *framesModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// scriptedCall is a tool call the model makes.
type scriptedCall struct{ id, name, input string }

func memCall(id, code string) scriptedCall {
	input, _ := json.Marshal(map[string]string{"name": "mem", "code": code})
	return scriptedCall{id, gnovmtools.RunToolName, string(input)}
}

func viewCall(id, path string) scriptedCall {
	input, _ := json.Marshal(map[string]string{"file_path": path})
	return scriptedCall{id, tools.ViewToolName, string(input)}
}

func workCall(id string, n int) scriptedCall {
	input, _ := json.Marshal(map[string]any{"n": n, "tag": id})
	return scriptedCall{id, "work", string(input)}
}

// calling is a step in which the model makes calls, all at once.
func calling(calls ...scriptedCall) []fantasy.StreamPart {
	var parts []fantasy.StreamPart
	for _, c := range calls {
		parts = append(parts,
			fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: c.id, ToolCallName: c.name},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: c.id},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: c.id, ToolCallName: c.name, ToolCallInput: c.input},
		)
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
}

// memReply is what fakeMem answers an input with: the realm's text, the
// frames it pushed and popped, or a refusal.
type memReply struct {
	text    string
	events  []gnovm.MemFrame
	refused bool
}

func pushed(id, purpose string) gnovm.MemFrame {
	return gnovm.MemFrame{Kind: "push", Id: id, Purpose: purpose}
}

func poppedFrame(id, result string, thoughts int) gnovm.MemFrame {
	return gnovm.MemFrame{Kind: "pop", Id: id, Result: result, Thoughts: thoughts}
}

// fakeMem stands in for gnovm_run on mem: it answers each input from
// replies, tells the frames record what the realm would, and renders the
// result as the tool does, with the record's clauses and the metadata.
type fakeMem struct {
	frames  *frameStacks
	replies map[string]memReply

	mu sync.Mutex
	// stacks is the stack the tool was asked for before each call, in
	// order, and clauses what the record answered each pop, by call id.
	stacks           [][]string
	clauses          map[string]string
	estimateAfterPop int64
}

type fakeMemInput struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (m *fakeMem) tool() fantasy.AgentTool {
	return fantasy.NewAgentTool(gnovmtools.RunToolName, "mem", func(ctx context.Context, p fakeMemInput, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		session := tools.GetSessionFromContext(ctx)
		m.mu.Lock()
		m.stacks = append(m.stacks, m.frames.stack(ctx, session))
		m.mu.Unlock()
		reply, ok := m.replies[p.Code]
		if !ok {
			return fantasy.NewTextErrorResponse("ERROR — nothing from this call was kept: no reply scripted for " + p.Code), nil
		}
		if reply.refused {
			return fantasy.NewTextErrorResponse("ERROR — nothing from this call was kept: " + reply.text), nil
		}
		events := make([]gnovm.MemFrame, len(reply.events))
		for i, ev := range reply.events {
			ev.Session = session
			events[i] = ev
		}
		clauses := m.frames.events(ctx, session, call.ID, events)
		m.mu.Lock()
		if m.clauses == nil {
			m.clauses = map[string]string{}
		}
		m.clauses[call.ID] = clauses
		if slices.ContainsFunc(events, func(ev gnovm.MemFrame) bool { return ev.Kind == "pop" }) {
			m.estimateAfterPop = m.frames.estimate(session)
		}
		m.mu.Unlock()
		text := reply.text
		if clauses != "" {
			text += "\n" + clauses
		}
		text += "\nok · mem · ran 1ms · kept as entry 1"
		if len(events) > 0 {
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), gnovmtools.RunResponseMetadata{Frames: events}), nil
		}
		return fantasy.NewTextResponse(text), nil
	})
}

// viewTool is a view that records the read with the file tracker and
// answers the file's lines, as the view tool does for a small file.
func viewTool(env fakeEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(tools.ViewToolName, "view", func(ctx context.Context, p struct {
		FilePath string `json:"file_path"`
	}, call fantasy.ToolCall,
	) (fantasy.ToolResponse, error) {
		data, err := os.ReadFile(p.FilePath)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		env.filetracker.RecordRead(ctx, tools.GetSessionFromContext(ctx), p.FilePath)
		return fantasy.NewTextResponse("1: " + string(data)), nil
	})
}

// workTool is a tool whose result is about 150 n tokens; tag keeps its
// calls apart for the loop detector.
func workTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("work", "work", func(ctx context.Context, p struct {
		N   int    `json:"n"`
		Tag string `json:"tag"`
	}, call fantasy.ToolCall,
	) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse(strings.Repeat(fmt.Sprintf("line of the work %s\n", p.Tag), 30*max(p.N, 1))), nil
	})
}

// contents is msgs without their provider options, for comparing what two
// requests sent.
func contents(msgs []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, len(msgs))
	for i, m := range msgs {
		out[i] = fantasy.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

// framesScenario is an agent whose gnovm_run is fakeMem, with a frames
// record of its own, in a session of its own.
type framesScenario struct {
	t      *testing.T
	env    fakeEnv
	frames *frameStacks
	mem    *fakeMem
	model  *framesModel
	agent  SessionAgent
	sessID string
	// popped collects the pop notices the record publishes.
	notices <-chan pubsub.Event[notify.Notification]
}

func newFramesScenario(t *testing.T, replies map[string]memReply, steps [][]fantasy.StreamPart, contextWindow int64) *framesScenario {
	t.Helper()
	env := testEnv(t)
	frames := newFrameStacks(env.sessions, env.messages, env.filetracker, env.workingDir)
	broker := pubsub.NewBroker[notify.Notification]()
	frames.notify = broker
	notices := broker.Subscribe(t.Context())
	mem := &fakeMem{frames: frames, replies: replies}
	model := &framesModel{steps: steps}
	modelCfg := Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: contextWindow, DefaultMaxTokens: 1000}}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: modelCfg, SmallModel: modelCfg, SystemPrompt: "test",
		IsYolo: true, Sessions: env.sessions, Messages: env.messages, FileTracker: env.filetracker,
		Frames: frames,
		Tools:  []fantasy.AgentTool{mem.tool(), viewTool(env), workTool()},
	})
	sess, err := env.sessions.Create(t.Context(), "frames")
	require.NoError(t, err)
	return &framesScenario{t: t, env: env, frames: frames, mem: mem, model: model, agent: agent, sessID: sess.ID, notices: notices}
}

// run is one turn, as `gnosh run` sends one (no title is generated).
func (s *framesScenario) run(prompt string) {
	s.t.Helper()
	_, err := s.agent.Run(s.t.Context(), SessionAgentCall{SessionID: s.sessID, Prompt: prompt, NonInteractive: true})
	require.NoError(s.t, err)
}

// rows is every row of the session, the popped ones included.
func (s *framesScenario) rows() []message.Message {
	s.t.Helper()
	require.NoError(s.t, s.env.messages.FlushAll(s.t.Context()))
	msgs, err := s.env.messages.List(s.t.Context(), s.sessID)
	require.NoError(s.t, err)
	return msgs
}

// poppedBy is, by tool call id, the frame whose pop removed the row
// holding the call or its result, "" for a row still in the context.
func (s *framesScenario) poppedBy() map[string]string {
	by := map[string]string{}
	for _, row := range s.rows() {
		for _, call := range row.ToolCalls() {
			by["call:"+call.ID] = row.Popped
		}
		for _, result := range row.ToolResults() {
			by["result:"+result.ToolCallID] = row.Popped
		}
	}
	return by
}

// resultOf is the text of the result of call id.
func (s *framesScenario) resultOf(id string) string {
	for _, row := range s.rows() {
		for _, result := range row.ToolResults() {
			if result.ToolCallID == id {
				return result.Content
			}
		}
	}
	return ""
}

// userRows is the session's user rows, prompts and hidden ones alike,
// each as its whole text.
func (s *framesScenario) userRows() []string {
	var out []string
	for _, row := range s.rows() {
		if row.Role != message.User {
			continue
		}
		var text []string
		for _, part := range row.Parts {
			if tc, ok := part.(message.TextContent); ok {
				text = append(text, tc.Text)
			}
		}
		out = append(out, strings.Join(text, "\n"))
	}
	return out
}

func (s *framesScenario) notice() (notify.Notification, bool) {
	select {
	case ev := <-s.notices:
		return ev.Payload, true
	case <-time.After(2 * time.Second):
		return notify.Notification{}, false
	}
}

const (
	pushG      = `mem.Frame("g").Push()`
	pushS1     = `mem.Frame("g/s1").Push()`
	popS1PushS = "mem.Pop(\"found it\")\nmem.Next()"
	popS1      = `mem.Pop("found it")`
	popS2      = `mem.Pop("20 of 20 passed")`
	popG       = `mem.Pop("fixed")`
	inG        = "in `g` \"fix it\": when it is done, pop it with what came of it and what the next todo needs."
	inS1       = "in `g/s1` \"find the cause\": when it is done, pop it with what came of it and what the next todo needs."
	inS2       = "in `g/s2` \"rerun\": when it is done, pop it with what came of it and what the next todo needs."
	doneS1     = "done `g/s1` \"find the cause\" → found it"
)

// The trampoline: a pop removes the work between the push and the pop
// from what the model is next sent and marks its rows, the reads inside
// are forgotten, the result carries the clauses, the prefix through the
// push's round is unchanged, the estimate of what went is spent by the
// next usage report, the stack is derived the same from the rows, and the
// next prompt tells the model where it stands.
func TestFrames_PopCutsTheContext(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "listen_test.go")
	require.NoError(t, os.WriteFile(file, []byte("package net\n"), 0o644))
	replies := map[string]memReply{
		pushG:      {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		pushS1:     {text: inS1, events: []gnovm.MemFrame{pushed("g/s1", "find the cause")}},
		popS1PushS: {text: doneS1 + "\n" + inS2, events: []gnovm.MemFrame{poppedFrame("g/s1", "found it", 2), pushed("g/s2", "rerun")}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(memCall("c2", pushS1)),
		calling(viewCall("v1", file)),
		calling(memCall("c3", popS1PushS)),
		saying("carrying on"),
		saying("still here"),
	}, 200_000)
	var forgotten [][]string
	s.frames.forgetGiven = func(sessionID string, removed []string) { forgotten = append(forgotten, removed) }
	s.run("fix the flaky test")

	// What the model was sent after the pop: the push rounds and the pop
	// round, not the view between.
	require.Equal(t, 5, s.model.requestCount())
	before, after := s.model.sent(3), s.model.sent(4)
	require.Contains(t, before, "assistant:v1")
	require.NotContains(t, after, "assistant:v1")
	require.NotContains(t, after, "tool:v1")
	require.Contains(t, after, "assistant:c2")
	require.Contains(t, after, "tool:c2")
	require.Contains(t, after, "assistant:c3")
	require.Contains(t, after, "tool:c3")
	validSequence(t, s.model.prompts[4])
	// The prefix through the push's call is the same, and the push's answer
	// is only added to, at its end, with what came after it (cutPopped), so
	// the cache holds as far as before but for those few tokens.
	answer := slices.Index(before, "tool:c2")
	require.Equal(t, contents(s.model.prompts[3][:answer]), contents(s.model.prompts[4][:answer]), "the prefix through the push's call is unchanged")
	was, now := resultText(s.model.prompts[3][answer], "c2"), resultText(s.model.prompts[4][answer], "c2")
	require.Equal(t, was+"\n[Then came the work in it, 1 tool call (view 1), removed from your context when you popped it.]", now)

	// The rows are marked, and nothing else is.
	by := s.poppedBy()
	require.Equal(t, "g/s1", by["call:v1"])
	require.Equal(t, "g/s1", by["result:v1"])
	for _, id := range []string{"c1", "c2", "c3"} {
		require.Empty(t, by["call:"+id], id)
		require.Empty(t, by["result:"+id], id)
	}
	model, err := s.env.messages.ListFromSummary(t.Context(), s.sessID, "")
	require.NoError(t, err)
	require.False(t, slices.ContainsFunc(model, func(m message.Message) bool { return hasToolCall(m, "v1") }), "the model's list skips the popped rows")

	// The clauses, after the realm's own lines.
	result := s.resultOf("c3")
	require.Contains(t, result, doneS1)
	require.Contains(t, result, "Everything since you entered `g/s1` was removed from your context: this result stays, and its 2 thoughts are in mem (mem.Show(\"g/s1\") lists them when you need them); the user still sees all of it. Your reads inside it are forgotten: view a file before you edit it.")
	require.Less(t, strings.Index(result, inS2), strings.Index(result, "Everything since"), "the clauses come after the realm's output")
	require.Contains(t, result, "kept as entry 1")

	// The read inside the frame is forgotten, and so are the views given
	// in the removed rows (the view's round: its assistant and tool rows).
	require.True(t, s.env.filetracker.LastReadTime(t.Context(), s.sessID, file).IsZero())
	require.Len(t, forgotten, 1)
	require.Len(t, forgotten[0], 2)

	// The estimate was held after the pop and spent by the next report.
	require.Positive(t, s.mem.estimateAfterPop)
	require.Zero(t, s.frames.estimate(s.sessID))

	// The stack, as kept and as derived again from the rows.
	require.Equal(t, []string{"g", "g/s2"}, s.frames.stack(t.Context(), s.sessID))
	again := newFrameStacks(s.env.sessions, s.env.messages, s.env.filetracker, s.env.workingDir)
	require.Equal(t, []string{"g", "g/s2"}, again.stack(t.Context(), s.sessID), "a restart derives the same stack")
	require.Equal(t, []poppedRange{{frame: "g/s1", pushCallID: "c2", popCallID: "c3"}}, again.ranges(t.Context(), s.sessID))
	require.Equal(t, [][]string{nil, {"g"}, {"g", "g/s1"}}, s.mem.stacks, "the stack the tool was told before each call")

	// The user was told, as a non-interactive run prints it.
	n, ok := s.notice()
	require.True(t, ok)
	require.Equal(t, notify.TypeFramePopped, n.Type)
	require.True(t, strings.HasPrefix(n.Message, "popped `g/s1`: about "), n.Message)
	require.True(t, strings.HasSuffix(n.Message, " tokens out of the model's context"), n.Message)

	// The next prompt tells the model where it stands, with the stack.
	s.run("go on")
	prompts := s.userRows()
	last := prompts[len(prompts)-1]
	require.Contains(t, last, "go on")
	require.Contains(t, last, "You are in `g/s2` \"rerun\" (your stack: `g`, then `g/s2`): pop it with its result when it is done")
	require.NotContains(t, s.model.sent(5), "assistant:v1", "the next turn's history skips the popped rows")
}

// Pops nest: the outer pop takes the inner push and pop rounds with
// everything else, and leaves its own push and pop.
func TestFrames_NestedPop(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "a.go")
	require.NoError(t, os.WriteFile(file, []byte("package a\n"), 0o644))
	replies := map[string]memReply{
		pushG:  {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		pushS1: {text: inS1, events: []gnovm.MemFrame{pushed("g/s1", "find the cause")}},
		popS1:  {text: doneS1, events: []gnovm.MemFrame{poppedFrame("g/s1", "found it", 0)}},
		popG:   {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 1)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(memCall("c2", pushS1)),
		calling(viewCall("v1", file)),
		calling(memCall("c3", popS1)),
		calling(memCall("c4", popG)),
		saying("all done"),
	}, 200_000)
	s.run("fix it")

	final := s.model.sent(5)
	require.Equal(t, []string{"system:test", "user:fix it", "assistant:c1", "tool:c1", "assistant:c4", "tool:c4"}, final[:6])
	validSequence(t, s.model.prompts[5])
	by := s.poppedBy()
	require.Equal(t, "g", by["call:c2"])
	require.Equal(t, "g", by["call:v1"], "the outer pop marks what the inner one had")
	require.Equal(t, "g", by["call:c3"])
	require.Empty(t, by["call:c1"])
	require.Empty(t, by["call:c4"])
	require.Empty(t, s.frames.stack(t.Context(), s.sessID))
	require.Contains(t, s.resultOf("c3"), "Everything since you entered `g/s1` was removed from your context: this result stays; the user still sees all of it.")
	require.Contains(t, s.resultOf("c4"), "its 1 thought is in mem")
}

// A frame spans turns: what the user says inside it goes with the pop,
// and the prompt that answers inside it is told where it stands.
func TestFrames_WordsInsideAFrame(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		popG:  {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 0)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		saying("which port?"),
		calling(memCall("c2", popG)),
		saying("done"),
	}, 200_000)
	s.run("fix it")
	require.Equal(t, []string{"g"}, s.frames.stack(t.Context(), s.sessID), "the stack persists across turns")
	s.run("8080")

	prompts := s.userRows()
	require.Contains(t, prompts[1], "8080")
	require.Contains(t, prompts[1], "You are in `g` \"fix it\": pop it", "the answer is told where the model stands")
	final := s.model.sent(3)
	require.NotContains(t, strings.Join(final, "\n"), "8080", "the user's words inside the frame went with the pop")
	require.NotContains(t, strings.Join(final, "\n"), "which port?")
	require.Contains(t, final, "assistant:c2")
	validSequence(t, s.model.prompts[3])
	rows := s.rows()
	answer := slices.IndexFunc(rows, func(m message.Message) bool {
		return m.Role == message.User && strings.Contains(m.Content().Text, "8080")
	})
	require.Equal(t, "g", rows[answer].Popped)
}

// A call made beside the push, in the same round, keeps its result in
// the push's tool message, and its rows are not marked.
func TestFrames_ParallelSibling(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		popG:  {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 0)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG), workCall("w1", 1)),
		calling(workCall("w2", 1)),
		calling(memCall("c2", popG)),
		saying("done"),
	}, 200_000)
	s.run("fix it")

	final := s.model.prompts[3]
	push := indexOfCall(final, "c1")
	require.GreaterOrEqual(t, push, 0)
	require.Equal(t, fantasy.MessageRoleTool, final[push+1].Role)
	require.Len(t, final[push+1].Content, 2, "both results stay in the push's tool message")
	require.NotContains(t, texts(final), "assistant:w2")
	by := s.poppedBy()
	require.Empty(t, by["result:w1"])
	require.Equal(t, "g", by["call:w2"])
}

// A refused input records nothing: the stack the next call is told is the
// one before it.
func TestFrames_RefusedInput(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG:  {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		pushS1: {text: "`g/s1` is held by session \"other\"", refused: true},
		popG:   {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 0)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(memCall("c2", pushS1)),
		calling(memCall("c3", popG)),
		saying("done"),
	}, 200_000)
	s.run("fix it")
	require.Equal(t, [][]string{nil, {"g"}, {"g"}}, s.mem.stacks)
	require.Empty(t, s.frames.stack(t.Context(), s.sessID))
	require.Equal(t, "g", s.poppedBy()["call:c2"], "the refused round is between the push and the pop")
}

// A copy of the conversation (/branch) carries the marks and starts at
// the root; a rewind derives the stack again from what remains.
func TestFrames_BranchAndRewind(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG:  {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		pushS1: {text: inS1, events: []gnovm.MemFrame{pushed("g/s1", "find the cause")}},
		popS1:  {text: doneS1, events: []gnovm.MemFrame{poppedFrame("g/s1", "found it", 0)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(memCall("c2", pushS1)),
		calling(workCall("w1", 1)),
		calling(memCall("c3", popS1)),
		saying("done"),
	}, 200_000)
	s.run("fix it")
	require.Equal(t, []string{"g"}, s.frames.stack(t.Context(), s.sessID))

	branch, err := s.env.sessions.Create(t.Context(), "branch")
	require.NoError(t, err)
	for _, row := range s.rows() {
		_, err := s.env.messages.Copy(t.Context(), branch.ID, row)
		require.NoError(t, err)
	}
	require.Empty(t, s.frames.stack(t.Context(), branch.ID), "a copy's pushes are another session's")
	copied, err := s.env.messages.ListFromSummary(t.Context(), branch.ID, "")
	require.NoError(t, err)
	require.False(t, slices.ContainsFunc(copied, func(m message.Message) bool { return hasToolCall(m, "w1") }), "the copy carries the marks")

	// A rewind to before the push of g/s1: its rows go, and the stack is
	// derived again.
	rows := s.rows()
	from := slices.IndexFunc(rows, func(m message.Message) bool { return hasToolCall(m, "c2") })
	for _, row := range rows[from:] {
		require.NoError(t, s.env.messages.Delete(t.Context(), row.ID))
	}
	s.frames.forget(s.sessID)
	require.Equal(t, []string{"g"}, s.frames.stack(t.Context(), s.sessID))
	require.Empty(t, s.frames.ranges(t.Context(), s.sessID))
}

// Inside a frame whose work grows past half the compaction point, the
// model is told once that the step is large.
func TestFrames_LargeStep(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{calling(memCall("c1", pushG))}
	for i := 1; i <= 7; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 2)))
	}
	steps = append(steps, saying("done"))
	// A window of 4,000 compacts at 3,200: half is 1,600 tokens of work.
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")

	var large int
	for _, text := range s.userRows() {
		if strings.Contains(text, largeStep) {
			large++
			require.Contains(t, text, "you are in `g`")
		}
	}
	require.Equal(t, 1, large, "given once")
	final := s.model.prompts[len(s.model.prompts)-1]
	n := 0
	for _, m := range final {
		for _, part := range m.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.Contains(p.Text, largeStep) {
				n++
			}
		}
	}
	require.Equal(t, 1, n, "and kept in the turn's later steps")
}

// A pop right after its push removes nothing and marks nothing.
func TestFrames_NothingBetween(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		popG:  {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 0)}},
	}
	s := newFramesScenario(t, replies, [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(memCall("c2", popG)),
		saying("done"),
	}, 200_000)
	s.run("fix it")
	require.Contains(t, s.resultOf("c2"), "nothing came between, so nothing was removed from your context.")
	for key, frame := range s.poppedBy() {
		require.Empty(t, frame, key)
	}
	require.Zero(t, s.mem.estimateAfterPop)
	_, ok := s.notice()
	require.False(t, ok, "nothing to tell the user")
}

// The mem reminder names the frame the model is in.
func TestMemReminderNamesTheFrame(t *testing.T) {
	t.Parallel()
	text := memReminder(nil, frameReminder(stackFrame{id: "g/s1", purpose: "find the cause"}))
	require.Contains(t, text, "ignore if not applicable. You are in `g/s1` \"find the cause\": pop it with its result when it is done; everything until then is removed from your context when you pop it, what the user says included, so keep what matters as a thought.\n</system_reminder>")
}

// A pop on the round that crosses the compaction point: the count is
// read net of what the pop removed, so the turn goes on uncompacted.
func TestFrames_PopOnTheCompactingRound(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		popG:  {text: "done `g` \"fix it\" → fixed", events: []gnovm.MemFrame{poppedFrame("g", "fixed", 0)}},
	}
	steps := [][]fantasy.StreamPart{calling(memCall("c1", pushG))}
	for i := 1; i <= 5; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 4)))
	}
	steps = append(steps, calling(memCall("c2", popG)), saying("done"))
	// A window of 4,000 compacts at 3,200 tokens.
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")

	// The pop's round was the one that crossed the point: what was sent
	// up to the pop's row was over it.
	rows := s.rows()
	pop := slices.IndexFunc(rows, func(m message.Message) bool { return hasToolCall(m, "c2") })
	var sent int64
	for _, row := range rows[:pop] {
		sent += estimateMessageTokens(row.ToAIMessage())
	}
	require.GreaterOrEqual(t, sent, int64(3_200), "the scenario must cross the point on the pop's round")
	require.GreaterOrEqual(t, s.mem.estimateAfterPop, int64(2_500))

	require.Equal(t, 8, s.model.requestCount(), "no summary was asked for")
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	require.Zero(t, s.frames.estimate(s.sessID), "spent by the report after the pop")
}

// At the point at which the conversation would be compacted, a session
// standing in a frame slides instead: the oldest rounds of the frame's
// work leave the model's context, marked as the slide's; the model is
// told so before its next reply; no summary is made; what went is spent
// by the next report; and a record derived again from the rows cuts the
// same and tells the same.
func TestFrames_SlideInsteadOfCompaction(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{calling(memCall("c1", pushG))}
	for i := 1; i <= 12; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 4)))
	}
	steps = append(steps, saying("done"))
	// A window of 4,000 compacts at 3,200 tokens: about 600 a round, the
	// point is crossed more than once inside the frame.
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")

	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	require.Equal(t, len(steps), s.model.requestCount(), "no summary was asked for")
	by := s.poppedBy()
	require.Empty(t, by["call:c1"], "the push's round stays")
	require.Empty(t, by["result:c1"])
	require.True(t, strings.HasPrefix(by["call:w1"], "slide:g:c1:"), "the first round went: %q", by["call:w1"])
	require.Equal(t, by["call:w1"], by["result:w1"])
	require.Empty(t, by["call:w12"], "the last round stays")
	marks := map[string]bool{}
	for _, mark := range by {
		if strings.HasPrefix(mark, "slide:") {
			marks[mark] = true
		}
	}
	require.GreaterOrEqual(t, len(marks), 2, "slid again when the point was crossed again: %v", marks)

	// Told once per slide, before the next reply; a slide after the
	// turn's last step is told by the next prompt's frame line instead.
	var told int
	for _, text := range s.userRows() {
		if strings.Contains(text, "The start of `g` left your context to make room") {
			told++
			require.Contains(t, text, `mem.Show("g")`)
		}
	}
	require.GreaterOrEqual(t, told, len(marks)-1)
	require.LessOrEqual(t, told, len(marks))
	notice, ok := s.notice()
	require.True(t, ok)
	require.Contains(t, notice.Message, "slid `g`")
	require.Contains(t, notice.Message, "instead of compacting")

	// Every request was valid; the last has the push and not what went.
	for i := range s.model.requestCount() {
		validSequence(t, s.model.prompts[i])
	}
	last := s.model.sent(s.model.requestCount() - 1)
	require.Contains(t, last, "assistant:c1")
	require.NotContains(t, last, "tool:w1")
	require.Contains(t, last, "tool:w12")
	if told == len(marks) {
		require.Zero(t, s.frames.estimate(s.sessID), "spent by the report after the slide")
	} else {
		require.Positive(t, s.frames.estimate(s.sessID), "a slide after the last step is held until the next request's report")
	}

	again := newFrameStacks(s.env.sessions, s.env.messages, s.env.filetracker, s.env.workingDir)
	require.Equal(t, s.frames.ranges(t.Context(), s.sessID), again.ranges(t.Context(), s.sessID))
	require.Equal(t, s.frames.stackLine(t.Context(), s.sessID), again.stackLine(t.Context(), s.sessID))
	require.Contains(t, again.stackLine(t.Context(), s.sessID), "The start of `g` left your context to make room")
}

// With frames under the top one, the bottom frame's own body goes first,
// whole, before any of the frame the model works in.
func TestFrames_SlideLowerFrameFirst(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG:  {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
		pushS1: {text: inS1, events: []gnovm.MemFrame{pushed("g/s1", "find the cause")}},
	}
	steps := [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(workCall("w1", 4)),
		calling(workCall("w2", 4)),
		calling(memCall("c2", pushS1)),
	}
	for i := 3; i <= 7; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 4)))
	}
	steps = append(steps, saying("done"))
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")

	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	require.Equal(t, len(steps), s.model.requestCount())
	by := s.poppedBy()
	require.Equal(t, "slide:g:c1:c2", by["call:w1"], "g's body, from its push to s1's")
	require.Equal(t, "slide:g:c1:c2", by["call:w2"])
	require.Empty(t, by["call:c2"], "s1's push stays")
	require.True(t, strings.HasPrefix(by["call:w3"], "slide:g/s1:c2:"), "then s1's oldest rounds: %q", by["call:w3"])
	require.Empty(t, by["call:w7"])
	require.Equal(t, []string{"g", "g/s1"}, s.frames.stack(t.Context(), s.sessID))
	require.Contains(t, s.frames.stackLine(t.Context(), s.sessID), "The start of `g` and the start of `g/s1` left your context to make room; mem.Show(\"g/s1\")")
	for i := range s.model.requestCount() {
		validSequence(t, s.model.prompts[i])
	}
}

// A session over the point as a prompt comes, standing in a frame, slides
// before the prompt goes out, where it was summarized: the prompt's frame
// line tells the model what left.
func TestFrames_SlideBeforeThePrompt(t *testing.T) {
	// t.Setenv below: not in parallel.
	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(workCall("w1", 6)),
		calling(workCall("w2", 2)),
		saying("so far so good"),
		calling(workCall("w3", 1)),
		saying("done"),
	}
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")
	require.Equal(t, 4, s.model.requestCount())
	sess, err := s.env.sessions.Get(t.Context(), s.sessID)
	require.NoError(t, err)
	total := sess.PromptTokens + sess.CompletionTokens
	// The point is brought to about 250 tokens under the conversation:
	// the first round, about 900, is what the slide takes, and the second
	// turn's small work keeps the conversation under the point.
	t.Setenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", fmt.Sprint((total-250)*100/4_000))
	require.Greater(t, total, compactAt(4_000, 0, true), "the scenario must be over the point as the prompt comes")

	s.run("go on")
	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	require.Equal(t, 6, s.model.requestCount())
	by := s.poppedBy()
	require.Equal(t, "slide:g:c1:w2", by["call:w1"], "the first round went; the rest and the reply stayed")
	require.Empty(t, by["call:w2"])
	require.Empty(t, by["call:w3"])
	var told bool
	for _, text := range s.userRows() {
		if strings.Contains(text, "go on") {
			told = strings.Contains(text, "The start of `g` left your context to make room")
		}
	}
	require.True(t, told, "the prompt's frame line tells of the slide")
	for i := range s.model.requestCount() {
		validSequence(t, s.model.prompts[i])
	}
	require.Zero(t, s.frames.estimate(s.sessID))
}

// A hidden prompt, a continuation the agent sends itself, carries no frame
// line, so a slide made before it is told by a hidden line of its own
// before the model's first reply.
func TestFrames_SlideBeforeAHiddenPrompt(t *testing.T) {
	// t.Setenv below: not in parallel.
	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{
		calling(memCall("c1", pushG)),
		calling(workCall("w1", 6)),
		calling(workCall("w2", 2)),
		saying("so far so good"),
		calling(workCall("w3", 1)),
		saying("done"),
	}
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")
	sess, err := s.env.sessions.Get(t.Context(), s.sessID)
	require.NoError(t, err)
	total := sess.PromptTokens + sess.CompletionTokens
	t.Setenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", fmt.Sprint((total-250)*100/4_000))
	require.Greater(t, total, compactAt(4_000, 0, true), "the scenario must be over the point as the prompt comes")

	_, err = s.agent.Run(t.Context(), SessionAgentCall{SessionID: s.sessID, Prompt: "carry on", HiddenUserMessage: true, NonInteractive: true})
	require.NoError(t, err)
	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	require.Equal(t, "slide:g:c1:w2", s.poppedBy()["call:w1"])
	users := s.userRows()
	carry := slices.IndexFunc(users, func(text string) bool { return strings.Contains(text, "carry on") })
	require.GreaterOrEqual(t, carry, 0)
	require.NotContains(t, users[carry], "left your context", "a hidden prompt has no frame line")
	require.True(t, slices.ContainsFunc(users[carry+1:], func(text string) bool {
		return strings.Contains(text, "The start of `g` left your context to make room")
	}), "told before the first reply: %q", users[carry:])
	for i := range s.model.requestCount() {
		validSequence(t, s.model.prompts[i])
	}
}

// Rounds before the first frame slide first, all but the first, which
// anchors the cut; then the frame's own start.
func TestFrames_SlideRootFirst(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{
		calling(workCall("w1", 4)),
		calling(workCall("w2", 4)),
		calling(memCall("c1", pushG)),
	}
	for i := 3; i <= 8; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 4)))
	}
	steps = append(steps, saying("done"))
	s := newFramesScenario(t, replies, steps, 4_000)
	s.run("fix it")

	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }), "the conversation was not compacted")
	by := s.poppedBy()
	require.Empty(t, by["call:w1"], "the first round anchors the cut")
	require.Equal(t, "slide::w1:c1", by["call:w2"], "the rounds before the frame, up to its push")
	require.Empty(t, by["call:c1"])
	require.True(t, strings.HasPrefix(by["call:w3"], "slide:g:c1:"), "then the frame's own start: %q", by["call:w3"])
	var root, both bool
	for _, text := range s.userRows() {
		if strings.Contains(text, "Your rounds before `g` left your context to make room") {
			root = true
		}
		if strings.Contains(text, "Your rounds before `g` and the start of `g` left your context to make room") {
			both = true
		}
	}
	require.True(t, root, "told of the rounds before the frame")
	require.True(t, both, "then of the frame's start too")
	for i := range s.model.requestCount() {
		validSequence(t, s.model.prompts[i])
	}
}

// A summary that could not bring the conversation under the point stops
// compaction (see compacts); it does not stop the slide, which frees what
// it says or says it cannot.
func TestFrames_SlideWhenCompactionGaveUp(t *testing.T) {
	t.Parallel()

	replies := map[string]memReply{
		pushG: {text: inG, events: []gnovm.MemFrame{pushed("g", "fix it")}},
	}
	steps := [][]fantasy.StreamPart{calling(memCall("c1", pushG))}
	for i := 1; i <= 8; i++ {
		steps = append(steps, calling(workCall(fmt.Sprintf("w%d", i), 4)))
	}
	steps = append(steps, saying("done"))
	s := newFramesScenario(t, replies, steps, 4_000)
	s.agent.(*sessionAgent).compactLevels.Set(s.sessID, compactNever)
	s.run("fix it")

	rows := s.rows()
	require.False(t, slices.ContainsFunc(rows, func(m message.Message) bool { return m.IsSummaryMessage }))
	require.Equal(t, len(steps), s.model.requestCount())
	by := s.poppedBy()
	require.True(t, strings.HasPrefix(by["call:w1"], "slide:g:c1:"), "slid all the same: %q", by["call:w1"])
	require.Empty(t, by["call:w8"])
}
