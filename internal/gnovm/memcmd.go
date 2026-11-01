// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// memUsage is what /mem does.
const memUsage = `/mem                 mem's outline
/mem show <name>     a goal, a step, a fact or a rule in full
/mem find <words>    what in mem matches the words
/mem log [n]         the last n changes (20): when, who, what ran, what it changed
/mem undo [entry]    reverse one change; with no entry, the last is shown first
/mem forget <name>   erase a goal, a step, a fact or a rule, with its history
/mem redact <text>   take a secret's exact text out of mem and its history
/mem repair [vm]     build mem's store and index again from its journal; or
                     repair a broken VM, keeping its entries before the damage`

// MemCommand runs /mem, the user's command for the project's mem, typed in
// session, and returns what to show them. titles names sessions by their
// ids, for /mem log. Its arguments are never logged: /mem redact carries a
// secret.
func (h *Host) MemCommand(ctx context.Context, project, session, args string, titles map[string]string) (string, error) {
	out, _, err := h.memCommand(ctx, project, session, args, titles)
	return out, err
}

// memCommand is MemCommand, with what it erased, if anything, for gnosh's
// own copies of mem's text.
func (h *Host) memCommand(ctx context.Context, project, session, args string, titles map[string]string) (string, *MemErasure, error) {
	var reply memReply
	text, err := h.memAnswer(ctx, project, session, args, titles, "/mem", &reply)
	return text, reply.erased, err
}

// MemRead answers one of /mem's reads (show, find, log) typed at a shell
// as command, gnosh mem: its text names command where a session's names
// /mem, and refused reports that mem refused it, or failed ("no item
// `x`"), for the command to exit 1. It takes nothing else: at a shell,
// mem is only read.
func (h *Host) MemRead(ctx context.Context, project, command, args string, titles map[string]string) (text string, refused bool, err error) {
	if f := strings.Fields(args); len(f) > 0 && f[0] != "show" && f[0] != "find" && f[0] != "log" {
		return command + " only reads mem: show, find and log", true, nil
	}
	var reply memReply
	text, err = h.memAnswer(ctx, project, "", args, titles, command, &reply)
	return text, reply.refused, err
}

// memReply is what a /mem answer leaves besides its text: what it erased,
// if anything, and whether mem refused it.
type memReply struct {
	erased  *MemErasure
	refused bool
}

// memAnswer answers /mem, typed as via ("/mem" in a session), into reply.
func (h *Host) memAnswer(ctx context.Context, project, session, args string, titles map[string]string, via string, reply *memReply) (string, error) {
	args = strings.TrimSpace(args)
	sub, rest := args, ""
	if i := strings.IndexFunc(args, unicode.IsSpace); i >= 0 {
		sub, rest = args[:i], strings.TrimSpace(args[i:])
	}
	if sub == "repair" && rest != "" && rest != MemVM {
		return h.repairVM(ctx, project, rest)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	m := h.mem(project)
	switch sub {
	case "", "show":
		var names []string
		for _, n := range strings.Fields(rest) {
			names = append(names, strconv.Quote(n))
		}
		out, err := m.user(ctx, session, "mem.Show("+strings.Join(names, ", ")+")", titles, via, reply)
		if line := m.meaningLine(ctx); err == nil && len(names) == 0 && line != "" {
			out = strings.TrimRight(out, "\n") + "\n" + line + "\n"
		}
		if err == nil && len(names) > 0 && !m.has(ctx, session, names) {
			reply.refused = true // Show writes a miss as text
		}
		return out, err
	case "find":
		if rest == "" {
			reply.refused = true
			return via + " find takes words: " + via + " find release key", nil
		}
		return m.user(ctx, session, "mem.Find("+strconv.Quote(rest)+")", titles, via, reply)
	case "log":
		n := 20
		if rest != "" {
			var err error
			if n, err = strconv.Atoi(rest); err != nil || n < 1 {
				reply.refused = true
				return via + " log takes how many changes to show: " + via + " log 50", nil
			}
		}
		return m.log(n, titles)
	case "undo":
		if rest == "" {
			return m.last(titles)
		}
		seq, err := strconv.ParseInt(strings.TrimPrefix(rest, "#"), 10, 64)
		if err != nil || seq < 1 {
			reply.refused = true
			return "/mem undo takes a change's number, as /mem log shows it: /mem undo 12", nil
		}
		return m.undo(ctx, session, seq)
	case "forget":
		if rest == "" {
			reply.refused = true
			return "/mem forget takes a name, as /mem shows it: /mem forget release-v2", nil
		}
		return m.user(ctx, session, "mem.Forget("+strconv.Quote(rest)+")", titles, via, reply)
	case "redact":
		if rest == "" {
			reply.refused = true
			return "/mem redact takes a secret's exact text: /mem redact sk-live-…", nil
		}
		return m.user(ctx, session, "mem.Redact("+strconv.Quote(rest)+")", titles, via, reply)
	case "repair":
		n, err := m.repair(ctx)
		if err != nil {
			return "", err
		}
		return "mem's store and index will be built again from its journal (" + memPlural(n, "change") + ") when it is next used", nil
	case "help":
		return memUsage, nil
	}
	return "/mem has no " + strconv.Quote(sub) + "; it takes:\n" + memUsage, nil
}

// repairVM is /mem repair for a broken VM, which a brokenError names.
func (h *Host) repairVM(ctx context.Context, project, name string) (string, error) {
	kept, cut, err := h.Repair(ctx, project, name)
	var ref *refusal
	switch {
	case errors.As(err, &ref):
		return ref.msg, nil
	case err != nil:
		return "", err
	}
	entries := "entries"
	if kept == 1 {
		entries = "entry"
	}
	return fmt.Sprintf("VM %s is repaired: %d %s kept, %d cut from the damage on; its old journal is kept beside it", name, kept, entries, cut), nil
}

// user runs code against mem as the user, typed in session as via ("/mem"),
// and returns what it printed, then why it failed, if it did, and what
// gnosh did besides. Only a busy mem is an error: the rest is the answer,
// a failure marked refused in reply. Sessions are named for the user
// (memSessions).
func (h *memHost) user(ctx context.Context, session, code string, titles map[string]string, via string, reply *memReply) (string, error) {
	res, err := h.call(ctx, memRequest{Code: code, Session: session, User: true})
	reply.erased = res.Erased
	var busy *busyError
	if errors.As(err, &busy) {
		return "", err
	}
	out := strings.TrimRight(res.Output, "\n")
	if err != nil {
		reply.refused = true
		out = strings.TrimLeft(out+"\n"+err.Error(), "\n")
	}
	for _, n := range res.Notes {
		out += "\n(" + n + ")"
	}
	// mem's text points the agent at mem.Show; the user has via.
	out = memShowRe.ReplaceAllStringFunc(out, func(call string) string {
		if name := memShowRe.FindStringSubmatch(call)[1]; name != "" {
			return via + " show " + name
		}
		return via
	})
	return memSessions(out, session, titles), nil
}

// has reports whether mem shows something under every one of names, which
// are quoted. A mem it cannot read has nothing.
func (h *memHost) has(ctx context.Context, session string, names []string) bool {
	checks := make([]string, len(names))
	for i, n := range names {
		checks[i] = "mem.Has(" + n + ")"
	}
	res, err := h.call(ctx, memRequest{Code: "println(" + strings.Join(checks, " && ") + ")", Session: session, User: true})
	return err == nil && strings.TrimSpace(res.Output) == "true"
}

// memShowRe is a call of mem.Show in mem's text, with the name it shows.
var memShowRe = regexp.MustCompile(`mem\.Show\((?:"([^"]*)")?\)`)

// memSessions names the sessions in mem's text, where gnomem says
// "session " and an id's first 8 bytes, as /mem log does: session, which
// /mem was typed in, as this session, and the others by their titles.
func memSessions(out, session string, titles map[string]string) string {
	if session != "" {
		out = strings.ReplaceAll(out, "session "+memShortID(session), "this session")
	}
	for id, title := range titles {
		if title != "" && id != session {
			out = strings.ReplaceAll(out, "session "+memShortID(id), "session "+strconv.Quote(memLine(title, 60)))
		}
	}
	return out
}

// entries reads mem's journal, which may be cut short by a torn last line
// that the next call cuts; none when mem has none yet.
func (h *memHost) entries() ([]memEntry, error) {
	_, entries, _, err := readMemJournal(filepath.Join(h.dir, "journal.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// log is the last n entries of mem's journal, the newest last.
func (h *memHost) log(n int, titles map[string]string) (string, error) {
	entries, err := h.entries()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "mem has no changes yet", nil
	}
	entries = entries[max(0, len(entries)-n):]
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = memEntryText(e, titles)
	}
	return strings.Join(texts, "\n"), nil
}

// last is the newest entry, with how to undo it.
func (h *memHost) last(titles map[string]string) (string, error) {
	entries, err := h.entries()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "mem has no changes to undo", nil
	}
	e := entries[len(entries)-1]
	return memEntryText(e, titles) + "\nto undo it: /mem undo " + strconv.FormatInt(e.Seq, 10), nil
}

// memEntryText is entry e as /mem log shows it: its number, when, who, what
// ran, then what it changed, an Op a line.
func memEntryText(e memEntry, titles map[string]string) string {
	when := time.Unix(e.Time, 0).In(time.FixedZone("", e.Offset)).Format("2006-01-02 15:04")
	lines := []string{fmt.Sprintf("#%d %s, %s: %s", e.Seq, when, memWho(e, titles), memLine(e.Code, 200))}
	for _, op := range e.Ops {
		lines = append(lines, "    "+memOpText(op))
	}
	for _, f := range e.Frames {
		if f.Kind == "pop" {
			lines = append(lines, "    pop `"+f.Id+"` → "+strconv.Quote(memLine(f.Result, 80)))
		} else {
			lines = append(lines, "    "+f.Kind+" `"+f.Id+"`")
		}
	}
	return strings.Join(lines, "\n")
}

// memWho is who made entry e, as the user reads it: "you", "gnosh", or the
// agent in a session, by its title where titles has it.
func memWho(e memEntry, titles map[string]string) string {
	switch {
	case e.Who == "user":
		return "you"
	case e.Who == "klash": // gnosh's own calls, recorded under its old name
		return "gnosh"
	case e.Session == "":
		return e.Who
	case titles[e.Session] != "":
		return "the agent in " + strconv.Quote(memLine(titles[e.Session], 60))
	}
	return "the agent in session " + memShortID(e.Session)
}

// memOpText is an Op as /mem log shows it.
func memOpText(op memOp) string {
	q := func(s string) string { return strconv.Quote(memLine(s, 80)) }
	name := "`" + op.Name + "`"
	switch op.Kind {
	case "set":
		return "set " + name + " " + op.Field + ": " + q(op.Old) + " → " + q(op.New)
	case "fact", "rule":
		if op.New == "" {
			return "remove " + op.Kind + " " + name
		}
		return op.Kind + " " + name + ": " + q(op.New)
	case "add", "note":
		return op.Kind + " " + name + ": " + q(op.New)
	case "unadd", "unnote":
		return op.Kind + " " + name + ": " + q(op.Old)
	case "redact":
		if op.Field == "statements" {
			return "redact past statements"
		}
		return "redact " + op.Field + " " + name
	}
	return op.Kind + " " + name
}

// undo reverses entry seq of mem's journal, under mem's lock: it applies
// the inverse of each of its Ops, the last first, in one transaction, and
// keeps them as an entry by the user, which undoing in turn redoes. It
// refuses an entry that forgets or redacts, which is final, and one that a
// later entry builds on: one with an Op on the same field of an item, its
// notes, the same fact or rule, or an item the entry added or one under it.
// An inverse that does not fit refuses it too, as an Op that would open a
// step under a closed goal.
func (h *memHost) undo(ctx context.Context, session string, seq int64) (string, error) {
	release, err := h.lock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	r, j, _, err := h.open()
	if err != nil {
		return "", err
	}
	defer r.close()
	x, err := h.openIndex(j)
	if err != nil {
		return "", err
	}
	defer x.close()
	_, entries, _, err := readMemJournal(j.path)
	if err != nil {
		return "", err
	}
	i := 0
	for i < len(entries) && entries[i].Seq != seq {
		i++
	}
	if i == len(entries) {
		return fmt.Sprintf("mem has no change #%d; /mem log lists them", seq), nil
	}
	target := entries[i]
	inverse, why := memInverse(target.Ops)
	if why != "" {
		return fmt.Sprintf("change #%d cannot be undone: %s", seq, why), nil
	}
	touched := map[string]bool{}
	var added []string
	for _, op := range target.Ops {
		touched[memOpKey(op)] = true
		if op.Kind == "add" {
			added = append(added, op.Name)
		}
	}
	for _, e := range entries[i+1:] {
		for _, op := range e.Ops {
			if touched[memOpKey(op)] || opTarget(op) == "item" && memUnder(op.Name, added) {
				return fmt.Sprintf("change #%d cannot be undone: #%d, after it, changed the same thing (%s); undo that first", seq, e.Seq, memOpText(op)), nil
			}
		}
	}
	now := h.now()
	ats := make([]int64, len(inverse))
	for k := range ats {
		ats[k] = now.Unix()
	}
	call, err := r.apply(inverse, ats)
	if err != nil {
		return fmt.Sprintf("change #%d cannot be undone: %s", seq, strings.TrimPrefix(err.Error(), "apply: ")), nil
	}
	_, offset := now.Zone()
	e := &memEntry{Seq: j.seq + 1, Time: now.Unix(), Offset: offset, Who: "user", Session: session, Code: "/mem undo " + strconv.FormatInt(seq, 10), Ops: inverse}
	if err := j.append(e); err != nil {
		return "", err
	}
	h.ring()
	// The entry is kept; a failed write below is caught up by the next call.
	if err := r.commit(call, h.stamp(j)); err != nil {
		return "", err
	}
	if err := x.apply([][]memOp{inverse}, h.indexStamp(j)); err != nil {
		return "", err
	}
	h.writeStatus(r, j)
	return fmt.Sprintf("undid #%d as #%d:\n%s", seq, e.Seq, memEntryText(*e, nil)), nil
}

// memInverse is the Ops that reverse ops, the last first; why says why
// there are none.
func memInverse(ops []memOp) (inverse []memOp, why string) {
	if len(ops) == 0 {
		return nil, "nothing it changed is left in mem's history"
	}
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		switch op.Kind {
		case "add":
			op.Kind, op.Old, op.New = "unadd", op.New, ""
		case "unadd":
			op.Kind, op.Old, op.New = "add", "", op.Old
		case "note":
			op.Kind, op.Old, op.New = "unnote", op.New, ""
		case "unnote":
			op.Kind, op.Old, op.New = "note", "", op.Old
		case "set", "fact", "rule":
			op.Old, op.New = op.New, op.Old
			if op.Kind == "rule" {
				op.By = "user"
			}
		case "forget", "redact":
			return nil, "it " + op.Kind + "s, which is final"
		default:
			return nil, "it has an Op of kind " + strconv.Quote(op.Kind)
		}
		inverse = append(inverse, op)
	}
	return inverse, ""
}

// memOpKey is what an Op changes, for telling whether two Ops change the
// same thing: a field of an item, its notes, the item itself, a fact or a
// rule.
func memOpKey(op memOp) string {
	switch op.Kind {
	case "set":
		return "item " + op.Name + " " + op.Field
	case "note", "unnote":
		return "item " + op.Name + " notes"
	}
	return opTarget(op) + " " + op.Name
}

// memUnder reports whether name is one of names, or under one: a step of
// it, or a thought on it.
func memUnder(name string, names []string) bool {
	for _, n := range names {
		if name == n || strings.HasPrefix(name, n+"/") || strings.HasPrefix(name, n+"#") {
			return true
		}
	}
	return false
}
