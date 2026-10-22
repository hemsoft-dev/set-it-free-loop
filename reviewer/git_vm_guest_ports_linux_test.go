//go:build linux

package reviewer

import (
	"context"
	"os"
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
		before := gitPacketFDCount(t)
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
		if gitPacketFDCount(t) != before-1 {
			t.Fatal("port watcher leaked a retained descriptor")
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
	before := gitPacketFDCount(t)
	if join, err := gitVMGuestWatchPorts(ctx, cancel, r, nil, r); err == nil {
		join()
		t.Fatal("missing port accepted")
	}
	if gitPacketFDCount(t) != before {
		t.Fatal("failed watch leaked retained descriptors")
	}
}
