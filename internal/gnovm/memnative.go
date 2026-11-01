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
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	gno "github.com/gnolang/gno/gnovm/pkg/gnolang"
	"github.com/gnolang/gno/gnovm/stdlibs"
)

// memContext is the context a mem call runs in: gno's own, which its natives
// read through the interface ExecContext answers, and what gnosh's mem
// natives read.
type memContext struct {
	stdlibs.ExecContext
	caller string
	ops    *[]memOp
	// pending is the Ops gnosh hands Apply, and pendingAt each one's call's
	// time.
	pending   []memOp
	pendingAt []int64
	// today is the user's local date, 2026-10-29.
	today string
	// bound is the goal the user's /goal binds the caller's session to.
	bound string
	// stack is the caller's session's stack of frames, bottom to top, as
	// gnosh derived it before the call; frames is where the call's pushes
	// and pops go (memCall.Frames), so frames() answers the stack as it
	// stands.
	stack  []string
	frames *[]MemFrame
	// nilPush is how many events the call had made when it last pushed
	// nil; -1 for never. A pop that a nil push followed gets no hint.
	nilPush *int
	// names are the names gnosh made for the texts the input keeps without
	// naming them (mem.Think(text), mem.Fact(text)), by text; the realm
	// asks for them with nameMade().
	names map[string]string
	// limit bounds the call (memrealm.go).
	limit memLimit
	// find and holding answer from mem's index and journal; nil finds
	// nothing. holding is also handed the Ops this call has made so far.
	find    func(words string, max int) (names, kinds []string, how string)
	holding func(text string, ops []memOp) (names, kinds []string, statements bool)
	// working says whether another session is working on a todo now
	// (PARALLEL.md); nil: none is. mayWait is whether the input's Next may
	// wait for them; waits is where it says what it waits for.
	working func(session, id string) bool
	mayWait bool
	waits   *[]MemWait
	// subAgent is set for a sub-agent's session: it is not told of peers
	// or sent on to other todos, since it works for its parent.
	subAgent bool
}

// memOp is one change to a mem, as gnomem's do hands it to gnosh and the
// journal keeps it (gnomem's Op).
type memOp struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Field string `json:"field,omitempty"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	By    string `json:"by,omitempty"`
}

// memNativeGas is what a mem native costs: a little bookkeeping in Go.
var memNativeGas = gno.NativeGasInfo{Base: 1000, SlopeIdx: -1, SlopeKind: gno.SizeFlat}

func init() {
	for _, name := range []gno.Name{"caller", "today", "bound", "frames", "pushed", "popped", "nameMade", "record", "pending", "find", "holding", "working", "mayWait", "waitOn", "subAgent", "quote", "jsonString"} {
		info := memNativeGas
		gno.RegisterNativeGas(memLibPath, name, &info)
	}
}

// memNative resolves gnomem's natives. Each answers only in a mem call,
// inside mem's realm: a Mem the model's code made or copied for itself lives
// outside the realm, and its Ops must never reach the journal. The two that
// only format, quote and jsonString, answer anywhere.
func memNative(name gno.Name) func(*gno.Machine) {
	switch name {
	case "caller":
		return func(m *gno.Machine) {
			memPush(m, memCallContext(m, "caller").caller)
		}
	case "bound":
		return func(m *gno.Machine) {
			memPush(m, memCallContext(m, "bound").bound)
		}
	case "frames":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "frames")
			memPush(m, stackAfter(ctx.stack, *ctx.frames))
		}
	case "pushed":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "pushed")
			if id := memArg(m, 0); id == "" {
				*ctx.nilPush = len(*ctx.frames)
				return
			}
			*ctx.frames = append(*ctx.frames, MemFrame{Kind: "push", Id: memArg(m, 0), Purpose: memArg(m, 1), Session: ctx.caller})
		}
	case "popped":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "popped")
			thoughts := m.LastBlock().GetPointerTo(nil, gno.NewValuePathBlock(1, 3, "")).TV.GetInt()
			*ctx.frames = append(*ctx.frames, MemFrame{Kind: "pop", Id: memArg(m, 0), Purpose: memArg(m, 1), Result: memArg(m, 2), Thoughts: int(thoughts), Session: ctx.caller})
		}
	case "nameMade":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "nameMade")
			memPush(m, ctx.names[memArg(m, 0)])
		}
	case "today":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "today")
			d := ctx.today
			if d == "" {
				d = time.Now().Format(time.DateOnly)
			}
			memPush(m, d)
		}
	case "find":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "find")
			var names, kinds []string
			how := ""
			if ctx.find != nil {
				max := m.LastBlock().GetPointerTo(nil, gno.NewValuePathBlock(1, 1, "")).TV.GetInt()
				names, kinds, how = ctx.find(memArg(m, 0), int(max))
			}
			memPush(m, names)
			memPush(m, kinds)
			memPush(m, how)
		}
	case "holding":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "holding")
			var names, kinds []string
			statements := false
			if ctx.holding != nil {
				names, kinds, statements = ctx.holding(memArg(m, 0), *ctx.ops)
			}
			memPush(m, names)
			memPush(m, kinds)
			memPush(m, statements)
		}
	case "working":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "working")
			memPush(m, ctx.working != nil && ctx.working(memArg(m, 0), memArg(m, 1)))
		}
	case "mayWait":
		return func(m *gno.Machine) {
			memPush(m, memCallContext(m, "mayWait").mayWait)
		}
	case "subAgent":
		return func(m *gno.Machine) {
			memPush(m, memCallContext(m, "subAgent").subAgent)
		}
	case "waitOn":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "waitOn")
			if ctx.waits != nil {
				*ctx.waits = append(*ctx.waits, MemWait{Id: memArg(m, 0), Session: memArg(m, 1)})
			}
		}
	case "record":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "record")
			*ctx.ops = append(*ctx.ops, memOp{
				Kind:  memArg(m, 0),
				Name:  memArg(m, 1),
				Field: memArg(m, 2),
				Old:   memArg(m, 3),
				New:   memArg(m, 4),
				By:    memArg(m, 5),
			})
		}
	case "pending":
		return func(m *gno.Machine) {
			ctx := memCallContext(m, "pending")
			fields := make([][]string, 6)
			for _, op := range ctx.pending {
				for i, s := range []string{op.Kind, op.Name, op.Field, op.Old, op.New, op.By} {
					fields[i] = append(fields[i], s)
				}
			}
			for i := range fields {
				memPush(m, fields[i])
			}
			memPush(m, ctx.pendingAt)
		}
	// The two that only format answer anywhere.
	case "quote":
		return func(m *gno.Machine) {
			memPush(m, strconv.Quote(memArg(m, 0)))
		}
	case "jsonString":
		return func(m *gno.Machine) {
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(memArg(m, 0))
			memPush(m, strings.TrimSuffix(b.String(), "\n"))
		}
	}
	return nil
}

// memPush returns v from a native.
func memPush[T any](m *gno.Machine, v T) {
	m.PushValue(gno.Go2GnoValue(m.Alloc, m.Store, reflect.ValueOf(&v).Elem()))
}

func memCallContext(m *gno.Machine, native string) memContext {
	ctx, ok := m.Context.(memContext)
	if !ok || ctx.ops == nil || ctx.frames == nil || ctx.nilPush == nil || m.Realm == nil || m.Realm.Path != memRealmPath {
		panic(native + ": only mem's own Mem, in a mem call, may change mem")
	}
	return ctx
}

// stackAfter is stack, a session's frames bottom to top, after events, a
// call's pushes and pops in order: a push goes on top, and a pop takes its
// frame off.
func stackAfter(stack []string, events []MemFrame) []string {
	out := append([]string(nil), stack...)
	for _, e := range events {
		switch e.Kind {
		case "push":
			out = append(out, e.Id)
		case "pop":
			for i := len(out) - 1; i >= 0; i-- {
				if out[i] == e.Id {
					out = append(out[:i:i], out[i+1:]...)
					break
				}
			}
		}
	}
	return out
}

// memArg is the native's i'th parameter, a string.
func memArg(m *gno.Machine, i int) string {
	return m.LastBlock().GetPointerTo(nil, gno.NewValuePathBlock(1, uint16(i), "")).TV.GetString()
}
