//go:build !windows

package lock

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile waits for the lock in the kernel: flock without LOCK_NB, on a
// goroutine of its own, so the caller can stop waiting when ctx ends and
// the waiter is woken the moment the holder lets go (no polling). A wait
// the caller gave up on goes on until the kernel grants the lock, and then
// closes f, which drops it. On error f is closed, now or then: the caller
// must not close it.
func lockFile(ctx context.Context, f *os.File) (func(), error) {
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	// flock runs on f's descriptor, which Control keeps valid while it
	// runs; a signal (the Go runtime's own included) interrupts a waiting
	// flock with EINTR, which is not an answer.
	flock := func(how int) error {
		var err error
		if cerr := rc.Control(func(fd uintptr) {
			for {
				if err = unix.Flock(int(fd), how); !errors.Is(err, unix.EINTR) {
					return
				}
			}
		}); cerr != nil {
			return cerr
		}
		return err
	}
	unlock := func() { _ = flock(unix.LOCK_UN) }
	switch err := flock(unix.LOCK_EX | unix.LOCK_NB); {
	case err == nil:
		return unlock, nil
	case !errors.Is(err, unix.EWOULDBLOCK):
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	got := make(chan error, 1)
	go func() { got <- flock(unix.LOCK_EX) }()
	select {
	case err := <-got:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		return unlock, nil
	case <-ctx.Done():
		go func() {
			<-got
			f.Close()
		}()
		return nil, fmt.Errorf("acquire lock: %w", ctx.Err())
	}
}

func tryLockFile(f *os.File) (func(), error) {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrContended
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}
