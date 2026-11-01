// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

//go:build unix

package gnovm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// wakePipe is a named pipe a waiting Next sleeps on (mempeers.go's ring
// writes it): the read end non-blocking, so Go's poller parks the reader
// (epoll on Linux, kqueue on macOS), and a write end of its own held for the
// wait, so an empty pipe reads as "nothing yet", never as its end. Both are
// close-on-exec.
type wakePipe struct {
	name  string
	dir   string
	r, w  *os.File
	once  sync.Once
	ready chan struct{}
}

// newWakePipe makes a pipe in mem dir's waiters/new/, opens both ends, and
// only then moves it into waiters/, pending, so a sweep never removes it
// before it can be read. nil when it cannot be made: Next then answers as
// one that may not wait.
func newWakePipe(dir string) *wakePipe {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil
	}
	name := hex.EncodeToString(b[:])
	fresh := filepath.Join(dir, "waiters", "new", name)
	if err := os.MkdirAll(filepath.Dir(fresh), 0o700); err != nil {
		return nil
	}
	if err := unix.Mkfifo(fresh, 0o600); err != nil {
		return nil
	}
	rfd, err := unix.Open(fresh, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(fresh)
		return nil
	}
	wfd, err := unix.Open(fresh, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(rfd)
		_ = os.Remove(fresh)
		return nil
	}
	p := &wakePipe{name: name, dir: dir, r: os.NewFile(uintptr(rfd), fresh), w: os.NewFile(uintptr(wfd), fresh), ready: make(chan struct{})}
	if err := os.Rename(fresh, filepath.Join(dir, "waiters", name)); err != nil {
		p.close()
		_ = os.Remove(fresh)
		return nil
	}
	return p
}

// rung is closed when the pipe is rung: a byte to read.
func (p *wakePipe) rung() <-chan struct{} {
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := p.r.Read(buf)
			if n > 0 {
				close(p.ready)
				return
			}
			if err != nil && !errors.Is(err, unix.EAGAIN) {
				return
			}
		}
	}()
	return p.ready
}

// close lets go of the pipe: the reader returns, and the file goes from
// waiters/ or waiters/active/, wherever the call left it.
func (p *wakePipe) close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		_ = p.r.Close()
		_ = p.w.Close()
		_ = os.Remove(filepath.Join(p.dir, "waiters", p.name))
		_ = os.Remove(filepath.Join(p.dir, "waiters", "active", p.name))
	})
}

// turnWatches are this process's waits on other sessions' turns: one per
// session, shared by every Next waiting on it, each closing its channel
// when the kernel grants it the session's turn lock in shared mode, that
// is, when the session's busy stretch ends or its process dies.
var turnWatches struct {
	sync.Mutex
	m map[string]chan struct{}
}

// turnEnded is closed when session's turn, in gnosh's gnovm data directory
// dir, ends; at once when the session has no turn lock.
func turnEnded(dir, session string) <-chan struct{} {
	path := turnLockPath(dir, session)
	turnWatches.Lock()
	defer turnWatches.Unlock()
	if ch, ok := turnWatches.m[path]; ok {
		return ch
	}
	if turnWatches.m == nil {
		turnWatches.m = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	turnWatches.m[path] = ch
	go func() {
		defer func() {
			turnWatches.Lock()
			delete(turnWatches.m, path)
			turnWatches.Unlock()
			close(ch)
		}()
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return
		}
		defer unix.Close(fd)
		for {
			err := unix.Flock(fd, unix.LOCK_SH)
			if err == nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				return
			}
			if !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	return ch
}
