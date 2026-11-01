// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

import bptree "klash/bptree/v0"

// This file and apply.gno fix how a Mem is stored, with bptree and the
// realm's source: gnosh pins their hash to its layout number (memLayout),
// and a change to any of them is a new layout, which rebuilds every mem from
// its journal.

// Mem is an agent's memory. Only its methods change it, and they all have
// pointer receivers: a value receiver would copy the Mem out of its realm.
type Mem struct {
	items bptree.BPTree // id → *Todo: a top-level todo's name, then the names of the todos down to it, joined by "/"
	facts bptree.BPTree // name → *fact
	rules bptree.BPTree // name → *Rule
	// The rest is kept by apply, from the three above:
	open   bptree.BPTree // stampKey(changed, name) → name: each open goal
	asks   bptree.BPTree // name → true: each open item for the user
	days   bptree.BPTree // day + "|" + name → name: each open item waiting for a day
	closed bptree.BPTree // stampKey(changed, name) → name: each done or dropped goal
	recent bptree.BPTree // stampKey(changed, name) → name: each fact
	taken  bptree.BPTree // goal name → true: every top-level name ever given
	titles bptree.BPTree // sameTitle(title) + "|" + name → name: each open goal, by its title
	words  bptree.BPTree // name → true: each open item waiting for words
	groups bptree.BPTree // factGroup(name) → how many facts the group has
	counts Counts
}

// Counts are a Mem's items by status, and the open ones that wait or are
// for the user.
type Counts struct{ Open, Done, Dropped, Forgotten, Waits, ForUser int }

// Todo is a piece of work: a top-level todo, one under another, or a
// question or request for the user; seen through the stack, a frame
// (frames.gno). Its id is its key: the top-level todo's name, then the
// names of the todos down to it.
type Todo struct {
	Title   string   // what to get done, or what is asked: a frame's purpose; "" for a step made from a thought with no reason given
	Status  string   // "" while open; "done", "dropped" or "forgotten"
	Result  string   // what came of it: the outcome, the user's answer, or why it was dropped
	Wait    string   // a day ("2026-12-01") or words ("PR #6399 merged"); "" none
	For     string   // "" the agent's; "user": a question or request for the user
	Doing   string   // the session that holds it (Push, Start); "" none
	Did     string   // the session that held it last, kept by apply when a hold ends: who did a done todo
	// Parallel says its todos may be done at once, in any order, by any
	// session (Frame.Parallel); otherwise they are in order.
	Parallel bool
	Steps   []string // its steps' names, in the order they were added; each one's id is this id, "/", the name
	Notes   []note   // its thoughts, oldest first; never edited
	Changed int64    // the latest change to it or to an item under it, Unix seconds
	By      string   // who made it: a session's id, or "user"
	// Kept by apply: how many of its steps are open, and done.
	OpenSteps, DoneSteps int
}

// note is a thought kept on an item: a labelled line, and the step it was
// promoted into, if any. Its address in an Op is the item's id, "#", its
// name: `fix-flaky/find-cause#port-env`.
type note struct {
	Name string
	Text string
	Step string // the id of the step made from it (a thought's Push); "" none
}

// fact is something true, kept under a name: "cmd/test",
// "models/qwen3-27b", "decision/mem-persistence".
type fact struct {
	Text    string
	Changed int64
}

// Rule is a standing instruction from the user. Every session is given
// every rule in full, with who set it.
type Rule struct {
	Text    string
	Changed int64
	By      string // who set it last: a session's id, or "user"
}

// Op is one change to a Mem, as the journal records it.
type Op struct {
	Kind  string // see below
	Name  string // the item, fact or rule; an add carries the full id; a note, unnote, or a set of "frame", a thought's address
	Field string // set: "status", "result", "wait", "for", "doing", or "frame" (a thought's step); redact: "item", "fact", "rule" or "statements"; forget: "" (an item), "fact" or "rule"
	Old   string // set, fact, rule: before; unadd: the title; unnote, redact: the text taken out
	New   string // add: the title; set, fact, rule: after; note: the text
	By    string // add, unadd: who made the item; rule: who set it
}

// The kinds of Op, and the inverse undo applies:
//
//	add     a new open item Name, titled New, made by By, after
//	        its parent's other steps                            ↔ unadd
//	unadd   item Name, untouched since added, is removed (undo
//	        only)                                               ↔ add
//	set     item Name's Field goes from Old to New; with Field
//	        "frame", the step thought Name was promoted into    ↔ set, Old and New swapped
//	note    New is appended to item Name's thoughts, under the
//	        name in Name's address, or one made from New's words ↔ unnote
//	unnote  the last of item Name's thoughts, Old, is taken off ↔ note
//	fact    fact Name goes from Old to New; "" is none          ↔ fact, swapped
//	rule    rule Name goes from Old to New; "" is none          ↔ rule, swapped
//	forget  item Name is emptied and closed as forgotten; or,
//	        with Field "fact" or "rule", that is removed        final
//	redact  Old becomes "(redacted)" in the item, thought, fact
//	        or rule Name (Field says which)                     final
//
// An Op stamps what it names, and the items above it, as changed at its
// call's time, whether or not a value changed, so a rebuild gives the same
// Changed. Text that is removed or replaced goes into a fresh array, so no
// old string stays reachable on disk.

// New returns an empty Mem.
func New() *Mem { return &Mem{} }
