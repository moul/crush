// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

// Peers: many sessions working one plan at once (PARALLEL.md). The pick
// decides which todo a session steps into, and whether its Next waits for
// the others. Every branch that differs from today's needs another session
// working now, so one session, or a later one taking up an ended one's
// plan, sees today's Next.

// pickFor is the todo under id that session steps into now, or "": todos
// beside it first, and only when none is free, a free todo under one a
// working session holds (pick, with help).
func (m *Mem) pickFor(id, session string, enter bool, done func(string) bool) string {
	if n := m.pick(id, session, enter, done, false); n != "" {
		return n
	}
	return m.pick(id, session, enter, done, true)
}

// pick goes through the open todos under id in outline order. It passes
// over a todo asked of the user or waiting (its own wait at the root, a wait
// at or above it in a frame, as today), and one another session holds while
// it is not working, with everything under it (today). The first free one
// with nothing open under it is the one (today); a free one with todos under
// it is, with enter (in a frame), entered (today), unless all that is left
// under it is working sessions' work; without (at the root), looked into for
// its first (today). A todo a working session holds is, with help, looked
// into for a free todo, by that todo's own order. In an ordered todo (not
// Parallel), a todo being worked on, at or under it, by a working session
// holds up the ones after it. done, when not nil, counts the todos it says
// as finished (see waitsFor).
func (m *Mem) pick(id, session string, enter bool, done func(string) bool, help bool) string {
	parent := m.get(id)
	ordered := parent == nil || !parent.Parallel
	found := ""
	m.eachStep(id, func(step string, s *Todo) bool {
		if s.Status != "" || done != nil && done(step) {
			return false
		}
		if s.For != "" || enter && m.waitsAbove(step) || !enter && m.waiting(s) {
			return false
		}
		if s.Doing != "" && s.Doing != session {
			if !working(s.Doing, step) {
				return false
			}
			if help {
				if w := m.pick(step, session, false, done, true); w != "" {
					found = w
					return true
				}
			}
			return ordered
		}
		if m.openUnder(step, done) == 0 {
			found = step
			return true
		}
		if enter {
			if m.peersOnly(step, session, done) {
				return ordered
			}
			found = step
			return true
		}
		if w := m.pick(step, session, false, done, help); w != "" {
			found = w
			return true
		}
		return ordered && m.worked(step, session, done)
	})
	return found
}

// openUnder is how many todos under id are open, not counting those done
// says are finished.
func (m *Mem) openUnder(id string, done func(string) bool) int {
	it := m.get(id)
	if it == nil {
		return 0
	}
	if done == nil {
		return it.OpenSteps
	}
	n := 0
	m.eachStep(id, func(step string, s *Todo) bool {
		if s.Status == "" && !done(step) {
			n++
		}
		return false
	})
	return n
}

// worked reports whether todo id, or one under it, is open and held by a
// session other than session that is working now (and not counted
// finished).
func (m *Mem) worked(id, session string, done func(string) bool) bool {
	it := m.get(id)
	if it == nil || it.Status != "" || done != nil && done(id) {
		return false
	}
	if it.Doing != "" && it.Doing != session && working(it.Doing, id) {
		return true
	}
	found := false
	m.eachStep(id, func(step string, _ *Todo) bool {
		found = m.worked(step, session, done)
		return found
	})
	return found
}

// peersOnly reports whether all that is left under todo id for session is
// working sessions' work: some is being worked on, and nothing is free.
func (m *Mem) peersOnly(id, session string, done func(string) bool) bool {
	return m.worked(id, session, done) && m.pickFor(id, session, false, done) == ""
}

// finished lists the todos under id that count as done once the working
// sessions other than session finish their work, bottom-up in outline
// order: one another working session holds counts once nothing open under
// it is left that does not count. A free todo, session's own, and those of
// sessions not working never count.
func (m *Mem) finished(id, session string) []string {
	var fin []string
	var visit func(n string) bool
	visit = func(n string) bool {
		it := m.get(n)
		if it == nil || it.Status != "" {
			return false
		}
		all := true
		m.eachStep(n, func(step string, s *Todo) bool {
			if s.Status == "" && !visit(step) {
				all = false
			}
			return false
		})
		if all && it.Doing != "" && it.Doing != session && working(it.Doing, n) {
			fin = append(fin, n)
			return true
		}
		return false
	}
	m.eachStep(id, func(step string, s *Todo) bool {
		if s.Status == "" {
			visit(step)
		}
		return false
	})
	return fin
}

// waitsFor is what Next waits for where session stands, when it has nothing
// for the session now: anchor is the frame it stands in (enter), or at the
// root the newest open task. It waits when, once the other working sessions
// finish their work (finished), the session would have a todo, or could pop
// the frame, or at the root take the task itself; it returns the todos it
// waits for then, and nothing otherwise.
func (m *Mem) waitsFor(anchor, session string, enter bool) []string {
	fin := m.finished(anchor, session)
	if len(fin) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, n := range fin {
		set[n] = true
	}
	done := func(n string) bool { return set[n] }
	would := m.pickFor(anchor, session, enter, done) != ""
	if !would && m.openUnder(anchor, done) == 0 {
		a := m.get(anchor)
		would = enter || a.Status == "" && m.free(a, session)
	}
	if !would {
		return nil
	}
	return fin
}

// newestOpen is the newest open top-level todo, the root's task; "" for
// none.
func (m *Mem) newestOpen() string {
	newest := ""
	m.open.ReverseIterate("", "", func(_ string, v any) bool {
		newest = v.(string)
		return true
	})
	return newest
}

// workers names the working sessions other than session that hold todo id's
// open todos, or ones under them; "" for none.
func (m *Mem) workers(id, session string) string {
	var names []string
	seen := map[string]bool{}
	m.eachBelow(id, false, func(n string, it *Todo) {
		if it.Status == "" && it.Doing != "" && it.Doing != session && !seen[it.Doing] && working(it.Doing, n) {
			seen[it.Doing] = true
			names = append(names, who(it.Doing))
		}
	})
	return joinList(names)
}

// doneByOthers is what the todos under id that another session did came to,
// one line each, for the session that enters id; "" for none.
func (m *Mem) doneByOthers(id, session string) string {
	var lines []string
	m.eachStep(id, func(step string, s *Todo) bool {
		if s.Status == "done" && s.Did != "" && s.Did != session {
			lines = append(lines, "its todo `"+step+"` was done by "+who(s.Did)+arrow(s.Result))
		}
		return false
	})
	if len(lines) == 0 {
		return ""
	}
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
