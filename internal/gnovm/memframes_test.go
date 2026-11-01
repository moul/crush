// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHostFrames is the host's side of frames (FRAMES.md): the stack a call
// runs with is the request's, the pushes and pops come back in the result
// and go on the journal entry, /mem log shows them, the receipt ends with
// the next hint after a pop no push followed, a pop's entry is undone like
// any other, and Recall finds a thought through the index.
func TestHostFrames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestHost(t, dir)
	ctx := context.Background()
	run := func(session string, stack []string, code string) *RunResult {
		t.Helper()
		res, err := h.Run(ctx, RunRequest{Project: "/proj", Session: session, Name: MemVM, Code: code, Frames: stack})
		require.NoError(t, err, code)
		return res
	}
	ok := func(session string, stack []string, code string) *RunResult {
		t.Helper()
		res := run(session, stack, code)
		require.NoError(t, res.Err, res.Output)
		return res
	}

	// At the empty root there is nothing to step into: a kept no-op.
	res := run("s1", nil, `mem.Next()`)
	require.Contains(t, res.Output, "no next todo: nothing is open")
	require.Empty(t, res.Frames)

	ok("s1", nil, `f := mem.Todo("fix the flaky test"); f.Todo("find why it fails"); f.Todo("rerun the suite")`)
	res = ok("s1", nil, `mem.Frame("fix-flaky-test").Push()`)
	require.Equal(t, []MemFrame{{Kind: "push", Id: "fix-flaky-test", Purpose: "fix the flaky test", Session: "s1"}}, res.Frames)
	require.Len(t, res.MemGoals, 1, "a push changes the goal, as the task list sees it")

	// The stack is the agent's: with none given, the session is at the root.
	res = run("s1", nil, `mem.Pop("x")`)
	require.ErrorContains(t, res.Err, "nothing to pop: you are at the root")

	stack := []string{"fix-flaky-test"}
	res = ok("s1", stack, `mem.Next()`)
	require.Equal(t, []MemFrame{{Kind: "push", Id: "fix-flaky-test/find-why-fails", Purpose: "find why it fails", Session: "s1"}}, res.Frames)
	stack = append(stack, "fix-flaky-test/find-why-fails")
	ok("s1", stack, `mem.Think("port", "the shared port is 8080")`)

	// Pop then push in one input: both events, in order; no hint, since a
	// push followed.
	res = ok("s1", stack, "mem.Pop(\"it races on the shared port\")\nmem.Next()")
	require.Equal(t, []MemFrame{
		{Kind: "pop", Id: "fix-flaky-test/find-why-fails", Purpose: "find why it fails", Result: "it races on the shared port", Thoughts: 1, Session: "s1"},
		{Kind: "push", Id: "fix-flaky-test/rerun-suite", Purpose: "rerun the suite", Session: "s1"},
	}, res.Frames)
	require.Contains(t, res.Output, "done `fix-flaky-test/find-why-fails` \"find why it fails\" → it races on the shared port\n")
	require.Contains(t, res.Output, "in `fix-flaky-test/rerun-suite` \"rerun the suite\": when it is done, pop it")
	require.NotContains(t, res.Output, "next:")
	stack = []string{"fix-flaky-test", "fix-flaky-test/rerun-suite"}

	// A lone pop ends with the hint; the thought of the popped step is
	// reached by name from the next, and by meaning through the index.
	res = ok("s1", stack, `mem.Thought("port")`)
	require.Equal(t, "thought `port` on `fix-flaky-test/find-why-fails`: the shared port is 8080\n", res.Output)
	res = ok("s1", stack, `mem.Recall("shared port")`)
	require.Contains(t, res.Output, "thought `port` on `fix-flaky-test/find-why-fails`: the shared port is 8080\n")
	res = ok("s1", stack, `mem.Pop("20 of 20 passed")`)
	require.True(t, strings.HasSuffix(res.Output, "next: pop `fix-flaky-test` with the evidence that it holds\n"), res.Output)
	seq := res.Seq
	stack = []string{"fix-flaky-test"}

	// The journal keeps the pushes and pops beside the Ops, and /mem log
	// shows them.
	log, err := h.MemCommand(ctx, "/proj", "s1", "log", nil)
	require.NoError(t, err)
	require.Contains(t, log, "    push `fix-flaky-test`\n")
	require.Contains(t, log, "    pop `fix-flaky-test/find-why-fails` → \"it races on the shared port\"\n    push `fix-flaky-test/rerun-suite`\n")
	require.Contains(t, log, "    pop `fix-flaky-test/rerun-suite` → \"20 of 20 passed\"")

	// Undo of the entry that popped a step undoes its Done: the step is open
	// and the session's again; the rows the pop removed stay removed (the
	// agent's side).
	out, err := h.MemCommand(ctx, "/proj", "s1", "undo "+strconv.FormatInt(seq, 10), nil)
	require.NoError(t, err)
	require.Contains(t, out, "undid #")
	res = ok("s1", stack, `println(mem.Show("fix-flaky-test/rerun-suite"))`)
	require.Contains(t, res.Output, "[ ] `fix-flaky-test/rerun-suite` \"rerun the suite\"")
	require.Contains(t, res.Output, "held by this session")

	// Another session: the step is held.
	res = run("s2", nil, `mem.Frame("fix-flaky-test/rerun-suite").Push()`)
	require.ErrorContains(t, res.Err, "`fix-flaky-test/rerun-suite` is held by session s1: only it can pop, close or drop it")
	require.Empty(t, res.Frames, "a refused input records no event")
	// At the root, Next steps into the newest open task's next free todo;
	// with none free, it is a no-op that says what holds them.
	res = run("s3", nil, `mem.Next()`)
	require.Contains(t, res.Output, "no next todo: `fix-flaky-test` \"fix the flaky test\": nothing in it can be done now: `fix-flaky-test/rerun-suite` is held by session s1")
	require.Empty(t, res.Frames)

	// A redacted secret leaves the journal's frames record too: the pop's
	// result in /mem log.
	ok("s1", stack, `mem.Redact("shared port")`)
	log, err = h.MemCommand(ctx, "/proj", "s1", "log", nil)
	require.NoError(t, err)
	require.NotContains(t, log, "shared port")
	require.Contains(t, log, "pop `fix-flaky-test/find-why-fails` → \"it races on the (redacted)\"")

	// A name gnosh made for a text (its small model's) reaches the realm by
	// text; a name mem has already gets -2; a text with no made name
	// is named from its words.
	res, err = h.Run(ctx, RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: "mem.Think(\"the suite shares port 8080\")\nmem.Think(\"the suite shares port 8080\")\nmem.Think(\"nobody named this one\")", Frames: stack, Names: map[string]string{"the suite shares port 8080": "shared-port"}})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	require.Contains(t, res.Output, "thought `shared-port` kept on `fix-flaky-test`\nthought `shared-port-2` kept on `fix-flaky-test`\nthought `nobody-named-one` kept on `fix-flaky-test`\n")
}

// TestHostFramesFresh: a pop of the frame the request names Fresh (entered
// in the agent's round before, with nothing since) is refused and keeps
// nothing, a pop and a Next in one input included; the same pop with a
// round between, or of a frame this input pushed itself, goes through.
func TestHostFramesFresh(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, t.TempDir())
	ctx := context.Background()
	run := func(stack []string, fresh, code string) *RunResult {
		t.Helper()
		res, err := h.Run(ctx, RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: code, Frames: stack, Fresh: fresh})
		require.NoError(t, err, code)
		return res
	}
	res := run(nil, "", `f := mem.Todo("fix the flaky test"); f.Todo("find why it fails"); f.Todo("rerun the suite")`)
	require.NoError(t, res.Err, res.Output)
	res = run([]string{"fix-flaky-test"}, "", `mem.Next()`)
	require.NoError(t, res.Err, res.Output)
	stack := []string{"fix-flaky-test", "fix-flaky-test/find-why-fails"}

	res = run(stack, "fix-flaky-test/find-why-fails", "mem.Pop(\"it races on the shared port\")\nmem.Next()")
	require.ErrorContains(t, res.Err, "not popped: nothing came between your entering `fix-flaky-test/find-why-fails` \"find why it fails\" and this pop, so it is not done. Do it, or check that it is done, then pop it.")
	require.Empty(t, res.Frames, "a refused input records no event")
	show := run(stack, "", `println(mem.Show("fix-flaky-test/find-why-fails"))`)
	require.Contains(t, show.Output, "[ ] `fix-flaky-test/find-why-fails`", "still open")

	// Done of the present frame is a pop too, and is refused the same way.
	res = run(stack, "fix-flaky-test/find-why-fails", `mem.Done("fix-flaky-test/find-why-fails", "it races on the shared port")`)
	require.ErrorContains(t, res.Err, "not popped: nothing came between your entering `fix-flaky-test/find-why-fails`")
	require.Empty(t, res.Frames)

	// Fresh names another frame than the top: the top pops.
	res = run(stack, "fix-flaky-test", `mem.Pop("it races on the shared port")`)
	require.NoError(t, res.Err, res.Output)
	require.Equal(t, "pop", res.Frames[0].Kind)

	// A frame this input pushed itself pops, whatever was fresh before.
	stack = []string{"fix-flaky-test"}
	res = run(stack, "fix-flaky-test", "mem.Next()\nmem.Pop(\"20 of 20 passed\")")
	require.NoError(t, res.Err, res.Output)
	require.Equal(t, []string{"push", "pop"}, []string{res.Frames[0].Kind, res.Frames[1].Kind})
}
