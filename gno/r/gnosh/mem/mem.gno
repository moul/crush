// Package mem is gnosh's shared mem on gno.land: plans whose todos many
// gnosh sessions, on any number of machines, work at once (SWARM.md).
//
// A plan is a tree of todos. Next gives the caller the next free todo
// inside the frame it stands in, held by a lease, in the same order gnosh's
// local mem picks ("first free leaf, enter-mode in a frame"). Every call is
// one transaction and the chain runs them one at a time, so two callers
// never get the same todo. When nothing is free but some todos are still
// held, the caller waits in a queue on chain, and the realm hands the next
// todo that frees up to the oldest waiter of that frame, in the call that
// frees it. A hold whose lease runs out comes back on the next call.
//
// Nothing here is private: titles, holders and results are public. The
// realm keeps no log of changes; every change goes out as an event, which
// each machine's watcher reads from the block.
package mem

import (
	"chain"
	"chain/runtime"
	"strconv"
	"strings"
	"time"

	"gno.land/p/nt/avl/v0"
)

// Bounds: everything a call walks or writes is capped, so no call's gas
// grows with a plan's size.
const (
	maxTitle     = 200  // bytes in a todo's title
	maxName      = 60   // bytes in a todo's name
	maxResult    = 4000 // bytes of a result, carried in its event (Emit caps a value at 4,096)
	maxAdd       = 64   // todos one AddTodos makes
	maxChildren  = 256  // todos under one todo
	maxDepth     = 8    // levels below a plan's root
	maxHolds     = 8    // todos one holder holds in a plan at once
	maxReclaim   = 4    // expired holds one call takes back
	maxServe     = 2    // waiters one call serves
	maxPage      = 200  // todos in one PlanPage
	maxLabel     = 64   // bytes in an agent label
	maxDropUnder = 32   // open todos one Drop closes with it
	maxParallel  = 16   // open todos under a todo when it is made parallel
	minLease     = 30   // seconds
	maxLease     = 3600 // seconds
)

// Plan is one shared plan. Its root todo is "t".
type Plan struct {
	ID      string
	Owner   address
	Open    bool     // anyone may work it; else its members only
	Members avl.Tree // address string → true
	Lease   int64    // seconds a hold lasts unless renewed
	Seq     int64    // events emitted so far
	Closed  bool
	Todos   avl.Tree // id → *Todo
	Expiry  avl.Tree // lease end (padded) + " " + id → id
	Waits   avl.Tree // frame id ("" for the whole plan) → *Queue
	Held    avl.Tree // holder → number of todos it holds
	Count   int      // todos
	OpenN   int      // todos still open
}

// Todo is one todo of a plan, and its hold if someone holds it.
type Todo struct {
	ID       string // its parent's id, "/", and its name; the root is "t"
	Title    string
	Parent   string
	Index    int      // its place among its parent's children, from 0
	Children []string // ids, in the order they were added
	Cursor   int      // a sequential todo's first open child, by Index
	Parallel bool     // its todos may be done at once, by anyone
	Status   string   // "", "done" or "dropped"
	Result   string   // sha256 of the result, hex; the text is in the event
	OpenKids int      // children still open
	Avail    avl.Tree // padded Index → id: the children that may be offered now
	Offered  bool     // it is in its parent's Avail
	Holder   string   // address|session key|label; "" when nobody holds it
	Token    int64    // the seq of the event that gave the hold; Pop names it
	Until    int64    // the hold's lease end, unix seconds of block time
}

// Queue is a frame's waiters, oldest first.
type Queue struct {
	Order avl.Tree // seq (padded) → *Waiter
	By    avl.Tree // holder → seq (padded)
}

// Waiter is a holder waiting for a todo inside a frame, until its lease
// ends unless renewed.
type Waiter struct {
	Holder string
	Frame  string
	Until  int64
}

var plans avl.Tree // id → *Plan

// holderOf is who calls: the user's address, the session key it signs with
// (none for the account's own key), and the agent's label. The label is the
// caller's word only: the address and session key authenticate; the label
// tells one account's agents apart.
func holderOf(cur realm, label string) string {
	prev := cur.Previous()
	if !prev.IsUser() {
		panic("only a user may call this, not another realm")
	}
	if len(label) == 0 || len(label) > maxLabel || strings.ContainsAny(label, "|\n") {
		panic("give an agent label of 1 to " + strconv.Itoa(maxLabel) + " bytes, without | or a newline")
	}
	session := "-"
	if key, _, _, isSession := runtime.GetSessionInfo(); isSession {
		session = key.String()
	}
	return prev.Address().String() + "|" + session + "|" + label
}

func addressOf(holder string) string {
	return strings.SplitN(holder, "|", 2)[0]
}

func getPlan(id string) *Plan {
	v := plans.Get(id)
	if v == nil {
		panic("no plan " + strconv.Quote(id))
	}
	return v.(*Plan)
}

func (p *Plan) todo(id string) *Todo {
	v := p.Todos.Get(id)
	if v == nil {
		panic("no todo " + strconv.Quote(id) + " in plan " + p.ID)
	}
	return v.(*Todo)
}

// mayWork refuses a caller who is neither the owner nor a member of a plan
// that is not open.
func (p *Plan) mayWork(holder string) {
	if p.Closed {
		panic("plan " + p.ID + " is closed")
	}
	a := addressOf(holder)
	if p.Open || a == p.Owner.String() {
		return
	}
	if !p.Members.Has(a) {
		panic(a + " is not a member of plan " + p.ID)
	}
}

func (p *Plan) mustOwn(cur realm) {
	prev := cur.Previous()
	if !prev.IsUser() || prev.Address() != p.Owner {
		panic("only the plan's owner may do this")
	}
}

// emit sends one change out as an event and returns its seq.
func (p *Plan) emit(kind, id string, attrs ...string) int64 {
	p.Seq++
	all := []string{"plan", p.ID, "seq", strconv.FormatInt(p.Seq, 10), "kind", kind, "todo", id}
	chain.Emit("mem", append(all, attrs...)...)
	return p.Seq
}

func now() int64 { return time.Now().Unix() }

// pad makes n sort as a string: 12 digits, zero-padded.
func pad(n int64) string {
	s := strconv.FormatInt(n, 10)
	for len(s) < 12 {
		s = "0" + s
	}
	return s
}

func cleanText(what, s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		panic(what + " is empty")
	}
	if len(s) > max {
		panic(what + " is longer than " + strconv.Itoa(max) + " bytes")
	}
	for _, c := range s {
		if c < 0x20 && c != '\t' {
			panic(what + " holds a control character")
		}
	}
	return s
}

// avlTree makes an empty tree (tests reset the plans with it).
func avlTree() avl.Tree { return avl.Tree{} }
