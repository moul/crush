// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

import (
	"strconv"
	"strings"
)

// Frames (FRAMES.md): the task tree seen through a stack. A Frame is a
// piece of work in mem, a todo; entering one (Push) marks
// where gnosh will cut the model's context, and leaving it with a result
// (Pop) cuts everything since. A thought is a labelled line kept on
// the frame the model stands in: it costs nothing, cuts nothing, and is
// how anything worth keeping outlives a pop. The stack itself is gnosh's,
// derived from the session's own pushes and pops; the realm is told it
// through frames(), and tells gnosh each push and pop through pushed() and
// popped().

// Frame is a piece of work in mem, seen through the stack. It is a
// snapshot, read-only; mem's calls change mem.
type Frame struct {
	Id       string    // its path of names: `fix-flaky`, `fix-flaky/find-cause`; "" for the root
	Purpose  string    // what it is for; "" for a step made from a thought with no reason given
	Result   string    // what came of it, once popped
	State    string    // "open", "doing", "waiting", "asked", "done" or "dropped": what NextTodo reads
	Doing    string    // the session working it, when State is "doing"
	Todos    []*Frame  // its todos, in order, one level
	Thoughts []Thought // its thoughts, oldest first
	m        *Mem
}

// Thought is a labelled line kept on a frame, and the step it was promoted
// into, if any. Its name is given to Think, or made from its text.
type Thought struct {
	Text  string
	Frame *Frame // the step it was promoted into; nil until then
	name  string // its label, unique among mem's thoughts
	frame string // the id of the frame it is on
	m     *Mem
}

// Fact is a fact as Recall returns it.
type Fact struct {
	Body string
	name string
}

// Namable is what a Frame, a Thought and a Fact have in common: a piece
// of memory under a name, given or made, which Recall returns. *Frame,
// Thought and Fact implement it; a type switch tells them apart.
type Namable interface {
	Name() string   // how a call names it: a frame's Id, a thought's name, a fact's name
	Kind() string   // "frame", "thought" or "fact"
	Says() string   // a frame's purpose, a thought's text, a fact's body
	String() string // how it is shown
}

// maxRecall is how many Namables Recall returns at most.
const maxRecall = 5

func (f *Frame) Name() string { return f.Id }
func (f *Frame) Kind() string { return "frame" }
func (f *Frame) Says() string { return f.Purpose }

// String shows the frame as mem.Show would: the root as the whole outline.
func (f *Frame) String() string {
	switch {
	case f == nil:
		return "no frame\n"
	case f.m == nil:
		return "frame `" + f.Id + "` " + quote(f.Purpose) + "\n"
	case f.Id == "":
		return f.m.Show()
	}
	return f.m.Show(f.Id)
}

// NextTodo is the todo mem.Next() would step into from f: its first todo,
// in the order of its Todos, whose State is "open", or "doing" by this
// session; with other sessions working, a free todo under one of theirs
// when none beside it is free (PARALLEL.md). nil when there is none, and
// at the unbound root, where a todo is entered by id or made and pushed.
func (f *Frame) NextTodo() *Frame {
	if f == nil || f.m == nil {
		return nil
	}
	return f.m.nextTodo(f.Id)
}

func (t Thought) Name() string { return t.name }
func (t Thought) Kind() string { return "thought" }
func (t Thought) Says() string { return t.Text }

func (t Thought) String() string {
	s := "thought `" + t.name + "` on `" + t.frame + "`: " + t.Text
	if t.Frame != nil {
		s += " (now the todo `" + t.Frame.Id + "`)"
	}
	return s + "\n"
}

func (f Fact) Name() string   { return f.name }
func (f Fact) Kind() string   { return "fact" }
func (f Fact) Says() string   { return f.Body }
func (f Fact) String() string { return "fact `" + f.name + "` = " + f.Body + "\n" }

// Frame is the present frame: the top of this session's stack, or, with
// the stack empty, the root: the goal the user's /goal bound the session
// to, or everything open. Given an id, it is that frame:
// mem.Frame("fix-flaky").Push() steps into a todo by id.
func (m *Mem) Frame(id ...string) *Frame {
	switch {
	case len(id) > 1:
		panic("Frame takes one id, or none for the present frame")
	case len(id) == 0:
		return m.frame(m.present())
	}
	n := id[0]
	if n == "" {
		return m.frame(bound())
	}
	if it := m.get(n); it == nil || it.Status == "forgotten" {
		panic(m.noFrame(n))
	}
	return m.frame(n)
}

// present is the frame this session is in: the top of its stack, or the
// root: the goal /goal bound it to, or "" for the unbound root.
func (m *Mem) present() string {
	if s := frames(); len(s) > 0 {
		return s[len(s)-1]
	}
	return bound()
}

// frame is frame id, with its todos one level deep and its thoughts; the
// unbound root's todos are the open top-level ones, newest first.
func (m *Mem) frame(id string) *Frame {
	f := m.shallow(id)
	if id == "" {
		m.open.ReverseIterate("", "", func(_ string, v any) bool {
			f.Todos = append(f.Todos, m.shallow(v.(string)))
			return false
		})
		return f
	}
	it := m.get(id)
	for _, s := range it.Steps {
		step := id + "/" + s
		if m.get(step).Status != "forgotten" {
			f.Todos = append(f.Todos, m.shallow(step))
		}
	}
	for _, n := range it.Notes {
		f.Thoughts = append(f.Thoughts, m.thoughtOf(id, n))
	}
	return f
}

// shallow is frame id without its todos and thoughts.
func (m *Mem) shallow(id string) *Frame {
	if id == "" {
		return &Frame{Purpose: "everything open", State: "open", m: m}
	}
	it := m.get(id)
	return &Frame{Id: id, Purpose: it.Title, Result: it.Result, State: m.state(id, it), Doing: it.Doing, m: m}
}

// thoughtOf is thought n of frame id, as a Thought.
func (m *Mem) thoughtOf(id string, n note) Thought {
	t := Thought{name: n.Name, Text: n.Text, frame: id, m: m}
	if n.Step != "" {
		if s := m.get(n.Step); s != nil && s.Status != "forgotten" {
			t.Frame = m.shallow(n.Step)
		}
	}
	return t
}

// state is item it's State: its status once closed; else "asked" for the
// user, "waiting" for a day or words (its own or an item's above it),
// "doing" for a session's, or "open".
func (m *Mem) state(id string, it *Todo) string {
	switch {
	case it.Status != "":
		return it.Status
	case it.For != "":
		return "asked"
	case m.waitsAbove(id):
		return "waiting"
	case it.Doing != "":
		return "doing"
	}
	return "open"
}

// waitsAbove reports whether item id waits, itself or through an item
// above it.
func (m *Mem) waitsAbove(id string) bool {
	for n := id; n != ""; n = parentOf(n) {
		if m.waiting(m.get(n)) {
			return true
		}
	}
	return false
}

// nextTodo is Frame.NextTodo for frame id: the pick, entering a todo with
// todos under it (peers.gno).
func (m *Mem) nextTodo(id string) *Frame {
	if id == "" || m.get(id) == nil {
		return nil
	}
	if n := m.pickFor(id, caller(), true, nil); n != "" {
		return m.shallow(n)
	}
	return nil
}

// noFrame says that id names no frame, and what it might have been.
func (m *Mem) noFrame(id string) string {
	msg := "there is no frame " + named(id)
	low := strings.ToLower(strings.TrimSpace(id))
	switch {
	case low != id && m.get(low) != nil:
		return msg + ": names are lowercase, typed as printed: `" + low + "`"
	case m.formed(id) != "":
		return msg + ": did you mean `" + m.formed(id) + "`?"
	case m.titled(id) != "":
		return msg + ": that is a task; the todo is `" + m.titled(id) + "`"
	}
	return msg + "; mem.Show() lists them"
}

// Todo adds a todo and returns it: under the frame you are in, after its
// other todos, or, at the unbound root, a top-level one. Its name is made
// from the task: gnosh's small model's label of it, or the task's words,
// with -2, -3 when that name is taken. A task an open todo beside it
// already has is refused.
func (m *Mem) Todo(task string) *Frame {
	return m.todoUnder(m.present(), task)
}

// Todo adds a todo under frame f, as mem.Todo does under the present frame:
// a plan is made without entering it, mem.Todo("…").Todo("…").
func (f *Frame) Todo(task string) *Frame {
	if f == nil || f.m == nil {
		panic("no frame to add a todo to: mem.Todo(task) makes one")
	}
	return f.m.todoUnder(f.Id, task)
}

// Wait holds the todo back until a day, or until what the words say (see
// Mem.Wait): mem.Todo("renew the TLS cert").Wait("2026-12-01").
func (f *Frame) Wait(until string) *Frame {
	if f == nil || f.m == nil || f.Id == "" {
		panic("no todo to wait: mem.Todo(task) makes one")
	}
	f.m.Wait(f.Id, until)
	return f
}

// Ask asks the user something, as a todo under f (see Mem.Ask).
func (f *Frame) Ask(question string) *Frame {
	if f == nil || f.m == nil || f.Id == "" {
		panic("no todo to ask under: mem.Todo(task) makes one, or mem.Ask(\"\", question) asks at the root")
	}
	f.m.Ask(f.Id, question)
	return f
}

// todoUnder adds a todo for task under parent ("" for the root).
func (m *Mem) todoUnder(parent, task string) *Frame {
	task = clip(text("the task", task), maxTitle)
	if task == "" {
		panic("a todo needs a task: mem.Todo(\"what to get done\")")
	}
	if validName(task) && strings.Contains(task, "-") {
		// A model that writes mem.Todo("fix-flaky") means the todo of that
		// name, as it would have given one; a nested todo named from it is
		// not what it meant.
		panic("`" + task + "` is a name, not a task: mem.Frame(" + quote(task) + ") is the todo of that name, and .Todo(\"what to get done\") on it adds under it; a task is words")
	}
	if parent == "" {
		m.refuseOpenTitle(task)
		id := m.freeName(m.madeName(task, "todo"), func(n string) bool { return m.freeTop(n) })
		m.do(Op{Kind: "add", Name: id, New: task, By: caller()})
		println("added " + ref(id, task))
		return m.frame(id)
	}
	p := m.openItem(parent)
	m.eachStep(parent, func(_ string, s *Todo) bool {
		if s.Status == "" && sameTitle(s.Title) == sameTitle(task) {
			panic(ref(parent, p.Title) + " already has an open todo " + strconv.Quote(task))
		}
		return false
	})
	id := parent + "/" + m.freeStep(parent, m.madeName(task, "todo"))
	m.addStep(parent, id, task)
	println("added " + ref(id, task))
	return m.frame(id)
}

// addStep adds step id, titled title, under open item parent.
func (m *Mem) addStep(parent, id, title string) {
	p := m.openItem(parent)
	if p.For != "" {
		panic(ref(parent, p.Title) + " is for the user: it has no todos")
	}
	if len(p.Steps) >= maxSteps {
		panic(ref(parent, p.Title) + " has " + strconv.Itoa(maxSteps) + " todos, the most an item may have")
	}
	m.do(Op{Kind: "add", Name: id, New: title, By: caller()})
}

// Next steps into the next todo, which becomes this session's, the top of
// its stack, and from where the context will be cut when it is popped:
// under the frame you stand in, the first open todo (or one this session
// is doing), as Frame().NextTodo() gives it; at the unbound root, the next
// free todo of the newest open task, so a session taking up another's work
// needs no name. When there is none it is a kept no-op that says what to do
// instead. A particular frame is entered with .Push() on it:
// mem.Todo("…").Push(), mem.Frame(id).Push(), mem.Thought("port").Push().
//
// With other sessions working on the plan, a Next that has nothing for you
// now waits for them when it can (only a bare mem.Next() ending the input),
// when you would have a todo, or could pop the frame you stand in, once
// they finish: it prints nothing, and gnosh runs it again when mem changes
// (PARALLEL.md).
func (m *Mem) Next() {
	p := m.present()
	if p == "" {
		if n := m.nextAtRoot(); n != "" {
			m.push(n, "")
			return
		}
	} else if f := m.nextTodo(p); f != nil {
		m.push(f.Id, "")
		return
	}
	if mayWait() {
		anchor, enter := p, true
		if p == "" {
			anchor, enter = m.newestOpen(), false
		}
		if anchor != "" && (p == "" || m.get(p).OpenSteps > 0) {
			if held := m.waitsFor(anchor, caller(), enter); len(held) > 0 {
				for _, n := range held {
					waitOn(n, m.get(n).Doing)
				}
				pushed("", "")
				return
			}
		}
	}
	pushed("", "")
	println(m.nothingNext())
}

// nextAtRoot is the todo Next steps into from the unbound root: the first
// todo this session can do now under the newest open task, in outline
// order, or that task itself when it is free and has no open todo; "" when
// there is none.
func (m *Mem) nextAtRoot() string {
	newest := m.newestOpen()
	if newest == "" {
		return ""
	}
	g := m.get(newest)
	if n := m.next(newest, caller()); n != "" {
		return n
	}
	if g.OpenSteps == 0 && m.free(g, caller()) {
		return newest
	}
	return ""
}

// Push steps into the frame: it becomes this session's, the top of its
// stack, and from here the context will be cut when it is popped.
func (f *Frame) Push() {
	if f == nil || f.m == nil {
		panic("no frame to push: mem.Frame(id) or mem.Todo(task) gives one")
	}
	f.m.push(f.Id, "")
}

// push enters frame id. thought is the text of the thought the frame was
// made from, when the push follows its making.
func (m *Mem) push(id, thought string) {
	stack := frames()
	switch {
	case id == "":
		panic("the root is not a frame to enter: push a todo, or make one: mem.Todo(\"…\").Push()")
	case id == bound():
		panic("you are in `" + id + "` already: it is this session's /goal; push its todos")
	}
	it := m.openItem(id)
	if it.For != "" {
		panic(ref(id, it.Title) + " is asked of the user: end your turn so they can answer, or mem.Drop(\"" + id + "\", \"why\")")
	}
	m.refuseWait(id)
	me := caller()
	switch {
	case it.Doing != "" && it.Doing != me:
		panic(m.heldText(id, it))
	case onStack(stack, id):
		panic("`" + id + "` is already on your stack")
	}
	if s := deepestUnder(stack, id); s != "" {
		panic("`" + id + "` cannot go above its own todo `" + s + "`, which is on your stack: pop or drop that first")
	}
	if it.Doing != me {
		m.do(Op{Kind: "set", Name: id, Field: "doing", Old: it.Doing, New: me})
	}
	pushed(id, it.Title)
	println(pushText(id, it.Title, thought))
	if others := m.doneByOthers(id, me); others != "" {
		print(others)
	}
	if w := m.workers(taskOf(id), me); w != "" && !subAgent() {
		println(w + oneOrMany(w, " also works", " also work") + " on this plan now: keep to this todo's files, and take the next todo with mem.Next().")
	}
}

// oneOrMany is one or many, for a list joinList made.
func oneOrMany(list, one, many string) string {
	if strings.Contains(list, " and ") {
		return many
	}
	return one
}

// taskOf is the top-level todo id is under, or id itself.
func taskOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return id
}

// Parallel says the todos under the todo may be done at once, in any order,
// by any session; what must follow them is the todo's own work, done before
// it is popped. One session takes them in order either way. It returns the
// todo, so it chains: mem.Todo("build the shapes").Parallel().
func (f *Frame) Parallel() *Frame {
	if f == nil || f.m == nil || f.Id == "" {
		panic("no todo to make parallel: mem.Todo(task) makes one")
	}
	f.m.parallel(f.Id)
	return f
}

// parallel marks open todo id's todos as ones that may be done at once.
func (m *Mem) parallel(id string) {
	it := m.openItem(id)
	if it.For != "" {
		panic(ref(id, it.Title) + " is asked of the user: it has no todos")
	}
	if it.Parallel {
		println("`" + id + "`'s todos may already be done at once")
		return
	}
	m.do(Op{Kind: "set", Name: id, Field: "parallel", New: "yes"})
	println("`" + id + "`'s todos may be done at once, by any session")
}

// pushText is what a push says: where the model is, and what a pop will do.
func pushText(id, title, thought string) string {
	head := "in " + ref(id, title)
	if title == "" && thought != "" {
		head = "in `" + id + "` (thought: " + quote(line(thought, 80)) + ")"
	}
	return head + ": when it is done, pop it with what came of it and what the next todo needs. " +
		"Everything from here until the pop is removed from your context then, what you were told included; " +
		"what matters after, keep as a thought first. Your result stays, and nothing you did is undone."
}

// nothingNext is what Next() with nothing to step into says: what there is
// to do instead, where the model stands.
func (m *Mem) nothingNext() string {
	p := m.present()
	if p == "" {
		newest := ""
		m.open.ReverseIterate("", "", func(_ string, v any) bool {
			newest = v.(string)
			return true
		})
		if newest == "" {
			return "no next todo: nothing is open: make a todo, mem.Todo(\"…\").Push()."
		}
		g := m.get(newest)
		msg := "no next todo: " + ref(newest, g.Title)
		switch {
		case g.OpenSteps > 0:
			msg += ": nothing in it can be done now: " + m.holdsOf(newest)
		case g.Doing != "" && g.Doing != caller():
			msg += " has no open todo and is held by " + who(g.Doing) + ", which pops it with its result"
		case m.waiting(g):
			msg += " has no open todo and" + waitText(g.Wait)
		default:
			msg += " has no open todo"
		}
		return msg + "; or make a todo, mem.Todo(\"…\").Push()."
	}
	it := m.get(p)
	if it.OpenSteps > 0 {
		return "no next todo: nothing in `" + p + "` can be done now: " + m.holdsOf(p) + "."
	}
	if len(frames()) == 0 {
		return "no next todo: `" + p + "` is this session's /goal and has no open todo: mem.Todo(\"…\") to plan more, or mem.Done when it holds."
	}
	return m.doneByOthers(p, caller()) + "no next todo: `" + p + "` has no open todo: pop it with its result."
}

// holdsOf says what holds back each open todo of frame id that NextTodo
// passed over: asked of the user, waiting, or held by another session.
func (m *Mem) holdsOf(id string) string {
	var parts []string
	m.eachStep(id, func(step string, s *Todo) bool {
		switch m.state(step, s) {
		case "asked":
			parts = append(parts, "`"+step+"` is asked of the user: end your turn so they can answer, or mem.Drop(\""+step+"\", \"why\")")
		case "waiting":
			parts = append(parts, "`"+step+"`"+waitText(m.waitOn(step)))
		case "doing":
			parts = append(parts, "`"+step+"` is held by "+who(s.Doing))
		}
		return false
	})
	return joinList(parts)
}

// heldText refuses a call on a frame another session holds; when the frame
// has a todo this session could enter, it says so, since a todo another
// session holds, ended or not, does not hold the free ones under it.
func (m *Mem) heldText(id string, it *Todo) string {
	msg := "`" + id + "` is held by " + who(it.Doing) + ": only it can pop, close or drop it (resume that session if it has ended)"
	if nt := m.nextTodo(id); nt != nil {
		msg += "; its todo " + ref(nt.Id, nt.Purpose) + " is free: mem.Frame(" + quote(nt.Id) + ").Push() steps into it"
	}
	return msg
}

// heldByAny reports whether item id is held by any session, or an open
// step under it is.
func (m *Mem) heldByAny(id string, it *Todo) bool {
	held := it.Status == "" && it.Doing != ""
	m.eachBelow(id, false, func(_ string, s *Todo) {
		held = held || s.Status == "" && s.Doing != ""
	})
	return held
}

// refuseHeld refuses to close, drop, forget or start item id while another
// session holds it, or holds an open todo under it.
func (m *Mem) refuseHeld(id string, it *Todo) {
	me := caller()
	if it.Doing != "" && it.Doing != me {
		panic(m.heldText(id, it))
	}
	m.eachBelow(id, false, func(step string, s *Todo) {
		if s.Status == "" && s.Doing != "" && s.Doing != me {
			panic(m.heldText(step, s))
		}
	})
}

// refuseInside refuses to close or drop item name while this session is
// in a frame under it, or has it on its stack below the top: the frame
// above must be popped or dropped first.
func (m *Mem) refuseInside(stack []string, name string) {
	if len(stack) == 0 || name == stack[len(stack)-1] {
		return
	}
	in := deepestUnder(stack, name)
	if in == "" && onStack(stack, name) {
		in = stack[len(stack)-1]
	}
	if in != "" {
		panic("you are still in `" + in + "`: pop or drop it before `" + name + "`")
	}
}

// refuseWait refuses to enter or start item id while it, or an item above
// it, waits.
func (m *Mem) refuseWait(id string) {
	for p := id; p != ""; p = parentOf(p) {
		if w := m.get(p); m.waiting(w) {
			until := "until " + w.Wait
			if !isDay(w.Wait) {
				until = "for " + strconv.Quote(w.Wait)
			}
			panic(ref(p, w.Title) + " waits " + until + ": start it then, or clear the wait with mem.Wait(\"" + p + "\", \"\")")
		}
	}
}

// onStack reports whether id is on stack.
func onStack(stack []string, id string) bool {
	for _, s := range stack {
		if s == id {
			return true
		}
	}
	return false
}

// deepestUnder is the last frame of stack under id, a step of it or of a
// step of it; "" for none.
func deepestUnder(stack []string, id string) string {
	found := ""
	for _, s := range stack {
		if strings.HasPrefix(s, id+"/") {
			found = s
		}
	}
	return found
}

// top is the top of stack, or "".
func top(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	return stack[len(stack)-1]
}

// Pop leaves the present frame done with its result, and pops it: gnosh
// then removes everything since the push from the model's context. result
// is the model's own words, one string, kept whole up to mem's limit; it
// is what stays. A frame entered in the model's round before this one,
// with nothing in between (fresh), is not popped: its todo was never done.
func (m *Mem) Pop(result string) {
	stack := frames()
	if len(stack) == 0 {
		panic("nothing to pop: you are at the root")
	}
	id := top(stack)
	it := m.item(id)
	if it.Status != "" {
		m.popClosed(id, it)
		return
	}
	if id == fresh() {
		panic("not popped: nothing came between your entering " + ref(id, it.Title) + " and this pop, so it is not done. Do it, or check that it is done, then pop it.")
	}
	m.pop(id, it, result, false)
}

// popClosed pops a frame of the stack that mem already holds closed: one
// closed by an undo, or kept on the stack by a rewind that lost its pop.
// Nothing changes in mem; the stack is told.
func (m *Mem) popClosed(id string, it *Todo) {
	popped(id, it.Title, it.Result, len(it.Notes))
	println("`" + id + "` was already " + it.Status + arrow(it.Result) + ": it is off your stack now")
}

// pop leaves frame id, the top of the stack: done with result, or, with
// dropped, dropped with result as why, its open todos dropped first.
func (m *Mem) pop(id string, it *Todo, result string, dropped bool) {
	r := clip(text("the result", result), maxText)
	if r == "" {
		if dropped {
			panic("say why: mem.Drop(\"" + id + "\", \"why\")")
		}
		panic("say what came of `" + id + "`, even \"nothing found\"")
	}
	if dropped {
		steps := m.drop(id, it, r)
		popped(id, it.Title, "dropped: "+r, len(it.Notes))
		msg := "dropped " + ref(id, it.Title)
		if len(steps) > 0 {
			msg += ", with its open todos " + names(outlineOrder(steps))
		}
		println(msg)
		return
	}
	if it.OpenSteps > 0 {
		if w := m.workers(id, caller()); w != "" {
			panic(ref(id, it.Title) + " has open todos " + m.refs(m.openSteps(id)) + ", and " + w + " works on some of them now: mem.Next() steps into a free one, or waits for them")
		}
		panic(ref(id, it.Title) + " has open todos " + m.refs(m.openSteps(id)) + ": push and pop them, or drop them, first")
	}
	if it.Wait != "" {
		m.do(Op{Kind: "set", Name: id, Field: "wait", Old: it.Wait})
	}
	if it.Doing != "" {
		m.do(Op{Kind: "set", Name: id, Field: "doing", Old: it.Doing})
	}
	m.do(Op{Kind: "set", Name: id, Field: "status", New: "done"})
	m.do(Op{Kind: "set", Name: id, Field: "result", New: r})
	popped(id, it.Title, r, len(it.Notes))
	println("done " + ref(id, it.Title) + " → " + line(r, maxText))
}

// NextHint is what to do after frame id was popped, for gnosh to end the
// receipt with when no push follows in the same input; "" for nothing.
// Gnosh calls it with the stack as it stands after the pop.
func (m *Mem) NextHint(id string) string {
	parent := parentOf(id)
	if parent == "" {
		return ""
	}
	p := m.get(parent)
	if p == nil || p.Status != "" {
		return ""
	}
	present := m.present()
	// With other sessions working on the plan, only Next knows what is
	// left for this one, here or where it steps from; never a name to push,
	// which another may take. A sub-agent works for its parent and is not
	// sent on.
	if !subAgent() {
		w := m.workers(parent, caller())
		if w == "" {
			from := present
			if from == "" {
				from = m.newestOpen()
			}
			if from != "" {
				w = m.workers(from, caller())
			}
		}
		if w != "" {
			return "next: mem.Next(), which says what is left for you while " + w + oneOrMany(w, " works", " work") + " on this plan"
		}
	}
	if nt := m.nextTodo(parent); nt != nil {
		if parent == present || present == "" && m.nextAtRoot() == nt.Id {
			return "next: " + ref(nt.Id, nt.Purpose) + ": mem.Next()"
		}
		return "next: " + ref(nt.Id, nt.Purpose) + ": mem.Frame(" + quote(nt.Id) + ").Push()"
	}
	switch {
	case p.OpenSteps > 0:
		return "next: nothing in `" + parent + "` can be done now: " + m.holdsOf(parent)
	case parent == present && onStack(frames(), parent):
		return m.doneByOthers(parent, caller()) + "next: pop `" + parent + "` with the evidence that it holds"
	}
	return "next: mem.Done(" + quote(parent) + ", \"…\") once it holds"
}

// Think keeps a thought on the present frame and returns it: one journal
// line, no cut, nothing on the server. Think(text) names it for you, from
// the text (gnosh's small model, or its words), with -2, -3 when mem has
// that name; Think(name, text) names it yourself. Thoughts are what a
// frame keeps besides its result, and the one way anything a frame learned
// or was told outlives its pop: they stay in mem under the frame, popped
// or not. At the unbound root there is no frame to think on.
func (m *Mem) Think(args ...string) Thought {
	p := m.present()
	if p == "" {
		panic("you are at the root, in no frame: mem.Fact(\"…\") keeps something to remember")
	}
	it := m.item(p)
	var name, thought string
	switch len(args) {
	case 1:
		thought = clip(text("the thought", args[0]), maxText)
		if thought == "" {
			panic("a thought needs words: mem.Think(\"what you found\")")
		}
		name = m.freeThought(m.madeName(thought, "note"))
	case 2:
		name = m.thoughtName(p, it, args[0])
		thought = clip(text("the thought", args[1]), maxText)
		if thought == "" {
			panic("a thought needs words: mem.Think(" + quote(name) + ", \"what you found\")")
		}
	default:
		panic("Think takes a thought, or a name and a thought: mem.Think(\"port-env\", \"the port is read from an env var\")")
	}
	m.do(Op{Kind: "note", Name: p + "#" + name, New: thought})
	println("thought `" + name + "` kept on `" + p + "`")
	return Thought{name: name, Text: thought, frame: p, m: m}
}

// madeName is the name mem makes for text when the agent gives none:
// gnosh's small model's, when it answered one that is a name, else the
// text's words (nameOf, fallback for a text with none).
func (m *Mem) madeName(text, fallback string) string {
	if n := nameMade(text); n != "" && validName(n) && !strings.Contains(n, "/") && len(n) <= maxGoalName {
		return n
	}
	return nameOf(text, fallback)
}

// thoughtName checks a new thought's name on frame id.
func (m *Mem) thoughtName(id string, it *Todo, name string) string {
	switch {
	case name == "":
		panic("a thought needs a name to find it by: mem.Think(\"port-env\", \"the port is read from an env var\")")
	case name == id || strings.Contains(name, "/") && m.get(name) != nil:
		panic("`" + name + "` is a frame; a thought's name is a label of your own on the frame you are in, `" + id + "`: mem.Think(\"attempt-1\", \"…\")")
	case strings.ToLower(name) != name && validName(strings.ToLower(name)):
		panic("names are lowercase: `" + strings.ToLower(name) + "`")
	case strings.Contains(name, "/") || !validName(name):
		panic(named(name) + " is not a name: a thought's name is lowercase words joined by \"-\"" + like(nameForm(stepName(name)), "port-env"))
	case len(name) > maxGoalName:
		panic("a thought's name is at most " + strconv.Itoa(maxGoalName) + " bytes: try `" + nameOf(name, "thought") + "`")
	case m.thoughtOn(name) != "":
		panic("a thought named `" + name + "` is on `" + m.thoughtOn(name) + "` already: a thought is never edited; add another, like `" + m.freeThought(name) + "`")
	}
	return name
}

// thoughtOn is the id of the item holding the thought named name, or "".
func (m *Mem) thoughtOn(name string) string {
	found := ""
	m.items.Iterate("", "", func(id string, v any) bool {
		if noteIndex(v.(*Todo), name) >= 0 {
			found = id
		}
		return found != ""
	})
	return found
}

// freeThought is base, or base-2, base-3, …, whichever names no thought in
// mem yet.
func (m *Mem) freeThought(base string) string {
	if m.thoughtOn(base) == "" {
		return base
	}
	for i := 2; ; i++ {
		if n := base + "-" + strconv.Itoa(i); m.thoughtOn(n) == "" {
			return n
		}
	}
}

// Thought is a thought by name: the present frame's, else the first found
// walking down the stack, each frame's own and then its todos', else the
// one of that name anywhere in mem; a name mem makes is unique, so a
// thought is reached by name from any frame.
func (m *Mem) Thought(name string) Thought {
	for _, f := range m.framesDown() {
		it := m.get(f)
		if it == nil {
			continue
		}
		if n := noteNamed(it, name); n != nil {
			return m.thoughtOf(f, *n)
		}
		for _, s := range it.Steps {
			step := f + "/" + s
			if n := noteNamed(m.get(step), name); n != nil {
				return m.thoughtOf(step, *n)
			}
		}
	}
	if id := m.thoughtOn(name); id != "" {
		return m.thoughtOf(id, *noteNamed(m.get(id), name))
	}
	panic("no thought named `" + name + "` in mem: mem.Recall(\"…\") finds one by meaning")
}

// framesDown is the present frame, then the stack from the top down.
func (m *Mem) framesDown() []string {
	var out []string
	if p := m.present(); p != "" {
		out = append(out, p)
	}
	stack := frames()
	for i := len(stack) - 1; i >= 0; i-- {
		if len(out) == 0 || stack[i] != out[0] {
			out = append(out, stack[i])
		}
	}
	return out
}

// Recall is what in mem is about words, by meaning where gnosh has an
// embedding server and by the words otherwise: frames by their purpose,
// thoughts by their text and facts by their body, a handful at most, each
// a Namable. A promoted thought is one entry, the thought.
func (m *Mem) Recall(words string) []Namable {
	words = text("the words", words)
	if words == "" {
		panic("Recall needs words: mem.Recall(\"shared port\")")
	}
	names, kinds, _ := find(words, maxRecall*2)
	promoted := map[string]bool{}
	for i, k := range kinds {
		if k == "thought" {
			if item, name := splitAddr(names[i]); m.get(item) != nil {
				if n := noteNamed(m.get(item), name); n != nil && n.Step != "" {
					promoted[n.Step] = true
				}
			}
		}
	}
	var out []Namable
	for i, n := range names {
		if len(out) == maxRecall {
			break
		}
		switch kinds[i] {
		case "item":
			if it := m.get(n); it != nil && it.Status != "forgotten" && !promoted[n] {
				out = append(out, m.frame(n))
			}
		case "thought":
			item, name := splitAddr(n)
			if it := m.get(item); it != nil && it.Status != "forgotten" {
				if t := noteNamed(it, name); t != nil {
					out = append(out, m.thoughtOf(item, *t))
				}
			}
		case "fact":
			if f := m.getFact(n); f != nil {
				out = append(out, Fact{name: n, Body: f.Text})
			}
		}
	}
	return out
}

// Push dives into the thought: it makes a todo named as the thought, if it
// has none, under the thought's frame when that is the present frame or on
// the stack, else under the present frame; the todo's purpose is why, if
// given, else empty, since the thought's text is there; then it enters the
// todo as mem.Push does. A thought already dived into enters its todo
// again. A Thought comes from Think, Thought(name) or Recall.
func (t Thought) Push(why ...string) {
	if t.m == nil {
		panic("no thought to push: mem.Think, mem.Thought(name) or mem.Recall(words) gives one")
	}
	t.m.pushThought(t, why...)
}

func (m *Mem) pushThought(t Thought, why ...string) {
	if len(why) > 1 {
		panic("a thought's Push takes one reason for the dive, or none")
	}
	frame := t.frame
	if frame == "" {
		frame = m.present()
	}
	if frame == "" {
		panic("you are at the root, in no frame: a thought lives on a frame; mem.Frame(id).Thoughts lists one's")
	}
	it := m.get(frame)
	if it == nil || it.Status == "forgotten" {
		panic(m.noFrame(frame))
	}
	n := noteNamed(it, t.name)
	if n == nil {
		panic("`" + frame + "` has no thought named `" + t.name + "`")
	}
	if n.Step != "" {
		if s := m.get(n.Step); s != nil && s.Status != "forgotten" {
			m.push(n.Step, n.Text)
			return
		}
	}
	present := m.present()
	parent := frame
	if it.Status != "" || !(frame == present || onStack(frames(), frame)) {
		parent = present
	}
	if parent == "" {
		why := "is not on your stack"
		if it.Status != "" {
			why = "is " + it.Status
		}
		panic("`" + frame + "` " + why + ", and you are at the root, in no frame: enter a frame to dive into `" + t.name + "` from")
	}
	purpose := ""
	if len(why) == 1 {
		purpose = clip(text("why", why[0]), maxTitle)
	}
	step := parent + "/" + m.freeStep(parent, t.name)
	m.addStep(parent, step, purpose)
	m.do(Op{Kind: "set", Name: frame + "#" + t.name, Field: "frame", Old: n.Step, New: step})
	println("thought `" + t.name + "` is now the todo `" + step + "`")
	m.push(step, n.Text)
}
