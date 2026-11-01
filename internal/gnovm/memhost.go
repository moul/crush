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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
	"golang.org/x/mod/semver"
)

// memHost is a project's mem on disk. Its directory holds:
//
//	journal.jsonl     every input that changed mem: the source (memjournal.go)
//	store-<gen>/      the realm, built from the journal of generation gen
//	index-<gen>.db    its rows' text, searched by words (memindex.go)
//	vectors-<gen>.db  their vectors, searched by meaning (memvectors.go)
//	status.json       mem.JSON(), for hooks and the <mem> block
//	lock              held through each call, so gnosh processes take turns
//
// A call takes the lock, opens the store and brings it up to the journal,
// runs, journals what it changed, commits that to the store, and closes the
// store again: opening it costs a few milliseconds.
type memHost struct {
	b   *base
	dir string
	// now is the time a call runs at.
	now func() time.Time
	// slice is how long one call may spend bringing the store up to the
	// journal before it says to call again. What it applied is kept.
	slice time.Duration
	// unchecked is what the stamps written while the store is brought up
	// to the journal say of it (memStamp.Unchecked).
	unchecked bool
	// embed is the embedding server mem.Find looks by meaning with; nil
	// looks by words alone.
	embed *memEmbedder
	// data is gnosh's gnovm data directory, where sessions' turn locks are
	// (mempeers.go); "" when there is none, as in some tests.
	data string
	// working, in tests, stands in for the turn locks and peers records
	// (workingFor).
	working func(session, id string) bool
}

func newMemHost(b *base, dir string) *memHost {
	return &memHost{b: b, dir: dir, now: time.Now, slice: memSlice}
}

// memSlice is how long one call may spend catching mem's store up with its
// journal before it lets go of the lock and is called again. (A variable
// only so tests can shorten it.)
var memSlice = 10 * time.Second

// memBatchOps is about how many Ops one transaction applies when the store
// catches up with the journal. (A variable only so tests can shorten it.)
var memBatchOps = 1000

// memRequest is one input to mem.
type memRequest struct {
	Code string
	// Session is the session the input comes from; "" for none.
	Session string
	// User is set when the user typed it (/mem), not the agent.
	User bool
	// Req is the request's id.
	Req string
	// Goal is the goal the user's /goal binds the session to.
	Goal string
	// Frames is the session's stack of frames, bottom to top.
	Frames []string
	// Names are the names gnosh made for the texts the input keeps without
	// naming them, by text (RunRequest.Names).
	Names map[string]string
	// Pipe is the wake pipe the client made for this input, pending in
	// mem's waiters/; SubAgent is set for a sub-agent's session, which never
	// waits (PARALLEL.md). An input whose last statement is a bare
	// mem.Next() may then wait for working sessions. MayWait, in tests, lets
	// it wait with no pipe.
	Pipe     string
	SubAgent bool
	MayWait  bool
	// Run is set for the agent's own gnovm_run call (Host.runMem), which
	// carries the session's stack in Frames: only such a call keeps the
	// session's peers record (afterCall). gnosh's own reads for a session,
	// the goal's at a turn's end, leave the record alone.
	Run bool
}

// memResult is what an input to mem did.
type memResult struct {
	Output string
	Ops    []memOp
	// Seq is the journal entry the input was kept as; 0 for a read.
	Seq int64
	// Notes say what gnosh did besides: rebuilt the store, caught it up.
	Notes []string
	// Renamed is the goals an erasure renamed, old name to new; Erased is
	// all an erasure took away, for gnosh's own copies (MemErasure).
	Renamed map[string]string
	Erased  *MemErasure
	// Goals are the goals the input changed, as its session sees them.
	Goals []MemGoal
	// Frames are the frames the input pushed and popped, in order.
	Frames []MemFrame
	// Instance is mem's instance id, which a mem made anew does not share.
	Instance string
	// Waits is what the input's Next waits for, when it waits.
	Waits []MemWait
}

// MemGoal is a goal a mem call changed, as a session's task list shows it:
// its steps, and what is next.
// MemFrame is a push or a pop of a frame by a call to mem: the model
// entering a piece of work, or leaving it with its result. The agent keeps
// them with the call's tool result, derives a session's stack of frames
// from them, and on a pop removes from the model's context everything since
// the push (FRAMES.md).
type MemFrame struct {
	// Kind is "push" or "pop".
	Kind string `json:"kind"`
	// Id is the frame: `fix-flaky`, `fix-flaky/find-cause`.
	Id string `json:"id"`
	// Purpose is the frame's purpose, for the stack line and the sidebar.
	Purpose string `json:"purpose,omitempty"`
	// Result is a pop's result.
	Result string `json:"result,omitempty"`
	// Thoughts is how many thoughts the frame holds when it is popped.
	Thoughts int `json:"thoughts,omitempty"`
	// Session is the session that made it.
	Session string `json:"session"`
}

type MemGoal struct {
	Name   string    `json:"name"`
	Title  string    `json:"title"`
	Status string    `json:"status"`
	Next   string    `json:"next"`
	Steps  []MemStep `json:"steps"`
}

// MemStep is a step of a MemGoal.
type MemStep struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// For is "user" for a question or a request for the user; Doing is the
	// session that started the step.
	For   string `json:"for"`
	Doing string `json:"doing"`
}

// call runs one input against mem, under mem's lock, and keeps what it
// changed: first as a journal entry, which is the commit point, then in the
// store. A failed input keeps nothing.
func (h *memHost) call(ctx context.Context, q memRequest) (memResult, error) {
	if err := h.b.warmMem(); err != nil {
		return memResult{}, err
	}
	release, err := h.lock(ctx)
	if err != nil {
		return memResult{}, err
	}
	defer release()
	r, j, notes, err := h.open()
	if err != nil {
		return memResult{Notes: notes}, err
	}
	defer r.close()
	x, err := h.openIndex(j)
	if err != nil {
		return memResult{Notes: notes}, err
	}
	defer x.close()
	who, caller := "klash", "klash"
	switch {
	case q.User:
		who, caller = "user", "user"
	case q.Session != "":
		who, caller = "agent", q.Session
	}
	mayWait := q.MayWait || q.Pipe != "" && !q.SubAgent && who == "agent" && endsWithBareNext(q.Code) && h.pendingPipe(q.Pipe)
	mc := memContext{
		caller: caller, bound: q.Goal, stack: q.Frames, names: q.Names, find: h.finder(ctx, x, j), holding: h.holding(j), limit: memLimit{ctx: ctx},
		working: h.workingFor(), mayWait: mayWait,
	}
	call, err := r.call(q.Code, mc)
	res := memResult{Output: call.Output, Ops: call.Ops, Frames: call.Frames, Notes: notes, Instance: j.instance}
	if err != nil {
		return res, err
	}
	if call.batch == nil {
		h.afterCall(r, q, mc, call, &res, false)
		return res, nil
	}
	_, offset := time.Unix(call.At, 0).Zone()
	e := &memEntry{Seq: j.seq + 1, Time: call.At, Offset: offset, Req: q.Req, Who: who, Session: q.Session, Code: q.Code, Ops: call.Ops, Frames: call.Frames}
	if er, ok := erasureOf(call.Ops); ok {
		r.close()
		x.close()
		res, err := h.erase(res, j, e, er)
		if err == nil {
			h.afterErasure(q, call, &res)
		}
		return res, err
	}
	if err := j.append(e); err != nil {
		res.Ops = nil
		return res, err
	}
	res.Seq = e.Seq
	// The entry is kept: the Nexts waiting on mem wake for it (afterCall),
	// though they wait for the lock this call holds. If the store's or the
	// index's write fails, the next call catches it up from the journal;
	// the waiting Nexts are woken for it all the same, and the session's
	// peers record takes its stack, but this call does not wait.
	if err := r.commit(call, h.stamp(j)); err != nil {
		call.Waits = nil
		h.afterCall(r, q, mc, call, &res, true)
		return res, err
	}
	if err := x.apply([][]memOp{call.Ops}, h.indexStamp(j)); err != nil {
		call.Waits = nil
		h.afterCall(r, q, mc, call, &res, true)
		return res, err
	}
	h.writeStatus(r, j)
	res.Goals = h.touched(r, caller, call.Ops, mc.working)
	if hint := nextHint(r, caller, q.Goal, q.Frames, call.Frames, call.NilPush, mc.working); hint != "" {
		res.Output += hint + "\n"
	}
	h.afterCall(r, q, mc, call, &res, true)
	return res, nil
}

// nextHint is what a receipt ends with after a pop that no push followed
// in the same input, a push of nil included: what mem.NextHint says to do
// next, with the stack as it stands after the call (FRAMES.md), and who is
// working (PARALLEL.md). "" when the call ended with no pop, or the hint
// cannot be read.
func nextHint(r *memRealm, caller, bound string, stack []string, events []MemFrame, nilPush int, working func(session, id string) bool) string {
	if len(events) == 0 || events[len(events)-1].Kind != "pop" || nilPush == len(events) {
		return ""
	}
	last := events[len(events)-1]
	call, err := r.call("print(mem.NextHint("+strconv.Quote(last.Id)+"))", memContext{caller: caller, bound: bound, stack: stackAfter(stack, events), working: working})
	if err != nil {
		slog.Warn("mem's next hint could not be read", "error", err)
		return ""
	}
	return strings.TrimSpace(call.Output)
}

// touched is the goals ops changed, as caller sees them, read from the
// store r now holds them; nil when there are none, or they cannot be read.
func (h *memHost) touched(r *memRealm, caller string, ops []memOp, working func(session, id string) bool) []MemGoal {
	var names []string
	for _, op := range ops {
		if g := goalPart(op.Name); opTarget(op) == "item" && op.Kind != "forget" && g != "" && !slices.Contains(names, g) {
			names = append(names, g)
		}
	}
	if len(names) == 0 {
		return nil
	}
	args := make([]string, len(names))
	for i, n := range names {
		args[i] = strconv.Quote(n)
	}
	call, err := r.call("print(mem.JSON("+strings.Join(args, ", ")+"))", memContext{caller: caller, working: working})
	var out struct {
		Goals []MemGoal `json:"goals"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(call.Output), &out)
	}
	if err != nil {
		slog.Warn("mem's changed goals could not be read", "error", err)
		return nil
	}
	return out.Goals
}

// memStatus is status.json: mem.JSON() as gnosh computed it for no session
// after a change, with what it was computed for. Hooks read it, and so does
// the <mem> block at a session's start, without opening mem.
type memStatus struct {
	Generation string `json:"generation"`
	Seq        int64  `json:"seq"`
	// Day is the user's local date it was computed on, and At when (Unix
	// seconds): when mem last changed.
	Day string          `json:"day"`
	At  int64           `json:"at"`
	Mem json.RawMessage `json:"mem"`
}

const memStatusFile = "status.json"

// writeStatus writes status.json for the store r as it holds j. It is best
// effort: a reader of an old one sees mem as it was.
func (h *memHost) writeStatus(r *memRealm, j *journal) {
	call, err := r.call(`print(mem.JSON())`, memContext{caller: "klash"})
	if err != nil {
		slog.Warn("mem's status could not be computed", "error", err)
		return
	}
	now := h.now()
	b, err := json.Marshal(memStatus{Generation: j.generation, Seq: j.seq, Day: now.Format(time.DateOnly), At: now.Unix(), Mem: json.RawMessage(call.Output)})
	if err != nil {
		slog.Warn("mem's status could not be written", "error", err)
		return
	}
	path := filepath.Join(h.dir, memStatusFile)
	tmp := path + ".tmp"
	err = os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		slog.Warn("mem's status could not be written", "error", err)
	}
}

// erase keeps an input that redacts or forgets, e: it writes the journal
// again with what e erases gone from every entry, under a new generation,
// as the commit point; deletes the store and the index, which hold the
// text in their own files; and builds them afresh from the new journal, as
// far as the call's slice goes (the next call goes on).
func (h *memHost) erase(res memResult, j *journal, e *memEntry, er memErasure) (memResult, error) {
	_, entries, _, err := readMemJournal(j.path)
	if err != nil {
		res.Ops = nil
		return res, err
	}
	entries, gone := erase(append(entries, *e), er)
	res.Renamed, res.Erased = gone.Renamed, &gone
	// status.json may hold an old name, or the text: it goes before the
	// journal is written again, so no crash between the two leaves it.
	if err := os.Remove(filepath.Join(h.dir, memStatusFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Ops = nil
		return res, err
	}
	nj, err := rewriteMemJournal(j.path, j.instance, entries, h.now())
	if err != nil {
		res.Ops = nil
		return res, err
	}
	res.Seq = e.Seq
	res.Notes = append(res.Notes, "mem's journal was written again without what was erased; its store and index are built afresh from it")
	h.renamePeers(res.Renamed)
	h.ring()
	h.removeOtherGenerations(nj.generation)
	r, j, _, err := h.open()
	if err == nil {
		var x *memIndex
		if x, err = h.openIndex(j); err == nil {
			x.close()
			h.writeStatus(r, j)
		}
		r.close()
	}
	var busy *busyError
	if errors.As(err, &busy) {
		err = nil
	}
	return res, err
}

// afterErasure is afterCall for an input that erased, once mem is written
// again without what it erased (erase rang the waiting Nexts): the
// session's peers record takes the stack the input left, under the names
// the erasure gave, and a Next that decided to wait says it did not, for
// an erasing input does not wait.
func (h *memHost) afterErasure(q memRequest, call memCall, res *memResult) {
	if len(call.Waits) > 0 {
		res.Output += "mem.Next() did not wait, as this input also erased: call mem.Next() again\n"
	}
	if !q.Run || q.Session == "" || q.User {
		return
	}
	stack := stackAfter(q.Frames, call.Frames)
	for i, f := range stack {
		stack[i] = renamedUnder(res.Renamed, f)
	}
	_ = h.writePeer(memPeer{Session: q.Session, Stack: stack})
}

// holding answers the holding native from mem's journal: the names whose
// Ops ever held text (an item's, fact's or rule's text, or its name, as
// typed or, for a name, in a name's form), with their kinds, and whether the
// statements of a past input held it. ops are the call's own so far. It
// reads the whole journal, for a rare act.
func (h *memHost) holding(j *journal) func(text string, ops []memOp) (names, kinds []string, statements bool) {
	return func(text string, ops []memOp) (names, kinds []string, statements bool) {
		er := memErasure{secrets: []string{text}}
		seen := map[string]bool{}
		see := func(op memOp) {
			target := opTarget(op)
			name := op.Name
			if target == "statements" {
				return
			}
			held := er.holds(op.Old) || er.holds(op.New) || er.nameHolds(name)
			if held && !seen[target+" "+name] {
				seen[target+" "+name] = true
				names, kinds = append(names, name), append(kinds, target)
			}
		}
		_, err := readEntriesOf(j.path, 0, 0, func(e memEntry, _ int64, _ string) error {
			for _, op := range e.Ops {
				see(op)
			}
			statements = statements || er.holds(e.Code)
			return nil
		})
		if err != nil {
			slog.Warn("mem's journal could not be searched", "error", err)
		}
		for _, op := range ops {
			see(op)
		}
		return names, kinds, statements
	}
}

// lock takes mem's lock, waiting for as long as another gnosh process
// holds it.
func (h *memHost) lock(ctx context.Context) (func(), error) {
	return lockMemDir(ctx, h.dir)
}

// lockMemDir takes the lock of mem in dir, waiting while another holds it
// (a gnosh process's call, or its main process reading the index or
// writing vectors) until ctx is done. The wait is the kernel's: the lock
// is granted the moment its holder lets go, with no polling and no limit
// but ctx; every holder's turn is one call, which mem bounds.
func lockMemDir(ctx context.Context, dir string) (func(), error) {
	if err := mkdirSync(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	if release, err := lock.TryFile(path); err == nil {
		return release, nil
	} else if !errors.Is(err, lock.ErrContended) {
		return nil, err
	}
	// A worker's heartbeats say it waits, so the client does not count the
	// wait against the request's deadline.
	if w := lockWaiting(ctx); w != nil {
		w.Store(true)
		defer w.Store(false)
	}
	release, err := lock.File(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &busyError{msg: "stopped waiting for mem, which another gnosh process was using"}
		}
		return nil, err
	}
	return release, nil
}

// lockWaitingKey carries a request's flag for "waiting for mem's lock".
type lockWaitingKey struct{}

// withLockWaiting is ctx carrying flag, which lockMemDir sets while it
// waits for mem's lock.
func withLockWaiting(ctx context.Context, flag *atomic.Bool) context.Context {
	return context.WithValue(ctx, lockWaitingKey{}, flag)
}

func lockWaiting(ctx context.Context) *atomic.Bool {
	flag, _ := ctx.Value(lockWaitingKey{}).(*atomic.Bool)
	return flag
}

// newerGno reports whether gno version v, a store's, is newer than this
// build's; versions that are not semver are taken as older.
func newerGno(v string) bool {
	mine := stdlibVersion()
	return semver.IsValid(v) && semver.IsValid(mine) && semver.Compare(v, mine) > 0
}

func (h *memHost) stamp(j *journal) memStamp {
	return memStamp{Layout: memLayout, Gno: stdlibVersion(), Generation: j.generation, Seq: j.seq, End: j.size, Hash: j.lastHash, Unchecked: h.unchecked}
}

// open opens mem's store, under the lock, and brings it up to the journal,
// reading only the journal's header and the entries the store lacks. A
// store that is missing, of another layout, gno version or generation, or
// not built from this journal, is built afresh from the whole journal.
// Stores of other generations are deleted.
func (h *memHost) open() (r *memRealm, j *journal, notes []string, err error) {
	path := filepath.Join(h.dir, "journal.jsonl")
	hdr, line, err := readJournalHeader(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = createMemJournal(path, h.now()); err != nil {
			return nil, nil, nil, err
		}
		hdr, line, err = readJournalHeader(path)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mem's journal: %w", err)
	}
	if hdr.Kind != memJournalKind {
		return nil, nil, nil, fmt.Errorf("mem's journal: %w: a %q journal", errBrokenJournal, hdr.Kind)
	}
	dir := filepath.Join(h.dir, "store-"+hdr.Generation)
	_, statErr := os.Stat(dir)
	if r, err = h.openStore(dir); err != nil {
		return nil, nil, nil, err
	}
	st, ok, err := r.stamp()
	// A store there already with no stamp holds no entry, only mem's realm
	// as some build deployed it, of a layout nothing says: it is made anew.
	// An empty mem's never got a stamp, so a later layout's code read an
	// earlier Mem, and every call failed ("index out of range [11] with
	// length 10" in Jae's lane, 2026-10-30).
	if err == nil && !ok && statErr == nil {
		r.close()
		if err = os.RemoveAll(dir); err == nil {
			r, err = h.openStore(dir)
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if err == nil && ok {
		if st.Layout > memLayout {
			r.close()
			return nil, nil, nil, &refusal{"a newer gnosh wrote mem (layout " + strconv.Itoa(st.Layout) + "); restart gnosh to use it"}
		}
		// Two builds on one mem: the newer gno rebuilds an older one's
		// store; the older refuses the newer's, as for a layout, rather
		// than each rebuilding the store over the other's, call after call.
		if newerGno(st.Gno) {
			r.close()
			return nil, nil, nil, &refusal{"a newer gnosh wrote mem (gno " + st.Gno + "); restart gnosh to use it"}
		}
		same := st.Layout == memLayout && st.Gno == stdlibVersion() && st.Generation == hdr.Generation
		if same {
			same, err = lineEndsAt(path, st.End, st.Hash)
		}
		if err == nil && !same {
			r.close()
			if err = os.RemoveAll(dir); err == nil {
				r, err = h.openStore(dir)
			}
			if err != nil {
				return nil, nil, nil, err
			}
			ok = false
			notes = append(notes, "mem's store was rebuilt from its journal")
		}
	}
	if err != nil {
		r.close()
		return nil, nil, nil, err
	}
	j = &journal{path: path, generation: hdr.Generation, gno: hdr.Gno, instance: hdr.Instance, size: int64(len(line)), lastHash: lineHash(line)}
	if ok {
		j.seq, j.size, j.lastHash = st.Seq, st.End, st.Hash
	}
	// A store built afresh, in this call or in earlier ones cut short, is
	// checked once the whole journal is in it; its stamps say so until.
	fresh := !ok || st.Unchecked
	h.unchecked = fresh
	defer func() { h.unchecked = false }()
	n, err := h.catchUp(r, j)
	switch {
	case n == 0 || len(notes) > 0:
	case n == 1:
		notes = append(notes, "mem's store caught up with 1 journal entry")
	default:
		notes = append(notes, "mem's store caught up with "+strconv.Itoa(n)+" journal entries")
	}
	if err != nil {
		r.close()
		return nil, nil, notes, err
	}
	// A store built afresh is checked, and stamped: an empty mem's too, so
	// a later layout's build knows what it holds.
	if fresh {
		if j.seq > 0 {
			if differs := h.checkRebuild(r, j); differs != "" {
				notes = append(notes, differs)
			}
		}
		h.unchecked = false
		if err := r.setStamp(h.stamp(j)); err != nil {
			r.close()
			return nil, nil, notes, err
		}
	}
	h.removeOtherGenerations(hdr.Generation)
	return r, j, notes, nil
}

// checkRebuild checks a store just built afresh from the whole journal
// (MEMPLAN §5): mem's own Check, and its counts against what mem showed
// before, status.json, when that was written at the same entry. The
// journal is the source, so the store stands; what differs is said, and
// "" when nothing does.
func (h *memHost) checkRebuild(r *memRealm, j *journal) string {
	var differs []string
	if call, err := r.call(`print(mem.Check())`, memContext{caller: "klash"}); err != nil {
		differs = append(differs, "its check failed: "+err.Error())
	} else if call.Output != "" {
		differs = append(differs, "its check found "+strconv.Quote(strings.TrimSpace(call.Output)))
	}
	if st, ok := readMemStatus(h.dir); ok && st.Generation == j.generation && st.Seq == j.seq {
		var before, after struct {
			Counts map[string]int `json:"counts"`
		}
		call, err := r.call(`print(mem.JSON())`, memContext{caller: "klash"})
		if err == nil && json.Unmarshal(st.Mem, &before) == nil && json.Unmarshal([]byte(call.Output), &after) == nil {
			var names []string
			for k := range before.Counts {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if was, now := before.Counts[k], after.Counts[k]; was != now {
					differs = append(differs, fmt.Sprintf("%s %d, where mem showed %d", k, now, was))
				}
			}
		}
	}
	if len(differs) == 0 {
		return ""
	}
	slog.Warn("mem's store, built afresh from its journal, differs from what mem showed before", "differs", differs)
	return "mem's store was built afresh from its journal, the source, and differs from what mem showed before: " + strings.Join(differs, "; ")
}

func (h *memHost) openStore(dir string) (*memRealm, error) {
	if err := mkdirSync(dir); err != nil {
		return nil, err
	}
	r, err := openMemRealm(h.b, dir)
	if err != nil {
		return nil, err
	}
	r.clock = h.now
	return r, nil
}

// errCallAgain stops a catch-up whose slice is spent.
var errCallAgain = errors.New("call again")

// readEntry is a journal entry read, with where its line ends and its hash:
// what a store holding it is stamped with.
type readEntry struct {
	memEntry
	end  int64
	hash string
}

// catchUp applies the journal's entries after j's end to r, many to a
// transaction, committing each batch with its stamp, so a catch-up cut
// short at any point resumes from the last batch kept. It stops when its
// slice is spent with entries left, saying to call again; it cuts a torn
// last line, the end of an append that did not finish. j ends where the
// store does. It returns how many entries it applied.
func (h *memHost) catchUp(r *memRealm, j *journal) (int, error) {
	deadline := time.Now().Add(h.slice)
	from := j.seq
	var batch []readEntry
	var ops []memOp
	var ats []int64
	applied := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		call, err := r.apply(ops, ats)
		if err == nil {
			err = r.commit(call, h.stampAt(j, batch[len(batch)-1]))
		} else {
			// Find the entry that does not fit, keeping those before it.
			err = h.applyEach(r, j, batch)
		}
		if err != nil {
			return err
		}
		e := batch[len(batch)-1]
		j.seq, j.size, j.lastHash = e.Seq, e.end, e.hash
		applied += len(batch)
		batch, ops, ats = batch[:0], ops[:0], ats[:0]
		return nil
	}
	spent := false
	torn, err := readEntriesOf(j.path, j.size, j.seq, func(e memEntry, end int64, hash string) error {
		// The slice is spent and an entry is left: the next call goes on.
		// A catch-up whose last batch outran the slice is done all the same.
		if spent {
			return errCallAgain
		}
		batch = append(batch, readEntry{e, end, hash})
		for _, op := range e.Ops {
			ops = append(ops, op)
			ats = append(ats, e.Time)
		}
		if len(ops) < memBatchOps {
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		spent = time.Now().After(deadline)
		return nil
	})
	if errors.Is(err, errCallAgain) {
		return applied, &busyError{msg: fmt.Sprintf("mem's store is catching up with its journal (%d entries after entry %d applied); call again", applied, from), again: true}
	}
	if err == nil {
		err = flush()
	}
	if err != nil {
		return applied, err
	}
	if torn {
		if err := j.cutTail(); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// stampAt is the stamp of a store holding the journal up to entry e.
func (h *memHost) stampAt(j *journal, e readEntry) memStamp {
	st := h.stamp(j)
	st.Seq, st.End, st.Hash = e.Seq, e.end, e.hash
	return st
}

// applyEach applies a batch's entries one at a time, keeping each, and
// fails at the first that does not fit, naming it.
func (h *memHost) applyEach(r *memRealm, j *journal, batch []readEntry) error {
	for _, e := range batch {
		ats := make([]int64, len(e.Ops))
		for i := range ats {
			ats[i] = e.Time
		}
		call, err := r.apply(e.Ops, ats)
		if err != nil {
			return fmt.Errorf("mem's journal entry %d does not apply to its store: %v; /mem repair rebuilds it", e.Seq, err)
		}
		if err := r.commit(call, h.stampAt(j, e)); err != nil {
			return err
		}
		j.seq, j.size, j.lastHash = e.Seq, e.end, e.hash
	}
	return nil
}

// removeOtherGenerations deletes the stores, indexes and vectors of
// generations other than gen (of every generation, for gen ""): what an
// erasure or a repair left behind.
func (h *memHost) removeOtherGenerations(gen string) {
	ds, _ := os.ReadDir(h.dir)
	for _, d := range ds {
		name := d.Name()
		other := strings.HasPrefix(name, "store-") && (gen == "" || name != "store-"+gen) ||
			strings.HasPrefix(name, "index-") && (gen == "" || !strings.HasPrefix(name, "index-"+gen+".db")) ||
			strings.HasPrefix(name, "vectors-") && (gen == "" || !strings.HasPrefix(name, "vectors-"+gen+".db"))
		if other {
			_ = os.RemoveAll(filepath.Join(h.dir, name))
		}
	}
}

// memIndexLayout is the layout of mem's index: a change to its schema or to
// how Ops become rows is a new number, and an index of another is rebuilt.
// 2 keeps an item's status; 3 gives thoughts rows of their own.
const memIndexLayout = 3

func (h *memHost) indexStamp(j *journal) memStamp {
	return memStamp{Layout: memIndexLayout, Generation: j.generation, Seq: j.seq, End: j.size, Hash: j.lastHash}
}

// openIndex opens the index of j's generation and brings it up to j, as
// open does the store: from the entries it lacks, or, if it is of another
// layout or not built from this journal, afresh from the whole journal.
func (h *memHost) openIndex(j *journal) (*memIndex, error) {
	path := filepath.Join(h.dir, "index-"+j.generation+".db")
	x, err := openMemIndex(path)
	if errors.Is(err, errOldIndex) {
		removeMemIndex(path)
		x, err = openMemIndex(path)
	}
	if err != nil {
		return nil, err
	}
	st, ok, err := x.stamp()
	if err == nil && ok && st.Layout > memIndexLayout {
		x.close()
		return nil, &refusal{"a newer gnosh wrote mem's index (layout " + strconv.Itoa(st.Layout) + "); restart gnosh to use it"}
	}
	if err == nil && ok {
		same := st.Layout == memIndexLayout && st.Generation == j.generation
		if same {
			same, err = lineEndsAt(j.path, st.End, st.Hash)
		}
		if err == nil && !same {
			x.close()
			removeMemIndex(path)
			if x, err = openMemIndex(path); err != nil {
				return nil, err
			}
			ok = false
		}
	}
	if err != nil {
		x.close()
		return nil, err
	}
	var off, seq int64
	if ok {
		off, seq = st.End, st.Seq
	}
	var batch [][]memOp
	var last memStamp
	n := 0
	_, err = readEntriesOf(j.path, off, seq, func(e memEntry, end int64, hash string) error {
		if e.Seq > j.seq {
			return nil // past what the store holds
		}
		batch = append(batch, e.Ops)
		n += len(e.Ops)
		last = memStamp{Layout: memIndexLayout, Generation: j.generation, Seq: e.Seq, End: end, Hash: hash}
		if n < memBatchOps {
			return nil
		}
		err := x.apply(batch, last)
		batch, n = nil, 0
		return err
	})
	if err == nil && len(batch) > 0 {
		err = x.apply(batch, last)
	}
	if err != nil {
		x.close()
		return nil, err
	}
	return x, nil
}

// MemRule is a rule a call to mem set (Was ""), changed, or removed (Text
// "").
type MemRule struct {
	Name string `json:"name"`
	Text string `json:"text,omitempty"`
	Was  string `json:"was,omitempty"`
}

// memRules is the rules ops set, changed or removed.
func memRules(ops []memOp) []MemRule {
	var out []MemRule
	for _, op := range ops {
		if op.Kind == "rule" && op.Old != op.New {
			out = append(out, MemRule{Name: op.Name, Text: op.New, Was: op.Old})
		}
	}
	return out
}

// The Host's side of mem. The tools name mem as a VM, though it is a realm
// on disk, kept by a memHost per project.

// runMem runs a gnovm_run call against the project's mem.
func (h *Host) runMem(ctx context.Context, req RunRequest) (*RunResult, error) {
	start := time.Now()
	res, err := h.mem(req.Project).call(ctx, memRequest{Code: req.Code, Session: req.Session, Req: req.Req, Goal: req.Goal, Frames: req.Frames, Names: req.Names, Pipe: req.WaitPipe, SubAgent: req.SubAgent, Run: true})
	out := &RunResult{VM: MemVM, Output: res.Output, Journaled: res.Seq > 0, Seq: res.Seq, Notes: res.Notes, Wall: time.Since(start), MemGoals: res.Goals, Erased: res.Erased, Frames: res.Frames, Waits: res.Waits}
	if res.Seq > 0 {
		out.Rules = memRules(res.Ops)
	}
	if err != nil {
		var busy *busyError
		var ref *refusal
		var perr *parseError
		if errors.As(err, &busy) || errors.As(err, &ref) || errors.As(err, &perr) {
			return nil, err
		}
		if hint := memHint(req.Code, err.Error()); hint != "" {
			out.Notes = append(out.Notes, hint)
		}
		out.Err = err
	}
	return out, nil
}

// mem is the project's mem, kept for the host's life.
func (h *Host) mem(project string) *memHost {
	dir := memDir(h.opts.Dir, project)
	if h.mems == nil {
		h.mems = map[string]*memHost{}
	}
	m := h.mems[dir]
	if m == nil {
		m = newMemHost(h.base, dir)
		m.now = h.opts.Now
		m.data = h.opts.Dir
		m.embed = newMemEmbedder(h.opts.Embeddings)
		h.mems[dir] = m
	}
	m.b = h.base
	return m
}

// memDir is where a project's mem lives under dir: keyed by the project's
// repository (memRoot), not by its working directory.
func memDir(dir, project string) string {
	return filepath.Join(dir, projectKey(memRoot(project)), "mem")
}

// memRoot is what a project's mem is keyed by: its repository's git common
// dir, so that a repository's worktrees and subdirectories share one mem;
// outside git, the project's directory. Symlinks are resolved first.
func memRoot(project string) string {
	dir, err := filepath.EvalSymlinks(project)
	if err != nil {
		dir = filepath.Clean(project)
	}
	for d := dir; ; d = filepath.Dir(d) {
		git := filepath.Join(d, ".git")
		if fi, err := os.Stat(git); err == nil {
			if fi.IsDir() {
				return git
			}
			if common := gitCommonDir(git); common != "" {
				return common
			}
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// gitCommonDir reads a worktree's .git file ("gitdir: <path>") and that git
// dir's commondir file, and returns the common dir they name; "" if they do
// not.
func gitCommonDir(gitFile string) string {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common = strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		return resolved
	}
	return filepath.Clean(common)
}

// memInfo is mem as gnovm_list shows it; ok is false while the project has
// no mem yet.
func (h *Host) memInfo(project string) (VMInfo, bool) {
	dir := memDir(h.opts.Dir, project)
	fi, err := os.Stat(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return VMInfo{}, false
	}
	info := VMInfo{Name: MemVM, Options: VMOptions{Persist: true, Description: "your memory: goals, steps, facts and the user's rules (klash/gnomem/v2)"}, JournalSize: fi.Size()}
	if st, ok := readMemStatus(dir); ok {
		info.Entries = st.Seq
	}
	return info, true
}

// kept reports whether mem kept the call with request id req.
func (h *memHost) kept(req string) (bool, error) {
	_, found, err := h.keptFrames(req)
	return found, err
}

// keptFrames is kept, with the frames the kept entry pushed and popped.
func (h *memHost) keptFrames(req string) ([]MemFrame, bool, error) {
	var frames []MemFrame
	found := false
	_, err := readEntriesOf(filepath.Join(h.dir, "journal.jsonl"), 0, 0, func(e memEntry, _ int64, _ string) error {
		if e.Req == req {
			found, frames = true, e.Frames
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return frames, found, err
}

// repair deletes mem's store, index and vectors, under its lock, so that
// the next call builds them afresh from the journal, the source, whose
// every entry it keeps. It returns how many there are.
func (h *memHost) repair(ctx context.Context) (int, error) {
	release, err := h.lock(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	h.removeOtherGenerations("")
	n := 0
	_, err = readEntriesOf(filepath.Join(h.dir, "journal.jsonl"), 0, 0, func(memEntry, int64, string) error {
		n++
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return n, err
}
