//go:build linux

package reviewer

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"slices"
	"testing"
	"time"
)

func TestGitVMGuestPortLossCancelsAndJoinsOriginalDescriptors(t *testing.T) {
	for selected := 0; selected < 3; selected++ {
		var readers, writers []*os.File
		for i := 0; i < 3; i++ {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			readers = append(readers, r)
			writers = append(writers, w)
		}
		ctx, cancel := context.WithCancel(context.Background())
		watch := gitWatchFDs(t, gitFDFiles(t, slices.Concat(readers, writers)...))
		join, err := gitVMGuestWatchPorts(ctx, cancel, readers...)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("healthy ports cancelled")
		case <-time.After(20 * time.Millisecond):
		}
		writers[selected].Close()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("lost peer did not cancel")
		}
		join()
		if leaked := watch.leaked(t); len(leaked) != 0 {
			t.Fatal("port watcher leaked a retained descriptor", leaked)
		}
		closeVMFiles(readers)
		closeVMFiles(writers)
		cancel()
	}
}

func TestGitVMGuestPortWatchPartialFailureRetainsNoDescriptors(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := gitWatchFDs(t, gitFDFiles(t, r, w))
	if join, err := gitVMGuestWatchPorts(ctx, cancel, r, nil, r); err == nil {
		join()
		t.Fatal("missing port accepted")
	}
	if leaked := watch.leaked(t); len(leaked) != 0 {
		t.Fatal("failed watch leaked retained descriptors", leaked)
	}
}

func TestGitVMGuestPortWatchPreservesOriginalStatusFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var readers, writers []*os.File
	for i := 0; i < 3; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, r)
		writers = append(writers, w)
	}
	defer closeVMFiles(readers)
	defer closeVMFiles(writers)
	flags := func(file *os.File) int {
		t.Helper()
		fd, err := gitControlFileFD(file)
		if err != nil {
			t.Fatal(err)
		}
		value, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := []int{flags(readers[0]), flags(readers[1]), flags(readers[2])}
	join, err := gitVMGuestWatchPorts(ctx, cancel, readers...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); join() }()
	for i, file := range readers {
		if flags(file) != before[i] {
			t.Fatal("port watch mutated original descriptor status flags")
		}
	}
}
