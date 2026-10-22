//go:build linux

package reviewer

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

// Virtio writes can wait indefinitely after host close. Watch disconnect state
// independently of protocol reads and writes. Retained duplicates keep every
// polled number bound to its original port until the watcher has joined.
func gitVMGuestWatchPorts(ctx context.Context, cancel context.CancelFunc, files ...*os.File) (func(), error) {
	if ctx == nil || ctx.Err() != nil || cancel == nil || len(files) != 3 {
		return nil, ErrVMBoot
	}
	var held []*os.File
	var polls []unix.PollFd
	for _, file := range files {
		if file == nil {
			closeVMFiles(held)
			return nil, ErrVMBoot
		}
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			closeVMFiles(held)
			return nil, ErrVMBoot
		}
		held = append(held, os.NewFile(uintptr(fd), "source-port-monitor"))
		// HUP/ERR/NVAL are reported even with no requested readiness events.
		polls = append(polls, unix.PollFd{Fd: int32(fd)})
	}
	watch, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for watch.Err() == nil {
			for i := range polls {
				polls[i].Revents = 0
			}
			_, err := unix.Poll(polls, 100)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				cancel()
				return
			}
			for _, p := range polls {
				if p.Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done; closeVMFiles(held) }, nil
}
