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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
)

const (
	// silenceKill is how long a worker running a request may send nothing
	// (no heartbeat, no reply) before the client kills it.
	silenceKill = 10 * time.Second
	// overdueKill is how far past its deadline a request may run before
	// the client kills the worker. The longest call is a 10 min run after
	// a replay slice; heartbeats are the real liveness check.
	overdueKill = 30 * time.Second
	// requestDeadline bounds any one request.
	requestDeadline = 15 * time.Minute
	// workerLogMax is how big the worker's stderr log grows before it is
	// rotated (one old copy is kept).
	workerLogMax = 1 << 20
)

// ClientOptions configures the process's client.
type ClientOptions struct {
	// Options are the host's: data directory, budget, defaults. Client is
	// filled in by Acquire if empty.
	Options
	// InProcess runs the VMs in this process rather than in a worker
	// (tests; and Windows, which cannot hand a child the protocol's pipes).
	InProcess bool
	// Binary is the worker's program: "" means this process's own. Under
	// go test it must be named, and must not be the test binary itself.
	Binary string
}

// Client is a gnosh process's way to its VMs: a worker it spawns on first
// use (and again if the worker dies), or a host of its own in-process. It
// is shared by the process (server mode has one App per workspace) and
// counted: the last Release closes it.
type Client struct {
	opts    ClientOptions
	mu      sync.Mutex
	refs    int
	host    *Host
	conn    *workerConn
	release func()
	// deaths records why workers ended, newest last, for gnovm_list.
	deaths []string
}

var (
	clientMu sync.Mutex
	client   *Client
)

// Acquire returns the process's client, making it on first use. Each Acquire
// is matched by a Release.
func Acquire(opts ClientOptions) (*Client, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		client.refs++
		return client, nil
	}
	if opts.Dir == "" {
		return nil, errors.New("gnovm: a data directory is required")
	}
	if opts.Client == "" {
		opts.Client = NewClientID()
	}
	if runtime.GOOS == "windows" {
		opts.InProcess = true
	}
	c := &Client{opts: opts, refs: 1}
	// The client lock marks this process's memory-only VMs as alive, for
	// as long as the process lives, whatever happens to its workers.
	if err := mkdirSync(filepath.Join(opts.Dir, ".clients")); err != nil {
		return nil, err
	}
	release, err := lock.TryFile(filepath.Join(opts.Dir, ".clients", opts.Client+".lock"))
	if err != nil {
		return nil, fmt.Errorf("gnovm: client lock: %w", err)
	}
	c.release = release
	client = c
	return c, nil
}

// Release gives back one Acquire; the last one closes the client: its
// worker is stopped, and its memory-only VMs are deleted.
func (c *Client) Release() {
	clientMu.Lock()
	defer clientMu.Unlock()
	c.refs--
	if c.refs > 0 {
		return
	}
	if client == c {
		client = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.close()
		c.conn = nil
	}
	if c.host != nil {
		_ = c.host.Close()
		c.host = nil
	}
	deleteEphemeral(c.opts.Dir, c.opts.Client)
	if c.release != nil {
		c.release()
		_ = os.Remove(filepath.Join(c.opts.Dir, ".clients", c.opts.Client+".lock"))
		c.release = nil
	}
}

// Deaths reports why this client's workers ended, newest last.
func (c *Client) Deaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deaths...)
}

// Run runs code in a VM; see Host.Run. A call to mem whose catch-up spent
// its slice is made again at once, here, so the model never retries it; a
// mem input whose Next may wait for working sessions waits here
// (memwait.go).
func (c *Client) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if req.Name != MemVM {
		return c.run(ctx, req)
	}
	if mayWaitInput(req) {
		return c.runWaiting(ctx, req)
	}
	res, err := c.runMem(ctx, req)
	if err == nil && res != nil && res.Journaled {
		c.memChanged(req.Project)
	}
	return res, err
}

// memChanged follows a change to a project's mem: what recall kept of it
// is dropped, an erasure's text first of all, and its vectors are made in
// the background, when there is an embedding server.
func (c *Client) memChanged(project string) {
	dir := memDir(c.opts.Dir, project)
	dropMemSnapshot(dir)
	fillMemVectors(dir, newMemEmbedder(c.opts.Embeddings))
}

func (c *Client) run(ctx context.Context, req RunRequest) (*RunResult, error) {
	return c.runVia(ctx, req, false)
}

// runAwaited is run whose reply is awaited even when ctx ends: the cancel
// is sent, and the reply, which then comes at once, is kept, so a mem call
// that committed before the cancel reached it is never lost.
func (c *Client) runAwaited(ctx context.Context, req RunRequest) (*RunResult, error) {
	return c.runVia(ctx, req, true)
}

func (c *Client) runVia(ctx context.Context, req RunRequest, await bool) (*RunResult, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return h.Run(ctx, req)
	}
	var body json.RawMessage
	var err error
	if await {
		body, err = c.callAwaited(ctx, msgRun, req)
	} else {
		body, err = c.call(ctx, msgRun, req)
	}
	if err != nil {
		return nil, err
	}
	var wr wireRun
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, err
	}
	res := wr.RunResult
	res.Err = fromWire(wr.Err)
	return &res, nil
}

// New makes or changes a VM; see Host.New.
func (c *Client) New(ctx context.Context, req NewRequest) (VMInfo, bool, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return VMInfo{}, false, err
		}
		return h.New(ctx, req)
	}
	body, err := c.call(ctx, msgNew, req)
	if err != nil {
		return VMInfo{}, false, err
	}
	var r newResp
	err = json.Unmarshal(body, &r)
	return r.Info, r.Created, err
}

// List describes the host and a project's VMs; see Host.List.
func (c *Client) List(ctx context.Context, project string) (ListResult, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return ListResult{}, err
		}
		return h.List(project)
	}
	body, err := c.call(ctx, msgList, listReq{Project: project})
	if err != nil {
		return ListResult{}, err
	}
	var r ListResult
	err = json.Unmarshal(body, &r)
	return r, err
}

// Drop evicts or deletes a VM; see Host.Drop.
func (c *Client) Drop(ctx context.Context, project, name, action string) error {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return err
		}
		return h.Drop(ctx, project, name, action)
	}
	_, err := c.call(ctx, msgDrop, dropReq{Project: project, Name: name, Action: action})
	return err
}

// Kept reports whether the call with request id req was kept; see
// Host.Kept. A run whose worker died before replying is resolved with it.
func (c *Client) Kept(ctx context.Context, project, name, req string) (bool, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return false, err
		}
		return h.Kept(project, name, req)
	}
	body, err := c.call(ctx, msgKept, keptReq{Project: project, Name: name, Req: req})
	if err != nil {
		return false, err
	}
	var kept bool
	err = json.Unmarshal(body, &kept)
	return kept, err
}

// KeptFrames is Kept for a mem call, with the frames the kept call pushed
// and popped, which a caller that lost the reply must still apply to its
// stack.
func (c *Client) KeptFrames(ctx context.Context, project, req string) ([]MemFrame, bool, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return nil, false, err
		}
		return h.KeptFrames(project, req)
	}
	body, err := c.call(ctx, msgKept, keptReq{Project: project, Name: MemVM, Req: req, Frames: true})
	if err != nil {
		return nil, false, err
	}
	var r keptResp
	if json.Unmarshal(body, &r) == nil {
		return r.Frames, r.Kept, nil
	}
	// A worker from before frames were asked for answers a bare bool.
	var kept bool
	err = json.Unmarshal(body, &kept)
	return nil, kept, err
}

// Repair repairs a broken VM; see Host.Repair.
func (c *Client) Repair(ctx context.Context, project, name string) (int, int, error) {
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return 0, 0, err
		}
		return h.Repair(ctx, project, name)
	}
	body, err := c.call(ctx, msgRepair, repairReq{Project: project, Name: name})
	if err != nil {
		return 0, 0, err
	}
	var r repairResp
	err = json.Unmarshal(body, &r)
	return r.Kept, r.Cut, err
}

// MemCommand runs /mem (see Host.MemCommand), and says what it erased, if
// anything, for gnosh's own copies of mem's text.
func (c *Client) MemCommand(ctx context.Context, project, session, args string, titles map[string]string) (string, *MemErasure, error) {
	defer c.memChanged(project)
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return "", nil, err
		}
		text, erased, err := h.memCommand(ctx, project, session, args, titles)
		for callAgain(ctx, err) {
			text, erased, err = h.memCommand(ctx, project, session, args, titles)
		}
		return text, erased, err
	}
	body, err := c.call(ctx, msgMem, memReq{Project: project, Session: session, Args: args, Titles: titles})
	for callAgain(ctx, err) {
		body, err = c.call(ctx, msgMem, memReq{Project: project, Session: session, Args: args, Titles: titles})
	}
	if err != nil {
		return "", nil, err
	}
	var resp memResp
	err = json.Unmarshal(body, &resp)
	return resp.Text, resp.Erased, err
}

// inProcess returns the client's own host when it runs VMs in-process, made
// on first use, or nil when it uses a worker.
func (c *Client) inProcess() (*Host, error) {
	if !c.opts.InProcess {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host == nil {
		h, err := NewHost(c.opts.Options)
		if err != nil {
			return nil, err
		}
		c.host = h
	}
	return c.host, nil
}

// workerDied is a request whose worker ended before it replied. For a run,
// whether the call was kept is not known until the next worker is asked.
type workerDied struct{ cause string }

func (e *workerDied) Error() string {
	return "the VM worker ended before it replied (" + e.cause + "); the outcome is unknown until it is asked again"
}

// call sends a request to the worker, spawning one if there is none, and
// waits for its reply.
func (c *Client) call(ctx context.Context, typ string, body any) (json.RawMessage, error) {
	return c.callVia(ctx, typ, body, false)
}

// callAwaited is call whose reply is awaited even when ctx ends (see
// workerConn.request).
func (c *Client) callAwaited(ctx context.Context, typ string, body any) (json.RawMessage, error) {
	return c.callVia(ctx, typ, body, true)
}

func (c *Client) callVia(ctx context.Context, typ string, body any, await bool) (json.RawMessage, error) {
	conn, err := c.worker()
	if err != nil {
		return nil, err
	}
	f, err := conn.request(ctx, typ, mustJSON(body), await)
	if err != nil {
		return nil, err
	}
	if f.Err != nil {
		return nil, fromWire(f.Err)
	}
	return f.Body, nil
}

// worker returns the live worker connection, spawning a worker if need be.
func (c *Client) worker() (*workerConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		select {
		case <-c.conn.dead:
			c.deaths = append(c.deaths, c.conn.cause())
			if len(c.deaths) > 8 {
				c.deaths = c.deaths[len(c.deaths)-8:]
			}
			c.conn = nil
		default:
			return c.conn, nil
		}
	}
	conn, err := spawnWorker(c.opts)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

// workerConn is one running worker.
type workerConn struct {
	cmd    *exec.Cmd
	reqW   *os.File
	repR   *os.File
	logF   *os.File
	wmu    sync.Mutex
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]*pendingReq
	last    atomic.Int64 // unix nanos of the last frame from the worker
	dead    chan struct{}
	why     string
	killed  string // set when the client killed it, and why
}

type pendingReq struct {
	done     chan *frame
	deadline time.Time
	// waiting is set while the worker says the request waits for mem's
	// lock; the deadline starts again when the wait ends.
	waiting bool
}

// spawnWorker starts a worker and shakes hands with it.
func spawnWorker(opts ClientOptions) (*workerConn, error) {
	bin, err := workerBinary(opts.Binary)
	if err != nil {
		return nil, err
	}
	if err := mkdirSync(opts.Dir); err != nil {
		return nil, err
	}
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return nil, err
	}
	logF, err := openWorkerLog(filepath.Join(opts.Dir, "worker.log"))
	if err != nil {
		reqR.Close()
		reqW.Close()
		repR.Close()
		repW.Close()
		return nil, err
	}
	cmd := exec.Command(bin, "gnovm-worker")
	cmd.ExtraFiles = []*os.File{reqR, repW} // fds 3 and 4
	// Not the project directory: gnosh loads a .env from its working
	// directory before main runs, and the project's is agent-writable.
	cmd.Dir = opts.Dir
	cmd.Env = workerEnv()
	cmd.Stdout = nil
	cmd.Stderr = logF
	setWorkerProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{reqR, reqW, repR, repW, logF} {
			f.Close()
		}
		return nil, fmt.Errorf("gnovm: starting the worker: %w", err)
	}
	reqR.Close()
	repW.Close()
	conn := &workerConn{cmd: cmd, reqW: reqW, repR: repR, logF: logF, pending: map[uint64]*pendingReq{}, dead: make(chan struct{})}
	conn.last.Store(time.Now().UnixNano())
	go conn.reap()

	// Handshake.
	h := myHello()
	h.Dir, h.Client = opts.Dir, opts.Client
	h.BudgetBytes, h.DefaultMemoryMB, h.DefaultGas, h.NsPerGas = opts.BudgetBytes, opts.DefaultMemoryMB, opts.DefaultGas, opts.NsPerGas
	h.Embeddings = opts.Embeddings
	if err := writeFrame(reqW, &frame{Type: msgHello, Body: mustJSON(h)}); err != nil {
		conn.kill("handshake: " + err.Error())
		return nil, fmt.Errorf("gnovm: worker handshake: %w", err)
	}
	type hres struct {
		f   *frame
		err error
	}
	ch := make(chan hres, 1)
	go func() { f, err := readFrame(repR); ch <- hres{f, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			conn.kill("handshake: " + r.err.Error())
			return nil, fmt.Errorf("gnovm: worker handshake: %w", r.err)
		}
		if r.f.Err != nil {
			conn.kill("handshake refused")
			return nil, fmt.Errorf("gnovm: worker refused: %w", fromWire(r.f.Err))
		}
	case <-time.After(10 * time.Second):
		conn.kill("no handshake in 10 s")
		return nil, errors.New("gnovm: the worker did not answer its handshake")
	}
	go conn.readLoop()
	go conn.watchdog()
	return conn, nil
}

// reap waits for the worker to exit, records why, and fails what was
// waiting on it.
func (c *workerConn) reap() {
	err := c.cmd.Wait()
	c.mu.Lock()
	why := c.killed
	if why == "" {
		why = "exited"
		if err != nil {
			why = err.Error()
		}
	}
	c.why = why
	pend := c.pending
	c.pending = map[uint64]*pendingReq{}
	c.mu.Unlock()
	close(c.dead)
	for _, p := range pend {
		close(p.done)
	}
	c.wmu.Lock()
	c.reqW.Close()
	c.wmu.Unlock()
	c.repR.Close()
	c.logF.Close()
}

// workerBinary picks the worker's program. Under go test the client never
// spawns its own binary, which is the test binary: a test that re-ran
// itself caused a fork bomb on the AI box in October.
func workerBinary(named string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if testing.Testing() {
		if named == "" {
			return "", errors.New("gnovm: tests run VMs in-process (ClientOptions.InProcess), or name a worker binary")
		}
		if sameFile(named, self) {
			return "", errors.New("gnovm: a test may not use its own binary as the worker")
		}
		return named, nil
	}
	if named != "" {
		return named, nil
	}
	// On Linux, /proc/self/exe is this program even after a deploy
	// replaced the file, so the worker matches the client's build.
	if runtime.GOOS == "linux" {
		return "/proc/self/exe", nil
	}
	return self, nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// workerEnv is the client's environment without the profiling switch,
// under any of its names: two processes cannot serve pprof on one port.
func workerEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GNOSH_PROFILE", "KLASH_PROFILE", "CRUSH_PROFILE":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// openWorkerLog opens the worker's stderr log, keeping one old copy once it
// passes workerLogMax.
func openWorkerLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > workerLogMax {
		_ = os.Rename(path, path+".old")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// request sends one request and waits for its reply. If ctx ends first, it
// tells the worker to cancel and returns; the late reply is dropped. With
// await, it tells the worker to cancel and still waits for the reply, which
// then comes at once (a request queued or waiting for a lock is cancelled
// before it holds anything; one past that finishes): what it kept is not
// lost.
func (c *workerConn) request(ctx context.Context, typ string, body json.RawMessage, await bool) (*frame, error) {
	id := c.nextID.Add(1)
	p := &pendingReq{done: make(chan *frame, 1), deadline: time.Now().Add(requestDeadline)}
	c.mu.Lock()
	select {
	case <-c.dead:
		c.mu.Unlock()
		return nil, &workerDied{cause: c.cause()}
	default:
	}
	c.pending[id] = p
	c.mu.Unlock()
	// An idle worker sends nothing; its silence counts from now.
	c.last.Store(time.Now().UnixNano())
	if err := c.send(&frame{ID: id, Type: typ, Body: body}); err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case f, ok := <-p.done:
		if !ok {
			return nil, &workerDied{cause: c.cause()}
		}
		return f, nil
	case <-ctx.Done():
		_ = c.send(&frame{ID: id, Type: msgCancel})
		if await {
			f, ok := <-p.done
			if !ok {
				return nil, &workerDied{cause: c.cause()}
			}
			return f, nil
		}
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *workerConn) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *workerConn) send(f *frame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.reqW, f)
}

// callAgain reports whether err is mem's catch-up having spent its slice,
// with ctx still live: the call goes on at once (each call applies more
// and keeps it; the lock is let go between, so others get their turns).
func callAgain(ctx context.Context, err error) bool {
	var busy *busyError
	return err != nil && errors.As(err, &busy) && busy.again && ctx.Err() == nil
}

// heard takes a heartbeat for its request: while the worker says the
// request waits for mem's lock, and once more when it stops waiting, the
// request's deadline starts again from now, and so does every other
// pending request's, since those queue behind it in the worker.
func (c *workerConn) heard(f *frame, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending[f.ID]
	if p == nil {
		return
	}
	if f.Waiting || p.waiting {
		for _, q := range c.pending {
			q.deadline = now.Add(requestDeadline)
		}
	}
	p.waiting = f.Waiting
}

// readLoop takes the worker's frames: replies to whoever waits, and every
// frame, heartbeats included, as a sign of life.
func (c *workerConn) readLoop() {
	for {
		f, err := readFrame(c.repR)
		if err != nil {
			return
		}
		now := time.Now()
		c.last.Store(now.UnixNano())
		if f.Type == msgHeartbeat {
			c.heard(f, now)
		}
		if f.Type != msgResult {
			continue
		}
		c.mu.Lock()
		p := c.pending[f.ID]
		delete(c.pending, f.ID)
		c.mu.Unlock()
		if p != nil {
			p.done <- f
		}
	}
}

// watchdog kills a worker that is running a request and has sent nothing
// for silenceKill, or whose request is overdueKill past its deadline.
func (c *workerConn) watchdog() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.dead:
			return
		case <-t.C:
		}
		if why := c.verdict(time.Now()); why != "" {
			c.kill(why)
		}
	}
}

// verdict says why the worker should be killed at now, or "" if it should
// not: it is running a request and has sent nothing for silenceKill, or a
// request is overdueKill past its deadline.
func (c *workerConn) verdict(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return ""
	}
	if silent := now.Sub(time.Unix(0, c.last.Load())); silent > silenceKill {
		return fmt.Sprintf("killed by gnosh: no heartbeat for %s", silent.Round(time.Second))
	}
	for _, p := range c.pending {
		if now.After(p.deadline.Add(overdueKill)) {
			return "killed by gnosh: a request ran past its deadline"
		}
	}
	return ""
}

// kill ends the worker, recording why.
func (c *workerConn) kill(why string) {
	c.mu.Lock()
	if c.killed == "" {
		c.killed = why
	}
	c.mu.Unlock()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// close stops the worker: its request pipe closes, so it finishes what it
// is doing and exits; after 3 s it is killed.
func (c *workerConn) close() {
	c.wmu.Lock()
	_ = c.reqW.Close()
	c.wmu.Unlock()
	select {
	case <-c.dead:
	case <-time.After(3 * time.Second):
		c.kill("killed by gnosh at close: did not exit in 3 s")
		<-c.dead
	}
}

func (c *workerConn) cause() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.why
}

// goal makes one of the goal calls, in-process or through the worker.
func (c *Client) goal(ctx context.Context, req goalReq) (goalResp, error) {
	if req.Op != "turn" && req.Op != "closed" {
		defer c.memChanged(req.Project)
	}
	if h, err := c.inProcess(); h != nil || err != nil {
		if err != nil {
			return goalResp{}, err
		}
		resp, err := h.goal(ctx, req)
		for callAgain(ctx, err) {
			resp, err = h.goal(ctx, req)
		}
		return resp, err
	}
	body, err := c.call(ctx, msgGoal, req)
	for callAgain(ctx, err) {
		body, err = c.call(ctx, msgGoal, req)
	}
	if err != nil {
		return goalResp{}, err
	}
	var resp goalResp
	err = json.Unmarshal(body, &resp)
	return resp, err
}

// BindGoal binds a session's /goal; see Host.BindGoal.
func (c *Client) BindGoal(ctx context.Context, project, session, condition string, prev *GoalBinding) (GoalBinding, string, error) {
	resp, err := c.goal(ctx, goalReq{Op: "bind", Project: project, Session: session, Condition: condition, Prev: prev})
	return resp.Binding, resp.Text, err
}

// GoalTurnEnd says what to do with a session's goal as its turn ends; see
// Host.GoalTurnEnd.
func (c *Client) GoalTurnEnd(ctx context.Context, project, session string, b GoalBinding, marker string) (GoalTurn, GoalBinding, error) {
	resp, err := c.goal(ctx, goalReq{Op: "turn", Project: project, Session: session, Binding: b, Marker: marker})
	return resp.Turn, resp.Binding, err
}

// GoalReopen reopens a goal the judge found not met; see Host.GoalReopen.
func (c *Client) GoalReopen(ctx context.Context, project string, b GoalBinding, reason string) error {
	_, err := c.goal(ctx, goalReq{Op: "reopen", Project: project, Binding: b, Reason: reason})
	return err
}

// ClosedGoals are the goals a session closed as done since its turn
// began, for the judge; see Host.ClosedGoals.
func (c *Client) ClosedGoals(ctx context.Context, project, session string, since time.Time, except string) ([]ClosedGoal, error) {
	resp, err := c.goal(ctx, goalReq{Op: "closed", Project: project, Session: session, Since: since.Unix(), Except: except})
	return resp.Closed, err
}

// GoalClear unbinds a session's /goal; see Host.GoalClear.
func (c *Client) GoalClear(ctx context.Context, project, session string, b GoalBinding) (string, error) {
	resp, err := c.goal(ctx, goalReq{Op: "clear", Project: project, Session: session, Binding: b})
	return resp.Text, err
}
