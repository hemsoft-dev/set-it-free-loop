//go:build linux

package reviewer

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func sourcePeerUID(connection *net.UnixConn) (uint32, error) {
	control, err := connection.SyscallConn()
	if err != nil {
		return 0, ErrSourceService
	}
	var peer *syscall.Ucred
	var peerErr error
	err = control.Control(func(fd uintptr) {
		peer, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || peerErr != nil || peer == nil {
		return 0, ErrSourceService
	}
	return peer.Uid, nil
}

// ServeSourceSocket serves one captured broker from the reader identity. It
// requires a private existing reader-owned root and a different non-root worker
// UID. The supervisor separately supplies the shared IPC GID and exposes only
// this job directory in the worker mount namespace. Matching IDs/permissions do
// not establish that namespace, provider isolation or entitlement.
func ServeSourceSocket(ctx context.Context, config SourceReaderConfig, verifier *Reader, ready func(SourceEndpoint) error) (*SourceAccess, error) {
	return serveSourceSocket(ctx, config, verifier, ready, sourcePeerUID)
}

func serveSourceSocket(ctx context.Context, config SourceReaderConfig, verifier *Reader, ready func(SourceEndpoint) error, peerUID func(*net.UnixConn) (uint32, error)) (*SourceAccess, error) {
	return serveSourceSocketDraining(ctx, nil, config, verifier, ready, peerUID)
}

// A trusted graceful stop closes only intake. An already accepted connection
// retains its original bounded phase context so queued work and clean EOF can
// produce the stopped audit. Hard cancellation still closes both endpoints.
func serveSourceSocketDraining(ctx, drain context.Context, config SourceReaderConfig, verifier *Reader, ready func(SourceEndpoint) error, peerUID func(*net.UnixConn) (uint32, error)) (access *SourceAccess, serveErr error) {
	uid, gid := os.Getuid(), os.Getgid()
	if config.validate() != nil || uid <= 0 || uid != config.ReaderUID || os.Geteuid() != uid || os.Getegid() != gid || config.WorkerUID == uid || config.WorkerGID != gid || ready == nil {
		return nil, ErrSourceService
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	root, err := filepath.EvalSymlinks(config.Root)
	if err != nil || root != config.Root {
		return nil, ErrSourceService
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || int(info.Sys().(*syscall.Stat_t).Uid) != uid {
		return nil, ErrSourceService
	}
	cleanupRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrSourceService
	}
	defer cleanupRoot.Close()
	if opened, err := cleanupRoot.Lstat("."); err != nil || !os.SameFile(opened, info) {
		return nil, ErrSourceService
	}
	ctx, cancel := context.WithTimeout(ctx, config.Session.Limits.Timeout)
	defer cancel()
	if verifier == nil || verifier.snapshot != config.Session.Snapshot {
		return nil, ErrSourceService
	}
	for _, path := range config.Paths {
		if !verifier.paths[path] {
			return nil, ErrSourceService
		}
	}
	limited, err := NewReader(verifier.store, config.Session.Snapshot, config.Paths, config.Session.Limits)
	if err != nil {
		return nil, ErrSourceService
	}
	broker, err := newSourceBroker(ctx, config.Session, limited)
	if err != nil {
		return nil, ErrSourceService
	}
	endpoint := SourceEndpoint{Socket: filepath.Join(root, Digest(config.Session), "source.sock"), ReaderUID: uid, SessionHash: Digest(config.Session), RequiredProfile: "isolated-source-profile-1"}
	directory := filepath.Dir(endpoint.Socket)
	if os.Mkdir(directory, 0700) != nil {
		return nil, ErrSourceService // never replace an existing/unknown session
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, ErrSourceService
	}
	var socketInfo os.FileInfo
	var listener *net.UnixListener
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
		if cleanupSourceEndpoint(cleanupRoot, filepath.Base(directory), dirInfo, socketInfo, nil) != nil && serveErr == nil {
			serveErr = ErrSourceService
		}
	}()
	listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		return broker.seal(), ErrSourceService
	}
	listener.SetUnlinkOnClose(false)
	socketInfo, err = os.Lstat(endpoint.Socket)
	if err != nil || int(socketInfo.Sys().(*syscall.Stat_t).Gid) != gid || os.Chmod(endpoint.Socket, 0660) != nil || os.Chmod(directory, 0710) != nil {
		return broker.seal(), ErrSourceService
	}
	var mu sync.Mutex
	var active *net.UnixConn
	stop := context.AfterFunc(ctx, func() {
		_ = listener.Close()
		mu.Lock()
		if active != nil {
			_ = active.Close()
		}
		mu.Unlock()
	})
	defer stop()
	if drain != nil {
		stopDrain := context.AfterFunc(drain, func() { _ = listener.Close() })
		defer stopDrain()
	}
	if ctx.Err() != nil {
		return broker.seal(), ctx.Err()
	}
	if ready(endpoint) != nil {
		return broker.seal(), ErrSourceService
	}
	for rejected := 0; rejected < 8; rejected++ {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return broker.seal(), ctx.Err()
			}
			return broker.seal(), ErrSourceService
		}
		peer, err := peerUID(connection)
		if err != nil || uint64(peer) != uint64(config.WorkerUID) {
			_ = connection.Close()
			broker.mu.Lock()
			broker.denials = append(broker.denials, Coverage{Reason: "unauthorized source peer omitted"})
			broker.mu.Unlock()
			continue
		}
		mu.Lock()
		active = connection
		if ctx.Err() != nil {
			_ = connection.Close()
		}
		mu.Unlock()
		inv := Invocation{Provider: config.Session.Identity.Provider, Source: broker, SourceEndpoint: &endpoint, Limits: config.Session.Limits, OutputLimit: config.Session.Limits.OutputBytes}
		err = ServeSourceMCP(ctx, inv, connection)
		_ = connection.Close()
		access := broker.seal()
		// A request flood is recorded in the receipt, which stays valid: the
		// phase then fails typed, never clean, with its audit and auth kept.
		if err != nil && !errors.Is(err, ErrSourceMCPFlood) {
			return access, ErrSourceService
		}
		return access, nil
	}
	return broker.seal(), ErrSourceService
}
