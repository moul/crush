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
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
)

// turns are this process's holds on sessions' turn locks (turnLockPath),
// counted: a run that starts while an earlier run of the same session here
// still holds it, as the agent's run for a queued prompt does from inside
// the run before it, shares the hold instead of waiting for itself.
var turns struct {
	sync.Mutex
	held map[string]*turnHold
}

type turnHold struct {
	n       int
	release func()
}

// HoldTurn holds session's turn lock for one run of it, so other sessions
// see it working (PARALLEL.md); the release it returns lets go of it when
// the session's last run in this process ends. The lock is waited for, with
// ctx: probes hold it for a moment, and a session open in two gnosh
// processes waits for the other's turn (it says so in the log). A hold that
// cannot be had is a no-op: the session is then not seen working.
func (l *Lazy) HoldTurn(ctx context.Context, session string) func() {
	if l == nil || l.opts.Dir == "" || session == "" {
		return func() {}
	}
	return holdTurn(ctx, l.opts.Dir, session)
}

func holdTurn(ctx context.Context, dir, session string) func() {
	path := turnLockPath(dir, session)
	if shareTurn(path) {
		return turnRelease(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("Could not make the turn locks' directory", "error", err)
		return func() {}
	}
	release, err := lock.TryFile(path)
	if errors.Is(err, lock.ErrContended) {
		// A peer's probe holds it for a moment; another gnosh process
		// with the session open holds it while busy there.
		slog.Debug("Waiting for this session's turn lock", "session_id", session)
		start := time.Now()
		release, err = lock.File(ctx, path)
		if d := time.Since(start); err == nil && d > time.Second {
			slog.Info("Waited for this session's turn lock, which another gnosh process held", "session_id", session, "waited", d.Round(time.Second))
		}
	}
	if err != nil {
		slog.Warn("Could not hold the session's turn lock; peers will not see it working", "session_id", session, "error", err)
		return func() {}
	}
	turns.Lock()
	if t := turns.held[path]; t != nil {
		t.n++
		turns.Unlock()
		release()
		return turnRelease(path)
	}
	if turns.held == nil {
		turns.held = map[string]*turnHold{}
	}
	turns.held[path] = &turnHold{n: 1, release: release}
	turns.Unlock()
	return turnRelease(path)
}

// shareTurn adds a run to a hold this process has on path already.
func shareTurn(path string) bool {
	turns.Lock()
	defer turns.Unlock()
	if t := turns.held[path]; t != nil {
		t.n++
		return true
	}
	return false
}

func turnRelease(path string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			turns.Lock()
			t := turns.held[path]
			if t == nil {
				turns.Unlock()
				return
			}
			t.n--
			if t.n > 0 {
				turns.Unlock()
				return
			}
			delete(turns.held, path)
			turns.Unlock()
			t.release()
		})
	}
}
