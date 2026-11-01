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
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/version"
)

const (
	// handshakeWait is how long a worker waits for its client's hello.
	handshakeWait = 2 * time.Second
	// heartbeatEvery is how often a worker reports progress on the
	// request it is running.
	heartbeatEvery = 500 * time.Millisecond
)

// errHandshake is a worker that was not started by a gnosh client, or by
// one of another build.
var errHandshake = errors.New("gnovm worker: no valid handshake")

// myHello is this build's half of the handshake.
func myHello() hello {
	return hello{Magic: protocolMagic, Protocol: protocolVersion, Build: version.BuildID, Gno: stdlibVersion()}
}

// serve runs the worker's side of the protocol on in and out until in
// ends: the handshake, then requests one at a time, with cancels taken at
// once by a reader of their own and a heartbeat for the whole life of each
// request. setup, if not nil, runs after a good handshake with what the
// client sent (the worker's process limits; tests pass nil).
func serve(in io.Reader, out io.Writer, setup func(hello)) error {
	w := &workerOut{w: out}
	frames := make(chan *frame)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := readFrame(in)
			if err != nil {
				readErr <- err
				close(frames)
				return
			}
			frames <- f
		}
	}()

	// The handshake comes first, and soon.
	var first *frame
	select {
	case first = <-frames:
	case <-time.After(handshakeWait):
	}
	var h hello
	if first == nil || first.Type != msgHello || json.Unmarshal(first.Body, &h) != nil ||
		h.Magic != protocolMagic {
		return errHandshake
	}
	mine := myHello()
	if h.Protocol != mine.Protocol || h.Build != mine.Build || h.Gno != mine.Gno {
		_ = w.send(&frame{Type: msgHello, Body: mustJSON(mine), Err: &wireError{
			Kind: "other",
			Msg:  fmt.Sprintf("worker is build %s (gno %s), client is %s (gno %s): restart gnosh", mine.Build, mine.Gno, h.Build, h.Gno),
		}})
		return errHandshake
	}
	host, err := NewHost(Options{
		Dir:             h.Dir,
		Client:          h.Client,
		BudgetBytes:     h.BudgetBytes,
		DefaultMemoryMB: h.DefaultMemoryMB,
		DefaultGas:      h.DefaultGas,
		NsPerGas:        h.NsPerGas,
		Embeddings:      h.Embeddings,
	})
	if err != nil {
		_ = w.send(&frame{Type: msgHello, Err: toWire(err)})
		return err
	}
	defer host.Close()
	if setup != nil {
		setup(h)
	}
	if err := w.send(&frame{Type: msgHello, Body: mustJSON(mine)}); err != nil {
		return err
	}

	// Requests queue; one runs at a time. The reader goroutine above keeps
	// reading, so a cancel reaches the running request at once.
	var (
		mu      sync.Mutex
		queue   []*frame
		running uint64
		cancel  context.CancelFunc
		wake    = make(chan struct{}, 1)
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mu.Lock()
			if len(queue) == 0 {
				mu.Unlock()
				if _, ok := <-wake; !ok {
					return
				}
				continue
			}
			f := queue[0]
			queue = queue[1:]
			ctx, c := context.WithCancel(context.Background())
			running, cancel = f.ID, c
			mu.Unlock()

			var waiting atomic.Bool
			ctx = withLockWaiting(ctx, &waiting)
			stop := w.heartbeats(f.ID, &waiting)
			reply := handle(ctx, host, f)
			stop()
			c()
			mu.Lock()
			running, cancel = 0, nil
			mu.Unlock()
			if err := w.send(reply); err != nil {
				log.Printf("gnovm worker: reply to %d: %v", f.ID, err)
			}
		}
	}()

	for f := range frames {
		switch f.Type {
		case msgCancel:
			mu.Lock()
			if running == f.ID && cancel != nil {
				cancel()
			} else {
				for i, q := range queue {
					if q.ID == f.ID {
						queue = append(queue[:i], queue[i+1:]...)
						_ = w.send(&frame{ID: f.ID, Type: msgResult, Err: toWire(&limitError{kind: "cancelled"})})
						break
					}
				}
			}
			mu.Unlock()
			_ = w.send(&frame{ID: f.ID, Type: msgAck})
		default:
			mu.Lock()
			queue = append(queue, f)
			mu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
	// The client is gone: stop what runs, and quit.
	mu.Lock()
	queue = nil
	if cancel != nil {
		cancel()
	}
	mu.Unlock()
	close(wake)
	<-done
	if err := <-readErr; err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// handle runs one request against the host and makes its reply.
func handle(ctx context.Context, host *Host, f *frame) *frame {
	reply := &frame{ID: f.ID, Type: msgResult}
	fail := func(err error) *frame { reply.Err = toWire(err); return reply }
	switch f.Type {
	case msgRun:
		var req RunRequest
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		res, err := host.Run(ctx, req)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(wireRun{RunResult: *res, Err: toWire(res.Err)})
	case msgNew:
		var req NewRequest
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		info, created, err := host.New(ctx, req)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(newResp{Info: info, Created: created})
	case msgList:
		var req listReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		list, err := host.List(req.Project)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(list)
	case msgDrop:
		var req dropReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		if err := host.Drop(ctx, req.Project, req.Name, req.Action); err != nil {
			return fail(err)
		}
	case msgKept:
		var req keptReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		if req.Frames && req.Name == MemVM {
			frames, kept, err := host.KeptFrames(req.Project, req.Req)
			if err != nil {
				return fail(err)
			}
			reply.Body = mustJSON(keptResp{Kept: kept, Frames: frames})
			break
		}
		kept, err := host.Kept(req.Project, req.Name, req.Req)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(kept)
	case msgRepair:
		var req repairReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		kept, cut, err := host.Repair(ctx, req.Project, req.Name)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(repairResp{Kept: kept, Cut: cut})
	case msgGoal:
		var req goalReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		resp, err := host.goal(ctx, req)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(resp)
	case msgMem:
		var req memReq
		if err := json.Unmarshal(f.Body, &req); err != nil {
			return fail(err)
		}
		out, erased, err := host.memCommand(ctx, req.Project, req.Session, req.Args, req.Titles)
		if err != nil {
			return fail(err)
		}
		reply.Body = mustJSON(memResp{Text: out, Erased: erased})
	default:
		return fail(fmt.Errorf("unknown request %q", f.Type))
	}
	return reply
}

// workerOut serialises the worker's writes: replies, acks and heartbeats
// come from different goroutines.
type workerOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *workerOut) send(f *frame) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return writeFrame(o.w, f)
}

// heartbeats sends a heartbeat for request id every heartbeatEvery until the
// returned stop is called. They come from their own goroutine, so a long
// metered run, a replay or a base load all keep the client from taking the
// worker for stuck; each says whether the request waits for mem's lock.
func (o *workerOut) heartbeats(id uint64, waiting *atomic.Bool) (stop func()) {
	quit := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		var n uint64
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				n++
				if o.send(&frame{ID: id, Type: msgHeartbeat, Progress: n, Waiting: waiting.Load()}) != nil {
					return
				}
			}
		}
	}()
	return func() { close(quit); <-stopped }
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
