package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Every call that changes a plan is one transaction. gnosh sends one
// input's shared calls as one transaction holding several calls, which the
// chain runs in order, all or nothing.

// NewPlan makes plan id with its root todo "t" titled title, worked by its
// owner and members, or by anyone if open; a hold lasts lease seconds
// unless renewed.
func NewPlan(cur realm, id, title string, open bool, lease int) string {
	prev := cur.Previous()
	if !prev.IsUser() {
		panic("only a user may make a plan")
	}
	if !validName(id, 40) {
		panic("a plan id is 1 to 40 of a-z, 0-9 and -")
	}
	if plans.Has(id) {
		panic("plan " + id + " exists")
	}
	if lease < minLease || lease > maxLease {
		panic("a lease is " + strconv.Itoa(minLease) + " to " + strconv.Itoa(maxLease) + " seconds")
	}
	p := &Plan{ID: id, Owner: prev.Address(), Open: open, Lease: int64(lease)}
	root := &Todo{ID: "t", Title: cleanText("the title", title, maxTitle)}
	p.Todos.Set(root.ID, root)
	p.Count, p.OpenN = 1, 1
	plans.Set(id, p)
	touched, served = nil, 0
	p.emit("plan", root.ID, "owner", p.Owner.String(), "open", strconv.FormatBool(open), "lease", strconv.Itoa(lease), "title", root.Title)
	p.update(root)
	return id
}

// AddMember lets addr work plan; only its owner may.
func AddMember(cur realm, plan, addr string) {
	p := getPlan(plan)
	p.mustOwn(cur)
	a := address(addr)
	if !a.IsValid() {
		panic("not an address: " + addr)
	}
	p.Members.Set(a.String(), true)
	p.emit("member", "", "address", a.String())
}

// AddTodos adds todos under parent, one for each line of names and of
// titles, in order: names are the ones gnosh made (a-z, 0-9, -), so later
// calls of the same transaction can name them; a name in use is refused.
// It returns their ids, one per line.
func AddTodos(cur realm, plan, label, parent, names, titles string) string {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	pt := p.todo(parent)
	if pt.Status != "" {
		panic(parent + " is " + pt.Status + "; reopen it first")
	}
	if depthOf(parent) >= maxDepth {
		panic("todos go at most " + strconv.Itoa(maxDepth) + " levels deep")
	}
	ns := strings.Split(strings.TrimSpace(names), "\n")
	ts := strings.Split(strings.TrimSpace(titles), "\n")
	if len(ns) != len(ts) {
		panic("give as many names as titles, one per line")
	}
	if len(ns) > maxAdd {
		panic("add at most " + strconv.Itoa(maxAdd) + " todos in one call")
	}
	if len(pt.Children)+len(ns) > maxChildren {
		panic("a todo holds at most " + strconv.Itoa(maxChildren) + " todos")
	}
	var ids []string
	for i, name := range ns {
		if !validName(name, maxName) {
			panic("a todo name is 1 to " + strconv.Itoa(maxName) + " of a-z, 0-9 and -: " + strconv.Quote(name))
		}
		id := pt.ID + "/" + name
		if p.Todos.Has(id) {
			panic("a todo named " + id + " exists")
		}
		t := &Todo{ID: id, Parent: pt.ID, Index: len(pt.Children), Title: cleanText("a title", ts[i], maxTitle)}
		p.Todos.Set(id, t)
		pt.Children = append(pt.Children, id)
		pt.OpenKids++
		p.Count++
		p.OpenN++
		p.emit("added", id, "parent", pt.ID, "title", t.Title)
		ids = append(ids, id)
	}
	if !pt.Parallel {
		p.advance(pt)
	}
	for _, id := range ids {
		p.update(p.todo(id))
	}
	p.update(pt)
	p.end()
	return strings.Join(ids, "\n")
}

// Parallel lets id's todos be done at once, by anyone. Mark a todo parallel
// before adding many todos under it (at most maxParallel open ones).
func Parallel(cur realm, plan, label, id string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	t := p.todo(id)
	if t.Parallel {
		return
	}
	if t.OpenKids > maxParallel {
		panic(id + " has " + strconv.Itoa(t.OpenKids) + " open todos; mark a todo parallel before adding more than " + strconv.Itoa(maxParallel))
	}
	t.Parallel = true
	p.emit("parallel", id)
	for _, c := range t.Children {
		p.update(p.todo(c))
	}
	p.update(t)
	p.end()
}

// Next gives the caller the next free todo in frame (a todo id, or "" for
// the whole plan), held for a lease: "held\t<id>\t<token>\t<title>". When
// nothing is free but something in frame is still held, the caller waits
// in frame's queue: "waiting\t<frame>"; a todo it gets later comes as an
// "assigned" event. When the frame the caller holds has nothing open under
// it, it is the caller's to pop: "pop\t<frame>". With nothing open in
// frame: "nothing\t<frame>".
func Next(cur realm, plan, label, frame string) string {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	p.mayHoldMore(holder)
	f := p.root()
	if frame != "" {
		f = p.todo(frame)
	}
	out := ""
	if t := p.walk(f); t != nil {
		p.unwait(holder, frame)
		p.take(t, holder, "held")
		out = "held\t" + t.ID + "\t" + strconv.FormatInt(t.Token, 10) + "\t" + t.Title
	} else if f.Status == "" && f.OpenKids == 0 && f.Holder == holder {
		p.unwait(holder, frame)
		out = "pop\t" + frame
	} else if f.Status != "" || (frame == "" && p.OpenN == 0) {
		p.unwait(holder, frame)
		out = "nothing\t" + frame
	} else {
		p.wait(holder, frame)
		out = "waiting\t" + frame
	}
	p.end()
	return out
}

// Take holds the named todo, open and held by nobody (gnosh's
// mem.Frame(id).Push() on a shared plan): "held\t<id>\t<token>\t<title>".
func Take(cur realm, plan, label, id string) string {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	t := p.todo(id)
	if t.Status != "" {
		panic(id + " is " + t.Status)
	}
	if t.Holder != "" {
		panic(id + " is held by " + t.Holder)
	}
	p.take(t, holder, "held")
	p.end()
	return "held\t" + t.ID + "\t" + strconv.FormatInt(t.Token, 10) + "\t" + t.Title
}

// Pop leaves id done with result: only its holder, naming the token of its
// hold, so a hold that lapsed and was taken again cannot be popped. The
// result goes out in the event; its sha256 stays.
func Pop(cur realm, plan, label, id string, token int, result string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	t := p.todo(id)
	p.mustHold(t, holder, token)
	if t.OpenKids > 0 {
		panic(id + " has open todos")
	}
	r := cleanText("the result", result, maxResult)
	sum := sha256.Sum256([]byte(r))
	t.Status = "done"
	t.Result = hex.EncodeToString(sum[:])
	p.emit("done", id, "holder", holder, "result", r)
	p.closed(t)
	p.end()
}

// Drop closes id as not to be done, with why, and its open todos with it
// (at most maxDropUnder): the plan's owner, or the holder of id.
func Drop(cur realm, plan, label, id, why string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	t := p.todo(id)
	if t.Status != "" {
		panic(id + " is " + t.Status + " already")
	}
	if addressOf(holder) != p.Owner.String() && t.Holder != holder {
		panic("only the plan's owner or the holder of " + id + " may drop it")
	}
	w := cleanText("why", why, maxResult)
	var under []string
	p.Todos.Iterate(id+"/", id+"0", func(k string, v any) bool {
		if v.(*Todo).Status == "" {
			under = append(under, k)
		}
		return len(under) > maxDropUnder
	})
	if len(under) > maxDropUnder {
		panic(id + " has more than " + strconv.Itoa(maxDropUnder) + " open todos: drop some of them first")
	}
	for i := len(under) - 1; i >= 0; i-- { // the deepest first
		u := p.todo(under[i])
		u.Status = "dropped"
		p.emit("dropped", u.ID, "why", "dropped with "+id)
		p.closed(u)
	}
	t.Status = "dropped"
	p.emit("dropped", id, "holder", holder, "why", w)
	p.closed(t)
	p.end()
}

// Reopen opens id again, done or dropped, with why; a closed parent
// reopens with it.
func Reopen(cur realm, plan, label, id, why string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	t := p.todo(id)
	if t.Status == "" {
		panic(id + " is open")
	}
	w := cleanText("why", why, maxResult)
	t.Status = ""
	t.Result = ""
	p.emit("reopened", id, "holder", holder, "why", w)
	p.reopened(t)
	p.end()
}

// Renew extends the caller's holds on ids (space-separated) for another
// lease, and, when waiting, its place in frame's queue: gnosh's renewer,
// for each session whose turn goes on, names the todos on its stack.
func Renew(cur realm, plan, label, ids, frame string, waiting bool) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	for _, id := range fields(ids) {
		t := p.todo(id)
		if t.Holder != holder {
			panic("you do not hold " + id)
		}
		p.extend(t)
	}
	if waiting {
		p.wait(holder, frame)
	}
	p.end()
}

// Release gives up the caller's holds on ids (space-separated) and its
// place in frame's queue: a session that quits, or pressed Esc while it
// waited. Its todos go free, to a waiter first.
func Release(cur realm, plan, label, ids, frame string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	p.unwait(holder, frame)
	for _, id := range fields(ids) {
		t := p.todo(id)
		if t.Holder != holder {
			continue
		}
		p.letGo(t)
		p.emit("released", id, "holder", holder)
	}
	p.end()
}

// Reclaim takes back expired holds and serves waiters, with no other
// change: a machine's watcher sends it when a hold its sessions wait on
// has run out and no other call would come.
func Reclaim(cur realm, plan, label string) {
	holder := holderOf(cur, label)
	p := getPlan(plan)
	p.mayWork(holder)
	p.begin()
	p.end()
}

// Close ends plan: nothing more may change in it. Only its owner.
func Close(cur realm, plan string) {
	p := getPlan(plan)
	p.mustOwn(cur)
	p.Closed = true
	p.emit("closed", "")
}

func (p *Plan) mustHold(t *Todo, holder string, token int) {
	if t.Holder != holder {
		panic("you do not hold " + t.ID + ": your hold was lost (its lease ran out, or it was dropped or released)")
	}
	if t.Token != int64(token) {
		panic("your hold on " + t.ID + " was lost and taken again (token " + strconv.FormatInt(t.Token, 10) + ", not " + strconv.Itoa(token) + ")")
	}
}

func fields(s string) []string {
	out := strings.Fields(s)
	if len(out) > maxHolds {
		panic("name at most " + strconv.Itoa(maxHolds) + " todos")
	}
	return out
}

func validName(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
