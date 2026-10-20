//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var ErrContainmentGate = errors.New("containment launch gate requires recovery")

// GateIdentity comes from the trusted manager observation, never a worker hello.
// The gate additionally compares PID/start time/boot ID against the kernel peer.
// InvocationID must still be qualified against the owning systemd unit by the
// manager adapter. This gate alone does not attest cgroups, shutdown or mounts.
type GateIdentity struct {
	InvocationID string `json:"invocation_id"`
	BootID       string `json:"boot_id"`
	PID          int    `json:"pid"`
	StartTicks   uint64 `json:"start_ticks"`
}

// FakeGateManifest is deliberately inert. The first stub can only hash a bounded
// marker. It has no command, mount, profile, credential or network destination.
type FakeGateManifest struct {
	Version int    `json:"version"`
	Marker  string `json:"marker"`
}

type gateRecord struct {
	Version      int          `json:"version"`
	Binding      AuthBinding  `json:"binding"`
	Phase        string       `json:"phase"`
	ManifestHash string       `json:"manifest_hash"`
	Identity     GateIdentity `json:"identity"`
	Protocol     int          `json:"protocol"`
}

type gateHello struct {
	Version      int         `json:"version"`
	Binding      AuthBinding `json:"binding"`
	InvocationID string      `json:"invocation_id"`
	ManifestHash string      `json:"manifest_hash"`
}

type gateGrant struct {
	Version  int              `json:"version"`
	Binding  AuthBinding      `json:"binding"`
	Identity GateIdentity     `json:"identity"`
	Manifest FakeGateManifest `json:"manifest"`
}

// ContainmentGate persists authority before launch, authenticates an actual Unix
// peer, and permanently revokes an old launch on recovery. It starts no process.
// A manager must separately drain uncertain starts and prove whole-unit stop.
type ContainmentGate struct {
	directory string
	binding   AuthBinding
	listener  *net.UnixListener
	mu        sync.Mutex
	peers     map[*net.UnixConn]bool
	closed    bool
}

func validFakeGateManifest(m FakeGateManifest) bool {
	if m.Version != 1 || len(m.Marker) == 0 || len(m.Marker) > 256 {
		return false
	}
	for _, c := range m.Marker {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

// PrepareContainmentGate creates a never-reused launch directory and fsyncs its
// prepared record before opening the endpoint. Call this before manager start.
func PrepareContainmentGate(root string, binding AuthBinding, manifest FakeGateManifest) (*ContainmentGate, error) {
	if !validFakeGateManifest(manifest) {
		return nil, ErrContainmentGate
	}
	return prepareContainmentGateHash(root, binding, Digest(manifest), 0)
}

func prepareContainmentGateHash(root string, binding AuthBinding, hash string, protocol int) (*ContainmentGate, error) {
	if !binding.valid() || !validAuthHash(hash) || (protocol != 0 && protocol != 2 && protocol != nativeVMGrantProtocol) || !filepath.IsAbs(root) || filepath.Clean(root) != root || privateDirectory(root) != nil {
		return nil, ErrContainmentGate
	}
	directory := filepath.Join(root, binding.LaunchID)
	if len(filepath.Join(directory, "gate.sock")) > 100 {
		return nil, ErrContainmentGate
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, ErrContainmentGate
	}
	// Never remove this directory on failure: even a partially prepared nonce is
	// spent. Retained tombstones prevent a delayed stub binding to a new launch.
	parent, err := os.Open(root)
	if err != nil {
		return nil, ErrContainmentGate
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return nil, ErrContainmentGate
	}
	g := &ContainmentGate{directory: directory, binding: binding, peers: map[*net.UnixConn]bool{}}
	held, err := g.lock(context.Background())
	if err != nil {
		return nil, ErrContainmentGate
	}
	defer held.close()
	record := gateRecord{Version: 1, Binding: binding, Phase: "prepared", ManifestHash: hash, Protocol: protocol}
	if err = g.write(held, record); err != nil {
		return nil, ErrContainmentGate
	}
	g.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: g.Endpoint(), Net: "unix"})
	if err != nil {
		return nil, ErrContainmentGate
	}
	g.listener.SetUnlinkOnClose(false)
	if err = os.Chmod(g.Endpoint(), 0600); err != nil {
		g.listener.Close()
		return nil, ErrContainmentGate
	}
	return g, nil
}

func (g *ContainmentGate) Endpoint() string { return filepath.Join(g.directory, "gate.sock") }
func (g *ContainmentGate) lock(ctx context.Context) (*authLock, error) {
	return (&AuthStore{directory: g.directory}).lock(ctx)
}
func (g *ContainmentGate) write(held *authLock, r gateRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAuth(held.root, "containment.json", raw)
}
func (g *ContainmentGate) read(held *authLock) (gateRecord, error) {
	raw, err := readAuth(held.root, "containment.json", false)
	if err != nil {
		return gateRecord{}, ErrContainmentGate
	}
	raw, err = legacyContainmentField(raw, "protocol", json.RawMessage("0"))
	if err != nil {
		return gateRecord{}, ErrContainmentGate
	}
	r, err := DecodeStrict[gateRecord](raw, 65536)
	if err != nil || r.Version != 1 || r.Binding != g.binding || !validAuthHash(r.ManifestHash) || (r.Protocol != 0 && r.Protocol != 2 && r.Protocol != 3 && r.Protocol != nativeVMGrantProtocol) {
		return r, ErrContainmentGate
	}
	switch r.Phase {
	case "prepared":
		if r.Identity != (GateIdentity{}) {
			return r, ErrContainmentGate
		}
	case "granted":
		if r.Identity == (GateIdentity{}) {
			return r, ErrContainmentGate
		}
	case "aborted":
	default:
		return r, ErrContainmentGate
	}
	return r, nil
}

// Only the named new controller-journal fields have legacy zero defaults.
// Duplicate keys/nulls/unknown fields remain errors. The old fake protocol stays
// version 1; a missing VM discriminator cannot become a VM grant on recovery.
func legacyContainmentField(raw []byte, field string, zero json.RawMessage) ([]byte, error) {
	if len(raw) > 65536 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return nil, ErrContainmentGate
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrContainmentGate
	}
	if _, ok := fields[field]; ok {
		return raw, nil
	}
	fields[field] = zero
	return json.Marshal(fields)
}

// OpenAbortedContainmentGate is recovery, never resume. It permanently revokes
// the journal before returning and never recreates an endpoint or sends work.
// This is NOT a stopped-worker receipt and must not release an auth lease.
func OpenAbortedContainmentGate(ctx context.Context, root string, binding AuthBinding) (*ContainmentGate, error) {
	if !binding.valid() || !filepath.IsAbs(root) || filepath.Clean(root) != root || privateDirectory(root) != nil {
		return nil, ErrContainmentGate
	}
	g := &ContainmentGate{directory: filepath.Join(root, binding.LaunchID), binding: binding, peers: map[*net.UnixConn]bool{}}
	if err := g.Abort(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *ContainmentGate) Abort(ctx context.Context) error {
	// Hold the cross-process flock through revocation. A grant holds that same
	// lock through its bounded write, so it cannot send after Abort commits.
	held, err := g.lock(ctx)
	if err != nil {
		return ErrContainmentGate
	}
	defer held.close()
	r, err := g.read(held)
	if err != nil {
		g.closePeers()
		return err
	}
	r.Phase = "aborted"
	if err = g.write(held, r); err != nil {
		g.closePeers()
		return ErrContainmentGate
	}
	g.closePeers()
	// Leave the socket name/tombstone in place; never unlink a foreign replacement
	// by path and never recreate a recovered launch's listener.
	return nil
}

func (g *ContainmentGate) closePeers() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.listener != nil {
		g.listener.Close()
	}
	for conn := range g.peers {
		conn.Close()
	}
}

func gatePeer(conn *net.UnixConn) (int, uint32, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, -1, err
	}
	var pid int
	var uid uint32
	peerFD := -1
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			peerErr = e
			return
		}
		peerFD, e = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		if e != nil {
			peerErr = e
			return
		}
		if e = unix.PidfdSendSignal(peerFD, 0, nil, 0); e != nil {
			peerErr = e
			return
		}
		pid, uid = int(cred.Pid), cred.Uid
	})
	if err != nil || peerErr != nil {
		if peerFD >= 0 {
			unix.Close(peerFD)
		}
		return 0, 0, -1, errors.Join(err, peerErr)
	}
	return pid, uid, peerFD, nil
}

func processGateIdentity(pid int, invocation string) (GateIdentity, error) {
	decoded, decodeErr := hex.DecodeString(invocation)
	if pid <= 0 || decodeErr != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != invocation {
		return GateIdentity{}, ErrContainmentGate
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return GateIdentity{}, err
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return GateIdentity{}, err
	}
	// comm is parenthesized and may contain spaces or ')' characters.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return GateIdentity{}, ErrContainmentGate
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return GateIdentity{}, ErrContainmentGate
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || ticks == 0 {
		return GateIdentity{}, ErrContainmentGate
	}
	return GateIdentity{InvocationID: invocation, BootID: strings.TrimSpace(string(boot)), PID: pid, StartTicks: ticks}, nil
}

func gateFrame[T any](reader io.Reader) (T, error) {
	var zero T
	raw, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil || len(raw) == 0 || len(raw) > 4096 {
		return zero, ErrContainmentGate
	}
	value, err := DecodeStrict[T](raw, 4096)
	if err != nil {
		return zero, ErrContainmentGate
	}
	return value, nil
}

func gateDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// GrantFake accepts only the exact observed manager peer and captured manifest.
// It persists granted before the write. One persisted grant consumes the launch
// even if delivery fails; recovery revokes, and never replays a grant.
func (g *ContainmentGate) GrantFake(ctx context.Context, identity GateIdentity, manifest FakeGateManifest) error {
	if !validFakeGateManifest(manifest) || ctx.Err() != nil {
		return ErrContainmentGate
	}
	raw, err := json.Marshal(gateGrant{Version: 1, Binding: g.binding, Identity: identity, Manifest: manifest})
	if err != nil {
		return ErrContainmentGate
	}
	return g.grant(ctx, identity, Digest(manifest), 0, raw, nil)
}

func (g *ContainmentGate) grant(ctx context.Context, identity GateIdentity, manifestHash string, protocol int, raw []byte, files []*os.File) error {
	if ctx.Err() != nil || !validAuthHash(manifestHash) || len(raw) == 0 || len(raw) > 4096 || (protocol != 0 && protocol != 2 && protocol != nativeVMGrantProtocol) || (protocol == 0 && len(files) != 0) || (protocol == 2 && len(files) != 4) || (protocol == nativeVMGrantProtocol && len(files) != nativeVMDescriptorCount) {
		return ErrContainmentGate
	}
	g.mu.Lock()
	listener := g.listener
	g.mu.Unlock()
	if listener == nil {
		return ErrContainmentGate
	}
	_ = listener.SetDeadline(gateDeadline(ctx))
	stopAccept := context.AfterFunc(ctx, func() { listener.Close() })
	defer stopAccept()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return ErrContainmentGate
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		conn.Close()
		return ErrContainmentGate
	}
	g.peers[conn] = true
	g.mu.Unlock()
	defer func() { conn.Close(); g.mu.Lock(); delete(g.peers, conn); g.mu.Unlock() }()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(gateDeadline(ctx))
	hello, err := gateFrame[gateHello](conn)
	if err != nil {
		return err
	}
	pid, uid, peerFD, err := gatePeer(conn)
	if peerFD >= 0 {
		defer unix.Close(peerFD)
	}
	if err != nil || uid != uint32(os.Geteuid()) || pid != identity.PID {
		return ErrContainmentGate
	}
	actual, err := processGateIdentity(pid, identity.InvocationID)
	helloVersion := 1
	if protocol == 2 {
		helloVersion = 2
	} else if protocol == nativeVMGrantProtocol {
		helloVersion = nativeVMGrantProtocol
	}
	if err != nil || actual != identity || hello.Version != helloVersion || hello.Binding != g.binding || hello.InvocationID != identity.InvocationID || hello.ManifestHash != manifestHash {
		return ErrContainmentGate
	}
	held, err := g.lock(ctx)
	if err != nil {
		return ErrContainmentGate
	}
	defer held.close()
	r, err := g.read(held)
	if err != nil || r.Phase != "prepared" || r.Protocol != protocol || r.ManifestHash != manifestHash || ctx.Err() != nil || unix.PidfdSendSignal(peerFD, 0, nil, 0) != nil {
		return ErrContainmentGate
	}
	r.Phase = "granted"
	r.Identity = identity
	if err = g.write(held, r); err != nil {
		return ErrContainmentGate
	}
	// The bound is reset after waiting for the journal lock. Never block Abort
	// indefinitely on an unread payload. No goroutine owns a deferred grant write.
	_ = conn.SetWriteDeadline(gateDeadline(ctx))
	var n int
	if len(files) == 0 {
		n, err = conn.Write(raw)
	} else {
		fds := make([]int, len(files))
		for i, file := range files {
			fds[i] = int(file.Fd())
		}
		var oob int
		rights := unix.UnixRights(fds...)
		n, oob, err = conn.WriteMsgUnix(raw, rights, nil)
		if oob != len(rights) {
			return ErrContainmentGate
		}
	}
	if err != nil || n != len(raw) {
		return ErrContainmentGate
	}
	return nil
}

// ContainmentStubFake is the first fixed inert stub. It only reports the digest
// of a synthetic marker after an authenticated grant. It never starts a payload,
// opens a profile or provides a shutdown attestation. InvocationID comes from
// the trusted launch environment, not a model request.
func ContainmentStubFake(ctx context.Context, endpoint string, binding AuthBinding, invocation, manifestHash string, supervisorPID int, output io.Writer) error {
	if !binding.valid() || !validAuthHash(manifestHash) || supervisorPID <= 0 || output == nil || ctx.Err() != nil {
		return ErrContainmentGate
	}
	own, err := processGateIdentity(os.Getpid(), invocation)
	if err != nil {
		return err
	}
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return ErrContainmentGate
	}
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		connected.Close()
		return ErrContainmentGate
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(gateDeadline(ctx))
	pid, uid, peerFD, err := gatePeer(conn)
	if peerFD >= 0 {
		defer unix.Close(peerFD)
	}
	if err != nil || pid != supervisorPID || uid != uint32(os.Geteuid()) {
		return ErrContainmentGate
	}
	raw, _ := json.Marshal(gateHello{1, binding, invocation, manifestHash})
	if _, err = conn.Write(raw); err != nil {
		return ErrContainmentGate
	}
	if err = conn.CloseWrite(); err != nil {
		return ErrContainmentGate
	}
	grant, err := gateFrame[gateGrant](conn)
	if err != nil || grant.Version != 1 || grant.Binding != binding || grant.Identity != own || !validFakeGateManifest(grant.Manifest) || Digest(grant.Manifest) != manifestHash || unix.PidfdSendSignal(peerFD, 0, nil, 0) != nil {
		return ErrContainmentGate
	}
	_, err = io.WriteString(output, Digest(grant.Manifest.Marker)+"\n")
	return err
}
