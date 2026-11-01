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
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pipeWorker runs serve in a goroutine over in-process pipes, with nothing
// exec'd, and returns the client's ends and serve's result.
func pipeWorker(t *testing.T) (send func(*frame), recv func() *frame, closeReq func(), served <-chan error) {
	t.Helper()
	reqR, reqW := io.Pipe()
	repR, repW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(reqR, repW, nil)
		repW.Close()
	}()
	send = func(f *frame) { require.NoError(t, writeFrame(reqW, f)) }
	recv = func() *frame {
		f, err := readFrame(repR)
		require.NoError(t, err)
		return f
	}
	// The client goes as a real one does, closing both its ends: a frame
	// the worker still writes, such as a cancel's ack sent after the
	// result, then fails instead of waiting for ever for a reader.
	return send, recv, func() { reqW.Close(); repR.Close() }, done
}

func clientHello(dir string) *frame {
	h := myHello()
	h.Dir, h.Client = dir, NewClientID()
	return &frame{Type: msgHello, Body: mustJSON(h)}
}

// recvResult skips heartbeats and acks to the next result.
func recvResult(t *testing.T, recv func() *frame) (f *frame, heartbeats int) {
	t.Helper()
	for {
		f := recv()
		switch f.Type {
		case msgHeartbeat:
			heartbeats++
		case msgAck:
		default:
			return f, heartbeats
		}
	}
}

func TestWorkerServesRequests(t *testing.T) {
	t.Parallel()
	send, recv, closeReq, served := pipeWorker(t)
	send(clientHello(t.TempDir()))
	h := recv()
	require.Equal(t, msgHello, h.Type)
	require.Nil(t, h.Err)

	send(&frame{ID: 1, Type: msgRun, Body: mustJSON(RunRequest{Project: "/p", Session: "s", Code: `x := 20`})})
	f, _ := recvResult(t, recv)
	require.EqualValues(t, 1, f.ID)
	require.Nil(t, f.Err)

	send(&frame{ID: 2, Type: msgRun, Body: mustJSON(RunRequest{Project: "/p", Session: "s", Code: `x * 2`})})
	f, _ = recvResult(t, recv)
	var wr wireRun
	require.NoError(t, json.Unmarshal(f.Body, &wr))
	require.Equal(t, "40", wr.Value)

	// A refusal keeps its kind across the wire.
	send(&frame{ID: 3, Type: msgRun, Body: mustJSON(RunRequest{Project: "/p", Name: "nosuchvm", Code: `1`})})
	f, _ = recvResult(t, recv)
	var ref *refusal
	require.ErrorAs(t, fromWire(f.Err), &ref)

	closeReq()
	require.NoError(t, <-served, "the worker quits cleanly when its client goes")
}

// A long run sends heartbeats the whole time, and a cancel stops it at once.
func TestWorkerHeartbeatsAndCancel(t *testing.T) {
	t.Parallel()
	send, recv, closeReq, served := pipeWorker(t)
	send(clientHello(t.TempDir()))
	recv()

	send(&frame{ID: 1, Type: msgRun, Body: mustJSON(RunRequest{Project: "/p", Session: "s", Code: `for {}`, Gas: maxGas})})
	// Two heartbeats while the run runs, however long a busy machine takes
	// to start its VM (a fixed wait of 1.6 s saw one, under a landing's
	// full suite beside a scenario pass); a run that sends none fails here
	// rather than hanging.
	watchdog := time.AfterFunc(30*time.Second, closeReq)
	for beats := 0; beats < 2; {
		switch f := recv(); f.Type {
		case msgHeartbeat:
			beats++
		case msgAck:
		default:
			t.Fatalf("a %s while the run ran, before two heartbeats", f.Type)
		}
	}
	watchdog.Stop()
	start := time.Now()
	send(&frame{ID: 1, Type: msgCancel})
	f, _ := recvResult(t, recv)
	require.Less(t, time.Since(start), 10*time.Second, "the cancel stopped it")
	var wr wireRun
	require.NoError(t, json.Unmarshal(f.Body, &wr))
	var lim *limitError
	require.ErrorAs(t, fromWire(wr.Err), &lim)
	require.Equal(t, "cancelled", lim.kind)

	closeReq()
	require.NoError(t, <-served)
}

func TestWorkerRefusesABadHandshake(t *testing.T) {
	t.Parallel()
	send, recv, _, served := pipeWorker(t)
	other := myHello()
	other.Build = "another-build"
	send(&frame{Type: msgHello, Body: mustJSON(other)})
	f := recv()
	require.NotNil(t, f.Err)
	require.Contains(t, f.Err.Msg, "restart gnosh")
	require.ErrorIs(t, <-served, errHandshake)

	// No hello at all: the worker gives up after handshakeWait.
	_, _, _, served = pipeWorker(t)
	require.ErrorIs(t, <-served, errHandshake)
}

// Tests run VMs in-process: a client under go test never spawns its own
// binary, the cause of the October fork bomb on the AI box. (Not parallel:
// the client is process-wide.)
func TestTestsCannotSpawnAWorker(t *testing.T) {
	c, err := Acquire(ClientOptions{Options: Options{Dir: t.TempDir()}})
	require.NoError(t, err)
	_, err = c.Run(context.Background(), RunRequest{Project: "/p", Session: "s", Code: `1`})
	require.ErrorContains(t, err, "in-process")
	c.Release()

	self, err := os.Executable()
	require.NoError(t, err)
	c, err = Acquire(ClientOptions{Options: Options{Dir: t.TempDir()}, Binary: self})
	require.NoError(t, err)
	_, err = c.Run(context.Background(), RunRequest{Project: "/p", Session: "s", Code: `1`})
	require.ErrorContains(t, err, "own binary")
	c.Release()
}

// The in-process client shares one host by refcount; the last Release
// deletes its memory-only VMs. (Not parallel: the client is process-wide.)
func TestInProcessClient(t *testing.T) {
	dir := t.TempDir()
	c1, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	c2, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	require.Same(t, c1, c2)

	res, err := c1.Run(context.Background(), RunRequest{Project: "/p", Session: "s", Code: `v := 7`})
	require.NoError(t, err)
	require.NoError(t, res.Err)
	c1.Release() // one App closes; the other still uses it
	res, err = c2.Run(context.Background(), RunRequest{Project: "/p", Session: "s", Code: `v`})
	require.NoError(t, err)
	require.Equal(t, "7", res.Value)

	ephemeral, _ := filepath.Glob(filepath.Join(dir, "*", ".ephemeral", "*"))
	require.NotEmpty(t, ephemeral)
	c2.Release()
	ephemeral, _ = filepath.Glob(filepath.Join(dir, "*", ".ephemeral", "*"))
	require.Empty(t, ephemeral, "the last Release deletes memory-only VMs")
}

// The client kills a worker that is running a request and silent, or
// overdue; an idle worker is left alone.
func TestWorkerKillVerdict(t *testing.T) {
	t.Parallel()
	c := &workerConn{pending: map[uint64]*pendingReq{}}
	now := time.Now()
	c.last.Store(now.Add(-time.Hour).UnixNano())
	require.Empty(t, c.verdict(now), "idle")

	c.pending[1] = &pendingReq{deadline: now.Add(time.Minute)}
	c.last.Store(now.Add(-2 * time.Second).UnixNano())
	require.Empty(t, c.verdict(now), "busy and heard from recently")
	c.last.Store(now.Add(-11 * time.Second).UnixNano())
	require.Contains(t, c.verdict(now), "no heartbeat")

	c.last.Store(now.UnixNano())
	c.pending[1].deadline = now.Add(-overdueKill - time.Second)
	require.Contains(t, c.verdict(now), "deadline")
}

// Errors keep their kinds across the wire.
func TestWireErrors(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		&refusal{"r"}, &busyError{msg: "b"}, &busyError{msg: "c", again: true}, &parseError{errors.New("p")}, &limitError{kind: "memory"},
		&compileError{"c"}, &panicked{"x"}, &brokenError{vm: "v", cause: errBrokenJournal},
		&runError{err: &panicked{"y"}, stack: "at main"},
	} {
		got := fromWire(toWire(err))
		require.IsType(t, err, got)
		require.Equal(t, err.Error(), got.Error())
	}
}

// A request waiting for mem's lock is not overdue: its deadline starts again
// from each heartbeat that says it waits, and once more when the wait ends.
func TestWorkerLockWaitMovesTheDeadline(t *testing.T) {
	t.Parallel()
	c := &workerConn{pending: map[uint64]*pendingReq{}}
	start := time.Now()
	c.pending[1] = &pendingReq{deadline: start.Add(requestDeadline)}
	c.last.Store(start.UnixNano())

	at := start.Add(20 * time.Minute)
	c.heard(&frame{ID: 1, Type: msgHeartbeat, Waiting: true}, at)
	c.last.Store(at.UnixNano())
	require.Empty(t, c.verdict(at), "20 minutes waiting for mem's lock is not overdue")

	end := at.Add(time.Minute)
	c.heard(&frame{ID: 1, Type: msgHeartbeat}, end)
	c.last.Store(end.Add(10 * time.Minute).UnixNano())
	require.Empty(t, c.verdict(end.Add(10*time.Minute)), "the deadline runs from the wait's end")
	require.Equal(t, end.Add(requestDeadline), c.pending[1].deadline)

	later := end.Add(time.Minute)
	c.heard(&frame{ID: 1, Type: msgHeartbeat}, later)
	require.Equal(t, end.Add(requestDeadline), c.pending[1].deadline, "an ordinary heartbeat does not move it")
	c.last.Store(end.Add(requestDeadline + overdueKill + time.Second).UnixNano())
	require.Contains(t, c.verdict(end.Add(requestDeadline+overdueKill+time.Second)), "deadline")
}

// A catch-up whose slice is spent goes on through the client at once: the
// caller gets the answer, never "call again". (Not parallel: the client is
// process-wide, and it shortens memBatchOps and memSlice.)
func TestClientCallsACatchUpAgain(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t, dir)
	for i := 1; i <= 10; i++ {
		run(t, h, MemVM, fmt.Sprintf(`mem.Todo("goal %d")`, i))
	}
	require.NoError(t, h.Close())
	stores, _ := filepath.Glob(filepath.Join(memDir(dir, "/proj"), "store-*"))
	require.Len(t, stores, 1)
	require.NoError(t, os.RemoveAll(stores[0]))

	defer func(n int, d time.Duration) { memBatchOps, memSlice = n, d }(memBatchOps, memSlice)
	memBatchOps, memSlice = 3, 0
	c, err := Acquire(ClientOptions{Options: Options{Dir: dir}, InProcess: true})
	require.NoError(t, err)
	defer c.Release()
	res, err := c.Run(context.Background(), RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: `println(mem.Show())`})
	require.NoError(t, err)
	require.NoError(t, res.Err)
	for i := 1; i <= 10; i++ {
		require.Contains(t, res.Output, fmt.Sprintf("`goal-%d`", i))
	}
}
