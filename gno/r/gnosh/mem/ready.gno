package mem

import (
	"strconv"
	"strings"
)

// What may be taken. Each todo keeps Avail, its children that may be
// offered now. A child is offered when it is open and either has nothing
// open under it and nobody holds it, or has an offered child of its own and
// is unheld or parallel; under a sequential todo only its first open child
// (its cursor) can be offered. When a child's standing changes, so does its
// parent's Avail, which may change the parent's own standing, and so on up
// to the root: at most maxDepth updates. Next walks down from its frame,
// taking the first offered child at each level, to a todo with nothing open
// under it: no call walks finished or held todos.

// touched is the todos that became offered in this call (the first
// maxTouched), whose frames' waiters may be served at its end; served
// counts the waiters served.
const maxTouched = 8

var (
	touched []string
	served  int
)

// begin starts a call that changes a plan: expired holds come back first.
func (p *Plan) begin() {
	touched = nil
	served = 0
	p.reclaim()
}

// end serves waiters the call's changes freed a todo for.
func (p *Plan) end() {
	p.serve()
}

func (t *Todo) cursorID() string {
	if t.Cursor < len(t.Children) {
		return t.Children[t.Cursor]
	}
	return ""
}

func (p *Plan) offerable(c *Todo) bool {
	if c.Status != "" {
		return false
	}
	if c.Parent != "" {
		parent := p.todo(c.Parent)
		if !parent.Parallel && parent.cursorID() != c.ID {
			return false
		}
	}
	if c.OpenKids == 0 {
		return c.Holder == ""
	}
	return c.Avail.Size() > 0 && (c.Holder == "" || c.Parallel)
}

// update brings c's place in its parent's Avail up to date, and so on up.
func (p *Plan) update(c *Todo) {
	for i := 0; i <= maxDepth+1 && c != nil; i++ {
		now := p.offerable(c)
		if now == c.Offered {
			return
		}
		c.Offered = now
		if now && len(touched) < maxTouched {
			touched = append(touched, c.ID)
		}
		if c.Parent == "" {
			return
		}
		parent := p.todo(c.Parent)
		k := padIndex(c.Index)
		if now {
			parent.Avail.Set(k, c.ID)
		} else {
			parent.Avail.Remove(k)
		}
		c = parent
	}
}

// advance moves a sequential todo's cursor past its closed children.
func (p *Plan) advance(t *Todo) {
	for t.Cursor < len(t.Children) && p.todo(t.Children[t.Cursor]).Status != "" {
		t.Cursor++
	}
}

// closed follows t's closing (done or dropped): its hold ends, its parent
// has one open todo fewer, a sequential parent's next child may be
// offered, and a parent with nothing left open may be taken itself.
func (p *Plan) closed(t *Todo) {
	p.OpenN--
	if t.Holder != "" {
		p.letGo(t)
	}
	p.update(t)
	if t.Parent == "" {
		return
	}
	parent := p.todo(t.Parent)
	parent.OpenKids--
	if !parent.Parallel {
		p.advance(parent)
		if next := parent.cursorID(); next != "" {
			p.update(p.todo(next))
		}
	}
	p.update(parent)
}

// reopened follows t's reopening: a closed parent reopens too, and a
// sequential parent's cursor comes back to t when t is earlier.
func (p *Plan) reopened(t *Todo) {
	p.OpenN++
	if t.Parent != "" {
		parent := p.todo(t.Parent)
		if parent.Status != "" {
			parent.Status = ""
			parent.Result = ""
			p.emit("reopened", parent.ID, "why", "a todo under it was reopened")
			p.reopened(parent)
		}
		parent.OpenKids++
		if !parent.Parallel && t.Index < parent.Cursor {
			old := parent.cursorID()
			parent.Cursor = t.Index
			if old != "" {
				p.update(p.todo(old))
			}
		}
		p.update(t)
		p.update(parent)
		return
	}
	p.update(t)
}

// walk finds, inside f (f included), a todo with nothing open under it
// that may be taken now, or nil.
func (p *Plan) walk(f *Todo) *Todo {
	node := f
	for i := 0; i <= maxDepth+1; i++ {
		if node.Status != "" {
			return nil
		}
		if node.OpenKids == 0 {
			if node.Holder == "" {
				return node
			}
			return nil
		}
		next := ""
		node.Avail.Iterate("", "", func(_ string, v any) bool {
			next = v.(string)
			return true
		})
		if next == "" {
			return nil
		}
		node = p.todo(next)
	}
	return nil
}

// --- holds ---

func expiryKey(until int64, id string) string { return pad(until) + " " + id }

func (p *Plan) heldBy(holder string) int {
	if v := p.Held.Get(holder); v != nil {
		return v.(int)
	}
	return 0
}

func (p *Plan) mayHoldMore(holder string) {
	if p.heldBy(holder) >= maxHolds {
		panic(holder + " holds " + strconv.Itoa(maxHolds) + " todos of plan " + p.ID + " already: pop, drop or release one first")
	}
}

// take gives t to holder for a lease; kind is "held" (it asked) or
// "assigned" (it waited, and this call served it).
func (p *Plan) take(t *Todo, holder, kind string) {
	p.mayHoldMore(holder)
	t.Holder = holder
	t.Until = now() + p.Lease
	t.Token = p.emit(kind, t.ID, "holder", holder, "until", strconv.FormatInt(t.Until, 10), "title", t.Title)
	p.Expiry.Set(expiryKey(t.Until, t.ID), t.ID)
	p.Held.Set(holder, p.heldBy(holder)+1)
	p.update(t)
}

// letGo ends t's hold.
func (p *Plan) letGo(t *Todo) {
	p.Expiry.Remove(expiryKey(t.Until, t.ID))
	if n := p.heldBy(t.Holder) - 1; n > 0 {
		p.Held.Set(t.Holder, n)
	} else {
		p.Held.Remove(t.Holder)
	}
	t.Holder, t.Token, t.Until = "", 0, 0
	p.update(t)
}

// extend renews t's hold for another lease.
func (p *Plan) extend(t *Todo) {
	p.Expiry.Remove(expiryKey(t.Until, t.ID))
	t.Until = now() + p.Lease
	p.Expiry.Set(expiryKey(t.Until, t.ID), t.ID)
}

// reclaim takes back up to maxReclaim holds whose lease ran out.
func (p *Plan) reclaim() {
	nowT := now()
	var ids []string
	p.Expiry.Iterate("", "", func(k string, v any) bool {
		if len(ids) >= maxReclaim {
			return true
		}
		until, _ := strconv.ParseInt(k[:12], 10, 64)
		if until > nowT {
			return true
		}
		ids = append(ids, v.(string))
		return false
	})
	for _, id := range ids {
		t := p.todo(id)
		holder := t.Holder
		p.letGo(t)
		p.emit("lapsed", id, "holder", holder)
	}
}

// --- waiters ---

func (p *Plan) queue(frame string, make bool) *Queue {
	if v := p.Waits.Get(frame); v != nil {
		return v.(*Queue)
	}
	if !make {
		return nil
	}
	q := &Queue{}
	p.Waits.Set(frame, q)
	return q
}

// wait puts holder in frame's queue, or renews its place there.
func (p *Plan) wait(holder, frame string) {
	q := p.queue(frame, true)
	until := now() + p.Lease
	if v := q.By.Get(holder); v != nil {
		w := q.Order.Get(v.(string))
		w.(*Waiter).Until = until
		return
	}
	seq := p.emit("waiting", frame, "holder", holder, "until", strconv.FormatInt(until, 10))
	k := pad(seq)
	q.Order.Set(k, &Waiter{Holder: holder, Frame: frame, Until: until})
	q.By.Set(holder, k)
}

// unwait takes holder out of frame's queue.
func (p *Plan) unwait(holder, frame string) {
	q := p.queue(frame, false)
	if q == nil {
		return
	}
	v := q.By.Get(holder)
	if v == nil {
		return
	}
	q.Order.Remove(v.(string))
	q.By.Remove(holder)
	if q.Order.Size() == 0 {
		p.Waits.Remove(frame)
	}
}

// head is the oldest waiter of frame's queue whose lease has not run out;
// expired ones met first are dropped (a few at a time).
func (p *Plan) head(frame string, nowT int64) (string, *Waiter) {
	q := p.queue(frame, false)
	if q == nil {
		return "", nil
	}
	var dead []string
	key := ""
	var found *Waiter
	q.Order.Iterate("", "", func(k string, v any) bool {
		w := v.(*Waiter)
		if w.Until < nowT {
			dead = append(dead, w.Holder)
			return len(dead) >= maxReclaim
		}
		key, found = k, w
		return true
	})
	for _, h := range dead {
		p.unwait(h, frame)
		p.emit("waitlapsed", frame, "holder", h)
	}
	return key, found
}

// framesOf is the frames that hold t: t, its ancestors, and the whole plan.
func (p *Plan) framesOf(id string) []string {
	out := []string{}
	for cur := id; cur != ""; cur = p.todo(cur).Parent {
		out = append(out, cur)
	}
	return append(out, "")
}

// serve gives the todos this call freed to the oldest waiters that can
// take them: for each freed todo, the oldest head of the queues of the
// frames holding it walks its frame and takes what it finds. At most
// maxServe a call; the rest wait for the next call (any Next, or a
// watcher's Reclaim).
func (p *Plan) serve() {
	nowT := now()
	for _, id := range touched {
		if served >= maxServe {
			return
		}
		bestKey, bestFrame := "", ""
		var best *Waiter
		for _, frame := range p.framesOf(id) {
			if k, w := p.head(frame, nowT); w != nil && (best == nil || k < bestKey) {
				best, bestKey, bestFrame = w, k, frame
			}
		}
		if best == nil || p.heldBy(best.Holder) >= maxHolds {
			continue
		}
		f := p.root()
		if bestFrame != "" {
			f = p.todo(bestFrame)
		}
		t := p.walk(f)
		if t == nil {
			continue
		}
		served++
		p.unwait(best.Holder, bestFrame)
		p.take(t, best.Holder, "assigned")
	}
}

func (p *Plan) root() *Todo { return p.todo("t") }

// --- ids and keys ---

func padIndex(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func depthOf(id string) int { return strings.Count(id, "/") }
