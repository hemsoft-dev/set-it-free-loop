//go:build linux

package reviewer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ProxySourceMCP copies only bounded MCP bytes to the captured private Unix
// endpoint. It verifies SO_PEERCRED before reading/sending any client data.
// This authenticates the server UID, not the job or its whole OS confinement.
// The supervisor must protect the endpoint path and bind it to the right broker.
func ProxySourceMCP(ctx context.Context, socket string, readerUID int, limits Limits, input io.ReadCloser, output io.WriteCloser) error {
	if input == nil || output == nil {
		return ErrSourceProxyUnavailable
	}
	var connection *net.UnixConn
	var closed sync.Once
	closeAll := func() {
		closed.Do(func() {
			if connection != nil {
				_ = connection.Close()
			}
			_ = input.Close()
			_ = output.Close()
		})
	}
	defer closeAll()
	if limits.Validate() != nil || !sourcePath(socket) || len(socket) > 107 || readerUID <= 0 || uint64(readerUID) > uint64(^uint32(0)) {
		return ErrSourceProxyUnavailable
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil || parent != filepath.Dir(socket) {
		return ErrSourceProxyUnavailable
	}
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ErrSourceProxyUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSourceProxyUnavailable
	}
	var ok bool
	connection, ok = raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return ErrSourceProxyUnavailable
	}
	control, err := connection.SyscallConn()
	if err != nil {
		return ErrSourceProxyUnavailable
	}
	var peer *syscall.Ucred
	var peerErr error
	err = control.Control(func(fd uintptr) {
		peer, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || peerErr != nil || peer == nil || uint64(peer.Uid) != uint64(readerUID) {
		return ErrSourceProxyUnavailable
	}
	stop := context.AfterFunc(ctx, closeAll)
	defer stop()
	type result struct {
		outbound bool
		err      error
	}
	done := make(chan result, 2)
	copyBounded := func(destination io.Writer, source io.Reader, limit int64) error {
		n, err := io.Copy(destination, io.LimitReader(source, limit))
		if err != nil {
			return ErrSourceProxyUnavailable
		}
		if n == limit {
			// Observe at most one extra byte without forwarding it. EOF at
			// exactly the advertised cap is valid; only an overrun exhausts it.
			var extra [1]byte
			more, err := io.ReadFull(source, extra[:])
			if more != 0 {
				return ErrSourceProxyBudget
			}
			if err != io.EOF {
				return ErrSourceProxyUnavailable
			}
		}
		return nil
	}
	go func() {
		err := copyBounded(connection, input, int64(limits.InputBytes+limits.ReaderCalls*2048))
		if err == nil {
			_ = connection.CloseWrite()
		}
		done <- result{false, err}
	}()
	go func() {
		err := copyBounded(output, connection, int64(12*limits.ReaderBytes+limits.ReaderCalls*8192+(128<<10)))
		done <- result{true, err}
	}()
	var failure error
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			failure = errors.Join(failure, r.err)
		}
		// A completed/failed server stream must also unblock a client that keeps
		// stdin open. Input EOF alone permits the server to finish its output.
		if r.outbound || r.err != nil {
			closeAll()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return failure
}
