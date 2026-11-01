// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

// nextFor is what to do next in task name, for session: "" if the task is
// closed, waits, or is for the user; else the first open item under it, in
// outline order, with no open todos, no wait on it or above it, not for the
// user, and not started by another session; else the task itself, if it
// has no open todos; else "".
func (m *Mem) nextFor(name string, g *Todo, session string) string {
	if g.Status != "" || m.waiting(g) || g.For != "" {
		return ""
	}
	if n := m.next(name, session); n != "" {
		return n
	}
	if g.OpenSteps == 0 {
		return name
	}
	return ""
}

// next is the first open item under name, in outline order, that this
// session can do now; "" if there is none: the pick, looking for a first
// leaf (peers.gno).
func (m *Mem) next(name string, session string) string {
	return m.pickFor(name, session, false, nil)
}

// free reports whether open item it can be done now by session: it does
// not wait, it is not for the user, and no other session started it.
func (m *Mem) free(it *Todo, session string) bool {
	return it.Status == "" && !m.waiting(it) && it.For == "" && (it.Doing == "" || it.Doing == session)
}

// waiting reports whether open item it still waits: for a day not yet come,
// or for what its words say.
func (m *Mem) waiting(it *Todo) bool {
	if it.Wait == "" {
		return false
	}
	if isDay(it.Wait) {
		return it.Wait > today()
	}
	return true
}

// held lists, for a goal whose next is "", the open items with no open
// steps that next passed over, each with what holds it: "day", "user",
// "words" or "session". A wait above an item counts for it.
func (m *Mem) held(name string, g *Todo, session string) (names, why []string) {
	var walk func(n string, it *Todo, above string)
	walk = func(n string, it *Todo, above string) {
		if it.Status != "" {
			return
		}
		reason := above
		switch {
		case reason != "":
		case it.For != "":
			reason = "user"
		case m.waiting(it) && isDay(it.Wait):
			reason = "day"
		case m.waiting(it):
			reason = "words"
		case it.Doing != "" && it.Doing != session:
			reason = "session"
		}
		if it.OpenSteps == 0 {
			if reason != "" {
				names = append(names, n)
				why = append(why, reason)
			}
			return
		}
		m.eachStep(n, func(step string, s *Todo) bool {
			walk(step, s, reason)
			return false
		})
	}
	walk(name, g, "")
	return names, why
}

// waitOn is what item name waits for: its own wait, or the nearest one
// above it; "" for none.
func (m *Mem) waitOn(name string) string {
	for n := name; n != ""; n = parentOf(n) {
		if w := m.get(n).Wait; w != "" {
			return w
		}
	}
	return ""
}

// whatNext ends a receipt: "; next: `x` "title"" for goal name, or what it
// waits on, or that it can be closed.
func (m *Mem) whatNext(goal string) string {
	g := m.get(goal)
	if g == nil || g.Status != "" {
		return ""
	}
	if m.waiting(g) {
		return "; it" + waitText(g.Wait)
	}
	session := caller()
	n := m.nextFor(goal, g, session)
	switch {
	case n == goal:
		return "; next: `" + goal + "` itself: mem.Done(\"" + goal + "\", \"…\") once it holds"
	case n != "":
		return "; next: " + ref(n, m.get(n).Title)
	}
	names, why := m.held(goal, g, session)
	if len(names) == 0 {
		return ""
	}
	return "; nothing in `" + goal + "` can be done now: " + m.holds(names, why)
}

// holds says what holds back each of names.
func (m *Mem) holds(names, why []string) string {
	var parts []string
	for i, n := range names {
		it := m.get(n)
		switch why[i] {
		case "user":
			parts = append(parts, "`"+n+"` is for the user")
		case "session":
			parts = append(parts, "`"+n+"` was started by "+who(it.Doing)+", "+ago(it.Changed))
		default:
			parts = append(parts, "`"+n+"`"+waitText(m.waitOn(n)))
		}
	}
	return joinList(parts)
}
