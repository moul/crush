package app

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func callMsg(sessionID, id, callID, cmd string) message.Message {
	return message.Message{ID: id, SessionID: sessionID, Role: message.Assistant, Parts: []message.ContentPart{
		message.ToolCall{ID: callID, Name: "bash", Input: `{"command":"` + cmd + `"}`, Finished: true},
	}}
}

func resultMsg(sessionID, id, callID string, exitCode string) message.Message {
	return message.Message{ID: id, SessionID: sessionID, Role: message.Tool, Parts: []message.ContentPart{
		message.ToolResult{ToolCallID: callID, Name: "bash", Metadata: `{"exit_code":` + exitCode + `}`},
	}}
}

// A provider that sends no call IDs gets them numbered from tool-call-0 in
// each response by fantasy's OpenAI provider, so every step of a run can
// call its first tool tool-call-0, and so can a sub-agent's steps. Each call
// is still traced, and a failure is marked on the call that failed.
func TestTraceToolCallsReusedIDs(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	trace := format.NewToolTrace(&out)
	for _, step := range []struct {
		msg message.Message
		sub bool
	}{
		{callMsg("s", "m1", "tool-call-0", "ls"), false},
		{callMsg("s", "m1", "tool-call-0", "ls"), false}, // republished
		{resultMsg("s", "r1", "tool-call-0", "0"), false},
		{callMsg("s", "m2", "tool-call-0", "go test ./..."), false},
		{resultMsg("s", "r2", "tool-call-0", "1"), false},
		{callMsg("s", "m3", "tool-call-0", "agent"), false},
		{callMsg("sub", "m4", "tool-call-0", "grep x"), true},
		{resultMsg("sub", "r4", "tool-call-0", "0"), true},
		{resultMsg("s", "r3", "tool-call-0", "2"), false},
	} {
		traceToolCalls(trace, step.msg, step.sub)
	}

	require.Equal(t, "tool bash ls\n"+
		"tool bash go test ./...\n"+
		"tool bash go test ./... (exit 1)\n"+
		"tool bash agent\n"+
		"  tool bash grep x\n"+
		"tool bash agent (exit 2)\n", out.String())
}
