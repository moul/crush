package format

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolTrace(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	trace := NewToolTrace(&out)

	trace.Call("s", "m", "c1", "bash", "go test ./...")
	trace.Call("s", "m", "c1", "bash", "go test ./...") // republished: printed once
	trace.Call("s", "m", "c2", "view", "main.go")
	trace.Call("s", "m", "c3", "bash", "true")
	trace.Call("s", "m", "", "ls", "") // no ID yet: nothing to key it by
	trace.Result("s", "c2", false, "")
	trace.Result("s", "c3", false, `{"exit_code":0}`)
	trace.Result("s", "c1", false, `{"output":"FAIL","exit_code":2}`)
	trace.Result("s", "c1", false, `{"exit_code":2}`) // republished: marked once
	trace.Call("s", "m", "c4", "edit", "a.go")
	trace.Result("s", "c4", true, "")
	trace.Result("s", "c9", true, "") // never traced: nothing to mark

	require.Equal(t, "tool bash go test ./...\n"+
		"tool view main.go\n"+
		"tool bash true\n"+
		"tool bash go test ./... (exit 2)\n"+
		"tool edit a.go\n"+
		"tool edit a.go (failed)\n", out.String(),
		"one line per call, and a second only when it went wrong")

	var none *ToolTrace
	none.Call("s", "m", "c1", "bash", "x") // a nil trace is off, not a panic
	none.Result("s", "c1", true, "")
}

// A sub-agent's calls are traced indented, under the call to the agent tool
// that started them, and a failed one is marked with the same indent.
func TestToolTraceSubCalls(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	trace := NewToolTrace(&out)
	trace.Call("s", "m", "a1", "agent", "Find where the queue is drained")
	trace.SubCall("s", "m", "s1", "grep", "messageQueue")
	trace.SubCall("s", "m", "s2", "view", "/src/agent.go")
	trace.Result("s", "s2", true, "")
	trace.SubCall("s", "m", "s1", "grep", "messageQueue") // republished: printed once
	require.Equal(t, "tool agent Find where the queue is drained\n"+
		"  tool grep messageQueue\n"+
		"  tool view /src/agent.go\n"+
		"  tool view /src/agent.go (failed)\n", out.String())
}
