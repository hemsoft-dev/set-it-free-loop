//go:build linux

package reviewer

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type nativeDelayedObjectRead struct {
	io.ReadWriteCloser
	ready <-chan struct{}
}

func (c nativeDelayedObjectRead) Read(data []byte) (int, error) {
	<-c.ready
	return c.ReadWriteCloser.Read(data)
}

// Release an already-ready peer EOF exactly when cleanup starts waiting for
// the object result. This reproduces a late-scheduled reader without a timer.
type nativeObjectDrainContext struct {
	context.Context
	once  sync.Once
	ready chan struct{}
}

func (c *nativeObjectDrainContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestNativeVMObjectShutdownDrainsPeerEOFBeforeClosingLocalEndpoint(t *testing.T) {
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer channels.Close()
	peers, err := channels.duplicateGuest()
	if err != nil {
		t.Fatal(err)
	}
	closeVMFiles(peers) // The owned VM has stopped; every peer descriptor is gone.
	done := make(chan error, 1)
	ctx := &nativeObjectDrainContext{Context: context.Background(), ready: make(chan struct{})}
	go func() {
		done <- ServeVMObjects(context.Background(), nativeDelayedObjectRead{channels.host[1], ctx.ready}, testSession(Codex), []string{"source.go"}, &fixtureStore{})
	}()
	p := &nativeVMHostPhase{channels: channels, objectsDone: done}
	if err := p.drainObjects(ctx, true); err != nil {
		t.Fatal("normal peer EOF was replaced by a local close error", err)
	}
}

func TestNativeVMControlCancellationInterruptsPendingReadBeforePhaseDeadline(t *testing.T) {
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer channels.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channels.host[0].SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	stop := channels.interruptControlOnCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { var b [1]byte; _, err := channels.host[0].Read(b[:]); done <- err }()
	select {
	case <-done:
		t.Fatal("control read returned before cancellation")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation waited for the original phase deadline")
	}
}

func TestNativeVMControlContextDoesNotInterruptNormalPeerExchange(t *testing.T) {
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer channels.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := channels.interruptControlOnCancel(ctx)
	defer stop()
	if err := channels.host[0].SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = channels.guest[0].Write([]byte("x")) }()
	var b [1]byte
	if _, err := io.ReadFull(channels.host[0], b[:]); err != nil || b[0] != 'x' {
		t.Fatal("normal control exchange interrupted", err)
	}
}
