//go:build linux

package reviewer

import (
	"context"
	"io"
	"sync"
	"testing"
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
