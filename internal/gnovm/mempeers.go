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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The host's side of peers, many sessions working one plan in mem at once
// (PARALLEL.md, in the crushtest notes). The realm decides whether a Next
// waits; the host says who is working, keeps what each session's last call
// left in its peers record, refuses a wait that would close a loop, and
// rings the pipes that wake waiting Nexts on every change; the client does
// the waiting (memwait.go).

// MemWait is a todo a waiting Next waits for, and the session holding it.
type MemWait struct {
	Id      string `json:"id"`
	Session string `json:"session"`
}

// sessionKey is a session id as a file name: its hash, since a sub-agent's
// id holds the provider's tool-call id.
func sessionKey(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:12])
}

// turnLockPath is session's turn lock, under gnosh's gnovm data directory
// dir: the agent holds it from when the session gets busy until it goes
// idle, and the kernel drops it if the process dies.
func turnLockPath(dir, session string) string {
	return filepath.Join(dir, "turns", sessionKey(session)+".lock")
}

// memPeer is a session's peers record, in mem's peers/ directory, written
// under mem's lock by each of its calls that changes it: the stack the call
// left, and what its Next waits for, on which pipe. The waits count only
// while that pipe is active: a wait the client gave up (Esc, a failed
// re-run) is over though no call has written the record since.
type memPeer struct {
	Session string    `json:"session"`
	Stack   []string  `json:"stack,omitempty"`
	Waits   []MemWait `json:"waits,omitempty"`
	Pipe    string    `json:"pipe,omitempty"`
}

func (h *memHost) peerPath(session string) string {
	return filepath.Join(h.dir, "peers", sessionKey(session)+".json")
}

func (h *memHost) readPeer(session string) (memPeer, bool) {
	b, err := os.ReadFile(h.peerPath(session))
	if err != nil {
		return memPeer{}, false
	}
	var p memPeer
	if json.Unmarshal(b, &p) != nil || p.Session != session {
		return memPeer{}, false
	}
	return p, true
}

func (h *memHost) writePeer(p memPeer) error {
	path := h.peerPath(p.Session)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// workingFor is the realm's working() for one call: session is working on
// todo id when its turn lock is held and its peers record's stack has the
// todo, or one above it, on it. Each session is looked up once per call.
// nil when the host cannot tell (no data directory), as with one session.
func (h *memHost) workingFor() func(session, id string) bool {
	if h.working != nil {
		return h.working
	}
	if h.data == "" {
		return nil
	}
	type seen struct {
		held  bool
		stack []string
	}
	cache := map[string]seen{}
	return func(session, id string) bool {
		s, ok := cache[session]
		if !ok {
			s.held = turnHeld(h.data, session)
			if s.held {
				p, _ := h.readPeer(session)
				s.stack = p.Stack
			}
			cache[session] = s
		}
		if !s.held {
			return false
		}
		for _, f := range s.stack {
			if f == id || strings.HasPrefix(id, f+"/") {
				return true
			}
		}
		return false
	}
}

// waitLoop reports whether session, waiting for waits' holders, would close
// a loop: a holder that, by its peers record, waits for session, or for one
// that does, and so on. Only sessions in a turn count, and only waits whose
// pipe is still active: a record a session left as it ended, or as it gave
// up a wait, waits for nothing. Read under mem's lock, where every record
// is written and every pipe activated, the check is exact but for a wait
// given up in the moment since.
func (h *memHost) waitLoop(session string, waits []MemWait) bool {
	held := func(s string) bool {
		return h.working != nil && h.working(s, "") || h.data != "" && turnHeld(h.data, s)
	}
	seen := map[string]bool{}
	var queue []string
	for _, w := range waits {
		queue = append(queue, w.Session)
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if s == session {
			return true
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		if !held(s) {
			continue
		}
		p, _ := h.readPeer(s)
		if p.Pipe == "" || !h.activePipe(p.Pipe) {
			continue
		}
		for _, w := range p.Waits {
			queue = append(queue, w.Session)
		}
	}
	return false
}

// renamePeers takes an erasure's renames into every peers record.
func (h *memHost) renamePeers(renamed map[string]string) {
	if len(renamed) == 0 {
		return
	}
	entries, _ := os.ReadDir(filepath.Join(h.dir, "peers"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(h.dir, "peers", e.Name()))
		var p memPeer
		if err != nil || json.Unmarshal(b, &p) != nil || p.Session == "" {
			continue
		}
		for i, f := range p.Stack {
			p.Stack[i] = renamedUnder(renamed, f)
		}
		for i, w := range p.Waits {
			p.Waits[i].Id = renamedUnder(renamed, w.Id)
		}
		_ = h.writePeer(p)
	}
}

// renamedUnder is id after renamed: its own new name, or its nearest
// renamed ancestor's with the rest of id after it.
func renamedUnder(renamed map[string]string, id string) string {
	for p := id; p != ""; {
		if r, ok := renamed[p]; ok {
			return r + id[len(p):]
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return id
}

// endsWithBareNext reports whether code's last statement is mem.Next(),
// alone, and the input's only mem.Next(): the only input whose Next may
// wait, so an input never keeps or loses what came before its Next
// depending on what peers do, and one wait answers one Next.
func endsWithBareNext(code string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\nfunc _() {\n"+code+"\n}\n", 0)
	if err != nil || len(f.Decls) != 1 {
		return false
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	es, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 || !isMemNext(call) {
		return false
	}
	nexts := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isMemNext(c) {
			nexts++
		}
		return true
	})
	return nexts == 1
}

// isMemNext reports whether call calls mem.Next.
func isMemNext(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Next" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "mem"
}

// validPipe reports whether name can be a wake pipe's: the client makes
// them hex.
func validPipe(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// afterCall is the peers' part of a call, under mem's lock, after it ran
// and, if it changed mem, after its entry was kept: it rings the waiting
// Nexts when the call changed mem or its session's stack, and for a Next
// that waits, refuses a loop, activates its pipe, or else answers as a Next
// that may not wait; then it writes the session's peers record.
func (h *memHost) afterCall(r *memRealm, q memRequest, ctx memContext, call memCall, res *memResult, committed bool) {
	agent := q.Run && q.Session != "" && !q.User
	stack := stackAfter(q.Frames, call.Frames)
	var prev memPeer
	had := false
	if agent {
		prev, had = h.readPeer(q.Session)
	}
	moved := agent && (!had || !slices.Equal(prev.Stack, stack))
	if committed || moved {
		h.ring()
	}
	waits := call.Waits
	if len(waits) > 0 {
		note := ""
		switch {
		case h.waitLoop(q.Session, waits):
			note = "not waiting for " + sessionsOf(waits) + ", which waits for this session: that would wait forever\n"
			waits = nil
		case q.Pipe != "" && !h.activate(q.Pipe):
			waits = nil
		}
		if waits == nil {
			ctx.mayWait = false
			ctx.stack = stack
			if fb, err := r.call("mem.Next()", ctx); err == nil {
				res.Output += note + fb.Output
			}
		}
	}
	res.Waits = waits
	if agent && (moved || len(waits) > 0 || len(prev.Waits) > 0) {
		p := memPeer{Session: q.Session, Stack: stack, Waits: waits}
		if len(waits) > 0 {
			p.Pipe = q.Pipe
		}
		_ = h.writePeer(p)
	}
}

// sessionsOf names the sessions waits are held by.
func sessionsOf(waits []MemWait) string {
	var names []string
	for _, w := range waits {
		n := "session " + w.Session
		if len(w.Session) > 8 {
			n = "session " + w.Session[:8]
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return strings.Join(names, ", ")
}
