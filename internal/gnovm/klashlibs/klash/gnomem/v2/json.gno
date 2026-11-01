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

// maxJSONGoals is how many open goals JSON() lists, newest first: more than
// the <mem> block shows, and few enough to compute after every change; and
// maxJSONDays and maxJSONAsks how many items waiting for a day, and
// questions for the user (counts.for_user counts them all).
const (
	maxJSONGoals = 20
	maxJSONDays  = 50
	maxJSONAsks  = 50
	maxJSONWords = 50
	maxJSONSteps = 100
)

// JSON returns, for gnosh and hooks, the goals with names, or with no names:
// the counts, the rules, the open items waiting for a day (soonest first,
// with the day, so a reader can tell what is due on any day), those
// waiting for words (by name, with the words), the questions for the user, the newest 20 open goals and the newest facts. A goal comes
// with its status, result, wait, for and doing, who made it and when it
// changed, how many of its steps are open and done, its next for caller()
// and that item's title, and, when its next is "", what holds back the open
// items Next passed over: "day", "user", "words" or "session", each with its
// title, the wait on it or above it, and who started it. A goal asked for by
// name also comes with the questions for the user open under it, and its
// steps, at most 100.
func (m *Mem) JSON(names ...string) string {
	var j jsonWriter
	j.open("{")
	if len(names) > 0 {
		j.key("goals")
		j.open("[")
		for _, n := range names {
			m.goalJSON(&j, n, true)
		}
		j.close("]")
		j.close("}")
		return j.String()
	}
	c := m.counts
	j.key("counts")
	j.open("{")
	for _, kv := range []struct {
		k string
		v int
	}{{"open", c.Open}, {"done", c.Done}, {"dropped", c.Dropped}, {"forgotten", c.Forgotten}, {"waits", c.Waits}, {"for_user", c.ForUser}, {"facts", m.facts.Size()}, {"rules", m.rules.Size()}} {
		j.key(kv.k)
		j.num(int64(kv.v))
	}
	j.close("}")
	j.key("rules")
	j.open("[")
	m.rules.Iterate("", "", func(n string, v any) bool {
		r := v.(*Rule)
		j.open("{")
		j.field("name", n)
		j.field("text", r.Text)
		j.field("by", r.By)
		j.key("changed")
		j.num(r.Changed)
		j.close("}")
		return false
	})
	j.close("]")
	j.key("days")
	j.open("[")
	i := 0
	m.days.Iterate("", "", func(_ string, v any) bool {
		n := v.(string)
		j.open("{")
		j.field("name", n)
		j.field("title", m.get(n).Title)
		j.field("wait", m.get(n).Wait)
		j.close("}")
		i++
		return i == maxJSONDays
	})
	j.close("]")
	j.key("words")
	j.open("[")
	i = 0
	m.words.Iterate("", "", func(n string, _ any) bool {
		j.open("{")
		j.field("name", n)
		j.field("title", m.get(n).Title)
		j.field("wait", m.get(n).Wait)
		j.close("}")
		i++
		return i == maxJSONWords
	})
	j.close("]")
	j.key("asks")
	j.open("[")
	i = 0
	m.asks.Iterate("", "", func(n string, _ any) bool {
		j.open("{")
		j.field("name", n)
		j.field("title", m.get(n).Title)
		j.close("}")
		i++
		return i == maxJSONAsks
	})
	j.close("]")
	j.key("goals")
	j.open("[")
	i = 0
	m.open.ReverseIterate("", "", func(_ string, v any) bool {
		m.goalJSON(&j, v.(string), false)
		i++
		return i == maxJSONGoals
	})
	j.close("]")
	j.key("facts")
	j.open("[")
	i = 0
	m.recent.ReverseIterate("", "", func(_ string, v any) bool {
		n := v.(string)
		f := m.getFact(n)
		j.open("{")
		j.field("name", n)
		j.field("text", f.Text)
		j.key("changed")
		j.num(f.Changed)
		j.close("}")
		i++
		return i == maxShowFacts
	})
	j.close("]")
	j.close("}")
	return j.String()
}

// goalJSON writes goal name's summary, and, withSteps, its steps: each
// with its name, title and status, whether it is for the user, and who
// started it.
func (m *Mem) goalJSON(j *jsonWriter, name string, withSteps bool) {
	g := m.get(name)
	if g == nil {
		j.open("{")
		j.field("name", name)
		j.field("status", "missing")
		j.close("}")
		return
	}
	session := caller()
	j.open("{")
	for _, kv := range [][2]string{{"name", name}, {"title", g.Title}, {"status", g.Status}, {"result", g.Result}, {"wait", g.Wait}, {"for", g.For}, {"doing", g.Doing}, {"by", g.By}} {
		j.field(kv[0], kv[1])
	}
	j.key("changed")
	j.num(g.Changed)
	j.key("steps_open")
	j.num(int64(g.OpenSteps))
	j.key("steps_done")
	j.num(int64(g.DoneSteps))
	if g.Parallel {
		j.key("parallel")
		j.boolean(true)
	}
	next, nextTitle := "", ""
	if g.Status != "forgotten" {
		next = m.nextFor(name, g, session)
	}
	if next != "" {
		nextTitle = m.get(next).Title
	}
	j.field("next", next)
	j.field("next_title", nextTitle)
	if withSteps {
		// The questions for the user open anywhere under the goal: one under
		// a step holds back what comes after it, and Next may not say so.
		j.key("asks")
		j.open("[")
		m.eachBelow(name, false, func(n string, it *Todo) {
			if it.Status == "" && it.For != "" {
				j.open("{")
				j.field("name", n)
				j.field("title", it.Title)
				j.close("}")
			}
		})
		j.close("]")
		j.key("steps")
		j.open("[")
		n := 0
		m.eachStep(name, func(step string, s *Todo) bool {
			j.open("{")
			for _, kv := range [][2]string{{"name", step}, {"title", s.Title}, {"status", s.Status}, {"for", s.For}, {"doing", s.Doing}} {
				j.field(kv[0], kv[1])
			}
			if s.Parallel {
				j.key("parallel")
				j.boolean(true)
			}
			j.close("}")
			n++
			return n == maxJSONSteps
		})
		j.close("]")
	}
	j.key("held")
	j.open("[")
	if next == "" && g.Status == "" {
		names, why := m.held(name, g, session)
		for i, n := range names {
			it := m.get(n)
			j.open("{")
			for _, kv := range [][2]string{{"name", n}, {"why", why[i]}, {"title", it.Title}, {"wait", m.waitOn(n)}, {"doing", it.Doing}} {
				j.field(kv[0], kv[1])
			}
			j.key("changed")
			j.num(it.Changed)
			j.close("}")
		}
	}
	j.close("]")
	j.close("}")
}

// jsonWriter writes JSON by hand: Gno has no encoding/json.
type jsonWriter struct {
	sb      strings.Builder
	first   []bool // per open object or array, whether nothing is in it yet
	inValue bool   // a key was just written, and its value comes next
}

// element starts a value: after a key, or as an element of what is open.
func (j *jsonWriter) element() {
	if j.inValue {
		j.inValue = false
		return
	}
	if n := len(j.first); n > 0 {
		if !j.first[n-1] {
			j.sb.WriteString(",")
		}
		j.first[n-1] = false
	}
}

func (j *jsonWriter) open(bracket string) {
	j.element()
	j.sb.WriteString(bracket)
	j.first = append(j.first, true)
}

func (j *jsonWriter) close(bracket string) {
	j.first = j.first[:len(j.first)-1]
	j.sb.WriteString(bracket)
}

// key writes a key of the open object; its value follows.
func (j *jsonWriter) key(k string) {
	j.element()
	j.sb.WriteString(jsonString(k) + ":")
	j.inValue = true
}

func (j *jsonWriter) field(k, v string) {
	j.key(k)
	j.element()
	j.sb.WriteString(jsonString(v))
}

func (j *jsonWriter) num(n int64) {
	j.element()
	j.sb.WriteString(strconv.FormatInt(n, 10))
}

func (j *jsonWriter) boolean(b bool) {
	j.element()
	if b {
		j.sb.WriteString("true")
	} else {
		j.sb.WriteString("false")
	}
}

func (j *jsonWriter) String() string { return j.sb.String() }
