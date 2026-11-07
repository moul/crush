package format

import (
	"encoding/json"
	"fmt"
	"io"
)

// ToolTrace prints the --verbose trace of a non-interactive run's tool
// calls, a sub-agent's indented: a line for each call when it is made, and
// a second line, marked,
// when its result says it went wrong: "(failed)" for a tool error, and
// "(exit N)" for a command that exited non-zero. A call is printed once
// however often the message carrying it is republished. A nil *ToolTrace
// traces nothing.
type ToolTrace struct {
	out   io.Writer
	calls map[traceKey]*tracedCall
}

// traceKey names a call by its session as well as its ID, and tracedCall
// keeps the message that made it: a provider that sends no call IDs gets
// them numbered from tool-call-0 in each response by fantasy's OpenAI
// provider, so the next step's call, or a sub-agent's, can have the ID of
// one already traced.
type traceKey struct{ sessionID, id string }

type tracedCall struct {
	messageID string
	line      string // "name summary"
	marked    bool
}

// NewToolTrace returns a trace that writes to out.
func NewToolTrace(out io.Writer) *ToolTrace {
	return &ToolTrace{out: out, calls: map[traceKey]*tracedCall{}}
}

// Call records a tool call whose input is complete, made in the given
// session by the given message, printing it the first time it is seen.
func (t *ToolTrace) Call(sessionID, messageID, id, name, summary string) {
	t.call("", sessionID, messageID, id, name, summary)
}

// SubCall records a tool call a sub-agent made, printed like Call but
// indented, under the call to the agent tool that started it.
func (t *ToolTrace) SubCall(sessionID, messageID, id, name, summary string) {
	t.call("  ", sessionID, messageID, id, name, summary)
}

func (t *ToolTrace) call(indent, sessionID, messageID, id, name, summary string) {
	if t == nil || id == "" {
		return
	}
	key := traceKey{sessionID, id}
	if c, seen := t.calls[key]; seen && c.messageID == messageID {
		return
	}
	line := indent + "tool " + name
	if summary != "" {
		line += " " + summary
	}
	t.calls[key] = &tracedCall{messageID: messageID, line: line}
	_, _ = fmt.Fprintln(t.out, line)
}

// Result records the result of the latest call with the given ID in the
// given session, given whether it is a tool error and its metadata,
// printing the call again, marked, the first time a result says the call
// went wrong. A command's exit status comes from the exit_code in its
// metadata, which the bash tool records.
func (t *ToolTrace) Result(sessionID, id string, isError bool, metadata string) {
	if t == nil {
		return
	}
	c, traced := t.calls[traceKey{sessionID, id}]
	if !traced || c.marked {
		return
	}
	var mark string
	if isError {
		mark = "(failed)"
	} else {
		var meta struct {
			ExitCode int `json:"exit_code"`
		}
		if json.Unmarshal([]byte(metadata), &meta) == nil && meta.ExitCode != 0 {
			mark = fmt.Sprintf("(exit %d)", meta.ExitCode)
		}
	}
	if mark == "" {
		return
	}
	c.marked = true
	_, _ = fmt.Fprintf(t.out, "%s %s\n", c.line, mark)
}
