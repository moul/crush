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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The demonstration of peers (PARALLEL.md, demonstration A): N gnosh-like
// processes work one tree in one mem at once, each running the loop a model
// would (Next; work; Pop with what came of it; Next), and finish the same
// tree as one process does, faster. Each peer is this test binary started
// again in TestPeerChild, its own client, its own turn lock; the work is a
// sleep, outside every lock.

// peerEnv marks a child: it runs TestPeerChild only, and never starts
// another.
const peerEnv = "GNOSH_PEER_CHILD"

// peerWork is how long todo id's own work takes, long beside a mem call (a
// few tens of ms) and a peer's start (about 600 ms), as a model's work is: a
// leaf 1 to 2.6 s, by its name; a shape and the build 400 ms; the notes
// 1.2 s; the task 300 ms.
func peerWork(id string) time.Duration {
	name := stepName(id)
	switch {
	case strings.HasSuffix(name, "-area"), strings.HasSuffix(name, "-perimeter"):
		sum := sha256.Sum256([]byte(name))
		return time.Duration(1000+400*int(sum[0]%5)) * time.Millisecond
	case strings.Contains(name, "notes"):
		return 1200 * time.Millisecond
	case !strings.Contains(id, "/"):
		return 300 * time.Millisecond
	}
	return 400 * time.Millisecond
}

// peerEvent is a line of a peer's log.
type peerEvent struct {
	At      int64  `json:"at"` // ms
	Session string `json:"session"`
	Kind    string `json:"kind"` // push, pop, wait, done
	Id      string `json:"id,omitempty"`
	Waited  string `json:"waited,omitempty"`
}

var waitedRe = regexp.MustCompile(`waited ([^ ]+) for `)

// stepName is the last segment of a todo's id.
func stepName(id string) string { return id[strings.LastIndex(id, "/")+1:] }

// TestPeerChild is a peer, when this binary is started as one.
func TestPeerChild(t *testing.T) {
	if os.Getenv(peerEnv) != "1" {
		t.Skip("a peer process only")
	}
	dir, project, session, role := os.Getenv("GNOSH_PEER_DIR"), os.Getenv("GNOSH_PEER_PROJECT"), os.Getenv("GNOSH_PEER_SESSION"), os.Getenv("GNOSH_PEER_ROLE")
	logf, err := os.Create(os.Getenv("GNOSH_PEER_LOG"))
	require.NoError(t, err)
	defer logf.Close()
	enc := json.NewEncoder(logf)
	note := func(kind, id, waited string) {
		require.NoError(t, enc.Encode(peerEvent{At: time.Now().UnixMilli(), Session: session, Kind: kind, Id: id, Waited: waited}))
	}
	ctx := context.Background()
	c, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	defer c.Release()
	defer holdTurn(ctx, dir, session)()

	if role == "hold" {
		// A holder that takes a todo and works on it until killed.
		res, err := c.Run(ctx, RunRequest{Project: project, Session: session, Name: MemVM, Code: `mem.Next()`, Req: session + "-0"})
		require.NoError(t, err)
		require.NoError(t, res.Err, res.Output)
		note("push", stackAfter(nil, res.Frames)[0], "")
		select {}
	}
	var stack []string
	code := "mem.Next()"
	if role == "planner" {
		code = "mem.Frame(" + strconv.Quote(os.Getenv("GNOSH_PEER_ROOT")) + ").Push()\nmem.Next()"
	}
	for n := 1; ; n++ {
		// A pop is noted as it is sent: the call it starts may then wait.
		if strings.HasPrefix(code, "mem.Pop(") {
			note("pop", stack[len(stack)-1], "")
		}
		res, err := c.Run(ctx, RunRequest{Project: project, Session: session, Name: MemVM, Code: code, Frames: stack, Req: fmt.Sprintf("%s-%d", session, n)})
		require.NoError(t, err)
		require.NoError(t, res.Err, res.Output)
		for _, m := range waitedRe.FindAllStringSubmatch(res.Output, -1) {
			note("wait", "", m[1])
		}
		for _, f := range res.Frames {
			if f.Kind == "push" {
				note("push", f.Id, "")
			}
		}
		stack = stackAfter(stack, res.Frames)
		switch {
		case len(stack) > 0 && strings.Contains(res.Output, "has no open todo: pop it"):
			id := stack[len(stack)-1]
			time.Sleep(peerWork(id))
			code = "mem.Pop(" + strconv.Quote(peerResult(t, c, project, session, stack, id)) + ")\nmem.Next()"
		case len(res.Frames) > 0 && res.Frames[len(res.Frames)-1].Kind == "push":
			code = "mem.Next()"
		case len(stack) == 0:
			note("done", "", "")
			return
		default:
			t.Fatalf("stuck in %v: %s", stack, res.Output)
		}
	}
}

var childResultRe = regexp.MustCompile("\\[x\\] `([^`]+)` \"[^\"]*\" → (.+)$")

// peerResult is what todo id came to: for a leaf, its name and a hash of
// it; for a todo with todos under it, its name and a hash of what they came
// to, read from mem, so the result depends on every result under it.
func peerResult(t *testing.T, c *Client, project, session string, stack []string, id string) string {
	res, err := c.Run(context.Background(), RunRequest{Project: project, Session: session, Name: MemVM, Code: "println(mem.Show(" + strconv.Quote(id) + "))", Frames: stack})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	var parts []string
	for _, line := range strings.Split(res.Output, "\n") {
		m := childResultRe.FindStringSubmatch(strings.TrimSpace(line))
		if m != nil && strings.HasPrefix(m[1], id+"/") && !strings.Contains(m[1][len(id)+1:], "/") {
			parts = append(parts, m[2])
		}
	}
	sum := sha256.Sum256([]byte(stepName(id) + "|" + strings.Join(parts, ",")))
	return stepName(id) + ":" + hex.EncodeToString(sum[:4])
}

// peersTree seeds the demonstration's tree in project's mem, as the user,
// and returns the ids of its todos, by title, and the root's id.
func peersTree(t *testing.T, dir, project string) (map[string]string, string) {
	h := newTestHost(t, dir)
	res, err := h.Run(context.Background(), RunRequest{Project: project, Name: MemVM, Code: `r := mem.Todo("ship the geometry release")
b := r.Todo("build the shapes").Parallel()
c := b.Todo("circle").Parallel()
c.Todo("circle area")
c.Todo("circle perimeter")
s := b.Todo("square").Parallel()
s.Todo("square area")
s.Todo("square perimeter")
g := b.Todo("triangle").Parallel()
g.Todo("triangle area")
g.Todo("triangle perimeter")
r.Todo("write the release notes")`})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	require.NoError(t, h.Close())
	ids := map[string]string{}
	for _, m := range regexp.MustCompile("added `([^`]+)` \"([^\"]+)\"").FindAllStringSubmatch(res.Output, -1) {
		ids[m[2]] = m[1]
	}
	require.Len(t, ids, 12, res.Output)
	return ids, ids["ship the geometry release"]
}

// peersRun starts peers, one process each, on a fresh copy of the tree,
// waits for them all, and returns how long it took, their events and the
// journal.
type peersOutcome struct {
	start   time.Time
	wall    time.Duration
	events  []peerEvent
	entries []memEntry
	ids     map[string]string
}

func peersRun(t *testing.T, roles []string) peersOutcome {
	dir := t.TempDir()
	project := filepath.Join(dir, "proj")
	require.NoError(t, os.MkdirAll(project, 0o700))
	ids, root := peersTree(t, dir, project)
	start := time.Now()
	var cmds []*exec.Cmd
	var logs []string
	for i, role := range roles {
		log := filepath.Join(dir, fmt.Sprintf("peer-%d.log", i))
		logs = append(logs, log)
		cmd := exec.Command(os.Args[0], "-test.run=^TestPeerChild$", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), peerEnv+"=1", "GNOSH_PEER_DIR="+dir, "GNOSH_PEER_PROJECT="+project,
			fmt.Sprintf("GNOSH_PEER_SESSION=peer%d", i+1), "GNOSH_PEER_ROLE="+role, "GNOSH_PEER_ROOT="+root, "GNOSH_PEER_LOG="+log)
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		require.NoError(t, cmd.Start())
		cmds = append(cmds, cmd)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("peer%d:\n%s", i+1, out.String())
			}
		})
	}
	for _, cmd := range cmds {
		require.NoError(t, cmd.Wait())
	}
	o := peersOutcome{wall: time.Since(start), ids: ids, start: start}
	for _, log := range logs {
		f, err := os.Open(log)
		require.NoError(t, err)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e peerEvent
			require.NoError(t, json.Unmarshal(sc.Bytes(), &e))
			o.events = append(o.events, e)
		}
		f.Close()
	}
	slices.SortStableFunc(o.events, func(a, b peerEvent) int { return int(a.At - b.At) })
	_, entries, _, err := readMemJournal(filepath.Join(memDir(dir, project), "journal.jsonl"))
	require.NoError(t, err)
	o.entries = entries
	return o
}

// results is what each todo came to, by id, from the journal's pops; and
// checks the journal: each todo pushed and popped once, by one session, a
// todo popped only after every todo under it, the notes started only after
// the build was done.
func (o peersOutcome) results(t *testing.T) map[string]string {
	t.Helper()
	pushedBy, popAt, results := map[string]string{}, map[string]int{}, map[string]string{}
	for i, e := range o.entries {
		for _, f := range e.Frames {
			switch f.Kind {
			case "push":
				require.Empty(t, pushedBy[f.Id], "`%s` entered twice", f.Id)
				pushedBy[f.Id] = f.Session
				if strings.HasSuffix(f.Id, "notes") {
					build := o.ids["build the shapes"]
					_, done := popAt[build]
					require.True(t, done, "the notes started before the build was done")
				}
			case "pop":
				require.Equal(t, pushedBy[f.Id], f.Session, "`%s` popped by another session than entered it", f.Id)
				_, again := popAt[f.Id]
				require.False(t, again, "`%s` popped twice", f.Id)
				popAt[f.Id] = i
				results[f.Id] = f.Result
			}
		}
	}
	for _, id := range o.ids {
		_, ok := popAt[id]
		require.True(t, ok, "`%s` was never done", id)
		for child, cat := range popAt {
			if strings.HasPrefix(child, id+"/") {
				require.Less(t, cat, popAt[id]+1, "`%s` popped before `%s` under it", id, child)
			}
		}
	}
	return results
}

// bounds are the critical path and the total work of the tree.
func (o peersOutcome) bounds() (critical, total time.Duration) {
	var longest func(id string) time.Duration
	longest = func(id string) time.Duration {
		var below time.Duration
		for _, c := range o.ids {
			if strings.HasPrefix(c, id+"/") && !strings.Contains(c[len(id)+1:], "/") {
				below = max(below, longest(c))
			}
		}
		return below + peerWork(id)
	}
	for _, id := range o.ids {
		total += peerWork(id)
	}
	root := o.ids["ship the geometry release"]
	// The task is ordered: the build, then the notes, then its own work.
	critical = longest(o.ids["build the shapes"]) + peerWork(o.ids["write the release notes"]) + peerWork(root)
	return critical, total
}

// simulate is what n root peers would take on the tree, by the rule they
// follow (the pick at the root, PARALLEL.md): each free peer takes the first
// todo in outline order that it can do now, a leaf, or a todo whose todos
// are all done; in an ordered todo, a todo being worked on, at or under it,
// holds up the ones after it; the task itself last. Each todo takes its work
// and cost, the mem calls a todo needs, measured; every peer starts at
// start. It is the bar the real run is held to: the rule's own schedule,
// not an ideal one.
func (o peersOutcome) simulate(n int, start, cost time.Duration) time.Duration {
	root := o.ids["ship the geometry release"]
	ordered := map[string]bool{root: true}
	var children func(id string) []string
	children = func(id string) []string {
		var out []string
		for _, title := range []string{"build the shapes", "circle", "circle area", "circle perimeter", "square", "square area", "square perimeter", "triangle", "triangle area", "triangle perimeter", "write the release notes"} {
			c := o.ids[title]
			if strings.HasPrefix(c, id+"/") && !strings.Contains(c[len(id)+1:], "/") {
				out = append(out, c)
			}
		}
		return out
	}
	done, held := map[string]bool{}, map[string]bool{}
	var worked func(id string) bool
	worked = func(id string) bool {
		if held[id] {
			return true
		}
		for _, c := range children(id) {
			if !done[c] && worked(c) {
				return true
			}
		}
		return false
	}
	ready := func(id string) bool {
		for _, c := range children(id) {
			if !done[c] {
				return false
			}
		}
		return true
	}
	var pick func(id string) string
	pick = func(id string) string {
		for _, c := range children(id) {
			switch {
			case done[c]:
			case held[c]:
				if ordered[id] {
					return ""
				}
			case ready(c):
				return c
			default:
				if w := pick(c); w != "" {
					return w
				}
				if ordered[id] && worked(c) {
					return ""
				}
			}
		}
		return ""
	}
	type busy struct {
		id    string
		until time.Duration
	}
	peers := make([]*busy, n)
	now := start
	for !done[root] {
		for i := range peers {
			if peers[i] != nil {
				continue
			}
			id := pick(root)
			if id == "" && ready(root) && !held[root] {
				id = root
			}
			if id != "" {
				held[id] = true
				peers[i] = &busy{id: id, until: now + peerWork(id) + cost}
			}
		}
		next := time.Duration(-1)
		for _, b := range peers {
			if b != nil && (next < 0 || b.until < next) {
				next = b.until
			}
		}
		if next < 0 {
			break
		}
		now = next
		for i, b := range peers {
			if b != nil && b.until == now {
				held[b.id], done[b.id] = false, true
				peers[i] = nil
			}
		}
	}
	return now
}

// firstPush is how long after the start the first push came: a peer's start.
func (o peersOutcome) firstPush() time.Duration {
	for _, e := range o.events {
		if e.Kind == "push" {
			return time.Duration(e.At-o.start.UnixMilli()) * time.Millisecond
		}
	}
	return 0
}

// The demonstration: one peer, three and eight, on the same tree; and three
// root peers with a planner that entered the task and works inside it.
func TestPeersDemonstration(t *testing.T) {
	if os.Getenv(peerEnv) == "1" {
		t.Skip("not in a peer")
	}
	if testing.Short() {
		t.Skip("starts peer processes")
	}
	one := peersRun(t, []string{"root"})
	base := one.results(t)
	critical, total := one.bounds()
	overhead := one.wall - total
	// What the mem calls of one todo cost, beside its work: one peer's
	// overhead, less its start, over the 12 todos.
	cost := (overhead - one.firstPush()) / 12
	t.Logf("1 peer: %s (work %s, critical path %s, start %s, mem calls %s a todo)", one.wall.Round(time.Millisecond), total, critical, one.firstPush().Round(time.Millisecond), cost.Round(time.Millisecond))
	for _, roles := range [][]string{
		{"root", "root", "root"},
		{"root", "root", "root", "root", "root", "root", "root", "root"},
		{"planner", "root", "root", "root"},
	} {
		n := len(roles)
		o := peersRun(t, roles)
		require.Equal(t, base, o.results(t), "%d peers came to the same tree", n)
		// A scheduler that never idles while work is free finishes within
		// the total over n plus the critical path (Graham); the overhead
		// one peer showed, spread the same way, plus a second of slack.
		bound := total/time.Duration(n) + critical + overhead + time.Second
		speedup := float64(one.wall) / float64(o.wall)
		waits := 0
		sessions := map[string]bool{}
		for _, e := range o.events {
			if e.Kind == "wait" {
				waits++
			}
			if e.Kind == "push" {
				sessions[e.Session] = true
			}
		}
		sim := o.simulate(n, o.firstPush(), cost)
		t.Logf("%d peers %v: %s, %.2fx one peer's; their rule's own schedule %s (%.2fx); bound %s; %d waits; %d peers did work",
			n, roles, o.wall.Round(time.Millisecond), speedup, sim.Round(time.Millisecond), float64(one.wall)/float64(sim), bound.Round(time.Millisecond), waits, len(sessions))
		if os.Getenv("GNOSH_PEERS_TIMELINE") != "" {
			for _, e := range o.events {
				t.Logf("  %6dms %-7s %-4s %s %s", e.At-o.start.UnixMilli(), e.Session, e.Kind, e.Id, e.Waited)
			}
		}
		require.Less(t, o.wall, bound, "%d peers", n)
		if roles[0] == "root" {
			// The run keeps to its rule's own schedule: no needless wait,
			// no serial step but mem's own short turns.
			require.Less(t, o.wall, sim*115/100+300*time.Millisecond, "%d peers keep to their rule's schedule", n)
		}
		require.Greater(t, speedup, 1.6, "%d peers are faster than one", n)
		require.GreaterOrEqual(t, len(sessions), min(n, 3), "the work was shared")
	}
}

// A waiting Next does nothing while nothing changes: a holder that works 3 s
// without a change causes no re-run, and its pop causes one. (Not parallel:
// the client is process-wide, and it counts re-runs.)
func TestPeersQuietHolder(t *testing.T) {
	if os.Getenv(peerEnv) == "1" {
		t.Skip("not in a peer")
	}
	dir := t.TempDir()
	other := newTestHost(t, dir)
	ctx := context.Background()
	res, err := other.Run(ctx, RunRequest{Project: "/q", Name: MemVM, Code: "r := mem.Todo(\"release\")\nr.Todo(\"bump\")\nr.Todo(\"tag\")"})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	defer holdTurn(ctx, dir, "h")()
	res, err = other.Run(ctx, RunRequest{Project: "/q", Session: "h", Name: MemVM, Code: `mem.Next()`})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)

	c, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	defer c.Release()
	defer holdTurn(ctx, dir, "w")()
	var reruns atomic.Int64
	waitRerun = func() { reruns.Add(1) }
	defer func() { waitRerun = nil }()
	done := make(chan *RunResult, 1)
	go func() {
		res, err := c.Run(ctx, RunRequest{Project: "/q", Session: "w", Name: MemVM, Code: `mem.Next()`, Req: "w-1"})
		require.NoError(t, err)
		done <- res
	}()
	time.Sleep(3 * time.Second)
	require.Zero(t, reruns.Load(), "nothing changed: no re-run")
	res, err = other.Run(ctx, RunRequest{Project: "/q", Session: "h", Name: MemVM, Code: `mem.Pop("bumped")`, Frames: []string{"release/bump"}})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pop did not wake the wait")
	}
	require.Equal(t, int64(1), reruns.Load(), "one change, one re-run")
	require.Equal(t, []string{"release/tag"}, stackAfter(nil, res.Frames), res.Output)
}

// A holder killed while it works: the kernel lets go of its turn lock, and
// the Next waiting on it wakes at once and goes on without it.
func TestPeersKilledHolder(t *testing.T) {
	if os.Getenv(peerEnv) == "1" {
		t.Skip("not in a peer")
	}
	if testing.Short() {
		t.Skip("starts a peer process")
	}
	dir := t.TempDir()
	project := filepath.Join(dir, "proj")
	require.NoError(t, os.MkdirAll(project, 0o700))
	h := newTestHost(t, dir)
	res, err := h.Run(context.Background(), RunRequest{Project: project, Name: MemVM, Code: "r := mem.Todo(\"release\")\nr.Todo(\"bump\")\nr.Todo(\"tag\")"})
	require.NoError(t, err)
	require.NoError(t, res.Err, res.Output)
	log := filepath.Join(dir, "holder.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPeerChild$", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), peerEnv+"=1", "GNOSH_PEER_DIR="+dir, "GNOSH_PEER_PROJECT="+project, "GNOSH_PEER_SESSION=holder", "GNOSH_PEER_ROLE=hold", "GNOSH_PEER_LOG="+log)
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	require.Eventually(t, func() bool { b, _ := os.ReadFile(log); return strings.Contains(string(b), `"push"`) }, 30*time.Second, 50*time.Millisecond)

	c, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	defer c.Release()
	ctx := context.Background()
	defer holdTurn(ctx, dir, "w")()
	done := make(chan *RunResult, 1)
	go func() {
		res, err := c.Run(ctx, RunRequest{Project: project, Session: "w", Name: MemVM, Code: `mem.Next()`, Req: "w-1"})
		require.NoError(t, err)
		done <- res
	}()
	time.Sleep(time.Second)
	require.Empty(t, done, "it waits for the working holder")
	killed := time.Now()
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder's death did not wake the wait")
	}
	require.Less(t, time.Since(killed), 2*time.Second)
	require.Contains(t, res.Output, "waited ")
	require.Equal(t, []string{"release/tag"}, stackAfter(nil, res.Frames), "its todo is passed over, as one held by a session not working: %s", res.Output)
}
