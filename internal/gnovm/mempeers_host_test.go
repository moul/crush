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
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The host's side of peers, with real turn locks, peers records and wake
// pipes (PARALLEL.md).

func TestEndsWithBareNext(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]bool{
		"mem.Next()":                            true,
		"mem.Pop(\"found\")\nmem.Next()":        true,
		"  mem.Next()  \n":                      true,
		"mem.Next()\nmem.Show()":                false,
		"mem.Frame(\"x\").Next()":               false,
		"for i := 0; i < 2; i++ { mem.Next() }": false,
		"f := func() { mem.Next() }\nf()":       false,
		"mem.Next(\"x\")":                       false,
		"func x() {}":                           false,
		"":                                      false,
	} {
		require.Equal(t, want, endsWithBareNext(code), "%q", code)
	}
}

// takeTurn holds session's turn lock in dir, as the agent does while the
// session is busy.
func takeTurn(t *testing.T, dir, session string) func() {
	t.Helper()
	path := turnLockPath(dir, session)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	release, err := lock.File(context.Background(), path)
	require.NoError(t, err)
	return release
}

func TestTurnHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.False(t, turnHeld(dir, "s1"), "no file: not running")
	release := takeTurn(t, dir, "s1")
	require.True(t, turnHeld(dir, "s1"))
	require.True(t, turnHeld(dir, "s1"), "a probe takes nothing from the holder")
	release()
	require.False(t, turnHeld(dir, "s1"))
	_, err := os.Stat(turnLockPath(dir, "s1"))
	require.NoError(t, err, "the file stays")
}

// testPipe is a pipe a client would make: in waiters/new/, both ends open,
// then pending in waiters/.
type testPipe struct {
	name   string
	rd, wr int
}

func makeWakePipe(t *testing.T, memdir, name string) *testPipe {
	t.Helper()
	fresh := filepath.Join(memdir, "waiters", "new", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(fresh), 0o700))
	require.NoError(t, unix.Mkfifo(fresh, 0o600))
	rd, err := unix.Open(fresh, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	wr, err := unix.Open(fresh, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	require.NoError(t, os.Rename(fresh, filepath.Join(memdir, "waiters", name)))
	p := &testPipe{name: name, rd: rd, wr: wr}
	t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
	return p
}

// rung is how many bytes the pipe holds now.
func (p *testPipe) rung() int {
	buf := make([]byte, 64)
	n, err := unix.Read(p.rd, buf)
	if err != nil {
		return 0
	}
	return n
}

func TestRingWakesActivePipesOnly(t *testing.T) {
	t.Parallel()
	h := newTestMemHost(t, t.TempDir())
	active := makeWakePipe(t, h.dir, "aa")
	require.True(t, h.activate("aa"))
	pending := makeWakePipe(t, h.dir, "bb")

	// A pipe whose waiter is gone: no reader.
	gone := filepath.Join(h.dir, "waiters", "active", "cc")
	require.NoError(t, unix.Mkfifo(gone, 0o600))
	stale := filepath.Join(h.dir, "waiters", "dd")
	require.NoError(t, unix.Mkfifo(stale, 0o600))

	h.ring()
	require.Equal(t, 1, active.rung(), "an active pipe is rung once")
	require.Equal(t, 0, pending.rung(), "a pending one is only probed")
	_, err := os.Stat(gone)
	require.True(t, os.IsNotExist(err), "an active pipe nobody reads is removed")
	_, err = os.Stat(stale)
	require.True(t, os.IsNotExist(err), "a pending one nobody reads is removed")
	_, err = os.Stat(filepath.Join(h.dir, "waiters", "bb"))
	require.NoError(t, err)

	// A full pipe is rung already: ring never blocks.
	for range 100000 {
		if _, err := unix.Write(active.wr, []byte{1}); err != nil {
			break
		}
	}
	done := make(chan struct{})
	go func() { h.ring(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ring blocked on a full pipe")
	}
}

// Two sessions with real turn locks: the second's Next waits for the
// first's todo, activating its pipe; the first's pop rings it; the next
// Next steps in. A holder not working, or holding without the todo on its
// stack, is not waited for; a sub-agent never waits; an input that does not
// end with a bare mem.Next() answers as today.
func TestPeersHostWaitAndWake(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	h := newTestMemHost(t, filepath.Join(data, "proj", "mem"))
	h.data = data
	ctx := context.Background()
	call := func(session string, stack []string, code, pipe string, sub bool) memResult {
		t.Helper()
		res, err := h.call(ctx, memRequest{Code: code, Session: session, Frames: stack, Pipe: pipe, SubAgent: sub})
		require.NoError(t, err, res.Output)
		return res
	}
	res, err := h.call(ctx, memRequest{Code: "r := mem.Todo(\"release\")\nr.Todo(\"bump the version\")\nr.Todo(\"tag it\")", User: true})
	require.NoError(t, err, res.Output)

	p1 := takeTurn(t, data, "p1")
	defer p1()
	defer takeTurn(t, data, "p2")()
	res = call("p1", nil, `mem.Next()`, "", false)
	require.Equal(t, []string{"release/bump-version"}, stackAfter(nil, res.Frames))
	rec, ok := h.readPeer("p1")
	require.True(t, ok)
	require.Equal(t, []string{"release/bump-version"}, rec.Stack, "the call's record holds the stack it left")

	pipe := makeWakePipe(t, h.dir, "01")
	res = call("p2", nil, `mem.Next()`, "01", false)
	require.Equal(t, []MemWait{{Id: "release/bump-version", Session: "p1"}}, res.Waits, res.Output)
	_, err = os.Stat(filepath.Join(h.dir, "waiters", "active", "01"))
	require.NoError(t, err, "the waiting call activated its pipe")
	require.Equal(t, 0, pipe.rung(), "it does not ring itself")

	res = call("p1", []string{"release/bump-version"}, `mem.Pop("now 2.0.0")`, "", false)
	require.Equal(t, 1, pipe.rung(), "the pop rings the waiting Next")

	res = call("p2", nil, `mem.Next()`, "", false)
	require.Equal(t, []string{"release/tag"}, stackAfter(nil, res.Frames), res.Output)

	// Not waited for: a sub-agent; an input with more after its Next; a
	// holder whose turn ended; a holder whose stack does not have the todo.
	res, err = h.call(ctx, memRequest{Code: "s := mem.Todo(\"ship\")\ns.Todo(\"build\")\ns.Todo(\"upload\")", User: true})
	require.NoError(t, err, res.Output)
	res = call("p1", nil, `mem.Next()`, "", false)
	require.Equal(t, []string{"ship/build"}, stackAfter(nil, res.Frames))
	makeWakePipe(t, h.dir, "02")
	res = call("p3", nil, `mem.Next()`, "02", true)
	require.Empty(t, res.Waits, "a sub-agent never waits")
	require.Contains(t, res.Output, "no next todo")
	makeWakePipe(t, h.dir, "03")
	res = call("p3", nil, "mem.Next()\nprintln(\"after\")", "03", false)
	require.Empty(t, res.Waits, "only a bare mem.Next() ending the input waits")
	makeWakePipe(t, h.dir, "04")
	p1()
	res = call("p3", nil, `mem.Next()`, "04", false)
	require.Empty(t, res.Waits, "a holder not working is not waited for: %s", res.Output)
}

// A wait that would close a loop is refused, and Next answers as one that
// may not wait.
func TestPeersHostLoop(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	h := newTestMemHost(t, filepath.Join(data, "proj", "mem"))
	h.data = data
	ctx := context.Background()
	res, err := h.call(ctx, memRequest{Code: "r := mem.Todo(\"release\")\nr.Todo(\"bump the version\")\nr.Todo(\"tag it\")", User: true})
	require.NoError(t, err, res.Output)
	defer takeTurn(t, data, "p1")()
	defer takeTurn(t, data, "p2")()
	res, err = h.call(ctx, memRequest{Code: `mem.Next()`, Session: "p1"})
	require.NoError(t, err)
	// p1's record says it waits for p2, as if it had waited for a todo of
	// p2's elsewhere.
	require.NoError(t, h.writePeer(memPeer{Session: "p1", Stack: []string{"release/bump-version"}, Waits: []MemWait{{Id: "elsewhere", Session: "p2"}}}))
	makeWakePipe(t, h.dir, "0a")
	res, err = h.call(ctx, memRequest{Code: `mem.Next()`, Session: "p2", Pipe: "0a"})
	require.NoError(t, err)
	require.Empty(t, res.Waits)
	require.Contains(t, res.Output, "which waits for this session: that would wait forever")
	require.Contains(t, res.Output, "no next todo")
}

func TestRenamePeers(t *testing.T) {
	t.Parallel()
	h := newTestMemHost(t, t.TempDir())
	require.NoError(t, h.writePeer(memPeer{Session: "p1", Stack: []string{"secret-plan", "secret-plan/a"}, Waits: []MemWait{{Id: "secret-plan/b", Session: "p2"}}}))
	h.renamePeers(map[string]string{"secret-plan": "forgotten-1"})
	p, ok := h.readPeer("p1")
	require.True(t, ok)
	require.Equal(t, []string{"forgotten-1", "forgotten-1/a"}, p.Stack)
	require.Equal(t, "forgotten-1/b", p.Waits[0].Id)
}
