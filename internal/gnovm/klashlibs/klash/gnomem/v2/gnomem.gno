// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

// Package gnomem is an agent's memory: the work it means to get done, as
// todos, one under another, and what came of each; what it waits for; what the user
// must answer or do; facts to keep; and the user's standing rules. Gnosh
// keeps one Mem per project, in the realm klash.local/r/mem on disk, and
// binds it to the name mem in every input the model runs there:
//
//	mem.Todo("release v2").Push()
//	mem.Todo("bump the version")
//	mem.Todo("tag v2.0.0")
//	mem.Todo("CI is green")
//	mem.Next()
//	mem.Think("the version lives in version.go")
//	mem.Pop("now 2.0.0")
//	mem.Ask("release-v2/tag-v2-0-0", "which key signs the tag?")
//	mem.Goal("renew the gno.shop TLS cert", "renew-cert").Wait("2026-12-01")
//	mem.Fact("cmd/test", "go test -race ./...")
//	mem.Rule("push-after-landing", "push to GitHub only after landed:")
//	mem.Find("release key")
//	mem.Show()
//
// Everything has a name: a lowercase path, printed in backticks. An item's
// id is its goal's name, then its steps' names: `release-v2`,
// `release-v2/bump`, `release-v2/bump/version`. A fact's or a rule's name
// is any path: `cmd/test`. Names are typed exactly as printed, never reused,
// and never renamed (an erasure aside).
//
// An item is open until it is done, dropped or forgotten, and no open item
// is ever under a closed one. A condition is a step: an item cannot be done
// while a step of it is open. An item may wait for a day or for what its
// words say; a wait holds back only next. An item for the user (a question
// or a request) is never the agent's next. A day is the user's local date.
// Seen through a session's stack, an item is a frame (frames.gno): a
// session holds the frames it has pushed, and nobody else closes, drops or
// takes one it holds.
//
// Every change is an Op, and one function, apply, changes a Mem. The calls
// only decide which Ops to make, and hand each to gnosh, which journals it.
// What an Op means never changes, so gnosh can rebuild a Mem from its Ops
// whatever this package's code or layout becomes. Items, facts and rules
// sit in B+trees keyed by name, so a call loads only what it touches, and a
// Mem has no size limit. Every call refuses text that is not valid UTF-8.
package gnomem

import (
	"strconv"
	"strings"
	"time"
)

// Caps on what a Mem keeps; longer text is cut at a character, with "…".
// (maxSteps, in apply.gno, caps the steps under one item.)
const (
	maxTitle = 200  // bytes of a title
	maxText  = 1000 // bytes of a result, thought, wait, fact or rule
)

// Bind is the todo the user's /goal binds a session to: the open top-level
// todo titled title, matched as Todo matches tasks, or else a new one, made
// by the user. replacing is the todo the session's earlier /goal added, if
// any: it is dropped, if it is still open and is not the one bound now.
// Only gnosh calls it, for /goal.
func (m *Mem) Bind(title string, replacing ...string) *Frame {
	if caller() != "user" {
		panic("only the user's /goal binds a todo; mem.Todo(…) makes one")
	}
	id := m.titled(title)
	if id == "" {
		id = m.todoUnder("", title).Id
	}
	for _, r := range replacing {
		if it := m.get(r); r != id && it != nil && it.Status == "" && !m.heldByAny(r, it) {
			m.Drop(r, "replaced by /goal "+strconv.Quote(clip(title, 80)))
		}
	}
	return m.frame(id)
}

// freeStep is base, or base-2, base-3, …, whichever names no step of
// parent yet.
func (m *Mem) freeStep(parent, base string) string {
	if m.get(parent+"/"+base) == nil {
		return base
	}
	for i := 2; ; i++ {
		if n := base + "-" + strconv.Itoa(i); m.get(parent+"/"+n) == nil {
			return n
		}
	}
}

// Ask adds a question or a request for the user as a todo under item name
// (with name "", as a top-level item named from the question). It is never
// the agent's next, the user is shown it, and the item it is under cannot
// be done until it is answered: Done(it, "the answer").
func (m *Mem) Ask(name, question string) {
	question = clip(text("the question", question), maxTitle)
	if question == "" {
		panic("a question needs words: mem.Ask(\"" + name + "\", \"what to ask the user\")")
	}
	var asked string
	if name == "" {
		m.refuseOpenTitle(question)
		asked = m.freeName(nameOf("q "+question, "ask"), func(n string) bool { return m.freeTop(n) })
	} else {
		parent := m.openItem(name)
		if parent.For != "" {
			panic(ref(name, parent.Title) + " is for the user already")
		}
		if len(parent.Steps) >= maxSteps {
			panic(ref(name, parent.Title) + " has " + strconv.Itoa(maxSteps) + " todos, the most an item may have")
		}
		asked = name + "/" + m.freeStep(name, nameOf(question, "ask"))
	}
	m.do(Op{Kind: "add", Name: asked, New: question, By: caller()})
	m.do(Op{Kind: "set", Name: asked, Field: "for", New: "user"})
	msg := "asked " + ref(asked, question) + " (for the user)"
	if name != "" {
		// "waits on it" says why, when nothing else in the todo is free.
		msg += "; `" + name + "` waits on it"
		if g := goalOf(name); m.nextFor(g, m.get(g), caller()) != "" {
			msg += m.whatNext(g)
		}
	}
	println(msg)
}

// Start marks open item name as being worked on by this session, with no
// cut (mem.Push enters it as a frame), so that other sessions' next skips
// it; Show marks it "(held by …)". Done and Drop clear the mark. An item
// another session holds is refused: that session pops or closes it. An
// item that waits, or is under one that waits, is not started: mem would
// say it is being worked on while it waits for a day or for something
// said, and a later session could not tell which holds.
func (m *Mem) Start(name string) {
	it := m.openItem(name)
	if it.For != "" {
		panic(ref(name, it.Title) + " is for the user: only they can do it")
	}
	m.refuseWait(name)
	me := caller()
	if it.Doing == me {
		println(ref(name, it.Title) + " is started already, by this session")
		return
	}
	if it.Doing != "" {
		panic(m.heldText(name, it))
	}
	m.do(Op{Kind: "set", Name: name, Field: "doing", Old: "", New: me})
	println("started " + ref(name, it.Title))
}

// Done closes open item name with what came of it, if anything is given,
// and clears its wait. It refuses while a step of it is open, naming those
// steps and saying to Done or Drop them first. Done of a done item says
// "already done → <result>" and changes nothing. Done of the present frame,
// the top of this session's stack, is Pop: a result is required, and
// gnosh cuts the context; Done of a frame below the top, or of one with a
// frame of the stack under it, is refused; and nobody closes a frame
// another session holds.
func (m *Mem) Done(name string, result ...string) {
	it := m.item(name)
	stack := frames()
	if it.Status != "" && name == top(stack) {
		m.popClosed(name, it)
		return
	}
	switch it.Status {
	case "done":
		println(ref(name, it.Title) + " is already done" + arrow(it.Result))
		return
	case "dropped":
		panic(ref(name, it.Title) + " was dropped: " + it.Result + "; mem.Reopen(\"" + name + "\") to take it up again")
	}
	m.refuseInside(stack, name)
	if name == top(stack) {
		m.refuseFresh(name, it)
		m.pop(name, it, strings.Join(result, " "), false)
		return
	}
	m.refuseHeld(name, it)
	if it.OpenSteps > 0 {
		panic(ref(name, it.Title) + " has open todos " + m.refs(m.openSteps(name)) + ": Done or Drop them first")
	}
	r := clip(text("the result", strings.Join(result, " ")), maxText)
	if it.Wait != "" {
		m.do(Op{Kind: "set", Name: name, Field: "wait", Old: it.Wait})
	}
	if it.Doing != "" {
		m.do(Op{Kind: "set", Name: name, Field: "doing", Old: it.Doing})
	}
	m.do(Op{Kind: "set", Name: name, Field: "status", New: "done"})
	if r != "" {
		m.do(Op{Kind: "set", Name: name, Field: "result", New: r})
	}
	verb := "done "
	if it.For != "" {
		verb = "answered "
	}
	println(verb + ref(name, it.Title) + arrow(r) + m.whatNext(goalOf(name)))
}

// Drop closes open item name as not to be done, with why. Its open steps
// are dropped with it (the deepest first), keeping no reason; they, and
// its wait, come back when it is reopened. Drop of the present frame pops
// it, dropped, and gnosh cuts the context, since the work is given up;
// Drop of a frame below the top, or of one with a frame of the stack under
// it, is refused, and so is Drop of a frame another session holds, or
// that has a step another session holds.
func (m *Mem) Drop(name, why string) {
	it := m.openItem(name)
	m.refuseBound(name, "drop")
	why = clip(text("why", why), maxText)
	if why == "" {
		panic("say why: mem.Drop(\"" + name + "\", \"why\")")
	}
	stack := frames()
	m.refuseInside(stack, name)
	m.refuseHeld(name, it)
	if name == top(stack) {
		m.pop(name, it, why, true)
		return
	}
	dropped := m.drop(name, it, why)
	msg := "dropped " + ref(name, it.Title)
	if len(dropped) > 0 {
		msg += ", with its open todos " + names(outlineOrder(dropped))
	}
	println(msg + m.whatNext(goalOf(name)))
}

// refuseBound refuses to drop or forget (verb) the goal the user's /goal
// binds this session to: the agent closes it when it holds, and only the
// user clears it.
func (m *Mem) refuseBound(name, verb string) {
	if name != "" && name == bound() {
		panic("`" + name + "` is the user's /goal for this session, so this session does not " + verb + " it: close it with mem.Done(\"" + name + "\", \"…\") once it holds, or \"stopped: …\" when the goal says to stop; the user clears it with /goal clear")
	}
}

// outlineOrder is ids, which a bottom-up walk listed, in outline order.
func outlineOrder(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}

// drop closes open item it, named name, as dropped with why, and its open
// steps with it, the deepest first, which it returns.
func (m *Mem) drop(name string, it *Todo, why string) []string {
	var dropped []string
	m.eachBelow(name, true, func(step string, s *Todo) {
		if s.Status != "" {
			return
		}
		if s.Doing != "" {
			m.do(Op{Kind: "set", Name: step, Field: "doing", Old: s.Doing})
		}
		m.do(Op{Kind: "set", Name: step, Field: "status", New: "dropped"})
		dropped = append(dropped, step)
	})
	if it.Doing != "" {
		m.do(Op{Kind: "set", Name: name, Field: "doing", Old: it.Doing})
	}
	m.do(Op{Kind: "set", Name: name, Field: "status", New: "dropped"})
	m.do(Op{Kind: "set", Name: name, Field: "result", New: why})
	return dropped
}

// Reopen opens item name again if it is done or dropped, with the items it
// belongs to and the steps dropped with any of them (the deepest first);
// what came of each becomes a thought, and so does why, if given: why it
// is taken up again, as Drop keeps why it was put down. An open item is
// left as it is. A frame popped and reopened does not bring back what its
// pop removed from a context.
func (m *Mem) Reopen(name string, why ...string) {
	it := m.item(name)
	reason := clip(text("why", strings.Join(why, " ")), maxText)
	if it.Status == "" {
		println(ref(name, it.Title) + " is open")
		return
	}
	// The closed items above it, from the top, then it.
	var chain []string
	for p := name; p != ""; p = parentOf(p) {
		if m.get(p).Status != "" {
			chain = append([]string{p}, chain...)
		}
	}
	var others []string
	for _, n := range chain {
		for _, r := range m.reopenOne(n) {
			if r != name {
				others = append(others, r)
			}
		}
	}
	if reason != "" {
		m.note(name, m.get(name), "reopened: "+reason)
	}
	msg := "reopened " + ref(name, it.Title)
	if len(others) > 0 {
		msg += ", with " + names(others)
	}
	println(msg + m.whatNext(goalOf(name)))
}

// reopenOne reopens item n, then the steps dropped with it, the deepest
// first, and returns their names.
func (m *Mem) reopenOne(n string) []string {
	it := m.get(n)
	if it.Status == "" {
		return nil
	}
	dropped := it.Status == "dropped"
	if it.Result != "" {
		m.note(n, it, "was "+it.Status+": "+it.Result)
		m.do(Op{Kind: "set", Name: n, Field: "result", Old: it.Result})
	}
	m.do(Op{Kind: "set", Name: n, Field: "status", Old: it.Status})
	out := []string{n}
	if !dropped {
		return out
	}
	m.eachBelow(n, false, func(step string, s *Todo) {
		// Steps dropped with their parent keep no reason; one dropped on
		// its own has one, and stays dropped.
		if s.Status == "dropped" && s.Result == "" && m.get(parentOf(step)).Status == "" {
			m.do(Op{Kind: "set", Name: step, Field: "status", Old: "dropped"})
			out = append(out, step)
		}
	})
	return out
}

// Note keeps a finding or an attempt on item name, open or closed, as a
// thought named from its words (Think keeps one on the present frame).
func (m *Mem) Note(name, text_ string) {
	it := m.item(name)
	text_ = clip(text("the note", text_), maxText)
	if text_ == "" {
		panic("a note needs words: mem.Note(\"" + name + "\", \"what you found\")")
	}
	tname := m.note(name, it, text_)
	println("noted on " + ref(name, it.Title) + " as thought `" + tname + "`")
}

// note keeps text on item it, named name, as a thought named from the
// text's words, and returns the thought's name.
func (m *Mem) note(name string, it *Todo, text string) string {
	tname := m.freeThought(nameOf(text, "note"))
	m.do(Op{Kind: "note", Name: name + "#" + tname, New: text})
	return tname
}

// Wait sets what open item name waits for: a day, "2026-12-01", until that
// day; or words, "PR #6399 merged", until Wait(name, "") or Done clears it.
// A wait holds back only next: every call still works on a waiting item.
// A wait for the next session ("next time we talk", "next conversation")
// is none, and the receipt says why: every session is shown what is open,
// and a wait in words reads there as not yet come. It also says that a
// reminder the user asked for is an Ask, which every session raises,
// where an open goal stays quiet beside other work.
// It refuses a day written any other way, alone or after until, after, on
// or by (Nov 3, 11/03, next Monday, tomorrow, in 2 days); a 2026-11-03 among
// other words or not on the calendar; a day before yesterday; and an item's
// name, since a step is how one item waits for another. A refusal names
// today ("today is Thu 2026-10-29").
func (m *Mem) Wait(name, until string) {
	it := m.openItem(name)
	until, why := clip(text("the wait", until), maxText), ""
	if namesNextTalk(until) {
		until, why = "", ": every session is shown what is open; a reminder the user asked for is an Ask, which every session raises: mem.Ask("+strconv.Quote(name)+", \"…\")"
	}
	until = m.parseWait(until)
	switch {
	case until == it.Wait && until == "":
		println(ref(name, it.Title) + " waits for nothing" + why)
		return
	case until == it.Wait:
		println(ref(name, it.Title) + waitText(it.Wait) + " already")
		return
	}
	m.do(Op{Kind: "set", Name: name, Field: "wait", Old: it.Wait, New: until})
	msg := ref(name, it.Title) + waitText(until) + why
	if goalOf(name) != name {
		msg += m.whatNext(goalOf(name))
	}
	println(msg)
}

// Fact keeps text under name, any path, replacing what was there; "" removes
// it. Fact(text) alone names it for you, from the text (gnosh's small
// model, or its words), under a name no fact has yet. Its receipt says
// whether it is new or changed (and from what), and lists the other facts
// beside it.
func (m *Mem) Fact(args ...string) {
	var name, fact string
	switch len(args) {
	case 1:
		fact = clip(text("the fact", args[0]), maxText)
		if fact == "" {
			panic("a fact needs words: mem.Fact(\"what to remember\")")
		}
		if validName(fact) {
			panic("`" + fact + "` is a name, not a fact: mem.Fact(name, text) sets a fact, and mem.Show(name) shows one")
		}
		name = m.freeName(m.madeName(fact, "fact"), func(n string) bool { return m.getFact(n) == nil })
	case 2:
		name, fact = args[0], args[1]
	default:
		panic("Fact takes a fact, or a name and a fact: mem.Fact(\"cmd/test\", \"go test -race ./...\")")
	}
	m.checkName(name, "fact")
	fact = clip(text("the fact", fact), maxText)
	old := ""
	if f := m.getFact(name); f != nil {
		old = f.Text
	}
	switch {
	case fact == old && fact == "":
		panic("no fact `" + name + "`" + m.factsBeside(name, false))
	case fact == old:
		println("fact `" + name + "` is unchanged")
		return
	}
	m.do(Op{Kind: "fact", Name: name, Old: old, New: fact})
	switch {
	case fact == "":
		println("removed fact `" + name + "`")
	case old == "":
		println("fact `" + name + "` is new" + m.factsBeside(name, true))
	default:
		println("fact `" + name + "` changed (was " + strconv.Quote(old) + ")")
	}
}

// Rule keeps a standing instruction from the user under name; "" removes
// it. Every session is given every rule in full, and gnosh tells the user
// of each change. Only the user's words become rules.
func (m *Mem) Rule(name, rule string) {
	m.checkName(name, "rule")
	rule = clip(text("the rule", rule), maxText)
	old := ""
	if r := m.getRule(name); r != nil {
		old = r.Text
	}
	switch {
	case rule == old && rule == "":
		panic("no rule `" + name + "`")
	case rule == old:
		println("rule `" + name + "` is unchanged")
		return
	}
	m.do(Op{Kind: "rule", Name: name, Old: old, New: rule, By: caller()})
	switch {
	case rule == "":
		println("removed rule `" + name + "`")
	case old == "":
		println("rule `" + name + "` is new; every session will be given it")
	default:
		println("rule `" + name + "` changed (was " + strconv.Quote(old) + ")")
	}
}

// Forget empties closed item name and every item under it (title, result,
// wait, thoughts) and closes them as forgotten; or it removes fact or rule
// name. Gnosh erases their text from the journal and the index. It refuses
// an open item (Drop it first), which the user's /mem forget drops; only
// the user forgets a rule; nobody forgets a frame a session holds; and
// every other call refuses a forgotten item. Its receipt names no text.
func (m *Mem) Forget(name string) {
	m.refuseBound(name, "forget")
	it := m.get(name)
	if it == nil {
		switch {
		case m.getFact(name) != nil:
			m.do(Op{Kind: "forget", Name: name, Field: "fact"})
			println("forgot fact `" + name + "`; gnosh erases its text from its files")
			return
		case m.getRule(name) != nil:
			if caller() != "user" {
				panic("rules are the user's: they forget one with /mem forget " + name)
			}
			m.do(Op{Kind: "forget", Name: name, Field: "rule"})
			println("forgot rule `" + name + "`; gnosh erases its text from its files")
			return
		}
		m.item(name) // refuses, saying why
	}
	verb := "forgot"
	switch it.Status {
	case "":
		if caller() != "user" {
			panic("`" + name + "` is open: mem.Drop(\"" + name + "\", \"why\") first, then forget it")
		}
		m.refuseInside(frames(), name)
		m.refuseHeld(name, it)
		m.drop(name, it, "forgotten")
		verb = "dropped and forgot"
	case "forgotten":
		println("`" + name + "` is forgotten already")
		return
	}
	var gone []string
	m.eachBelow(name, true, func(step string, s *Todo) {
		if s.Status != "forgotten" {
			m.do(Op{Kind: "forget", Name: step})
			gone = append(gone, step)
		}
	})
	m.do(Op{Kind: "forget", Name: name})
	msg := verb + " `" + name + "`"
	if len(gone) > 0 {
		msg += " and its " + plural(len(gone), "todo")
	}
	println(msg + "; gnosh erases their text from its files")
}

// Redact replaces a secret's exact text with "(redacted)" wherever mem holds
// or ever held it: titles, results, waits, thoughts, facts, rules, and the
// statements of past inputs. It is for a secret's exact text: it refuses
// text under 6 bytes, text held nowhere, text that overlaps "(redacted)",
// and text held in more than 3 places (/mem redact has no such limit). Its
// receipt says how many places, never which or what.
func (m *Mem) Redact(secret string) {
	text("the text", secret)
	if len(secret) < 6 {
		panic("Redact takes a secret's exact text, 6 bytes or more")
	}
	if overlapsMarker(secret) {
		panic("that text overlaps \"(redacted)\": give the secret alone")
	}
	// An erasure renames an item whose name holds the secret; a frame on
	// the stack renamed under the session would be lost to it.
	if f := nameForm(secret); f != "" {
		for _, id := range frames() {
			if strings.Contains(id, f) {
				panic("that text is in the name of `" + id + "`, which is on your stack: pop it first, then redact")
			}
		}
	}
	names, kinds, statements := holding(secret)
	places := len(names)
	if statements && places == 0 {
		places = 1
	}
	switch {
	case places == 0:
		panic("mem holds no such text; Redact takes a secret's exact text")
	case places > 3 && caller() != "user":
		panic("that text is in " + strconv.Itoa(places) + " places: Redact is for a secret's exact text, and the user can erase wider with /mem redact")
	}
	// A rule is the user's words, followed by every session: redacting a
	// phrase of one changes what it says, for good. The user may.
	if caller() != "user" {
		for i, k := range kinds {
			if k == "rule" {
				panic("that text is in the rule `" + names[i] + "`, and rules are the user's: they redact it with /mem redact")
			}
		}
	}
	for i, n := range names {
		m.do(Op{Kind: "redact", Name: n, Field: kinds[i], Old: secret})
	}
	if len(names) == 0 {
		m.do(Op{Kind: "redact", Field: "statements", Old: secret})
	}
	println("redacted it in " + plural(places, "place") + "; gnosh rewrites mem's files")
}

// do applies op and hands it to gnosh; nothing else changes a Mem.
// time.Now() is the call's time, which gnosh fixes once per call, so all
// its Ops share it.
func (m *Mem) do(op Op) {
	m.apply(op, time.Now().Unix())
	record(op.Kind, op.Name, op.Field, op.Old, op.New, op.By)
}

// Apply applies the Ops gnosh hands it, each stamped with its call's time,
// without recording them: gnosh rebuilds a Mem from its journal with it,
// many entries at a time, and undoes with it. It panics unless gnosh runs
// the input.
func (m *Mem) Apply() {
	if who := caller(); who != "klash" && who != "user" {
		panic("only gnosh may apply Ops")
	}
	kinds, names, fields, olds, news, bys, ats := pending()
	for i := range kinds {
		m.apply(Op{Kind: kinds[i], Name: names[i], Field: fields[i], Old: olds[i], New: news[i], By: bys[i]}, ats[i])
	}
}
