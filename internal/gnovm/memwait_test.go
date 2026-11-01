// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

//go:build unix

package gnovm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A turn hold is counted: a nested run of the same session shares it, and
// the lock goes with the last release.
func TestHoldTurnIsCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outer := holdTurn(context.Background(), dir, "s1")
	require.True(t, turnHeld(dir, "s1"))
	done := make(chan func(), 1)
	go func() { done <- holdTurn(context.Background(), dir, "s1") }()
	var inner func()
	select {
	case inner = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a nested hold waited for the outer one")
	}
	outer()
	require.True(t, turnHeld(dir, "s1"), "the nested run still holds it")
	inner()
	inner()
	require.False(t, turnHeld(dir, "s1"), "gone with the last release, once")
}

// A client's Next waits for another process's held todo, asleep on its
// pipe, and wakes on the pop that frees it: one result, with the wait in it
// and the push. Then a wait ended by the holder's turn ending, and one ended
// by Esc, which keeps what came before it. (Not parallel: the client is
// process-wide.)
func TestClientNextWaitsAndWakes(t *testing.T) {
	dir := t.TempDir()
	other := newTestHost(t, dir) // another gnosh process
	ctx := context.Background()
	run := func(session string, stack []string, code string) *RunResult {
		t.Helper()
		res, err := other.Run(ctx, RunRequest{Project: "/proj", Session: session, Name: MemVM, Code: code, Frames: stack})
		require.NoError(t, err)
		require.NoError(t, res.Err, res.Output)
		return res
	}
	runIn := func(project string) func(session string, stack []string, code string) *RunResult {
		return func(session string, stack []string, code string) *RunResult {
			t.Helper()
			res, err := other.Run(ctx, RunRequest{Project: project, Session: session, Name: MemVM, Code: code, Frames: stack})
			require.NoError(t, err)
			require.NoError(t, res.Err, res.Output)
			return res
		}
	}
	run2, run3 := runIn("/proj2"), runIn("/proj3")
	res, err := other.Run(ctx, RunRequest{Project: "/proj", Name: MemVM, Code: "r := mem.Todo(\"release\")\nr.Todo(\"bump the version\")\nr.Todo(\"tag it\")"})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	holder := holdTurn(ctx, dir, "p1")
	defer holder()
	run("p1", nil, `mem.Next()`)

	c, err := Acquire(ClientOptions{Options: Options{Dir: dir, Now: testNow}, InProcess: true})
	require.NoError(t, err)
	defer c.Release()
	me := holdTurn(ctx, dir, "p2")
	defer me()

	type answer struct {
		res *RunResult
		err error
		at  time.Time
	}
	got := make(chan answer, 1)
	go func() {
		res, err := c.Run(ctx, RunRequest{Project: "/proj", Session: "p2", Name: MemVM, Code: `mem.Next()`, Req: "call-1"})
		got <- answer{res, err, time.Now()}
	}()
	waitingNow(t, dir, "/proj")
	select {
	case a := <-got:
		t.Fatalf("Next did not wait: %v %+v", a.err, a.res)
	default:
	}
	popped := time.Now()
	run("p1", []string{"release/bump-version"}, `mem.Pop("now 2.0.0")`)
	var a answer
	select {
	case a = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the pop did not wake the waiting Next")
	}
	require.NoError(t, a.err)
	require.NoError(t, a.res.Err)
	require.Less(t, a.at.Sub(popped), 3*time.Second, "woken by the pop itself")
	require.Contains(t, a.res.Output, "waited ")
	require.Contains(t, a.res.Output, "for `release/bump-version` (session p1)")
	require.Contains(t, a.res.Output, "in `release/tag`")
	require.Equal(t, []MemFrame{{Kind: "push", Id: "release/tag", Purpose: "tag it", Session: "p2"}}, a.res.Frames)
	require.Empty(t, a.res.Waits)

	// The holder's turn ends: the wait wakes, and Next passes over the todo
	// of a session not working, as one session's Next does today.
	run("p2", []string{"release/tag"}, `mem.Pop("tagged v2.0.0")`)
	res, err = other.Run(ctx, RunRequest{Project: "/proj2", Name: MemVM, Code: "s := mem.Todo(\"ship\")\ns.Todo(\"build\")\ns.Todo(\"upload\")"})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	run2("p1", nil, `mem.Next()`)
	go func() {
		res, err := c.Run(ctx, RunRequest{Project: "/proj2", Session: "p2", Name: MemVM, Code: `mem.Next()`, Req: "call-2"})
		got <- answer{res, err, time.Now()}
	}()
	waitingNow(t, dir, "/proj2")
	ended := time.Now()
	holder()
	select {
	case a = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder's turn ending did not wake the wait")
	}
	require.NoError(t, a.err)
	require.Less(t, a.at.Sub(ended), 3*time.Second)
	require.Contains(t, a.res.Output, "for `ship/build` (session p1)", a.res.Output)
	require.Equal(t, []string{"ship/upload"}, stackAfter(nil, a.res.Frames), a.res.Output)
	run2("p2", []string{"ship/upload"}, `mem.Pop("uploaded")`)

	// Esc during a wait: it stops at once; a pop before the Next is kept,
	// its frame in the result.
	holder2 := holdTurn(ctx, dir, "p1")
	defer holder2()
	res, err = other.Run(ctx, RunRequest{Project: "/proj3", Name: MemVM, Code: "q := mem.Todo(\"quarterly\").Parallel()\nq.Todo(\"collect\")\nq.Todo(\"report\")"})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	run3("p1", nil, `mem.Next()`)
	cres, err := c.Run(ctx, RunRequest{Project: "/proj3", Session: "p2", Name: MemVM, Code: `mem.Next()`, Req: "call-3"})
	require.NoError(t, err)
	require.Equal(t, "quarterly/report", stackAfter(nil, cres.Frames)[0], cres.Output)
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		res, err := c.Run(cctx, RunRequest{Project: "/proj3", Session: "p2", Name: MemVM, Code: "mem.Pop(\"reported\")\nmem.Next()", Frames: []string{"quarterly/report"}, Req: "call-4"})
		got <- answer{res, err, time.Now()}
	}()
	waitingNow(t, dir, "/proj3")
	stopped := time.Now()
	cancel()
	select {
	case a = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("Esc did not stop the wait")
	}
	require.NoError(t, a.err, "what was kept is never an error")
	require.Less(t, a.at.Sub(stopped), 2*time.Second)
	require.True(t, strings.Contains(a.res.Output, "stopped waiting for"), a.res.Output)
	require.Equal(t, []MemFrame{{Kind: "pop", Id: "quarterly/report", Purpose: "report", Result: "reported", Session: "p2"}}, a.res.Frames)
}

// waitingNow waits until a Next waits on project's mem, its pipe active:
// what a test does next then happens during the wait, however long the
// waiter took to get there.
func waitingNow(t *testing.T, dir, project string) {
	t.Helper()
	active := filepath.Join(memDir(dir, project), "waiters", "active")
	require.Eventually(t, func() bool {
		es, _ := os.ReadDir(active)
		return len(es) > 0
	}, 20*time.Second, 10*time.Millisecond, "no Next waits on %s's mem", project)
}
