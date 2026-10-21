//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGitBrokerOriginalPeerCannotBeForgedOrRetainedAfterShutdown(t *testing.T) {
	type retainedPeer struct {
		peer     GitReadPeer
		identity GateIdentity
		fd       int
	}
	captured := make(chan retainedPeer, 1)
	var first atomic.Bool
	var exchanges atomic.Int32
	invocation := strings.Repeat("d", 32)
	source := gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		exchanges.Add(1)
		return io.NopCloser(strings.NewReader("0000")), nil
	})
	admit := func(ctx context.Context, peer GitReadPeer) error {
		if err := ownGitBrokerPeer(ctx, peer); err != nil {
			return err
		}
		identity, fd, err := retainGitBrokerPeer(peer, invocation)
		if err != nil {
			return err
		}
		if first.CompareAndSwap(false, true) {
			captured <- retainedPeer{peer, identity, fd}
		} else {
			unix.Close(fd)
		}
		return nil
	}
	endpoint, finish := startGitBrokerTest(t, source, admit)
	conn := gitBrokerConnect(t, endpoint)
	var original retainedPeer
	select {
	case original = <-captured:
	case <-time.After(time.Second):
		t.Fatal("original kernel peer not retained")
	}
	defer unix.Close(original.fd)
	if original.identity.PID != os.Getpid() || original.identity.InvocationID != invocation || gitCheckProcessPIDFD(original.identity, original.fd) != nil {
		t.Fatal("original peer identity lost")
	}
	flags, err := unix.FcntlInt(uintptr(original.fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inheritable peer pidfd")
	}
	for _, mutate := range []func(*GitReadPeer){func(v *GitReadPeer) { v.PID++ }, func(v *GitReadPeer) { v.UID++ }, func(v *GitReadPeer) { v.GID++ }, func(v *GitReadPeer) { v.kernel = nil }} {
		changed := original.peer
		mutate(&changed)
		if _, fd, err := retainGitBrokerPeer(changed, invocation); err == nil {
			unix.Close(fd)
			t.Fatal("forged peer metadata retained")
		}
	}
	if _, fd, err := retainGitBrokerPeer(original.peer, "invalid"); err == nil {
		unix.Close(fd)
		t.Fatal("invalid invocation accepted")
	}
	metadata := GitReadPeer{PID: original.peer.PID, UID: original.peer.UID, GID: original.peer.GID}
	if _, fd, err := retainGitBrokerPeer(metadata, invocation); err == nil {
		unix.Close(fd)
		t.Fatal("metadata alone manufactured capability")
	}
	fmt.Fprintf(conn, "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\n\r\n")
	if raw := gitBrokerReply(t, conn); string(raw) != "0000" {
		t.Fatal("exchange changed")
	}
	// Retention races handler shutdown. Every success owns a duplicate, and
	// every failure leaves no descriptor. Shutdown must revoke the old handle.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 64 {
			if _, fd, err := retainGitBrokerPeer(original.peer, invocation); err == nil {
				unix.Close(fd)
			}
		}
	}()
	conn.Close()
	result := finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("peer retention did not drain")
	}
	if exchanges.Load() != 1 || result.audit.Requests != 1 {
		t.Fatal("unexpected transport count")
	}
	if original.peer.kernel.alive(original.peer) == nil {
		t.Fatal("closed original handle remained live")
	}
	if _, fd, err := retainGitBrokerPeer(original.peer, invocation); err == nil {
		unix.Close(fd)
		t.Fatal("retained after shutdown")
	}
	if gitCheckProcessPIDFD(original.identity, original.fd) != nil {
		t.Fatal("broker closed caller-owned duplicate")
	}
	// Make the spent original number a valid live pidfd again. The revoked
	// capability must refuse before consulting that newly reused number.
	number := original.peer.kernel.fd
	replacement, err := unix.FcntlInt(uintptr(original.fd), unix.F_DUPFD_CLOEXEC, number)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(replacement)
	if replacement != number {
		t.Fatal("fixture could not reuse spent descriptor")
	}
	if gitCheckProcessPIDFD(original.identity, replacement) != nil {
		t.Fatal("replacement fixture is not live")
	}
	if _, fd, err := retainGitBrokerPeer(original.peer, invocation); err == nil {
		unix.Close(fd)
		t.Fatal("descriptor reuse revived old peer")
	}
}
