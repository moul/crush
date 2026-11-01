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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	gno "github.com/gnolang/gno/gnovm/pkg/gnolang"
	"github.com/gnolang/gno/gnovm/pkg/gnomod"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/db/goleveldb"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/gnolang/gno/tm2/pkg/store/cache"
	"github.com/gnolang/gno/tm2/pkg/store/dbadapter"
	storetypes "github.com/gnolang/gno/tm2/pkg/store/types"
)

// memRealmPath is the realm that holds a project's mem: one *gnomem.Mem,
// made when the realm is deployed into a new store.
const memRealmPath = "klash.local/r/mem"

const memRealmSource = `package mem

import gnomem "klash/gnomem/v2"

var M = gnomem.New()
`

// memLibPath is the library the realm's Mem comes from, whose natives
// (memnative.go) answer only inside a mem call.
const memLibPath = "klash/gnomem/v2"

// memLayout is the layout of a mem's store. gnomem v2's types.gno and
// apply.gno, bptree v0 and the realm's source fix how a Mem is stored, and
// TestMemLayoutPinned pins their hash to this number: changing them means a
// new number, and a store of an older one is rebuilt from its journal.
// 3 indexes open goals by title, items waiting for words, and fact groups;
// 4 names steps and thoughts (FRAMES.md); 5 keeps who held a todo last and
// whether its todos may be done at once (PARALLEL.md).
const memLayout = 5

// memLayoutFiles are the library sources memLayout covers.
var memLayoutFiles = []string{
	"klashlibs/klash/gnomem/v2/types.gno",
	"klashlibs/klash/gnomem/v2/apply.gno",
	"klashlibs/klash/bptree/v0/node.gno",
	"klashlibs/klash/bptree/v0/tree.gno",
}

// memRunAddr is the address mem inputs run as. gno gives a realm only to a
// script at the address's run path, as it does for a chain's MsgRun.
var memRunAddr = crypto.AddressFromPreimage([]byte("klash mem"))

// memRealm is a project's mem: the realm in a goleveldb store on disk. The
// libraries it runs on stay in the base, in memory, and are never written
// to the store: a gnosh upgrade brings new library code.
type memRealm struct {
	b  *base
	db *goleveldb.GoLevelDB
	kv storetypes.CommitStore
	// clock is the time a call runs at: its Ops' time, and the user's
	// local date for days.
	clock func() time.Time
}

// memPackages are the packages a mem input may import, preloaded into the
// base: a mem call loads no package itself, since anything it loaded would
// be written into mem's store.
var memPackages = []string{memLibPath, "fmt", "strings", "strconv", "sort", "math", "errors", "time", "bytes", "unicode/utf8"}

// openMemRealm opens the mem in dir, deploying the realm into a new store.
func openMemRealm(b *base, dir string) (*memRealm, error) {
	if err := b.warmMem(); err != nil {
		return nil, err
	}
	db, err := goleveldb.NewGoLevelDB("mem", dir)
	if err != nil {
		return nil, err
	}
	r := &memRealm{b: b, db: db, kv: dbadapter.StoreConstructor(db, storetypes.StoreOptions{}), clock: time.Now}
	if err := r.deploy(); err != nil {
		db.Close()
		return nil, err
	}
	if err := r.b.preprocessMemRealm(); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *memRealm) close() error { return r.db.Close() }

// warmMem loads what every mem call runs on into this process's base: the
// libraries, compiled, and gno's shared byte cache of them (a mem call's
// transaction reads the libraries' objects and types from that cache, not
// from the store it writes to; filling it copies every library's bytes,
// about 100 ms). It is done once per process and touches no mem on disk,
// so a call does it before taking mem's lock: a new gnosh process pays for
// it without holding up the others (PARALLEL.md).
func (b *base) warmMem() error {
	if _, err := b.ensure(memPackages); err != nil {
		return err
	}
	if !b.memLibsCached {
		b.store.PopulateStdlibCache(libraryPaths())
		b.memLibsCached = true
	}
	return nil
}

// deploy runs the realm's package into the store when it is not there yet.
func (r *memRealm) deploy() (err error) {
	kv := cache.New(r.kv)
	tx := r.b.store.BeginTransaction(kv, kv, nil, nil)
	tx.SetPackageGetter(nil)
	if tx.GetPackage(memRealmPath, false) != nil {
		return nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("deploying mem's realm: %v", p)
		}
	}()
	m := gno.NewMachineWithOptions(gno.MachineOptions{
		Store:   tx,
		Context: memContext{ExecContext: execContext(0, time.Now()), caller: "klash"},
	})
	defer m.Release()
	m.RunMemPackage(memRealmMemPackage(), true)
	kv.Write()
	return nil
}

// preprocessMemRealm puts the realm's block nodes in the base, once: gno
// keeps a package's syntax trees in memory only, so every process rebuilds
// them, as a chain node does at restart, from gnosh's own copy of the source.
func (b *base) preprocessMemRealm() (err error) {
	if b.memRealmReady {
		return nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("preprocessing mem's realm: %v", p)
		}
	}()
	m := gno.NewMachineWithOptions(gno.MachineOptions{Store: b.store})
	defer m.Release()
	mpkg := memRealmMemPackage()
	fset := m.ParseMemPackage(mpkg)
	pn := gno.NewPackageNode(gno.Name(mpkg.Name), mpkg.Path, fset)
	b.store.SetBlockNode(pn)
	gno.PredefineFileSet(b.store, pn, fset)
	for _, fn := range fset.Files {
		fn = gno.Preprocess(b.store, pn, fn).(*gno.FileNode)
		gno.SaveBlockNodes(b.store, fn)
	}
	if pn.FileSet == nil {
		pn.FileSet = fset
	}
	b.memRealmReady = true
	return nil
}

// memCall is what one mem input did.
type memCall struct {
	Output string
	Ops    []memOp
	// Frames is the frames the input pushed and popped, in order; NilPush
	// is how many of them had come when it last pushed nil, -1 for never
	// (the hint after a pop is left out when a nil push followed it).
	Frames  []MemFrame
	NilPush int
	// Waits is what the input's Next waits for, when it waits; nil
	// otherwise (PARALLEL.md).
	Waits []MemWait
	// At is the call's time, which its Ops share.
	At int64
	// batch is what the call wrote, held back from the store until commit;
	// nil for a read.
	batch storetypes.Store
}

// memStamp is what a store holds, kept in it under memStampKey and written
// in the batch of each change: the layout and the gno version it was built
// under, and the journal's generation and the entry it holds, with where
// that entry's line ends and the line's hash.
type memStamp struct {
	Layout     int    `json:"layout"`
	Gno        string `json:"gno"`
	Generation string `json:"generation"`
	Seq        int64  `json:"seq"`
	End        int64  `json:"end"`
	Hash       string `json:"hash,omitempty"`
	// Unchecked marks a store being built afresh from the journal, whose
	// rebuild has not been checked yet (memHost.checkRebuild): one cut
	// across calls is checked by the call that finishes it.
	Unchecked bool `json:"unchecked,omitempty"`
}

// memStampKey keeps the stamp beside gno's own keys.
var memStampKey = []byte("klash/stamp")

// stamp is the store's stamp; ok is false for a store that has none yet.
func (r *memRealm) stamp() (st memStamp, ok bool, err error) {
	b := r.kv.Get(nil, memStampKey)
	if b == nil {
		return memStamp{}, false, nil
	}
	return st, true, json.Unmarshal(b, &st)
}

// setStamp writes the store's stamp alone.
func (r *memRealm) setStamp(st memStamp) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	r.kv.Set(nil, memStampKey, b)
	return nil
}

// commit writes what call changed to the store, with the stamp the store
// then holds, in one batch: nothing of a call reaches the store before.
func (r *memRealm) commit(call memCall, st memStamp) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	call.batch.Set(nil, memStampKey, b)
	call.batch.Write()
	return nil
}

// apply applies ops, each made at its time in ats, through gnomem's Apply,
// which records nothing: they are the journal's already. It may take as
// long as a batch of a rebuild needs.
func (r *memRealm) apply(ops []memOp, ats []int64) (memCall, error) {
	if ops == nil {
		// Entries an erasure emptied: none of their Ops is left, but the
		// store's stamp moves past them, so the call must still be a write.
		ops = []memOp{}
	}
	return r.call("mem.Apply()", memContext{caller: "klash", pending: ops, pendingAt: ats, limit: memLimit{gas: maxGas, wall: 10 * time.Minute}})
}

// What a mem call may use, unless its memContext says otherwise: about ten
// seconds of work, a minute of wall clock, and 128 MB.
const (
	memGas      = 10_000_000_000
	memWall     = time.Minute
	memCapBytes = 128 << 20
)

// memLimit bounds a mem call: its gas, how long it may take, and what
// cancels it. Zero values are the defaults above.
type memLimit struct {
	ctx  context.Context
	gas  int64
	wall time.Duration
}

// call runs a mem input, shaped as a program's main (shapeMemInput), by
// ctx's caller, within ctx's limit. A failed input keeps nothing, and an
// input that made no Ops is a read: it must have written nothing either,
// since only gnomem's do changes mem. What a call that made Ops wrote
// waits in its batch for commit.
func (r *memRealm) call(code string, ctx memContext) (call memCall, err error) {
	src, err := shapeMemInput(code)
	if err != nil {
		return call, err
	}
	lim := ctx.limit
	if lim.ctx == nil {
		lim.ctx = context.Background()
	}
	if lim.gas == 0 {
		lim.gas = memGas
	}
	if lim.wall == 0 {
		lim.wall = memWall
	}
	meter := &callMeter{}
	meter.begin(lim.ctx, lim.gas, time.Now().Add(lim.wall))
	defer meter.end()
	kv := cache.New(r.kv)
	wc := &writeCounter{Store: kv}
	tx := r.b.store.BeginTransaction(wc, wc, nil, nil)
	tx.SetPackageGetter(nil)
	var out bytes.Buffer
	now := r.clock()
	call.At = now.Unix()
	ctx.ExecContext = execContext(0, now)
	ctx.OriginCaller = memRunAddr.Bech32()
	ctx.ops = &call.Ops
	ctx.frames = &call.Frames
	call.NilPush = -1
	ctx.nilPush = &call.NilPush
	ctx.waits = &call.Waits
	if ctx.today == "" {
		ctx.today = now.Format(time.DateOnly)
	}
	mpkg, bodyLine := memInput(src)
	oneLine := !strings.Contains(strings.TrimSpace(code), "\n")
	fail := func(msg string) {
		// What the input printed before it failed may read as done; say
		// plainly that none of it was kept.
		if len(call.Ops) > 0 {
			msg += "; nothing was kept" + memNotKept(call.Ops)
		}
		call.Ops = nil
		call.Frames = nil
		err = errors.New(msg)
	}
	defer func() {
		call.Output = out.String()
		if p := recover(); p != nil {
			_, inside := p.(runtime.Error)
			inside = inside && memOnlyCalls(code)
			if inside {
				slog.Error("mem failed inside gnosh", "error", p, "stack", string(debug.Stack()))
			}
			if terr := meter.tripped(); terr != nil {
				fail(classifyTrip(terr).Error())
			} else if inside {
				fail(memInsideText(p))
			} else if msg := memPanicText(p, bodyLine, oneLine); memCommaJoined(code) && strings.Contains(msg, "expected 1 expression") {
				fail(msg + " (mem's calls go one per line, with no comma after them)")
			} else {
				fail(msg)
			}
		}
	}()
	opts := gno.MachineOptions{Output: &out, Store: tx, Context: ctx, BoundedPanicRender: true, GasMeter: meter, Alloc: gno.NewAllocator(memCapBytes)}
	m := gno.NewMachineWithOptions(opts)
	defer m.Release()
	_, pv := m.RunMemPackage(mpkg, false)
	m2 := gno.NewMachineWithOptions(opts)
	defer m2.Release()
	m2.SetActivePackage(pv)
	m2.RunMain()
	if terr := meter.tripped(); terr != nil {
		// A trip the GnoVM recovered by itself still fails the call.
		fail(classifyTrip(terr).Error())
		return call, err
	}
	if len(call.Ops) == 0 && ctx.pending == nil {
		if wc.writes > 0 {
			return call, fmt.Errorf("gnomem: an input that made no Ops wrote %d keys", wc.writes)
		}
		return call, nil
	}
	call.batch = kv
	return call, nil
}

// memPanicText is a failed mem input's refusal or error as the model reads
// it: gno's panic text on one line, without its wrapping or the generated
// package's path. A compile error's place is dropped for a one-line input,
// and given as the input's line otherwise; bodyLine is the line of the
// generated main.gno that the input's statements start on.
func memPanicText(p any, bodyLine int, oneLine bool) string {
	msg := fmt.Sprint(p)
	if isAllocLimit(msg) {
		return (&limitError{kind: "memory"}).Error()
	}
	for _, prefix := range []string{"unhandled panic: ", "panic: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.ReplaceAll(msg, "klash.local/e/"+memRunAddr.String()+"/run/", "")
	if m := memPlaceRe.FindStringSubmatch(msg); m != nil {
		place := ""
		if line, err := strconv.Atoi(m[1]); err == nil && !oneLine && line >= bodyLine {
			place = "line " + strconv.Itoa(line-bodyLine+1) + ": "
		}
		msg = place + strings.TrimSuffix(msg[len(m[0]):], ":")
	}
	// gno's words for a call chained off one that returns nothing, as
	// mem.Fact(…).Fact(…), and Go's rule that models forget: a call over
	// several lines ends each line with a comma, the last one too.
	msg = strings.Replace(msg, "unexpected selector expression type *gnolang.tupleType",
		"only mem.Todo(…) and mem.Frame(…) chain (.Todo, .Wait, .Ask, .Push): mem's other calls return nothing, so put each on its own line", 1)
	if strings.Contains(msg, "missing ',' before newline in argument list") {
		msg += " (a call over several lines needs a comma after its last argument too, as in Go)"
	}
	// gno names the realm's variable mem<VPBlock(3,1)>; and a call of
	// mem's or a Goal's with the wrong number of arguments is told what it
	// takes (memArgCount).
	msg = memVPBlockRe.ReplaceAllString(msg, "mem")
	if m := memArgCountRe.FindStringSubmatch(msg); m != nil {
		if said := memArgCount(m[1], m[2], m[3], m[4]); said != "" {
			msg = said
		}
	}
	return msg
}

var (
	memVPBlockRe  = regexp.MustCompile(`mem<VPBlock\(\d+,\d+\)>`)
	memArgCountRe = regexp.MustCompile(`wrong argument count in call to (.+)\.(\w+); want (\d+) got (\d+)`)
)

// memInsideText is what a mem input that only called mem is told when Go's
// own runtime failed under it, in gnosh or the GnoVM: the input did nothing
// wrong. Told only "runtime error: index out of range [11] with length
// 10", the agent in Jae's lane took it for its own to fix, and spent seven
// minutes searching the disk, reading mem's files and the binary's
// strings, and trying to drop mem, until he cancelled it (2026-10-30).
func memInsideText(p any) string {
	return "mem failed inside gnosh, not in your call: " + fmt.Sprint(p) + ". This is a bug in gnosh: tell the user what failed, and go on without mem; do not look through mem's files or the system to repair it"
}

// memOnlyCalls reports whether a mem input only calls mem: each statement
// a call on mem, or a chain of calls off one, with literal arguments,
// perhaps printed. Nothing of its own could fail.
func memOnlyCalls(code string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\nfunc _() {\n"+code+"\n}\n", parser.SkipObjectResolution)
	if err != nil || len(f.Decls) != 1 {
		return false
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || len(fn.Body.List) == 0 {
		return false
	}
	for _, s := range fn.Body.List {
		e, ok := s.(*ast.ExprStmt)
		if !ok || !onMem(e.X) {
			return false
		}
	}
	return true
}

// onMem reports whether x is mem, a call on it with literal arguments, a
// chain of such calls, or print or println of them.
func onMem(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name == "mem"
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
			for _, a := range x.Args {
				if !onMem(a) {
					return false
				}
			}
			return len(x.Args) > 0
		}
		for _, a := range x.Args {
			if _, ok := a.(*ast.BasicLit); !ok {
				return false
			}
		}
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && onMem(sel.X)
	}
	return false
}

// memNotKept names what a failed input's receipts said it added, and the
// facts and rules they said it set, for its refusal: ", so `ship`, added
// above, is not in mem". A model took a fact whose receipt it saw for kept.
func memNotKept(ops []memOp) string {
	var added, set []string
	for _, op := range ops {
		switch {
		case op.Kind == "add":
			added = append(added, "`"+op.Name+"`")
		case (op.Kind == "fact" || op.Kind == "rule") && op.New != "" && !slices.Contains(set, op.Kind+" `"+op.Name+"`"):
			set = append(set, op.Kind+" `"+op.Name+"`")
		}
	}
	list := func(names []string) string {
		if len(names) > 3 {
			names = append(names[:3:3], "the rest")
		}
		if len(names) == 1 {
			return names[0]
		}
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	switch {
	case len(added) > 0 && len(set) > 0:
		return ", so " + list(added) + " (added above) and " + list(set) + " (set above) are not in mem"
	case len(added)+len(set) == 0:
		return ""
	}
	names, verb := added, "added"
	if len(set) > 0 {
		names, verb = set, "set"
	}
	if len(names) == 1 {
		return ", so " + names[0] + ", " + verb + " above, is not in mem"
	}
	return ", so " + list(names) + ", " + verb + " above, are not in mem"
}

// memPlaceRe is where gno puts a compile error: main.gno:5:1-8: .
var memPlaceRe = regexp.MustCompile(`^main\.gno:(\d+):[\d:-]+: `)

// writeCounter counts the writes a call makes to its store.
type writeCounter struct {
	storetypes.Store
	writes int
}

func (w *writeCounter) Set(gctx *storetypes.GasContext, key, value []byte) {
	w.writes++
	w.Store.Set(gctx, key, value)
}

func (w *writeCounter) Delete(gctx *storetypes.GasContext, key []byte) {
	w.writes++
	w.Store.Delete(gctx, key)
}

// memInput is a mem input as gno.land runs a MsgRun: a private package at
// the run path, whose main is the input's, with mem bound to the realm's
// Mem. It also returns the line of main.gno that main's statements start
// on.
func memInput(src memSource) (*std.MemPackage, int) {
	path := "klash.local/e/" + memRunAddr.String() + "/run"
	mod := &gnomod.File{Module: path, Gno: gno.GnoVerLatest, Private: true}
	var main strings.Builder
	main.WriteString("package main\n\n")
	for _, imp := range src.imports {
		main.WriteString("import " + imp + "\n")
	}
	for _, d := range src.decls {
		main.WriteString("\n" + d + "\n")
	}
	main.WriteString("\nfunc main() {\n")
	bodyLine := strings.Count(main.String(), "\n") + 1
	main.WriteString(src.main + "}\n")
	mpkg := &std.MemPackage{
		Type: gno.MPUserProd,
		Name: "main",
		Path: path,
		Files: []*std.MemFile{
			{Name: "main.gno", Body: main.String()},
			{Name: "mem.gno", Body: memBinding},
		},
	}
	mpkg.SetFile("gnomod.toml", mod.WriteString())
	mpkg.Sort()
	return mpkg, bodyLine
}

// memBinding is the file gnosh adds to every mem input: mem, bound to the
// realm's Mem, and how a trailing read of it is shown.
const memBinding = `package main

import (
	gnomem "` + memLibPath + `"
	memr "` + memRealmPath + `"
)

var mem = memr.M

// showMem_ prints what a trailing read of mem returned, ending its line.
func showMem_(s string) {
	if s != "" && s[len(s)-1] != '\n' {
		s += "\n"
	}
	print(s)
}

// showRecall_ prints what mem.Recall found, one a line.
func showRecall_(found []gnomem.Namable) {
	if len(found) == 0 {
		print("nothing in mem is about that: mem.Show() lists what it holds, and mem.Find(\"words\") matches words\n")
		return
	}
	for _, x := range found {
		showMem_(x.String())
	}
}
`

func memRealmMemPackage() *std.MemPackage {
	mod := &gnomod.File{Module: memRealmPath, Gno: gno.GnoVerLatest}
	mpkg := &std.MemPackage{
		Type:  gno.MPUserProd,
		Name:  "mem",
		Path:  memRealmPath,
		Files: []*std.MemFile{{Name: "mem.gno", Body: memRealmSource}},
	}
	mpkg.SetFile("gnomod.toml", mod.WriteString())
	mpkg.Sort()
	return mpkg
}
