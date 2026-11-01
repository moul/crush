// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

// Package gnovm runs named Gno virtual machines for gnosh: each with a
// memory cap and a gas limit per call, its state kept as a journal of the
// calls that succeeded, persisted on request, and evicted from memory when
// the budget needs the room. DESIGN.md has the design and its reviews.
package gnovm

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
)

const (
	// MemVM is the name the tools give mem, the agent's memory: not a VM
	// but a realm on disk (memhost.go), made when first used.
	MemVM = "mem"

	scratchPrefix = "scratch-"
	// maxNamedVMs is how many named VMs a project may have; scratch VMs
	// do not count.
	maxNamedVMs = 64
	// maxJournal is the largest journal a VM may grow to.
	maxJournal = 8 << 20

	// Declarations keep their syntax trees in memory and make each later
	// declaration slower to add, so they are capped per call and per VM.
	maxDeclPerEntry = 32 << 10
	maxDeclPerVM    = 256 << 10
	maxDeclFiles    = 2000

	defaultMemoryMB = 16
	maxMemoryMB     = 64
	defaultGas      = 1_000_000_000
	maxGas          = 100_000_000_000

	// lockWait is the least a run waits for a VM another gnosh process is
	// using: another process's call, or its replay of a VM, can outlast a
	// short run's own wall-clock limit on a busy machine.
	lockWait = 30 * time.Second
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// A VM is rebuilt by replaying its journal, on one thread that every session
// shares, so what a journal may hold is bounded by what it costs to replay
// as well as by its size: a call that ran longer than maxEntryWall is not
// kept, and a VM whose kept calls ran for maxReplayWall in all keeps no more.
// (Variables only so tests can shorten them.)
var (
	maxEntryWall  = 30 * time.Second
	maxReplayWall = 120 * time.Second
)

// Options configures a Host.
type Options struct {
	// Dir is the data directory, <XDG data>/gnosh/gnovm.
	Dir string
	// Client names the gnosh process the host serves, for its memory-only
	// VMs, which live until that process exits; NewClientID makes one.
	Client string
	// BudgetBytes caps the memory the VMs may use; 0 picks it from the
	// host's free memory.
	BudgetBytes int64
	// Defaults for new VMs; zero values mean the built-in defaults.
	DefaultMemoryMB int
	DefaultGas      int64
	NsPerGas        float64
	// Probe reads the host's memory; nil means ProbeHostMemory.
	Probe func() (HostMemory, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Embeddings is the embedding server mem finds by meaning with, an
	// OpenAI-compatible base URL such as http://127.0.0.1:8091/v1. mem's
	// text, and what the user types, are sent there. "" finds by words
	// alone, and sends nothing anywhere.
	Embeddings string
}

// NewClientID returns a fresh id for a gnosh process.
func NewClientID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b[:])
}

// VMOptions are a VM's settings, kept in its machine.json.
type VMOptions struct {
	MemoryMB    int     `json:"memory_mb"`
	Gas         int64   `json:"gas"`
	NsPerGas    float64 `json:"ns_per_gas"`
	Persist     bool    `json:"persist"`
	Pinned      bool    `json:"pinned,omitempty"`
	Description string  `json:"description,omitempty"`
}

type machineFile struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	VMOptions
}

// Host owns the VMs of one gnosh process. It runs one operation at a time:
// the GnoVM is single-threaded, and its stores are not safe to share.
type Host struct {
	mu      sync.Mutex
	opts    Options
	base    *base
	vms     map[string]*vmState // by project key + "/" + name
	mems    map[string]*memHost // by directory (memDir)
	clock   int64               // for least-recently-used eviction
	release func()              // drops the client lock, if this host holds it
	swept   map[string]bool     // project keys whose dead clients were swept
}

// vmState is what the host knows of one VM.
type vmState struct {
	key     string // project key + "/" + name
	project string // project key
	name    string
	dir     string
	opts    VMOptions
	created time.Time
	j       *journal
	res     *vm
	lastUse int64
	used    time.Time
	broken  error
	// brokenAt is the entry a replay failed at, if that is why it broke,
	// in the journal generation brokenGen: a repair elsewhere makes it
	// stale.
	brokenAt  int64
	brokenGen string
	// cost sums the journal's entries.
	cost journalCost
}

// journalCost is what a journal's entries add up to: the gas and wall time
// they ran for, and the declarations and packages a replay of them keeps in
// memory.
type journalCost struct {
	gas       int64
	wall      time.Duration
	declBytes int64
	declFiles int
	imports   int
	paths     map[string]bool
}

func (c *journalCost) add(e *entry) {
	c.gas += e.Gas
	c.wall += time.Duration(e.WallNS)
	for _, o := range e.Ops {
		c.declBytes += o.declBytes()
		if o.File != "" {
			c.declFiles++
		}
		if o.Import != nil {
			if c.paths == nil {
				c.paths = map[string]bool{}
			}
			c.paths[o.Import.Path] = true
		}
	}
	c.imports = len(c.paths)
}

func journalStats(entries []entry) journalCost {
	var c journalCost
	for i := range entries {
		c.add(&entries[i])
	}
	return c
}

func (s *vmState) persistent() bool { return s.opts.Persist }

// NewHost makes a host. It takes the client's lock, unless the client
// process already holds it (a worker's client does), and sweeps the
// memory-only VMs of gnosh processes that are gone as it meets each
// project.
func NewHost(opts Options) (*Host, error) {
	if opts.Dir == "" || opts.Client == "" {
		return nil, errors.New("gnovm: Dir and Client are required")
	}
	if opts.DefaultMemoryMB <= 0 {
		opts.DefaultMemoryMB = defaultMemoryMB
	}
	opts.DefaultMemoryMB = min(opts.DefaultMemoryMB, maxMemoryMB)
	if opts.DefaultGas <= 0 {
		opts.DefaultGas = defaultGas
	}
	if opts.NsPerGas <= 0 {
		opts.NsPerGas = 1
	}
	if opts.Probe == nil {
		opts.Probe = ProbeHostMemory
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := &Host{opts: opts, vms: map[string]*vmState{}, swept: map[string]bool{}}
	if err := mkdirSync(filepath.Join(opts.Dir, ".clients")); err != nil {
		return nil, err
	}
	release, err := lock.TryFile(h.clientLock(opts.Client))
	switch {
	case err == nil:
		h.release = release
	case !errors.Is(err, lock.ErrContended):
		return nil, err
	}
	return h, nil
}

func (h *Host) clientLock(client string) string {
	return filepath.Join(h.opts.Dir, ".clients", client+".lock")
}

// Close drops every VM from memory and deletes this client's memory-only
// VMs.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, s := range h.vms {
		s.res = nil
		delete(h.vms, key)
	}
	// Memory-only VMs end with the gnosh process, which holds the client
	// lock: a worker's host leaves them for the next worker, and the
	// client deletes them when it closes (deleteEphemeral).
	if h.release != nil {
		deleteEphemeral(h.opts.Dir, h.opts.Client)
		h.release()
		_ = os.Remove(h.clientLock(h.opts.Client))
		h.release = nil
	}
	return nil
}

// deleteEphemeral deletes a client's memory-only VMs, in every project.
func deleteEphemeral(dir, client string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*", ".ephemeral", client))
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
}

// projectKey names a project's directory: its path, made safe, and a hash of
// it, so two projects never share one.
func projectKey(project string) string {
	clean := filepath.Clean(project)
	sum := sha256.Sum256([]byte(clean))
	var b strings.Builder
	for _, r := range strings.Trim(clean, string(filepath.Separator)) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 48 {
		s = s[len(s)-48:]
	}
	return s + "-" + hex.EncodeToString(sum[:4])
}

// ScratchName is the name of a session's scratch VM.
func ScratchName(session string) string {
	sum := sha256.Sum256([]byte(session))
	return scratchPrefix + hex.EncodeToString(sum[:4])
}

func (h *Host) projectDir(project string) string { return filepath.Join(h.opts.Dir, project) }
func (h *Host) ephemeralRoot(project string) string {
	return filepath.Join(h.projectDir(project), ".ephemeral", h.opts.Client)
}

func (h *Host) persistDir(project, name string) string {
	return filepath.Join(h.projectDir(project), "vms", name)
}

func (h *Host) lockPath(project, name string) string {
	return filepath.Join(h.projectDir(project), "locks", name+".lock")
}

// sweep deletes the memory-only VMs of gnosh processes that are gone: their
// client lock is free or missing.
func (h *Host) sweep(project string) {
	if h.swept[project] {
		return
	}
	h.swept[project] = true
	root := filepath.Join(h.projectDir(project), ".ephemeral")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		client := e.Name()
		if client == h.opts.Client {
			continue
		}
		path := h.clientLock(client)
		if _, err := os.Stat(path); err == nil {
			release, err := lock.TryFile(path)
			if err != nil {
				continue // alive
			}
			_ = os.RemoveAll(filepath.Join(root, client))
			_ = os.Remove(path)
			release()
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, client))
	}
}

// lookup finds a VM's state: one this host knows, a persistent one on disk,
// or this client's memory-only one on disk (from before a worker restart).
// It returns nil if there is none.
//
// A name becomes a path, so it is checked here, where every entry point
// looks a VM up: "./goals" or "../../<project>/vms/x" must not reach the goals
// VM or another project's. This client's memory-only VM comes before a
// persistent VM of the same name, and whether a VM is persistent follows from
// where it lives, not from its machine.json, which a crash while making it
// persistent can leave behind.
func (h *Host) lookup(project, name string) (*vmState, error) {
	if !nameRe.MatchString(name) {
		return nil, &refusal{fmt.Sprintf("%q is not a VM name: a lowercase letter, then up to 31 lowercase letters, digits, - or _", name)}
	}
	key := project + "/" + name
	if s, ok := h.vms[key]; ok {
		return s, nil
	}
	h.sweep(project)
	for _, dir := range []string{filepath.Join(h.ephemeralRoot(project), name), h.persistDir(project, name)} {
		mf, err := readMachineFile(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mf.Persist = dir == h.persistDir(project, name)
		s := &vmState{key: key, project: project, name: name, dir: dir, opts: mf.VMOptions, created: mf.Created}
		h.vms[key] = s
		return s, nil
	}
	return nil, nil
}

func readMachineFile(dir string) (machineFile, error) {
	var mf machineFile
	b, err := os.ReadFile(filepath.Join(dir, "machine.json"))
	if err != nil {
		return mf, err
	}
	if err := json.Unmarshal(b, &mf); err != nil {
		return mf, fmt.Errorf("%s: %w", filepath.Join(dir, "machine.json"), err)
	}
	return mf, nil
}

func (s *vmState) writeMachineFile() error {
	b, err := json.MarshalIndent(machineFile{Name: s.name, Created: s.created, VMOptions: s.opts}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(s.dir, "machine.json"), append(b, '\n'))
}

// create makes a VM on disk: its directory, machine.json and an empty
// journal, built in a temporary directory and renamed into place, so a
// crash never leaves half a VM, and two processes making the same
// persistent VM at once cannot both succeed.
func (h *Host) create(project, name string, opts VMOptions) (*vmState, error) {
	s := &vmState{key: project + "/" + name, project: project, name: name, opts: opts, created: h.opts.Now().UTC()}
	if opts.Persist {
		s.dir = h.persistDir(project, name)
	} else {
		s.dir = filepath.Join(h.ephemeralRoot(project), name)
	}
	parent := filepath.Dir(s.dir)
	if err := mkdirSync(parent); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(parent, ".new-"+name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	final := s.dir
	s.dir = tmp
	if _, err := createJournal(filepath.Join(tmp, "journal.jsonl"), h.opts.Now()); err != nil {
		return nil, err
	}
	if err := s.writeMachineFile(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			return nil, &refusal{fmt.Sprintf("another gnosh process made a VM named %s just now; use it, or pick another name", name)}
		}
		return nil, err
	}
	if err := syncDir(parent); err != nil {
		return nil, err
	}
	s.dir = final
	j, _, _, err := readJournal(filepath.Join(final, "journal.jsonl"))
	if err != nil {
		return nil, err
	}
	s.j = j
	h.vms[s.key] = s
	return s, nil
}

// lockVM takes a persistent VM's cross-process lock, waiting while another
// gnosh process holds it, until ctx is done. Memory-only VMs belong to one
// gnosh process and need none.
func (h *Host) lockVM(ctx context.Context, s *vmState) (func(), error) {
	if !s.persistent() {
		return func() {}, nil
	}
	if err := mkdirSync(filepath.Dir(h.lockPath(s.project, s.name))); err != nil {
		return nil, err
	}
	path := h.lockPath(s.project, s.name)
	for {
		release, err := lock.TryFile(path)
		if err == nil {
			_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
			return release, nil
		}
		if !errors.Is(err, lock.ErrContended) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			holder := "another gnosh process"
			if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
				holder = "gnosh process " + strings.TrimSpace(string(b))
			}
			return nil, &busyError{msg: fmt.Sprintf("VM %s is in use by %s; try again shortly", s.name, holder)}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// busyError is a VM that cannot be used right now; trying again later may
// work. again marks mem's catch-up whose slice is spent: the work goes on
// with the next call, which the client makes at once, never the model.
type busyError struct {
	msg   string
	again bool
}

func (e *busyError) Error() string { return e.msg }

// sync brings this host's view of a VM up to date with its files, under the
// VM's lock: it reads the journal the first time, cuts a torn tail another
// process left, notices options another process changed, and catches the
// resident VM up with entries another process appended, or drops it if the
// journal was rewritten.
func (h *Host) sync(ctx context.Context, s *vmState) error {
	if s.persistent() {
		mf, err := readMachineFile(s.dir)
		if errors.Is(err, os.ErrNotExist) {
			// Deleted by another process.
			delete(h.vms, s.key)
			return &refusal{fmt.Sprintf("there is no VM named %s; it was deleted", s.name)}
		}
		if err != nil {
			return err
		}
		if mf.MemoryMB != s.opts.MemoryMB || !mf.Created.Equal(s.created) {
			// Its cap changed, or it was deleted and made again.
			s.res, s.j = nil, nil
		}
		mf.Persist = true // it lives in vms/
		s.opts, s.created = mf.VMOptions, mf.Created
	}
	if s.j == nil {
		j, entries, torn, err := readJournal(filepath.Join(s.dir, "journal.jsonl"))
		if err != nil {
			s.broken = err
			return &brokenError{vm: s.name, cause: err}
		}
		if torn {
			if err := j.cutTail(); err != nil {
				return err
			}
		}
		s.j, s.broken, s.brokenAt, s.brokenGen = j, nil, 0, ""
		s.cost = journalStats(entries)
		s.res = nil
		return nil
	}
	if !s.persistent() {
		return nil
	}
	appended, rewritten, err := s.j.changed()
	if err != nil {
		return err
	}
	if rewritten {
		s.j, s.res = nil, nil
		return h.sync(ctx, s)
	}
	if !appended {
		return nil
	}
	tail, torn, err := s.j.readFrom()
	if err != nil {
		s.broken = err
		return &brokenError{vm: s.name, cause: err}
	}
	for _, t := range tail {
		s.cost.add(&t.entry)
		s.j.saw(t)
	}
	// The resident VM, if any, catches up in resident(), within the
	// caller's budget like any replay.
	if torn {
		return s.j.cutTail()
	}
	return nil
}

// resident returns the VM in memory, brought up to its journal's end: made
// and admitted to the budget first if it is not resident (evicting others
// if need be), then replayed. A replay runs in slices: it stops between
// entries once the caller's budget is spent, after at least one entry, and
// keeps what it built, so the next call carries on from there and every
// call makes progress without holding the host for long.
func (h *Host) resident(ctx context.Context, s *vmState, budget time.Duration) (*vm, []string, error) {
	if s.broken != nil {
		return nil, nil, &brokenError{vm: s.name, cause: s.broken}
	}
	var notes []string
	if s.res != nil && s.res.unusable {
		s.res = nil
	}
	if s.res == nil {
		evicted, err := h.admit(s)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range evicted {
			notes = append(notes, "evicted VM "+name+" from memory to make room (it is rebuilt when next used)")
		}
		s.res = newVM(h.base, int64(s.opts.MemoryMB)<<20)
	}
	if s.res.seq == s.j.seq {
		return s.res, notes, nil
	}
	tail, _, err := readEntries(s.j.path, s.res.offset, s.res.seq)
	if err != nil {
		return nil, notes, err
	}
	var todo []tailEntry
	var paths []string
	for _, t := range tail {
		if t.Seq > s.j.seq {
			break
		}
		todo = append(todo, t)
		for _, o := range t.Ops {
			if o.Import != nil {
				paths = append(paths, o.Import.Path)
			}
		}
	}
	if _, err := h.base.ensure(paths); err != nil {
		s.broken = fmt.Errorf("replay: %w", err)
		s.res = nil
		return nil, notes, s.broken
	}
	if s.res.seq == 0 && s.j.gno != "" && s.j.gno != stdlibVersion() {
		notes = append(notes, fmt.Sprintf("VM %s's journal was written under gno %s and is replayed under gno %s; if its state looks wrong, rebuild it in a new VM",
			s.name, s.j.gno, stdlibVersion()))
	}
	start := time.Now()
	for i := range todo {
		if i > 0 && time.Since(start)+3*time.Duration(todo[i].WallNS) > budget {
			return nil, notes, &busyError{msg: fmt.Sprintf(
				"VM %s is being rebuilt from its journal (%d of %d entries done in %s); call again to continue",
				s.name, i, len(todo), time.Since(start).Round(time.Millisecond))}
		}
		if err := h.replayEntry(ctx, s, &todo[i].entry); err != nil {
			var lim *limitError
			if errors.As(err, &lim) && (lim.kind == "cancelled" || lim.kind == "wall") {
				// Cancelled, or slowed past its safety deadline (a
				// stalled CPU, a stopped process): not the journal's
				// fault. The entry cut short left the VM unusable.
				s.res = nil
				if lim.kind == "wall" {
					return nil, notes, &busyError{msg: fmt.Sprintf("rebuilding VM %s ran slower than expected at entry %d; call again", s.name, todo[i].Seq)}
				}
				return nil, notes, err
			}
			s.broken = fmt.Errorf("replaying entry %d: %v", todo[i].Seq, err)
			s.brokenAt, s.brokenGen = todo[i].Seq, s.j.generation
			s.res = nil
			return nil, notes, s.broken
		}
		s.res.offset = todo[i].end
	}
	if len(todo) > 0 {
		notes = append(notes, fmt.Sprintf("rebuilt VM %s from %d journal entries in %s", s.name, len(todo), time.Since(start).Round(time.Millisecond)))
	}
	return s.res, notes, nil
}

// replayEntry applies one journaled entry to the resident VM, without a gas
// limit: it ran within one when it was journaled, and its meaning must not
// change. Its wall-clock limit is ten times what it took then, and 5 s
// more, only to stop a replay that has somehow stopped matching the run.
func (h *Host) replayEntry(ctx context.Context, s *vmState, e *entry) error {
	deadline := time.Now().Add(10*time.Duration(e.WallNS) + 5*time.Second)
	if _, _, err := s.res.apply(ctx, e, math.MaxInt64/4, deadline, false); err != nil {
		return err
	}
	s.res.seq = e.Seq
	return nil
}

func (h *Host) touch(s *vmState) {
	h.clock++
	s.lastUse = h.clock
	s.used = h.opts.Now()
}

func (h *Host) ensureBase() {
	if h.base == nil {
		h.base = newBase()
	}
}

// wallLimit is a call's wall-clock limit: three times what its gas should
// take at the VM's ns per gas, at least 5 s and at most 10 min.
func wallLimit(gas int64, nsPerGas float64) time.Duration {
	d := time.Duration(3 * float64(gas) * nsPerGas)
	return min(max(d, 5*time.Second), 10*time.Minute)
}

// RunRequest is one gnovm_run call.
type RunRequest struct {
	// Project is the project's directory; VMs belong to a project.
	Project string
	// Session is the calling session's id, for its scratch VM.
	Session string
	// Name is the VM; "" means the session's scratch VM, or, for code
	// with a package clause, a fresh VM for just this call.
	Name string
	Code string
	// Gas is the call's gas limit; 0 means the VM's.
	Gas int64
	// Reset starts a memory-only VM over before running.
	Reset bool
	// Req identifies the call, so its outcome can be asked about later.
	Req string
	// Goal is the goal in mem the user's /goal binds the session to, which
	// the session's own mem calls may not drop or forget.
	Goal string `json:",omitempty"`
	// Frames is the session's stack of frames in mem, bottom to top: the
	// frames it has pushed and not popped (FRAMES.md), which the agent
	// derives from its own rows.
	Frames []string `json:",omitempty"`
	// Names are names made for the texts the call keeps without naming
	// them (mem.Think(text), mem.Fact(text)), by text: the agent asks its
	// small model before the call; the realm falls back to the words.
	Names map[string]string `json:",omitempty"`
}

// RunResult is what a call did.
type RunResult struct {
	VM        string
	Value     string
	Output    string
	Gas       int64
	GasLimit  int64
	NsPerGas  float64
	Wall      time.Duration
	MemUsed   int64
	MemCap    int64
	Journaled bool
	Seq       int64
	// Program is set when the code ran as a program in a fresh VM that
	// was thrown away after.
	Program bool
	Notes   []string
	// MemGoals are the goals a call to mem changed, as its session sees
	// them, for the session's task list.
	MemGoals []MemGoal `json:",omitempty"`
	// Erased is what a call to mem erased, for gnosh's own copies of mem's
	// text (gnovm_run hands it to OnMemErased).
	Erased *MemErasure `json:",omitempty"`
	// Rules are the rules a call to mem set, changed or removed, which the
	// user is told of (gnovm_run hands them to OnMemRules): every session
	// is given them as the user's own words.
	Rules []MemRule `json:",omitempty"`
	// Frames are the frames a call to mem pushed and popped, in order
	// (gnovm_run keeps them with its result and hands them to OnMemFrames).
	Frames []MemFrame `json:",omitempty"`
	// Err is the run's failure; nothing from the call was kept. (The wire
	// carries it apart, keeping its kind; see wireRun.)
	Err error `json:"-"`
}

// Run runs code in a VM. The error is for calls that could not be run at
// all (a refusal, a busy or broken VM); a failure of the code itself is in
// RunResult.Err.
func (h *Host) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	project := projectKey(req.Project)
	if req.Gas < 0 || req.Gas > maxGas {
		return nil, &refusal{fmt.Sprintf("gas must be between 1 and %d", int64(maxGas))}
	}

	name := req.Name
	if name == MemVM {
		return h.runMem(ctx, req)
	}
	if name == "scratch" && req.Session != "" {
		name = ScratchName(req.Session)
	}
	if name == "" {
		prog, err := normalize(req.Code, 1, nil, nil)
		if err != nil {
			return nil, err
		}
		if prog.pkgClause {
			return h.runProgram(ctx, req)
		}
		name = ScratchName(req.Session)
	}
	scratch := name == ScratchName(req.Session)
	var notes []string

	s, err := h.lookup(project, name)
	if err != nil {
		return nil, err
	}
	switch {
	case s == nil && scratch:
		s, err = h.create(project, name, h.defaultOptions())
		if err != nil {
			return nil, err
		}
		notes = append(notes, "new scratch VM "+name+": memory only, it ends with this gnosh process; name a VM with gnovm_new to keep state")
	case s == nil:
		return nil, &refusal{fmt.Sprintf("there is no VM named %s; make it with gnovm_new, or leave out name to use your scratch VM", name)}
	case req.Reset && s.persistent():
		return nil, &refusal{fmt.Sprintf("VM %s is persistent, so reset would destroy saved state; delete it with gnovm_drop first", name)}
	case req.Reset:
		opts := s.opts
		if err := h.deleteVM(s); err != nil {
			return nil, err
		}
		s, err = h.create(project, name, opts)
		if err != nil {
			return nil, err
		}
		notes = append(notes, "reset VM "+name)
	}
	return h.runIn(ctx, s, req, notes)
}

// runIn runs a call in an existing VM, under its lock.
func (h *Host) runIn(ctx context.Context, s *vmState, req RunRequest, notes []string) (*RunResult, error) {
	gas := req.Gas
	if gas == 0 {
		gas = s.opts.Gas
	}
	wall := wallLimit(gas, s.opts.NsPerGas)
	lockCtx, cancel := context.WithTimeout(ctx, max(wall, lockWait))
	release, err := h.lockVM(lockCtx, s)
	cancel()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := h.sync(ctx, s); err != nil {
		return nil, err
	}
	// Syntax errors are found before the VM is rebuilt, if it must be;
	// the real split needs the VM's names (see normalize).
	if _, err := normalize(req.Code, s.j.seq+1, nil, nil); err != nil {
		return nil, err
	}
	v, rnotes, err := h.resident(ctx, s, wall)
	notes = append(notes, rnotes...)
	if err != nil {
		return nil, err
	}
	h.touch(s)
	return h.runResident(ctx, s, v, req, notes, gas, wall)
}

// runResident runs a call in a resident VM, under its lock: normalised,
// run, and, unless it only read, journaled.
func (h *Host) runResident(ctx context.Context, s *vmState, v *vm, req RunRequest, notes []string, gas int64, wall time.Duration) (*RunResult, error) {
	prog, err := normalize(req.Code, s.j.seq+1, v.names(), nil)
	if err != nil {
		return nil, err
	}
	if elsewhere := h.declaredElsewhere(s, prog.assigned); len(elsewhere) > 0 {
		if prog, err = normalize(req.Code, s.j.seq+1, v.names(), elsewhere); err != nil {
			return nil, err
		}
	}
	ops, imported, err := h.importOps(v, prog)
	if err != nil {
		return nil, err
	}
	if n := importedNote(imported); n != "" {
		notes = append(notes, n)
	}
	readOnly := readOnlyOps(v, prog, len(ops))
	if err := h.checkLimits(s, ops, readOnly); err != nil {
		return nil, err
	}
	res := &RunResult{VM: s.name, GasLimit: gas, NsPerGas: s.opts.NsPerGas, Notes: notes}
	e := &entry{Seq: s.j.seq + 1, Req: req.Req, Time: h.opts.Now().UnixNano(), Ops: ops}
	start := time.Now()
	value, used, runErr := v.apply(ctx, e, gas, start.Add(wall), true)
	res.Wall = time.Since(start)
	res.Gas, res.Value, res.Output = used, value, v.out.String()
	res.MemUsed, res.MemCap = v.memory()
	if runErr != nil {
		// Nothing is journaled, and a VM that ran code is rebuilt.
		if v.unusable {
			s.res = nil
		}
		var ref *refusal
		var perr *parseError
		if errors.As(runErr, &ref) || errors.As(runErr, &perr) {
			return nil, runErr
		}
		res.Err = runErr
		// The hint goes first, right under the error: what to do comes
		// before "new scratch VM" and "imported fmt for you".
		if hint := cmp.Or(h.compileHint(s, ops, runErr), goHint(ops, runErr)); hint != "" {
			res.Notes = append([]string{hint}, res.Notes...)
		}
		return res, nil
	}
	if strings.HasPrefix(s.name, scratchPrefix) {
		if n := loopNote(req.Code, res.Output+"\n"+res.Value); n != "" {
			res.Notes = append([]string{n}, res.Notes...)
		}
	}
	e.WallNS, e.Gas = int64(res.Wall), used
	if v.shadowsBuiltin() {
		readOnly = nil // this input redeclared a builtin a read relied on
	}
	e.Ops = journaledOps(ops, readOnly)
	if len(e.Ops) == 0 {
		return res, nil
	}
	if res.Wall > maxEntryWall {
		// Its effects are in the VM but cannot be kept: rebuild.
		s.res = nil
		res.Err = &refusal{fmt.Sprintf("this call ran for %s, and calls over %s are not kept in a VM, which would have to run them again to rebuild; "+
			"nothing from it was kept. Run long computations as a program (with a package clause), and keep only their results in a VM",
			res.Wall.Round(time.Millisecond), maxEntryWall)}
		return res, nil
	}
	if err := s.j.append(e); err != nil {
		// The VM has state its journal lacks: rebuild it.
		s.res = nil
		if errors.Is(err, errSyncFailed) {
			s.broken = err
		}
		res.Err = fmt.Errorf("the call ran, but saving it failed, so it was not kept: %w", err)
		return res, nil
	}
	v.seq, v.offset = e.Seq, s.j.size
	s.cost.add(e)
	res.Journaled, res.Seq = true, e.Seq
	// A VM grows after it is admitted: keep the others within the budget.
	for _, name := range h.enforceBudget(s) {
		res.Notes = append(res.Notes, "evicted VM "+name+" from memory to make room (it is rebuilt when next used)")
	}
	return res, nil
}

// checkLimits refuses, before it runs, a call the VM's journal could not
// keep: one too big to journal, one that would take the VM past its
// declaration limits, or any call that changes state once the VM's kept
// calls have run for maxReplayWall in all.
func (h *Host) checkLimits(s *vmState, ops []op, readOnly []bool) error {
	keeps := len(journaledOps(ops, readOnly)) > 0
	if keeps && s.j.size >= maxJournal {
		return &refusal{fmt.Sprintf("VM %s's journal is full (%d MB): it still answers expressions, but keeps nothing more; make a new VM, or delete this one", s.name, maxJournal>>20)}
	}
	b, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if len(b) > maxEntry-1024 {
		return &refusal{fmt.Sprintf("this call is %d bytes once journaled; the most one call may be is %d", len(b), maxEntry-1024)}
	}
	var decl int64
	files := 0
	for _, o := range ops {
		decl += o.declBytes()
		if o.File != "" {
			files++
		}
	}
	switch {
	case decl > maxDeclPerEntry:
		return &refusal{fmt.Sprintf("this call declares %d KB of functions, types and variables; the most one call may declare is %d KB. Split it up",
			decl>>10, maxDeclPerEntry>>10)}
	case s.cost.declBytes+decl > maxDeclPerVM:
		return &refusal{fmt.Sprintf("VM %s holds %d KB of declarations, and this call would take it past its %d KB limit; use a new VM",
			s.name, s.cost.declBytes>>10, maxDeclPerVM>>10)}
	case s.cost.declFiles+files > maxDeclFiles:
		return &refusal{fmt.Sprintf("VM %s has had %d inputs with declarations, its limit; use a new VM", s.name, maxDeclFiles)}
	case keeps && s.cost.wall >= maxReplayWall:
		return &refusal{fmt.Sprintf("VM %s's kept calls have run for %s in all, its limit (it must run them all again to rebuild); "+
			"it still answers expressions, but keeps nothing more. Use a new VM", s.name, s.cost.wall.Round(time.Second))}
	}
	return nil
}

// importOps prepends the input's imports to its ops, as import ops bound
// package-wide, loading each package into the base first; with them, the
// imports of packages the input uses without importing them (autoImports),
// whose paths it returns. An import the VM already has is left out.
func (h *Host) importOps(v *vm, prog *program) ([]op, []string, error) {
	have := v.names()
	for name := range v.imports {
		have[name] = true
	}
	auto := autoImports(prog, have)
	specs := append(prog.imports[:len(prog.imports):len(prog.imports)], auto...)
	paths := make([]string, 0, len(specs))
	for _, spec := range specs {
		paths = append(paths, spec.Path)
	}
	names, err := h.base.ensure(paths)
	if err != nil {
		return nil, nil, &refusal{err.Error()}
	}
	var ops []op
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "_" {
			continue
		}
		name := spec.Name
		if name == "" {
			name = names[spec.Path]
		}
		if v.imports[name] == spec.Path || seen[name+" "+spec.Path] {
			continue
		}
		seen[name+" "+spec.Path] = true
		ops = append(ops, op{Import: &importOp{Name: name, Path: spec.Path}})
	}
	added := make([]string, 0, len(auto))
	for _, spec := range auto {
		added = append(added, spec.Path)
	}
	return append(ops, prog.ops...), added, nil
}

// importedNote tells the model which imports it was spared, so it learns
// that it may leave them out.
func importedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "imported " + strings.Join(paths, ", ") + " for you"
}

// journaledOps drops the ops that only read: replaying them would change
// nothing. readOnly is aligned with ops, or nil.
func journaledOps(ops []op, readOnly []bool) []op {
	if readOnly == nil {
		return ops
	}
	var kept []op
	for i, o := range ops {
		if !readOnly[i] {
			kept = append(kept, o)
		}
	}
	return kept
}

// readOnlyOps marks the ops of an input, after its n-len(prog.ops) import
// ops, that only read (see program.readOnly), or returns nil if none may be
// trusted to: when the VM has redeclared a builtin that made one look pure.
func readOnlyOps(v *vm, prog *program, n int) []bool {
	if v.shadowsBuiltin() {
		return nil
	}
	// fmt is an ordinary name: a VM's own fmt, with a Println that changes
	// state, is not the package.
	if prog.usesFmt && v.imports["fmt"] != "fmt" {
		return nil
	}
	ro := make([]bool, n)
	copy(ro[n-len(prog.ops):], prog.readOnly)
	return ro
}

// runProgram runs code that has a package clause in a fresh VM that is
// thrown away after, as `go run` would: a program is a whole, and running
// it again after a fix must not meet its own earlier declarations.
func (h *Host) runProgram(ctx context.Context, req RunRequest) (*RunResult, error) {
	opts := h.defaultOptions()
	gas := req.Gas
	if gas == 0 {
		gas = opts.Gas
	}
	s := &vmState{name: "program", opts: opts}
	if _, err := h.admit(s); err != nil {
		return nil, err
	}
	prog, err := normalize(req.Code, 1, nil, nil)
	if err != nil {
		return nil, err
	}
	v := newVM(h.base, int64(opts.MemoryMB)<<20)
	ops, imported, err := h.importOps(v, prog)
	if err != nil {
		return nil, err
	}
	wall := wallLimit(gas, opts.NsPerGas)
	e := &entry{Seq: 1, Time: h.opts.Now().UnixNano(), Ops: ops}
	start := time.Now()
	value, used, runErr := v.apply(ctx, e, gas, start.Add(wall), true)
	res := &RunResult{VM: "(program)", Program: true, GasLimit: gas, NsPerGas: opts.NsPerGas, Wall: time.Since(start)}
	if n := importedNote(imported); n != "" {
		res.Notes = append(res.Notes, n)
	}
	res.Gas, res.Value, res.Output = used, value, v.out.String()
	res.MemUsed, res.MemCap = v.memory()
	var ref *refusal
	var perr *parseError
	if errors.As(runErr, &ref) || errors.As(runErr, &perr) {
		return nil, runErr
	}
	res.Err = runErr
	return res, nil
}

func (h *Host) defaultOptions() VMOptions {
	return VMOptions{MemoryMB: h.opts.DefaultMemoryMB, Gas: h.opts.DefaultGas, NsPerGas: h.opts.NsPerGas}
}

// NewRequest makes a VM, or changes one. Nil fields are left as they are, or
// take the defaults for a new VM.
type NewRequest struct {
	Project string
	// Session is the calling session: it may name its own scratch VM, as
	// "scratch" or by its full name, to change its settings.
	Session     string
	Name        string
	MemoryMB    *int
	Gas         *int64
	NsPerGas    *float64
	Persist     *bool
	Pinned      *bool
	Description *string
}

// VMInfo describes a VM.
type VMInfo struct {
	Name        string
	Options     VMOptions
	Scratch     bool
	Resident    bool
	MemUsed     int64
	Charge      int64
	Entries     int64
	LifetimeGas int64
	JournalSize int64
	LastUsed    time.Time
	Broken      string
}

// New makes a VM, or changes the settings of one: memory_mb may only grow
// (a smaller cap could not rebuild state that fit the larger one), and a VM
// may become persistent but not stop being so.
func (h *Host) New(ctx context.Context, req NewRequest) (VMInfo, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	scratch := req.Session != "" && (req.Name == "scratch" || req.Name == ScratchName(req.Session))
	if scratch {
		req.Name = ScratchName(req.Session)
		if req.Persist != nil && *req.Persist {
			return VMInfo{}, false, &refusal{"a scratch VM cannot be persistent; make a named VM with gnovm_new to keep state"}
		}
	} else {
		if !nameRe.MatchString(req.Name) {
			return VMInfo{}, false, &refusal{"a VM name is a lowercase letter, then up to 31 lowercase letters, digits, - or _"}
		}
		if strings.HasPrefix(req.Name, scratchPrefix) {
			return VMInfo{}, false, &refusal{fmt.Sprintf("the name %s is reserved", req.Name)}
		}
	}
	if req.MemoryMB != nil && (*req.MemoryMB < 1 || *req.MemoryMB > maxMemoryMB) {
		return VMInfo{}, false, &refusal{fmt.Sprintf("memory_mb must be between 1 and %d", maxMemoryMB)}
	}
	if req.Gas != nil && (*req.Gas < 1 || *req.Gas > maxGas) {
		return VMInfo{}, false, &refusal{fmt.Sprintf("gas must be between 1 and %d", int64(maxGas))}
	}
	if req.NsPerGas != nil && (*req.NsPerGas < 0.1 || *req.NsPerGas > 100) {
		return VMInfo{}, false, &refusal{"ns_per_gas must be between 0.1 and 100"}
	}
	project := projectKey(req.Project)
	if req.Name == MemVM {
		return VMInfo{}, false, &refusal{`mem is your memory, kept by gnosh and made when first used: run gnovm_run with name "mem"; it has no settings to change`}
	}
	s, err := h.lookup(project, req.Name)
	if err != nil {
		return VMInfo{}, false, err
	}
	if s == nil {
		if n, err := h.countNamed(project); err != nil {
			return VMInfo{}, false, err
		} else if n >= maxNamedVMs && !scratch {
			return VMInfo{}, false, &refusal{fmt.Sprintf("this project has %d VMs, the most it may have; delete one with gnovm_drop", n)}
		}
		opts := h.defaultOptions()
		applyOptions(&opts, req)
		s, err = h.create(project, req.Name, opts)
		if err != nil {
			return VMInfo{}, false, err
		}
		return h.info(s), true, nil
	}

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	release, err := h.lockVM(lockCtx, s)
	cancel()
	if err != nil {
		return VMInfo{}, false, err
	}
	defer release()
	if err := h.sync(ctx, s); err != nil {
		return VMInfo{}, false, err
	}
	opts := s.opts
	applyOptions(&opts, req)
	if opts.MemoryMB < s.opts.MemoryMB {
		return VMInfo{}, false, &refusal{fmt.Sprintf("memory_mb can only grow (VM %s has %d); for less, make a new VM", s.name, s.opts.MemoryMB)}
	}
	if s.opts.Persist && !opts.Persist {
		return VMInfo{}, false, &refusal{fmt.Sprintf("VM %s is persistent; to discard its saved state, delete it with gnovm_drop", s.name)}
	}
	if opts == s.opts {
		return h.info(s), false, nil
	}
	if opts.Persist && !s.opts.Persist {
		if err := h.persist(s); err != nil {
			return VMInfo{}, false, err
		}
	}
	if opts.MemoryMB != s.opts.MemoryMB {
		// The allocator's cap is fixed when it is made: rebuild.
		s.res = nil
	}
	s.opts = opts
	if err := s.writeMachineFile(); err != nil {
		return VMInfo{}, false, err
	}
	return h.info(s), false, nil
}

func applyOptions(opts *VMOptions, req NewRequest) {
	if req.MemoryMB != nil {
		opts.MemoryMB = *req.MemoryMB
	}
	if req.Gas != nil {
		opts.Gas = *req.Gas
	}
	if req.NsPerGas != nil {
		opts.NsPerGas = *req.NsPerGas
	}
	if req.Persist != nil {
		opts.Persist = *req.Persist
	}
	if req.Pinned != nil {
		opts.Pinned = *req.Pinned
	}
	if req.Description != nil {
		opts.Description = truncate(*req.Description, 200)
	}
}

// persist moves a memory-only VM's files to the project's persistent VMs.
func (h *Host) persist(s *vmState) error {
	dst := h.persistDir(s.project, s.name)
	if _, err := os.Stat(dst); err == nil {
		return &refusal{fmt.Sprintf("another gnosh process has a persistent VM named %s; pick another name", s.name)}
	}
	if err := mkdirSync(filepath.Dir(dst)); err != nil {
		return err
	}
	// machine.json says persistent before the move, so no crash leaves a
	// persistent VM that its own file calls memory-only (lookup goes by
	// location anyway).
	opts := s.opts
	opts.Persist = true
	saved := s.opts
	s.opts = opts
	if err := s.writeMachineFile(); err != nil {
		s.opts = saved
		return err
	}
	if err := os.Rename(s.dir, dst); err != nil {
		s.opts = saved
		_ = s.writeMachineFile()
		return err
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	s.dir = dst
	if s.j != nil {
		s.j.path = filepath.Join(dst, "journal.jsonl")
	}
	return nil
}

// countNamed counts a project's named VMs: persistent ones on disk and this
// client's memory-only ones.
func (h *Host) countNamed(project string) (int, error) {
	names := map[string]bool{}
	for _, dir := range []string{filepath.Join(h.projectDir(project), "vms"), h.ephemeralRoot(project)} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		for _, e := range entries {
			if e.IsDir() && nameRe.MatchString(e.Name()) && !strings.HasPrefix(e.Name(), scratchPrefix) {
				names[e.Name()] = true
			}
		}
	}
	return len(names), nil
}

// Drop evicts a VM from memory (action "evict"), or deletes it ("delete").
func (h *Host) Drop(ctx context.Context, projectPath, name, action string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == MemVM {
		return &refusal{"mem is your memory: it is never dropped; the user erases from it with /mem forget"}
	}
	project := projectKey(projectPath)
	s, err := h.lookup(project, name)
	if err != nil {
		return err
	}
	if s == nil {
		return &refusal{fmt.Sprintf("there is no VM named %s", name)}
	}
	switch action {
	case "evict":
		s.res = nil
		return nil
	case "delete":
		lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		release, err := h.lockVM(lockCtx, s)
		cancel()
		if err != nil {
			return err
		}
		defer release()
		return h.deleteVM(s)
	}
	return &refusal{fmt.Sprintf("action must be evict or delete, not %q", action)}
}

// deleteVM deletes a VM's files: a persistent VM's go to the project's trash
// (its lock stays where it is, outside), a memory-only VM's are removed.
func (h *Host) deleteVM(s *vmState) error {
	delete(h.vms, s.key)
	s.res = nil
	if !s.persistent() {
		return os.RemoveAll(s.dir)
	}
	trash := filepath.Join(h.projectDir(s.project), ".trash")
	if err := mkdirSync(trash); err != nil {
		return err
	}
	dst := filepath.Join(trash, fmt.Sprintf("%s-%s", s.name, h.opts.Now().UTC().Format("20060102T150405.000000000")))
	if err := os.Rename(s.dir, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.dir))
}

// ListResult is the state of the host and of a project's VMs.
type ListResult struct {
	Memory    HostMemory
	ProbeErr  string
	Budget    int64
	Used      int64
	BaseBytes int64
	VMs       []VMInfo
}

// List describes the host's memory and a project's VMs: its persistent ones
// on disk, and this client's memory-only ones.
func (h *Host) List(projectPath string) (ListResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	project := projectKey(projectPath)
	h.sweep(project)
	var out ListResult
	mem, err := h.opts.Probe()
	if err != nil {
		out.ProbeErr = err.Error()
	}
	out.Memory = mem
	out.Budget = h.budgetFor(mem, err)
	out.Used = h.usedBytes()
	if h.base != nil {
		out.BaseBytes = h.base.heap
	}
	states, err := h.projectStates(project)
	if err != nil {
		return out, err
	}
	if info, ok := h.memInfo(projectPath); ok {
		out.VMs = append(out.VMs, info)
	}
	for _, s := range states {
		if s.name == MemVM {
			continue // a VM of v1's mem, left on disk
		}
		if s.j == nil && s.broken == nil {
			if j, entries, _, err := readJournal(filepath.Join(s.dir, "journal.jsonl")); err == nil {
				s.j = j
				s.cost = journalStats(entries)
			} else {
				s.broken = err
			}
		}
		out.VMs = append(out.VMs, h.info(s))
	}
	sort.Slice(out.VMs, func(i, j int) bool { return out.VMs[i].Name < out.VMs[j].Name })
	return out, nil
}

func (h *Host) info(s *vmState) VMInfo {
	in := VMInfo{
		Name:        s.name,
		Options:     s.opts,
		Scratch:     strings.HasPrefix(s.name, scratchPrefix),
		Resident:    s.res != nil,
		LifetimeGas: s.cost.gas,
		LastUsed:    s.used,
		Charge:      h.charge(s),
	}
	if s.res != nil {
		in.MemUsed, _ = s.res.memory()
	}
	if s.j != nil {
		in.Entries, in.JournalSize = s.j.seq, s.j.size
	}
	if s.broken != nil {
		in.Broken = s.broken.Error()
	}
	return in
}

// Kept reports whether the call with request id req was journaled in a VM,
// for a caller that lost the reply.
func (h *Host) Kept(projectPath, name, req string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == MemVM {
		return h.mem(projectPath).kept(req)
	}
	s, err := h.lookup(projectKey(projectPath), name)
	if err != nil || s == nil {
		return false, err
	}
	_, entries, _, err := readJournal(filepath.Join(s.dir, "journal.jsonl"))
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Req == req {
			return true, nil
		}
	}
	return false, nil
}

// Repair makes a broken VM usable again: it cuts the journal before the
// first entry that cannot be read or replayed, keeps the old journal beside
// it as a .bak, and starts a new generation, so other gnosh processes
// rebuild rather than catch up. It reports how many entries it kept and
// cut.
func (h *Host) Repair(ctx context.Context, projectPath, name string) (kept, cut int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == MemVM {
		kept, err := h.mem(projectPath).repair(ctx)
		return kept, 0, err
	}
	s, err := h.lookup(projectKey(projectPath), name)
	if err != nil {
		return 0, 0, err
	}
	if s == nil {
		return 0, 0, &refusal{fmt.Sprintf("there is no VM named %s", name)}
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	release, err := h.lockVM(lockCtx, s)
	cancel()
	if err != nil {
		return 0, 0, err
	}
	defer release()
	// Another gnosh may have repaired it, or gone on writing to it, since
	// this one saw it broken: look at the journal as it is now.
	_ = h.sync(ctx, s)
	if s.broken == nil {
		return 0, 0, &refusal{fmt.Sprintf("VM %s is not broken", name)}
	}
	path := filepath.Join(s.dir, "journal.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	var hdr journalHeader
	if len(lines) == 0 || json.Unmarshal(lines[0], &hdr) != nil || hdr.Generation == "" {
		return 0, 0, &refusal{fmt.Sprintf("VM %s's journal header cannot be read, so it cannot be repaired; delete the VM", name)}
	}
	var good [][]byte
	total := 0
	seq := int64(0)
	for _, line := range lines[1:] {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		total++
		var e entry
		if len(good) < total-1 || !bytes.HasSuffix(line, []byte("\n")) ||
			json.Unmarshal(line, &e) != nil || e.Seq != seq+1 ||
			(s.brokenAt > 0 && s.brokenGen == hdr.Generation && e.Seq >= s.brokenAt) {
			continue
		}
		seq = e.Seq
		good = append(good, line)
	}
	hdr.Generation = newGeneration()
	hdr.Gno = stdlibVersion()
	head, err := json.Marshal(hdr)
	if err != nil {
		return 0, 0, err
	}
	out := append(head, '\n')
	for _, line := range good {
		out = append(out, line...)
	}
	bak := fmt.Sprintf("%s.bak-%s", path, h.opts.Now().UTC().Format("20060102T150405"))
	if err := writeFileSync(bak, data); err != nil {
		return 0, 0, err
	}
	if err := writeFileSync(path, out); err != nil {
		return 0, 0, err
	}
	s.j, s.res, s.broken, s.brokenAt, s.brokenGen = nil, nil, nil, 0, ""
	return len(good), total - len(good), nil
}
