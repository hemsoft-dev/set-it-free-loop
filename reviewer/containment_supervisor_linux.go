//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
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
	"syscall"

	"golang.org/x/sys/unix"
)

type cgroupBinding struct {
	Path  string
	Inode uint64
}
type containmentKernel interface {
	BootID() (string, error)
	Capture(string, string) (cgroupBinding, error)
	Empty(cgroupBinding) error
}

type linuxContainmentKernel struct{}

func (linuxContainmentKernel) BootID() (string, error) { return currentBootID() }
func validUnitCgroup(group, unit string) bool {
	return validContainmentUnit(unit) && strings.HasPrefix(group, "/user.slice/") && filepath.Clean(group) == group && strings.HasSuffix(group, "/"+unit)
}
func openContainmentCgroup(group string) (*os.Root, error) {
	if !strings.HasPrefix(group, "/user.slice/") || filepath.Clean(group) != group {
		return nil, ErrAuthRecoveryRequired
	}
	root, err := os.OpenRoot("/sys/fs/cgroup")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenRoot(strings.TrimPrefix(group, "/"))
}
func cgroupFile(root *os.Root, name string) (string, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return "", ErrAuthRecoveryRequired
	}
	return strings.TrimSpace(string(raw)), nil
}
func cgroupInode(root *os.Root) (uint64, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	var fs unix.Statfs_t
	if unix.Fstatfs(int(dir.Fd()), &fs) != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return 0, ErrAuthRecoveryRequired
	}
	info, err := dir.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, ErrAuthRecoveryRequired
	}
	return st.Ino, nil
}
func (linuxContainmentKernel) Capture(group, unit string) (cgroupBinding, error) {
	if !validUnitCgroup(group, unit) {
		return cgroupBinding{}, ErrAuthRecoveryRequired
	}
	root, err := openContainmentCgroup(group)
	if err != nil {
		return cgroupBinding{}, err
	}
	defer root.Close()
	inode, err := cgroupInode(root)
	if err != nil {
		return cgroupBinding{}, err
	}
	for name, want := range map[string]string{"memory.max": "1073741824", "memory.swap.max": "0", "pids.max": "64"} {
		v, err := cgroupFile(root, name)
		if err != nil || v != want {
			return cgroupBinding{}, ErrAuthRecoveryRequired
		}
	}
	cpu, err := cgroupFile(root, "cpu.max")
	fields := strings.Fields(cpu)
	if err != nil || len(fields) != 2 {
		return cgroupBinding{}, ErrAuthRecoveryRequired
	}
	quota, e1 := strconv.ParseUint(fields[0], 10, 64)
	period, e2 := strconv.ParseUint(fields[1], 10, 64)
	if e1 != nil || e2 != nil || quota == 0 || quota != period {
		return cgroupBinding{}, ErrAuthRecoveryRequired
	}
	return cgroupBinding{group, inode}, nil
}
func (linuxContainmentKernel) Empty(binding cgroupBinding) error {
	if binding.Inode == 0 || !strings.HasPrefix(binding.Path, "/user.slice/") || filepath.Clean(binding.Path) != binding.Path {
		return ErrAuthRecoveryRequired
	}
	root, err := openContainmentCgroup(binding.Path)
	// A cgroup cannot be removed while it contains processes or subgroups. The
	// supervisor must already have settled its only start and revoked the gate.
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer root.Close()
	inode, err := cgroupInode(root)
	if err != nil || inode != binding.Inode {
		return ErrAuthRecoveryRequired
	}
	events, err := cgroupFile(root, "cgroup.events")
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	seen := false
	for _, line := range strings.Split(events, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return ErrAuthRecoveryRequired
		}
		if fields[0] == "populated" {
			if seen || fields[1] != "0" {
				return ErrAuthRecoveryRequired
			}
			seen = true
		}
	}
	if !seen {
		return ErrAuthRecoveryRequired
	}
	return nil
}

type unitJournal struct {
	Version                                            int
	Binding                                            AuthBinding
	Unit, BootID, ManagerOwner, StubHash, ManifestHash string
	Started, StartSettled, Captured, Aborted, Stopped  bool
	StartJob, StopJob                                  string
	Identity                                           GateIdentity
	Cgroup                                             cgroupBinding
}

// SystemdSupervisor currently starts only the fixed fake-marker stub. It owns
// the durable start/stop binding and implements AuthStopVerifier for precisely
// that payload. It does not select a provider, a profile, QEMU or live inference.
// A new payload requires a separately pinned adapter and mount/stop proof.
type SystemdSupervisor struct {
	root, stub, stubHash string
	manager              containmentManager
	kernel               containmentKernel
	mu                   sync.Mutex
	gates                map[AuthBinding]*ContainmentGate
	starts               map[AuthBinding]chan struct{}
}

// OpenSystemdSupervisor is explicit construction, never an Engine fallback.
// root must already be private. stub is the pinned cmd/unitstub executable.
func OpenSystemdSupervisor(ctx context.Context, root, stub, stubHash string) (*SystemdSupervisor, error) {
	if privateDirectory(root) != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || verifyContainmentStub(stub, stubHash) != nil {
		return nil, ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	manager, err := openUserContainmentManager(ctx)
	if err != nil {
		return nil, err
	}
	return newSystemdSupervisor(root, stub, stubHash, manager, linuxContainmentKernel{}), nil
}
func newSystemdSupervisor(root, stub, hash string, m containmentManager, k containmentKernel) *SystemdSupervisor {
	return &SystemdSupervisor{root: root, stub: stub, stubHash: hash, manager: m, kernel: k, gates: map[AuthBinding]*ContainmentGate{}, starts: map[AuthBinding]chan struct{}{}}
}
func verifyContainmentStub(path, hash string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !validAuthHash(hash) {
		return ErrAuthRecoveryRequired
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 || info.Size() > 64<<20 {
		return ErrAuthRecoveryRequired
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(file, (64<<20)+1))
	if err != nil || n != info.Size() || hex.EncodeToString(h.Sum(nil)) != hash {
		return ErrAuthRecoveryRequired
	}
	return nil
}
func (s *SystemdSupervisor) gate(binding AuthBinding) *ContainmentGate {
	return &ContainmentGate{directory: filepath.Join(s.root, binding.LaunchID), binding: binding, peers: map[*net.UnixConn]bool{}}
}
func (s *SystemdSupervisor) readUnit(held *authLock, binding AuthBinding) (unitJournal, error) {
	raw, err := readAuth(held.root, "unit.json", false)
	if err != nil {
		return unitJournal{}, ErrAuthRecoveryRequired
	}
	r, err := DecodeStrict[unitJournal](raw, 65536)
	if err != nil || r.Version != 1 || r.Binding != binding || !binding.valid() || r.Unit != "sfl-alpha-"+binding.LaunchID+".service" || r.StubHash != s.stubHash || !validAuthHash(r.ManifestHash) || r.ManagerOwner == "" || r.BootID == "" {
		return r, ErrAuthRecoveryRequired
	}
	if r.StartSettled && !r.Started || r.Captured && (!r.Started || !r.StartSettled || r.Identity.PID <= 0 || r.Cgroup.Inode == 0 || !validUnitCgroup(r.Cgroup.Path, r.Unit)) || r.Stopped && !r.Aborted {
		return r, ErrAuthRecoveryRequired
	}
	return r, nil
}
func (s *SystemdSupervisor) updateUnit(ctx context.Context, binding AuthBinding, change func(*unitJournal) error) error {
	if !binding.valid() {
		return ErrAuthRecoveryRequired
	}
	gate := s.gate(binding)
	held, err := gate.lock(ctx)
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer held.close()
	r, err := s.readUnit(held, binding)
	if err != nil {
		return err
	}
	if err = change(&r); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAuth(held.root, "unit.json", raw)
}
func (s *SystemdSupervisor) snapshotUnit(ctx context.Context, binding AuthBinding) (unitJournal, error) {
	var r unitJournal
	err := s.updateUnit(ctx, binding, func(current *unitJournal) error { r = *current; return nil })
	return r, err
}
func (s *SystemdSupervisor) PrepareFake(binding AuthBinding, manifest FakeGateManifest) error {
	if verifyContainmentStub(s.stub, s.stubHash) != nil {
		return ErrAuthRecoveryRequired
	}
	boot, err := s.kernel.BootID()
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	gate, err := PrepareContainmentGate(s.root, binding, manifest)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			gate.Abort(context.Background())
		}
	}()
	held, err := gate.lock(context.Background())
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer held.close()
	r := unitJournal{Version: 1, Binding: binding, Unit: "sfl-alpha-" + binding.LaunchID + ".service", BootID: boot, ManagerOwner: s.manager.Owner(), StubHash: s.stubHash, ManifestHash: Digest(manifest)}
	raw, err := json.Marshal(r)
	if err != nil || writeAuth(held.root, "unit.json", raw) != nil {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	s.gates[binding] = gate
	s.mu.Unlock()
	success = true
	return nil
}
func expectedFakeObservation(r unitJournal, o unitObservation) bool {
	return o.Unit == r.Unit && o.Path.IsValid() && o.InvocationID != "" && o.Description == "SFL fake containment "+Digest(r.Binding) &&
		o.Transient && o.Job.ID == 0 && o.MemoryMax == 1<<30 && o.MemorySwapMax == 0 && o.TasksMax == 64 && o.CPUQuotaPerSecUSec == 1000000 &&
		o.KillMode == "control-group" && o.Restart == "no" && o.CollectMode == "inactive" && o.RemainAfterExit && o.NoNewPrivileges
}
func (s *SystemdSupervisor) captureUnit(ctx context.Context, r unitJournal) (unitJournal, error) {
	if r.ManagerOwner != s.manager.Owner() {
		return r, ErrAuthRecoveryRequired
	}
	boot, err := s.kernel.BootID()
	if err != nil || boot != r.BootID {
		return r, ErrAuthRecoveryRequired
	}
	o, err := s.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedFakeObservation(r, o) {
		return r, ErrAuthRecoveryRequired
	}
	identity, err := unitProcessIdentity(ctx, s.manager, o)
	if err != nil || identity.BootID != boot {
		return r, ErrAuthRecoveryRequired
	}
	group, err := s.kernel.Capture(o.ControlGroup, r.Unit)
	if err != nil {
		return r, ErrAuthRecoveryRequired
	}
	if r.Captured && (identity != r.Identity || group != r.Cgroup) {
		return r, ErrAuthRecoveryRequired
	}
	r.StartSettled = true
	r.Captured = true
	r.Identity = identity
	r.Cgroup = group
	return r, nil
}
func (s *SystemdSupervisor) StartFake(ctx context.Context, binding AuthBinding) error {
	if !binding.valid() {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	if _, ok := s.starts[binding]; ok {
		s.mu.Unlock()
		return ErrAuthRecoveryRequired
	}
	done := make(chan struct{})
	s.starts[binding] = done
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.starts, binding); close(done); s.mu.Unlock() }()
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	var r unitJournal
	err := s.updateUnit(ctx, binding, func(current *unitJournal) error {
		if current.Started || current.Aborted || current.Stopped || current.ManagerOwner != s.manager.Owner() {
			return ErrAuthRecoveryRequired
		}
		heldGate := s.gate(binding)
		held, err := os.OpenRoot(heldGate.directory)
		if err != nil {
			return ErrAuthRecoveryRequired
		}
		// The outer unit flock also serializes the gate record. Do not reacquire it.
		gateRecord, err := heldGate.read(&authLock{root: held})
		held.Close()
		if err != nil || gateRecord.Phase != "prepared" {
			return ErrAuthRecoveryRequired
		}
		if verifyContainmentStub(s.stub, s.stubHash) != nil {
			return ErrAuthRecoveryRequired
		}
		current.Started = true
		r = *current
		return nil
	})
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(binding)
	args := []string{s.stub, s.gate(binding).Endpoint(), string(raw), r.ManifestHash, strconv.Itoa(os.Getpid())}
	job, err := s.manager.Start(ctx, r.Unit, s.stub, args, "SFL fake containment "+Digest(binding))
	if err != nil {
		return errors.Join(ErrAuthRecoveryRequired, err)
	}
	if err = s.updateUnit(context.WithoutCancel(ctx), binding, func(current *unitJournal) error { current.StartJob = job; return nil }); err != nil {
		return err
	}
	if err = s.manager.WaitJob(ctx, job, r.Unit); err != nil {
		return err
	}
	if err = s.updateUnit(context.WithoutCancel(ctx), binding, func(current *unitJournal) error { current.StartSettled = true; return nil }); err != nil {
		return err
	}
	r, err = s.snapshotUnit(ctx, binding)
	if err != nil || r.Aborted {
		return ErrAuthRecoveryRequired
	}
	captured, err := s.captureUnit(ctx, r)
	if err != nil {
		return err
	}
	return s.updateUnit(ctx, binding, func(current *unitJournal) error {
		if current.Aborted {
			return ErrAuthRecoveryRequired
		}
		current.StartSettled = true
		current.Captured = true
		current.Identity = captured.Identity
		current.Cgroup = captured.Cgroup
		return nil
	})
}
func (s *SystemdSupervisor) GrantFake(ctx context.Context, binding AuthBinding, manifest FakeGateManifest) error {
	r, err := s.snapshotUnit(ctx, binding)
	if err != nil || r.Aborted || !r.StartSettled || !r.Captured || r.Stopped || r.ManifestHash != Digest(manifest) {
		return ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	if verifyContainmentStub(s.stub, s.stubHash) != nil {
		return ErrAuthRecoveryRequired
	}
	if _, err = s.captureUnit(ctx, r); err != nil {
		return err
	}
	s.mu.Lock()
	gate := s.gates[binding]
	s.mu.Unlock()
	if gate == nil {
		return ErrAuthRecoveryRequired
	}
	return gate.GrantFake(ctx, r.Identity, manifest)
}

// AbortAndWait revokes the exact gate before draining an in-flight start. A
// missing unit is never enough: uncertain starts remain journaled until their
// exact live invocation can be captured, or a changed kernel boot proves death.
func (s *SystemdSupervisor) AbortAndWait(ctx context.Context, binding AuthBinding) error {
	if !binding.valid() {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	gate := s.gates[binding]
	s.mu.Unlock()
	if gate != nil {
		if err := gate.Abort(ctx); err != nil {
			return errors.Join(ErrAuthRecoveryRequired, err)
		}
	} else {
		if _, err := OpenAbortedContainmentGate(ctx, s.root, binding); err != nil {
			return errors.Join(ErrAuthRecoveryRequired, err)
		}
	}
	if err := s.updateUnit(ctx, binding, func(r *unitJournal) error { r.Aborted = true; return nil }); err != nil {
		return err
	}
	s.mu.Lock()
	done := s.starts[binding]
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(ErrAuthRecoveryRequired, ctx.Err())
		}
	}
	r, err := s.snapshotUnit(ctx, binding)
	if err != nil {
		return err
	}
	boot, err := s.kernel.BootID()
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	if !r.Started || boot != r.BootID {
		return s.updateUnit(ctx, binding, func(r *unitJournal) error { r.Stopped = true; return nil })
	}
	if r.ManagerOwner != s.manager.Owner() {
		return ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	if !r.StartSettled || !r.Captured {
		captured, err := s.captureUnit(ctx, r)
		if err != nil {
			return ErrAuthRecoveryRequired
		}
		if err = s.updateUnit(ctx, binding, func(r *unitJournal) error {
			r.StartSettled = true
			r.Captured = true
			r.Identity = captured.Identity
			r.Cgroup = captured.Cgroup
			return nil
		}); err != nil {
			return err
		}
		r = captured
	}
	if r.Stopped {
		return s.kernel.Empty(r.Cgroup)
	}
	before, err := s.manager.Observe(ctx, r.Unit)
	if err != nil || before.InvocationID != r.Identity.InvocationID || before.Unit != r.Unit || before.Description != "SFL fake containment "+Digest(binding) {
		return ErrAuthRecoveryRequired
	}
	job, err := s.manager.Stop(ctx, r.Unit)
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	if err = s.updateUnit(ctx, binding, func(r *unitJournal) error { r.StopJob = job; return nil }); err != nil {
		return err
	}
	if err = s.manager.WaitJob(ctx, job, r.Unit); err != nil {
		return ErrAuthRecoveryRequired
	}
	// Do not infer emptiness from MainPID=0, a successful StopUnit or a missing
	// service. Verify the captured cgroup inode/subtree independently in the kernel.
	if err = s.kernel.Empty(r.Cgroup); err != nil {
		return ErrAuthRecoveryRequired
	}
	after, err := s.manager.Observe(ctx, r.Unit)
	if err != nil || after.InvocationID != r.Identity.InvocationID || after.Job.ID != 0 || after.MainPID != 0 || (after.ActiveState != "inactive" && after.ActiveState != "failed") {
		return ErrAuthRecoveryRequired
	}
	return s.updateUnit(ctx, binding, func(r *unitJournal) error { r.Stopped = true; return nil })
}

// ConfirmAuthStopped reopens the durable binding and rechecks physical stop.
// Its only supported payload creates no profile mounts. An auth journal cannot
// supply an executable, a different payload or a replacement launch identity.
func (s *SystemdSupervisor) ConfirmAuthStopped(ctx context.Context, binding AuthBinding) error {
	return s.AbortAndWait(ctx, binding)
}
func (s *SystemdSupervisor) Close() error {
	s.mu.Lock()
	gates := make([]*ContainmentGate, 0, len(s.gates))
	for _, g := range s.gates {
		gates = append(gates, g)
	}
	s.mu.Unlock()
	var result error
	for _, g := range gates {
		result = errors.Join(result, g.Abort(context.Background()))
	}
	return errors.Join(result, s.manager.Close())
}
