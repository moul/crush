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
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// turnHeld reports whether session's turn lock is held now, in dir: the
// session is in a busy stretch. The file is opened read-only and never
// made, and probed in shared mode, so probes never queue behind each other;
// a missing file is a session not running.
func turnHeld(dir, session string) bool {
	fd, err := unix.Open(turnLockPath(dir, session), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return false
	}
	return errors.Is(err, unix.EWOULDBLOCK)
}

// ring writes one byte to each pipe in mem's waiters/active/, so every Next
// waiting on one wakes once for this change. It never blocks: a full pipe
// is rung already, and a pipe nobody reads any more is removed. Pending
// pipes in waiters/ are only probed, never written (a byte before a wait
// decided would be a false wake), and removed when nobody reads them.
func (h *memHost) ring() {
	waiters := filepath.Join(h.dir, "waiters")
	active, _ := os.ReadDir(filepath.Join(waiters, "active"))
	for _, e := range active {
		path := filepath.Join(waiters, "active", e.Name())
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENOENT) {
				_ = os.Remove(path)
			}
			continue
		}
		_, err = unix.Write(fd, []byte{1})
		_ = unix.Close(fd)
		if errors.Is(err, unix.EPIPE) {
			_ = os.Remove(path)
		}
	}
	pending, _ := os.ReadDir(waiters)
	for _, e := range pending {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(waiters, e.Name())
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				_ = os.Remove(path)
			}
			continue
		}
		_ = unix.Close(fd)
	}
}

// activate moves pending pipe name into waiters/active/, where every change
// rings it; false when it is not there (its waiter went, or a sweep found
// no reader) or cannot be moved.
func (h *memHost) activate(name string) bool {
	if !validPipe(name) {
		return false
	}
	to := filepath.Join(h.dir, "waiters", "active", name)
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return false
	}
	return os.Rename(filepath.Join(h.dir, "waiters", name), to) == nil
}

// activePipe reports whether pipe name is active, in waiters/active/: its
// Next waits on it.
func (h *memHost) activePipe(name string) bool {
	if !validPipe(name) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(h.dir, "waiters", "active", name))
	return err == nil && fi.Mode()&os.ModeNamedPipe != 0
}

// pendingPipe reports whether pending pipe name is in waiters/.
func (h *memHost) pendingPipe(name string) bool {
	if !validPipe(name) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(h.dir, "waiters", name))
	return err == nil && fi.Mode()&os.ModeNamedPipe != 0
}
