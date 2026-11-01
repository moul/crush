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
	"slices"
	"strings"
	"time"
)

// The client's side of peers (PARALLEL.md): a mem input whose Next may wait
// is sent with a wake pipe; when its Next waits, the client sleeps on the
// pipe, which every change to mem rings, and on the holders' turns ending,
// then runs mem.Next() again, as a new call, until it steps in or has
// nothing to wait for. The model gets one result.

// mayWaitInput reports whether req is a mem input whose Next may wait: a
// bare mem.Next() ends it, and it is not a sub-agent's.
func mayWaitInput(req RunRequest) bool {
	return req.Name == MemVM && !req.SubAgent && req.Session != "" && endsWithBareNext(req.Code)
}

// runWaiting runs req, a mem input whose Next may wait, and waits with it.
func (c *Client) runWaiting(ctx context.Context, req RunRequest) (*RunResult, error) {
	dir := memDir(c.opts.Dir, req.Project)
	var merged *RunResult
	stack := req.Frames
	for n := 0; ; n++ {
		pipe := newWakePipe(dir)
		if pipe != nil {
			req.WaitPipe = pipe.name
		}
		res, err := c.runMem(ctx, req, n > 0)
		if err == nil && res != nil && res.Err == nil && res.Journaled {
			c.memChanged(req.Project)
		}
		if err != nil || res == nil || res.Err != nil {
			pipe.close()
			if merged == nil {
				return res, err
			}
			// What came before stays: it was kept, and its frames must
			// reach the agent.
			why := ""
			switch {
			case err != nil:
				why = err.Error()
			case res != nil && res.Err != nil:
				why = res.Err.Error()
			}
			merged.Output += "(mem.Next() again failed: " + why + "; mem.Show() says where you stand)\n"
			return merged, nil
		}
		merged = mergeRuns(merged, res)
		stack = stackAfter(stack, res.Frames)
		if len(res.Waits) == 0 || pipe == nil {
			pipe.close()
			merged.Waits = nil
			return merged, nil
		}
		start := time.Now()
		woke := c.sleep(ctx, pipe, res.Waits)
		pipe.close()
		if woke != nil {
			merged.Output += "stopped waiting for " + waitsText(res.Waits) + " after " + roundWait(time.Since(start)) + ": " + woke.Error() + "\n"
			merged.Waits = nil
			return merged, nil
		}
		merged.Output += "waited " + roundWait(time.Since(start)) + " for " + waitsText(res.Waits) + "\n"
		req = RunRequest{Project: req.Project, Session: req.Session, Name: MemVM, Code: "mem.Next()", Req: fmt.Sprintf("%s/next-%d", req.Req, n+1), Goal: req.Goal, Frames: stack}
	}
}

// runMem runs one mem call, called again at once while its catch-up spends
// its slice. A re-run is never left behind by Esc: its cancel is sent and
// its reply awaited (rerun), so a push it made is never lost.
func (c *Client) runMem(ctx context.Context, req RunRequest, rerun bool) (*RunResult, error) {
	run := c.run
	if rerun {
		run = c.runAwaited
	}
	res, err := run(ctx, req)
	for callAgain(ctx, err) {
		res, err = run(ctx, req)
	}
	return res, err
}

// sleep waits until mem changes (the pipe is rung), a holder's turn ends,
// or ctx ends; nil for the first two.
func (c *Client) sleep(ctx context.Context, pipe *wakePipe, waits []MemWait) error {
	woke := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	signal := func(ch <-chan struct{}) {
		select {
		case <-ch:
			select {
			case woke <- struct{}{}:
			default:
			}
		case <-stop:
		}
	}
	go signal(pipe.rung())
	var seen []string
	for _, w := range waits {
		if !slices.Contains(seen, w.Session) {
			seen = append(seen, w.Session)
			go signal(turnEnded(c.opts.Dir, w.Session))
		}
	}
	select {
	case <-woke:
		return nil
	case <-ctx.Done():
		return errors.New("the turn was stopped")
	}
}

// mergeRuns puts a re-run's result after what came before, as one result.
func mergeRuns(prev, next *RunResult) *RunResult {
	if prev == nil {
		out := *next
		return &out
	}
	out := *prev
	out.Output += next.Output
	out.Notes = append(out.Notes, next.Notes...)
	out.Rules = append(out.Rules, next.Rules...)
	out.Frames = append(out.Frames, next.Frames...)
	out.Wall += next.Wall
	out.Journaled = out.Journaled || next.Journaled
	if next.Seq > 0 {
		out.Seq = next.Seq
	}
	if next.Erased != nil {
		out.Erased = next.Erased
	}
	for _, g := range next.MemGoals {
		if i := slices.IndexFunc(out.MemGoals, func(o MemGoal) bool { return o.Name == g.Name }); i >= 0 {
			out.MemGoals[i] = g
		} else {
			out.MemGoals = append(out.MemGoals, g)
		}
	}
	out.Waits = next.Waits
	return &out
}

// waitsText names what a Next waits for: the todos and their sessions.
func waitsText(waits []MemWait) string {
	var parts []string
	for _, w := range waits {
		s := w.Session
		if len(s) > 8 {
			s = s[:8]
		}
		parts = append(parts, "`"+w.Id+"` (session "+s+")")
	}
	return strings.Join(parts, ", ")
}

// roundWait is a wait's length as a person reads it: 850ms, 41s, 2m5s.
func roundWait(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
