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

// maxSteps caps the steps under one item.
const maxSteps = 9999

// apply is the only code that changes a Mem. It reads only the Mem, op and
// at, and panics, so nothing is kept, when op does not fit:
//   - set, fact, rule, unnote: Old is not the current value (unnote: the
//     last thought);
//   - add: the id exists, its parent is missing or closed, or the parent has
//     maxSteps steps; unadd: the item is closed, or has steps, thoughts, a
//     result or a wait;
//   - note: the item already has a thought of that name;
//   - a set would open an item under a closed one, or close one with an
//     open step;
//   - a forget names an open item, or a missing fact or rule.
//
// A redact naming a missing item, thought, fact or rule changes and stamps
// nothing.
func (m *Mem) apply(op Op, at int64) {
	switch op.Kind {
	case "add":
		m.applyAdd(op)
	case "unadd":
		m.applyUnadd(op)
	case "set":
		m.applySet(op, at)
	case "note":
		m.applyNote(op)
	case "unnote":
		item, name := splitAddr(op.Name)
		it := m.mustItem(item)
		last := len(it.Notes) - 1
		if last < 0 || it.Notes[last].Text != op.Old || name != "" && it.Notes[last].Name != name {
			panic("apply: `" + item + "`'s last thought is not " + strconv.Quote(op.Old))
		}
		notes := make([]note, last)
		copy(notes, it.Notes)
		it.Notes = notes
	case "fact":
		m.applyFact(op, at)
		return
	case "rule":
		m.applyRule(op, at)
		return
	case "forget":
		switch op.Field {
		case "fact":
			f := m.getFact(op.Name)
			if f == nil {
				panic("apply: no fact `" + op.Name + "`")
			}
			m.recent.Remove(stampKey(f.Changed, op.Name))
			m.facts.Remove(op.Name)
			m.group(op.Name, -1)
			return
		case "rule":
			if m.getRule(op.Name) == nil {
				panic("apply: no rule `" + op.Name + "`")
			}
			m.rules.Remove(op.Name)
			return
		}
		it := m.mustItem(op.Name)
		if it.Status == "" {
			panic("apply: cannot forget open `" + op.Name + "`")
		}
		it.Title, it.Result, it.Notes = "", "", nil
		if it.Wait != "" {
			m.setWait(op.Name, it, "")
		}
		m.setStatus(op.Name, it, "forgotten", at)
	case "redact":
		if !m.applyRedact(op) {
			return
		}
		if op.Field != "item" {
			m.stampFact(op.Field, op.Name, at)
			return
		}
	default:
		panic("apply: unknown op " + strconv.Quote(op.Kind))
	}
	item, _ := splitAddr(op.Name)
	m.stamp(item, at)
}

func (m *Mem) applyAdd(op Op) {
	if m.get(op.Name) != nil {
		panic("apply: `" + op.Name + "` exists")
	}
	if strings.IndexByte(op.Name, '#') >= 0 {
		panic("apply: `" + op.Name + "` is a thought's address, not an item's")
	}
	if p := parentOf(op.Name); p != "" {
		parent := m.get(p)
		if parent == nil || parent.Status != "" {
			panic("apply: `" + p + "` is missing or closed")
		}
		if len(parent.Steps) >= maxSteps {
			panic("apply: `" + p + "` has " + strconv.Itoa(maxSteps) + " steps, the most an item may have")
		}
		steps := make([]string, len(parent.Steps), len(parent.Steps)+1)
		copy(steps, parent.Steps)
		parent.Steps = append(steps, op.Name[len(p)+1:])
		parent.OpenSteps++
	} else {
		m.taken.Set(op.Name, true)
		m.open.Set(stampKey(0, op.Name), op.Name)
		m.titles.Set(sameTitle(op.New)+"|"+op.Name, op.Name)
	}
	m.items.Set(op.Name, &Todo{Title: op.New, By: op.By})
	m.counts.Open++
}

func (m *Mem) applyUnadd(op Op) {
	it := m.mustItem(op.Name)
	if it.Status != "" || it.Result != "" || it.Wait != "" || it.For != "" || it.Doing != "" || len(it.Notes) > 0 || len(it.Steps) > 0 {
		panic("apply: `" + op.Name + "` has changed since it was added")
	}
	m.items.Remove(op.Name)
	if p := parentOf(op.Name); p == "" {
		m.open.Remove(stampKey(it.Changed, op.Name))
		m.titles.Remove(sameTitle(it.Title) + "|" + op.Name)
	} else {
		parent := m.get(p)
		name := op.Name[len(p)+1:]
		steps := make([]string, 0, len(parent.Steps))
		for _, s := range parent.Steps {
			if s != name {
				steps = append(steps, s)
			}
		}
		parent.Steps = steps
		parent.OpenSteps--
	}
	m.counts.Open--
}

// applyNote appends a thought to an item: under the name in op's address,
// or, for an address with none (a note from before thoughts had names),
// under one made from the text's words.
func (m *Mem) applyNote(op Op) {
	item, name := splitAddr(op.Name)
	it := m.mustItem(item)
	if name == "" {
		name = freeThoughtName(it, nameOf(op.New, "note"))
	} else if noteNamed(it, name) != nil {
		panic("apply: `" + item + "` already has a thought named `" + name + "`")
	}
	notes := make([]note, len(it.Notes), len(it.Notes)+1)
	copy(notes, it.Notes)
	it.Notes = append(notes, note{Name: name, Text: op.New})
}

func (m *Mem) applySet(op Op, at int64) {
	if op.Field == "frame" {
		item, name := splitAddr(op.Name)
		it := m.mustItem(item)
		i := noteIndex(it, name)
		if i < 0 {
			panic("apply: `" + item + "` has no thought named `" + name + "`")
		}
		if it.Notes[i].Step != op.Old {
			panic("apply: thought `" + op.Name + "`'s frame is " + strconv.Quote(it.Notes[i].Step) + ", not " + strconv.Quote(op.Old))
		}
		notes := make([]note, len(it.Notes))
		copy(notes, it.Notes)
		notes[i].Step = op.New
		it.Notes = notes
		return
	}
	it := m.mustItem(op.Name)
	var cur string
	switch op.Field {
	case "status":
		cur = it.Status
	case "result":
		cur = it.Result
	case "wait":
		cur = it.Wait
	case "for":
		cur = it.For
	case "doing":
		cur = it.Doing
	case "parallel":
		if it.Parallel {
			cur = "yes"
		}
	default:
		panic("apply: unknown field " + strconv.Quote(op.Field))
	}
	if cur != op.Old {
		panic("apply: `" + op.Name + "`'s " + op.Field + " is " + strconv.Quote(cur) + ", not " + strconv.Quote(op.Old))
	}
	switch op.Field {
	case "status":
		m.setStatus(op.Name, it, op.New, at)
	case "result":
		it.Result = op.New
	case "wait":
		m.setWait(op.Name, it, op.New)
	case "for":
		if it.Status == "" {
			if it.For != "" {
				m.asks.Remove(op.Name)
				m.counts.ForUser--
			}
			if op.New != "" {
				m.asks.Set(op.Name, true)
				m.counts.ForUser++
			}
		}
		it.For = op.New
	case "doing":
		if op.New == "" && op.Old != "" {
			it.Did = op.Old
		}
		it.Doing = op.New
	case "parallel":
		it.Parallel = op.New == "yes"
	}
}

// setStatus moves item name to status, keeping the counts and indexes.
func (m *Mem) setStatus(name string, it *Todo, status string, at int64) {
	if status == it.Status {
		return
	}
	var parent *Todo
	if p := parentOf(name); p != "" {
		parent = m.get(p)
	}
	if status == "" {
		if parent != nil && parent.Status != "" {
			panic("apply: cannot open `" + name + "` under closed `" + parentOf(name) + "`")
		}
	} else if it.Status == "" && it.OpenSteps > 0 {
		panic("apply: cannot close `" + name + "` while a todo under it is open")
	}
	if parent != nil {
		parent.countStep(it.Status, -1)
		parent.countStep(status, 1)
	}
	top := parent == nil
	if it.Status == "" {
		// Leaving open.
		m.counts.Open--
		if top {
			m.open.Remove(stampKey(it.Changed, name))
			m.titles.Remove(sameTitle(it.Title) + "|" + name)
		}
		if it.For != "" {
			m.asks.Remove(name)
			m.counts.ForUser--
		}
		if it.Wait != "" {
			m.counts.Waits--
			m.unwait(name, it.Wait)
		}
	} else {
		m.counts.countStatus(it.Status, -1)
		if top {
			m.closed.Remove(stampKey(it.Changed, name))
		}
	}
	it.Status = status
	if status == "" {
		m.counts.Open++
		if top {
			m.open.Set(stampKey(it.Changed, name), name)
			m.titles.Set(sameTitle(it.Title)+"|"+name, name)
		}
		if it.For != "" {
			m.asks.Set(name, true)
			m.counts.ForUser++
		}
		if it.Wait != "" {
			m.counts.Waits++
			m.rewait(name, it.Wait)
		}
	} else {
		m.counts.countStatus(status, 1)
		if top && status != "forgotten" {
			// stamp re-keys it to at, after this Op.
			m.closed.Set(stampKey(it.Changed, name), name)
		}
	}
}

// countStep counts a step of it in or out of its open and done steps.
func (it *Todo) countStep(status string, d int) {
	switch status {
	case "":
		it.OpenSteps += d
	case "done":
		it.DoneSteps += d
	}
}

// countStatus counts an item in or out of the closed status it has.
func (c *Counts) countStatus(status string, d int) {
	switch status {
	case "done":
		c.Done += d
	case "dropped":
		c.Dropped += d
	case "forgotten":
		c.Forgotten += d
	}
}

// setWait changes open-or-closed item name's wait, keeping the indexes.
func (m *Mem) setWait(name string, it *Todo, wait string) {
	if it.Status == "" {
		if it.Wait != "" {
			m.counts.Waits--
			m.unwait(name, it.Wait)
		}
		if wait != "" {
			m.counts.Waits++
			m.rewait(name, wait)
		}
	}
	it.Wait = wait
}

// rewait puts open item name, waiting for wait, in the days or the words
// index; unwait takes it out.
func (m *Mem) rewait(name, wait string) {
	if isDay(wait) {
		m.days.Set(wait+"|"+name, name)
	} else {
		m.words.Set(name, true)
	}
}

func (m *Mem) unwait(name, wait string) {
	if isDay(wait) {
		m.days.Remove(wait + "|" + name)
	} else {
		m.words.Remove(name)
	}
}

func (m *Mem) applyFact(op Op, at int64) {
	f := m.getFact(op.Name)
	cur := ""
	if f != nil {
		cur = f.Text
	}
	if cur != op.Old {
		panic("apply: fact `" + op.Name + "` is " + strconv.Quote(cur) + ", not " + strconv.Quote(op.Old))
	}
	if f != nil {
		m.recent.Remove(stampKey(f.Changed, op.Name))
	}
	if op.New == "" {
		m.facts.Remove(op.Name)
		m.group(op.Name, -1)
		return
	}
	if f == nil {
		f = &fact{}
		m.facts.Set(op.Name, f)
		m.group(op.Name, 1)
	}
	f.Text, f.Changed = op.New, at
	m.recent.Set(stampKey(at, op.Name), op.Name)
}

func (m *Mem) applyRule(op Op, at int64) {
	r := m.getRule(op.Name)
	cur := ""
	if r != nil {
		cur = r.Text
	}
	if cur != op.Old {
		panic("apply: rule `" + op.Name + "` is " + strconv.Quote(cur) + ", not " + strconv.Quote(op.Old))
	}
	if op.New == "" {
		m.rules.Remove(op.Name)
		return
	}
	if r == nil {
		r = &Rule{}
		m.rules.Set(op.Name, r)
	}
	r.Text, r.Changed, r.By = op.New, at, op.By
}

// applyRedact replaces op.Old with "(redacted)" in what op names, and
// reports whether there was anything to change. An item's address with a
// thought's name redacts that thought alone.
func (m *Mem) applyRedact(op Op) bool {
	re := func(s string) string { return strings.ReplaceAll(s, op.Old, redacted) }
	switch op.Field {
	case "item":
		item, name := splitAddr(op.Name)
		it := m.get(item)
		if it == nil {
			return false
		}
		if name != "" {
			i := noteIndex(it, name)
			if i < 0 {
				return false
			}
			notes := make([]note, len(it.Notes))
			copy(notes, it.Notes)
			notes[i].Text = re(notes[i].Text)
			it.Notes = notes
			return true
		}
		if parentOf(item) == "" && it.Status == "" {
			m.titles.Remove(sameTitle(it.Title) + "|" + item)
			m.titles.Set(sameTitle(re(it.Title))+"|"+item, item)
		}
		it.Title, it.Result = re(it.Title), re(it.Result)
		if it.Wait != "" {
			m.setWait(item, it, re(it.Wait))
		}
		notes := make([]note, len(it.Notes))
		for i, n := range it.Notes {
			notes[i] = note{Name: n.Name, Text: re(n.Text), Step: n.Step}
		}
		it.Notes = notes
	case "fact":
		f := m.getFact(op.Name)
		if f == nil {
			return false
		}
		f.Text = re(f.Text)
	case "rule":
		r := m.getRule(op.Name)
		if r == nil {
			return false
		}
		r.Text = re(r.Text)
	default:
		return false
	}
	return true
}

const redacted = "(redacted)"

// stamp marks item name, and the items above it, as changed at at,
// re-keying its goal in the open or closed index, which order by time.
func (m *Mem) stamp(name string, at int64) {
	for n := name; n != ""; n = parentOf(n) {
		it := m.get(n)
		if it == nil {
			continue
		}
		if parentOf(n) == "" {
			switch it.Status {
			case "":
				m.open.Remove(stampKey(it.Changed, n))
				m.open.Set(stampKey(at, n), n)
			case "done", "dropped":
				m.closed.Remove(stampKey(it.Changed, n))
				m.closed.Set(stampKey(at, n), n)
			}
		}
		it.Changed = at
	}
}

// stampFact marks fact or rule name as changed at at.
func (m *Mem) stampFact(kind, name string, at int64) {
	switch kind {
	case "fact":
		if f := m.getFact(name); f != nil {
			m.recent.Remove(stampKey(f.Changed, name))
			f.Changed = at
			m.recent.Set(stampKey(at, name), name)
		}
	case "rule":
		if r := m.getRule(name); r != nil {
			r.Changed = at
		}
	}
}

// sameTitle is a title as titles are matched: ASCII case folded, runs of
// white space as one space, trailing . , ; : ! ? dropped. The titles
// index is keyed by it, so it lives here, under the layout's pin.
func sameTitle(t string) string {
	t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
	return strings.TrimRight(t, ".,;:!?")
}

// factGroup is the group a fact's name is in, as Show lists hidden facts:
// its first part with the "/", as `cmd/`, or the whole name when it has
// none.
func factGroup(name string) string {
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i+1]
	}
	return name
}

// group counts fact name in or out of its group.
func (m *Mem) group(name string, d int) {
	g := factGroup(name)
	n := d
	if v := m.groups.Get(g); v != nil {
		n += v.(int)
	}
	if n <= 0 {
		m.groups.Remove(g)
	} else {
		m.groups.Set(g, n)
	}
}

// stampKey orders by time, then name: 12 digits of Unix seconds.
func stampKey(at int64, name string) string {
	s := strconv.FormatInt(at, 10)
	for len(s) < 12 {
		s = "0" + s
	}
	return s + "|" + name
}

// mustItem is item name, which an Op must find.
func (m *Mem) mustItem(name string) *Todo {
	it := m.get(name)
	if it == nil {
		panic("apply: no item `" + name + "`")
	}
	return it
}

func (m *Mem) get(name string) *Todo {
	v := m.items.Get(name)
	if v == nil {
		return nil
	}
	return v.(*Todo)
}

func (m *Mem) getFact(name string) *fact {
	v := m.facts.Get(name)
	if v == nil {
		return nil
	}
	return v.(*fact)
}

func (m *Mem) getRule(name string) *Rule {
	v := m.rules.Get(name)
	if v == nil {
		return nil
	}
	return v.(*Rule)
}

// splitAddr splits a thought's address, item#name, into the item and the
// thought's name; a plain item id has no thought.
func splitAddr(addr string) (item, name string) {
	if i := strings.IndexByte(addr, '#'); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, ""
}

// noteIndex is where item it keeps its thought named name; -1 for none.
func noteIndex(it *Todo, name string) int {
	for i, n := range it.Notes {
		if n.Name == name {
			return i
		}
	}
	return -1
}

// noteNamed is item it's thought named name, or nil.
func noteNamed(it *Todo, name string) *note {
	if i := noteIndex(it, name); i >= 0 {
		return &it.Notes[i]
	}
	return nil
}

// freeThoughtName is base, or base-2, base-3, …, whichever names no thought
// of it yet.
func freeThoughtName(it *Todo, base string) string {
	if noteIndex(it, base) < 0 {
		return base
	}
	for i := 2; ; i++ {
		if n := base + "-" + strconv.Itoa(i); noteIndex(it, n) < 0 {
			return n
		}
	}
}

// isDay reports whether a wait is a day, 2026-12-01, which the days index
// holds, rather than words.
func isDay(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i != 4 && i != 7 && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// parentOf is the id of the item name is a step of; "" for a goal.
func parentOf(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}
