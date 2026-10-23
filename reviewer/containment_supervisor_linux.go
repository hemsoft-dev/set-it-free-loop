//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type cgroupHierarchy struct {
	Device, Inode, MountNamespace, CgroupNamespace uint64
}
type cgroupBinding struct {
	Path      string
	Inode     uint64
	Hierarchy cgroupHierarchy
}
type containmentKernel interface {
	BootID() (string, error)
	Capture(string, string) (cgroupBinding, error)
	Empty(cgroupBinding) error
	Terminate(context.Context, cgroupBinding) error
	Executable(int, string) error
}

type linuxContainmentKernel struct{}

func (linuxContainmentKernel) Executable(pid int, hash string) error {
	return verifyRunningStub(pid, hash)
}
func (linuxContainmentKernel) BootID() (string, error) { return currentBootID() }
func validUnitCgroup(group, unit string) bool {
	return validContainmentUnit(unit) && strings.HasPrefix(group, "/user.slice/") && filepath.Clean(group) == group && strings.HasSuffix(group, "/"+unit)
}

// Only a missing child beneath the verified, captured hierarchy can prove
// removal. An absent mount, ordinary directory or changed namespace cannot.
func openContainmentCgroupAt(anchor, group string, expected cgroupHierarchy) (*os.Root, cgroupHierarchy, error) {
	if !strings.HasPrefix(group, "/user.slice/") || filepath.Clean(group) != group {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	defer root.Close()
	inode, err := cgroupInode(root)
	if err != nil {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	info, err := root.Stat(".")
	if err != nil {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	hierarchy := cgroupHierarchy{Device: uint64(st.Dev), Inode: inode}
	for name, target := range map[string]*uint64{"mnt": &hierarchy.MountNamespace, "cgroup": &hierarchy.CgroupNamespace} {
		info, err := os.Stat("/proc/self/ns/" + name)
		if err != nil {
			return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
		}
		*target = st.Ino
	}
	if expected != (cgroupHierarchy{}) && hierarchy != expected {
		return nil, cgroupHierarchy{}, ErrAuthRecoveryRequired
	}
	child, err := root.OpenRoot(strings.TrimPrefix(group, "/"))
	return child, hierarchy, err
}
func cgroupFile(root *os.Root, name string) (string, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readCgroupFile(file)
}

// readCgroupFile keeps the read error: a group removed after its file was
// opened refuses the read, which only a fresh lookup may prove as removal.
func readCgroupFile(file *os.File) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(raw) > 4096 {
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
	if err := unix.Fstatfs(int(dir.Fd()), &fs); err != nil {
		return 0, err
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
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
	return captureContainmentGroup(group, unit, "1073741824", "64")
}

type nativeContainmentKernel struct{ linuxContainmentKernel }

func (nativeContainmentKernel) Capture(group, unit string) (cgroupBinding, error) {
	return captureContainmentGroup(group, unit, "3221225472", "128")
}

func captureContainmentGroup(group, unit, memory, tasks string) (cgroupBinding, error) {
	if !validUnitCgroup(group, unit) {
		return cgroupBinding{}, ErrAuthRecoveryRequired
	}
	root, hierarchy, err := openContainmentCgroupAt("/sys/fs/cgroup", group, cgroupHierarchy{})
	if err != nil {
		return cgroupBinding{}, err
	}
	defer root.Close()
	inode, err := cgroupInode(root)
	if err != nil {
		return cgroupBinding{}, err
	}
	if err := verifyContainmentLimits(root, memory, tasks); err != nil {
		return cgroupBinding{}, err
	}
	return cgroupBinding{Path: group, Inode: inode, Hierarchy: hierarchy}, nil
}

func verifyContainmentLimits(root *os.Root, memory, tasks string) error {
	for name, want := range map[string]string{"memory.max": memory, "memory.swap.max": "0", "pids.max": tasks} {
		v, err := cgroupFile(root, name)
		if err != nil || v != want {
			return ErrAuthRecoveryRequired
		}
	}
	cpu, err := cgroupFile(root, "cpu.max")
	fields := strings.Fields(cpu)
	if err != nil || len(fields) != 2 {
		return ErrAuthRecoveryRequired
	}
	quota, e1 := strconv.ParseUint(fields[0], 10, 64)
	period, e2 := strconv.ParseUint(fields[1], 10, 64)
	if e1 != nil || e2 != nil || quota == 0 || quota != period {
		return ErrAuthRecoveryRequired
	}
	return nil
}
func (linuxContainmentKernel) Empty(binding cgroupBinding) error {
	if binding.Inode == 0 || binding.Hierarchy == (cgroupHierarchy{}) || !strings.HasPrefix(binding.Path, "/user.slice/") || filepath.Clean(binding.Path) != binding.Path {
		return ErrAuthRecoveryRequired
	}
	root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", binding.Path, binding.Hierarchy)
	// A cgroup cannot be removed while it contains processes or subgroups. The
	// supervisor must already have settled its only start and revoked the gate.
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer root.Close()
	return openedCgroupEmpty(root, binding)
}

// systemd removes an empty unit group about as soon as it is unpopulated. That
// can land between opening the original and reading it: the opened original
// then refuses lookups (ENOENT) and IO (ENODEV). Only a fresh missing path
// beneath the verified hierarchy proves that removal; a replacement never does.
func cgroupRemovedAfterOpen(err error, binding cgroupBinding) error {
	if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENODEV) {
		return ErrAuthRecoveryRequired
	}
	root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", binding.Path, binding.Hierarchy)
	if os.IsNotExist(err) {
		return nil
	}
	if err == nil {
		root.Close()
	}
	return ErrAuthRecoveryRequired
}

func openedCgroupEmpty(root *os.Root, binding cgroupBinding) error {
	inode, err := cgroupInode(root)
	if err != nil {
		return cgroupRemovedAfterOpen(err, binding)
	}
	if inode != binding.Inode {
		return ErrAuthRecoveryRequired
	}
	events, err := cgroupFile(root, "cgroup.events")
	if err != nil {
		return cgroupRemovedAfterOpen(err, binding)
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
	PayloadKind                                        string
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
	image                *retainedStubImage
	closed               bool
	payloadKind          string
}

// OpenSystemdSupervisor is explicit construction, never an Engine fallback.
// root must already be private. stub is the pinned cmd/unitstub executable.
func OpenSystemdSupervisor(ctx context.Context, root, stub, stubHash string) (*SystemdSupervisor, error) {
	if privateDirectory(root) != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	manager, err := openUserContainmentManager(ctx)
	if err != nil {
		return nil, err
	}
	supervisor := newSystemdSupervisor(root, stub, stubHash, manager, linuxContainmentKernel{})
	if supervisor.image == nil {
		manager.Close()
		return nil, ErrAuthRecoveryRequired
	}
	return supervisor, nil
}
func newSystemdSupervisor(root, stub, hash string, m containmentManager, k containmentKernel) *SystemdSupervisor {
	image, _ := retainStubImage(root, stub, hash)
	return &SystemdSupervisor{root: root, stub: stub, stubHash: hash, manager: m, kernel: k, gates: map[AuthBinding]*ContainmentGate{}, starts: map[AuthBinding]chan struct{}{}, image: image}
}
func (s *SystemdSupervisor) gate(binding AuthBinding) *ContainmentGate {
	return &ContainmentGate{directory: filepath.Join(s.root, binding.LaunchID), binding: binding, peers: map[*net.UnixConn]bool{}}
}
func (s *SystemdSupervisor) readUnit(held *authLock, binding AuthBinding) (unitJournal, error) {
	raw, err := readAuth(held.root, "unit.json", false)
	if err != nil {
		return unitJournal{}, ErrAuthRecoveryRequired
	}
	raw, err = legacyContainmentField(raw, "PayloadKind", json.RawMessage(`""`))
	if err != nil {
		return unitJournal{}, ErrAuthRecoveryRequired
	}
	r, err := DecodeStrict[unitJournal](raw, 65536)
	if err != nil || r.Version != 1 || r.PayloadKind != s.payloadKind || r.Binding != binding || !binding.valid() || r.Unit != "sfl-alpha-"+binding.LaunchID+".service" || r.StubHash != s.stubHash || !validAuthHash(r.ManifestHash) || r.ManagerOwner == "" || r.BootID == "" {
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
	if s.payloadKind != "" || !validFakeGateManifest(manifest) {
		return ErrAuthRecoveryRequired
	}
	return s.preparePayload(binding, Digest(manifest), 0)
}

func (s *SystemdSupervisor) preparePayload(binding AuthBinding, hash string, protocol int) error {
	if !binding.valid() || !validAuthHash(hash) || (s.payloadKind == "" && protocol != 0) || (s.payloadKind == "vm-fixture" && protocol != 2) || (s.payloadKind == "native-vm" && protocol != nativeVMGrantProtocol) || (s.payloadKind != "" && s.payloadKind != "vm-fixture" && s.payloadKind != "native-vm") {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	if s.closed || s.image == nil || s.starts[binding] != nil || s.gates[binding] != nil {
		s.mu.Unlock()
		return ErrAuthRecoveryRequired
	}
	done := make(chan struct{})
	s.starts[binding] = done
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.starts, binding); close(done); s.mu.Unlock() }()
	if s.image.verify() != nil {
		return ErrAuthRecoveryRequired
	}
	boot, err := s.kernel.BootID()
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	gate, err := prepareContainmentGateHash(s.root, binding, hash, protocol)
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
	r := unitJournal{Version: 1, Binding: binding, Unit: "sfl-alpha-" + binding.LaunchID + ".service", BootID: boot, ManagerOwner: s.manager.Owner(), StubHash: s.stubHash, ManifestHash: hash, PayloadKind: s.payloadKind}
	raw, err := json.Marshal(r)
	if err != nil || writeAuth(held.root, "unit.json", raw) != nil {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrAuthRecoveryRequired
	}
	s.gates[binding] = gate
	s.mu.Unlock()
	success = true
	return nil
}
func expectedFakeObservation(r unitJournal, o unitObservation) bool {
	return r.PayloadKind == "" && expectedPayloadObservation(r, o)
}
func payloadDescription(kind string, binding AuthBinding) string {
	if kind == "native-vm" {
		return "SFL native VM containment " + Digest(binding)
	}
	if kind == "vm-fixture" {
		return "SFL sealed VM containment " + Digest(binding)
	}
	return "SFL fake containment " + Digest(binding)
}
func expectedPayloadObservation(r unitJournal, o unitObservation) bool {
	memory, tasks := uint64(1<<30), uint64(64)
	if r.PayloadKind == "native-vm" {
		memory, tasks = 3<<30, 128
	}
	return (r.PayloadKind == "" || r.PayloadKind == "vm-fixture" || r.PayloadKind == "native-vm") && o.Unit == r.Unit && o.Path.IsValid() && o.InvocationID != "" && o.Description == payloadDescription(r.PayloadKind, r.Binding) &&
		o.Transient && o.Job.ID == 0 && o.MemoryMax == memory && o.MemorySwapMax == 0 && o.TasksMax == tasks && o.CPUQuotaPerSecUSec == 1000000 &&
		o.KillMode == "control-group" && o.Restart == "no" && expectedUnitLifecycle(o, r.Aborted) && o.NoNewPrivileges
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
	if err != nil {
		return r, err
	}
	if !expectedPayloadObservation(r, o) {
		return r, errors.Join(ErrAuthRecoveryRequired, errors.New("systemd restriction mismatch"))
	}
	identity, err := unitProcessIdentity(ctx, s.manager, o)
	if err != nil {
		return r, fmt.Errorf("%w: pidfd unit mapping", ErrAuthRecoveryRequired)
	}
	if err = s.kernel.Executable(int(o.MainPID), r.StubHash); err != nil {
		return r, fmt.Errorf("%w: executable image", err)
	}
	if identity.BootID != boot {
		return r, fmt.Errorf("%w: boot identity", ErrAuthRecoveryRequired)
	}
	group, err := s.kernel.Capture(o.ControlGroup, r.Unit)
	if err != nil {
		return r, fmt.Errorf("%w: kernel cgroup capture", ErrAuthRecoveryRequired)
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
	if s.payloadKind != "" {
		return ErrAuthRecoveryRequired
	}
	return s.startPayload(ctx, binding)
}
func (s *SystemdSupervisor) startPayload(ctx context.Context, binding AuthBinding) error {
	if !binding.valid() {
		return ErrAuthRecoveryRequired
	}
	s.mu.Lock()
	if _, ok := s.starts[binding]; ok || s.closed || s.image == nil {
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
		if s.image.verify() != nil {
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
	args := []string{retainedStubExec, s.gate(binding).Endpoint(), string(raw), r.ManifestHash, strconv.Itoa(os.Getpid())}
	job, err := s.manager.Start(ctx, r.Unit, retainedStubExec, args, payloadDescription(s.payloadKind, binding), int(s.image.file.Fd()))
	if err != nil {
		return errors.Join(ErrAuthRecoveryRequired, err)
	}
	if err = s.postStartUpdate(ctx, binding, func(current *unitJournal) error { current.StartJob = job; return nil }); err != nil {
		return err
	}
	if err = s.manager.WaitJob(ctx, job, r.Unit); err != nil {
		return err
	}
	if err = s.postStartUpdate(ctx, binding, func(current *unitJournal) error { current.StartSettled = true; return nil }); err != nil {
		return err
	}
	r, err = s.snapshotUnit(ctx, binding)
	if err != nil || r.Aborted {
		return ErrAuthRecoveryRequired
	}
	captured, err := s.captureStartedUnit(ctx, r)
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
func (s *SystemdSupervisor) postStartUpdate(ctx context.Context, binding AuthBinding, change func(*unitJournal) error) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	return s.updateUnit(recovery, binding, change)
}

func (s *SystemdSupervisor) GrantFake(ctx context.Context, binding AuthBinding, manifest FakeGateManifest) error {
	if s.payloadKind != "" {
		return ErrAuthRecoveryRequired
	}
	r, err := s.snapshotUnit(ctx, binding)
	if err != nil || r.Aborted || !r.StartSettled || !r.Captured || r.Stopped || r.ManifestHash != Digest(manifest) {
		return ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	if s.image.verify() != nil {
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
		if s.kernel.Empty(r.Cgroup) != nil {
			return ErrAuthRecoveryRequired
		}
		return s.manager.RetireUnit(ctx, r.Unit)
	}
	before, err := s.manager.Observe(ctx, r.Unit)
	if err != nil || before.InvocationID != r.Identity.InvocationID || before.Unit != r.Unit || before.Description != payloadDescription(r.PayloadKind, binding) {
		return ErrAuthRecoveryRequired
	}
	// StopUnit accepts a reusable name, without an invocation precondition.
	// Only the captured original cgroup descriptor may receive termination.
	if err = s.kernel.Terminate(ctx, r.Cgroup); err != nil {
		return ErrAuthRecoveryRequired
	}
	// Termination is a request, never the physical shutdown receipt.
	if err = s.kernel.Empty(r.Cgroup); err != nil {
		return ErrAuthRecoveryRequired
	}
	after, err := waitForOriginalUnitStop(ctx, s.manager, s.kernel, before, r.Cgroup)
	if err != nil || after.InvocationID != r.Identity.InvocationID || after.Unit != r.Unit || after.Description != payloadDescription(r.PayloadKind, binding) || after.ControlGroup != r.Cgroup.Path && after.ControlGroup != "" || after.Job.ID != 0 || after.MainPID != 0 || (after.ActiveState != "inactive" && after.ActiveState != "failed" && after.ActiveState != "active") {
		return ErrAuthRecoveryRequired
	}
	if err := s.updateUnit(ctx, binding, func(r *unitJournal) error { r.Stopped = true; return nil }); err != nil {
		return err
	}
	return s.manager.RetireUnit(ctx, r.Unit)
}

// ConfirmAuthStopped reopens the durable binding and rechecks physical stop.
// Its only supported payload creates no profile mounts. An auth journal cannot
// supply an executable, a different payload or a replacement launch identity.
func (s *SystemdSupervisor) ConfirmAuthStopped(ctx context.Context, binding AuthBinding) error {
	if s.payloadKind != "" {
		return ErrAuthRecoveryRequired
	}
	return s.AbortAndWait(ctx, binding)
}
func (s *SystemdSupervisor) Close() error {
	s.mu.Lock()
	s.closed = true
	gates := make([]*ContainmentGate, 0, len(s.gates))
	for _, g := range s.gates {
		gates = append(gates, g)
	}
	waits := make([]chan struct{}, 0, len(s.starts))
	for _, done := range s.starts {
		waits = append(waits, done)
	}
	s.mu.Unlock()
	ctx, cancel := boundedManagerContext(context.Background())
	defer cancel()
	var result error
	for _, g := range gates {
		result = errors.Join(result, g.Abort(ctx))
	}
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(result, s.manager.Close(), ErrAuthRecoveryRequired)
		}
	}
	// Close the transport before releasing the image descriptor. No new start
	// can register after closed, and every in-flight sender has drained.
	return errors.Join(result, s.manager.Close(), s.image.Close())
}
