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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The realm's side of peers (PARALLEL.md): which todo each session gets
// while others work, and when its Next waits. Working sessions are stood in
// by a set the test keeps; the host's turn locks are the real thing.

// peersMem is a mem whose working sessions are those the test says.
type peersMem struct {
	t  *testing.T
	h  *memHost
	mu sync.Mutex
	on map[string]bool
}

func newPeersMem(t *testing.T) *peersMem {
	p := &peersMem{t: t, h: newTestMemHost(t, t.TempDir()), on: map[string]bool{}}
	p.h.working = func(session, _ string) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.on[session]
	}
	return p
}

func (p *peersMem) working(sessions ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.on = map[string]bool{}
	for _, s := range sessions {
		p.on[s] = true
	}
}

// peer is a session with its stack, as the agent keeps it.
type peer struct {
	p       *peersMem
	session string
	stack   []string
}

func (p *peersMem) peer(session string) *peer { return &peer{p: p, session: session} }

// run is one input; wait lets its Next wait.
func (s *peer) run(code string, wait bool) memResult {
	s.p.t.Helper()
	res, err := s.p.h.call(context.Background(), memRequest{Code: code, Session: s.session, Frames: s.stack, MayWait: wait})
	require.NoError(s.p.t, err, res.Output)
	s.stack = stackAfter(s.stack, res.Frames)
	return res
}

func (s *peer) top() string {
	if len(s.stack) == 0 {
		return ""
	}
	return s.stack[len(s.stack)-1]
}

// shapes is the demonstration's tree: a parallel task with two parallel
// shapes, each with two leaves.
func shapes(t *testing.T, p *peersMem) {
	t.Helper()
	user := p.peer("user")
	user.run(`g := mem.Todo("make geom pass its tests").Parallel()
c := g.Todo("circle").Parallel()
c.Todo("circle area")
c.Todo("circle perimeter")
s := g.Todo("square").Parallel()
s.Todo("square area")
s.Todo("square perimeter")`, false)
}

// A todo marked parallel says so in Show and JSON, keeps it across a
// rebuild from the journal, and one session still takes its todos in order.
func TestPeersParallelMark(t *testing.T) {
	t.Parallel()
	p := newPeersMem(t)
	shapes(t, p)
	a := p.peer("s1")
	res := a.run(`println(mem.Show())`, false)
	require.Contains(t, res.Output, "its todos at once")
	res = a.run(`print(mem.JSON())`, false)
	require.Contains(t, res.Output, `"parallel":true`)

	stores, _ := filepath.Glob(filepath.Join(p.h.dir, "store-*"))
	require.Len(t, stores, 1)
	require.NoError(t, os.RemoveAll(stores[0]))
	res = a.run(`println(mem.Show())`, false)
	require.Contains(t, res.Output, "its todos at once", "rebuilt from the journal")

	res = a.run(`mem.Next()`, true)
	require.Equal(t, []string{"make-geom-pass-tests/circle/circle-area"}, a.stack, res.Output)
	a.run("mem.Pop(\"pi r squared\")\nmem.Next()", true)
	require.Equal(t, "make-geom-pass-tests/circle/circle-perimeter", a.top(), "one session, in order")
}

// Root peers take different leaves; a peer with nothing free waits while a
// shape's own work is still to come, and when the shape is ready it gets
// it, told what the others found; one session alone never waits.
func TestPeersRootWaitAndJoin(t *testing.T) {
	t.Parallel()
	p := newPeersMem(t)
	shapes(t, p)
	p1, p2, p3, p4, p5 := p.peer("p1"), p.peer("p2"), p.peer("p3"), p.peer("p4"), p.peer("p5")
	p.working("p1", "p2", "p3", "p4", "p5")
	for _, s := range []*peer{p1, p2, p3, p4} {
		s.run(`mem.Next()`, true)
	}
	require.Equal(t, "make-geom-pass-tests/circle/circle-area", p1.top())
	require.Equal(t, "make-geom-pass-tests/circle/circle-perimeter", p2.top())
	require.Equal(t, "make-geom-pass-tests/square/square-area", p3.top())
	require.Equal(t, "make-geom-pass-tests/square/square-perimeter", p4.top())

	res := p5.run(`mem.Next()`, true)
	require.Empty(t, res.Output, "a waiting Next prints nothing")
	require.Empty(t, res.Frames)
	require.Len(t, res.Waits, 4, "it waits for the four leaves")
	res = p5.run(`mem.Next()`, false)
	require.Contains(t, res.Output, "no next todo", "a Next that may not wait answers as today")
	require.Empty(t, res.Waits)

	p1.run(`mem.Pop("pi r squared")`, false)
	p2.run(`mem.Pop("2 pi r")`, false)
	res = p5.run(`mem.Next()`, true)
	require.Equal(t, "make-geom-pass-tests/circle", p5.top(), res.Output)
	require.Contains(t, res.Output, "its todo `make-geom-pass-tests/circle/circle-area` was done by session p1 → pi r squared")
	require.Contains(t, res.Output, "its todo `make-geom-pass-tests/circle/circle-perimeter` was done by session p2 → 2 pi r")
}

// In an ordered todo a todo being worked on holds up the ones after it;
// once its session is not working, it is passed over, as today.
func TestPeersOrder(t *testing.T) {
	t.Parallel()
	p := newPeersMem(t)
	p.peer("user").run(`r := mem.Todo("release")
r.Todo("bump the version")
r.Todo("tag it")`, false)
	p1, p2 := p.peer("p1"), p.peer("p2")
	p.working("p1", "p2")
	p1.run(`mem.Next()`, true)
	require.Equal(t, "release/bump-version", p1.top())
	res := p2.run(`mem.Next()`, true)
	require.Empty(t, p2.stack, "the tag waits for the bump: %s", res.Output)
	require.Equal(t, []memWait{{Id: "release/bump-version", Session: "p1"}}, res.Waits)

	p.working("p2")
	res = p2.run(`mem.Next()`, true)
	require.Equal(t, "release/tag", p2.top(), "a hold of a session not working is passed over, as today: %s", res.Output)
}

// A planner that pushed the task works inside it with the peers: it prefers
// a free shape beside it to helping inside one a peer holds, waits for its
// own frame's pop, and peers do not wait for what only its pop is left of.
func TestPeersPlannerAndFrames(t *testing.T) {
	t.Parallel()
	p := newPeersMem(t)
	shapes(t, p)
	pl, p1 := p.peer("planner"), p.peer("p1")
	p.working("planner", "p1")
	pl.run(`mem.Frame("make-geom-pass-tests").Push()`, false)
	p1.run(`mem.Frame("make-geom-pass-tests/circle").Push()`, false)

	res := pl.run(`mem.Next()`, true)
	require.Equal(t, "make-geom-pass-tests/square", pl.top(), "a free shape beside it before helping: %s", res.Output)
	pl.run(`mem.Next()`, true)
	pl.run(`mem.Pop("side squared")`, false)
	p1.run(`mem.Next()`, true)
	require.Equal(t, "make-geom-pass-tests/circle/circle-area", p1.top())
	res = pl.run(`mem.Next()`, true)
	require.Equal(t, "make-geom-pass-tests/square/square-perimeter", pl.top(), res.Output)
	pl.run(`mem.Pop("4 side")`, false)
	res = pl.run(`mem.Pop("square passes")`, false)
	require.Contains(t, res.Output, "done `make-geom-pass-tests/square`")
	res = pl.run(`mem.Next()`, true)
	require.Equal(t, "make-geom-pass-tests/circle/circle-perimeter", pl.top(), "nothing beside it is free: it helps inside the circle p1 holds: %s", res.Output)

	// p1 cannot pop the circle while the planner works under it; it waits.
	p1.run(`mem.Pop("pi r squared")`, false)
	res, err := p.h.call(context.Background(), memRequest{Code: `mem.Pop("circle passes")`, Session: "p1", Frames: p1.stack})
	require.Error(t, err)
	require.Contains(t, err.Error(), "works on now: mem.Next() waits for them")
	res = p1.run(`mem.Next()`, true)
	require.Len(t, res.Waits, 1, "the circle's holder waits to pop it: %s", res.Output)

	pl.run(`mem.Pop("2 pi r")`, false)
	res = p1.run(`mem.Next()`, true)
	require.Contains(t, res.Output, "no next todo: `make-geom-pass-tests/circle` has no open todo: pop it")
	p1.run(`mem.Pop("circle passes")`, false)

	// Only the planner's own pop is left: a root peer does not wait for it.
	q := p.peer("q")
	p.working("planner", "p1", "q")
	res = q.run(`mem.Next()`, true)
	require.Empty(t, res.Waits, res.Output)
	require.True(t, strings.Contains(res.Output, "no next todo"), res.Output)
}

// One session never waits: at "pop it" with nobody working it answers as
// today, whatever the input allows.
func TestPeersLoneSessionNeverWaits(t *testing.T) {
	t.Parallel()
	p := newPeersMem(t)
	s := p.peer("s1")
	s.run(`mem.Todo("fix it").Push()
mem.Todo("find why")`, false)
	s.run(`mem.Next()`, true)
	res := s.run("mem.Pop(\"found\")\nmem.Next()", true)
	require.Empty(t, res.Waits)
	require.Contains(t, res.Output, "no next todo: `fix` has no open todo: pop it")
	p.working("s1")
	res = s.run(`mem.Next()`, true)
	require.Empty(t, res.Waits, "its own work is no peer's")
	require.Contains(t, res.Output, "no next todo: `fix` has no open todo: pop it")
}
