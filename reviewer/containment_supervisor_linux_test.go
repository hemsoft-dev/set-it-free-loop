//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type fakeContainmentKernel struct {
	mu                        sync.Mutex
	boot                      string
	group                     cgroupBinding
	populated, changed        bool
	pendingImage, imageChecks int
	terminate                 func() error
}

func (k *fakeContainmentKernel) Executable(int, string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.imageChecks++
	if k.pendingImage > 0 {
		k.pendingImage--
		return errors.Join(ErrAuthRecoveryRequired, errStubImagePending)
	}
	return nil
}
func (k *fakeContainmentKernel) BootID() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.boot, nil
}
func (k *fakeContainmentKernel) Capture(group, unit string) (cgroupBinding, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !validUnitCgroup(group, unit) || k.changed {
		return cgroupBinding{}, ErrAuthRecoveryRequired
	}
	return k.group, nil
}
func (k *fakeContainmentKernel) Empty(group cgroupBinding) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.populated || k.changed || group != k.group {
		return ErrAuthRecoveryRequired
	}
	return nil
}

func (k *fakeContainmentKernel) Terminate(ctx context.Context, group cgroupBinding) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrAuthRecoveryRequired
	}
	k.mu.Lock()
	if k.changed || group != k.group {
		k.mu.Unlock()
		return ErrAuthRecoveryRequired
	}
	fn := k.terminate
	k.mu.Unlock()
	if fn == nil {
		return ErrAuthRecoveryRequired
	}
	return fn()
}

type fakeContainmentManager struct {
	mu                               sync.Mutex
	owner                            string
	observation                      unitObservation
	kernel                           *fakeContainmentKernel
	startErr, observeErr, waitErr    error
	starts, stops                    int
	holdStart                        chan struct{}
	entered                          chan struct{}
	child                            *exec.Cmd
	childDone                        chan struct{}
	output                           bytes.Buffer
	holdPopulated, changedInvocation bool
	terminalState                    string
	spawn                            bool
	afterStart                       func()
	afterWait                        func()
}

func (m *fakeContainmentManager) Owner() string { return m.owner }
func (m *fakeContainmentManager) Close() error  { return nil }
func (m *fakeContainmentManager) Start(ctx context.Context, unit, stub string, args []string, description string, imageFD int) (string, error) {
	m.mu.Lock()
	m.starts++
	entered, hold := m.entered, m.holdStart
	m.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return "", m.startErr
	}
	m.observation.Unit = unit
	m.observation.Description = description
	if m.spawn {
		raw, _ := json.Marshal(authTestBinding())
		cmd := exec.Command(os.Args[0], "-test.run=^TestSystemdSupervisorStubChild$")
		cmd.Env = []string{"SFL_UNIT_TEST_CHILD=1", "SFL_UNIT_BINDING=" + string(raw), "SFL_UNIT_ENDPOINT=" + args[1], "SFL_UNIT_MANIFEST=" + args[3]}
		cmd.Stdout = &m.output
		cmd.Stderr = &m.output
		if err := cmd.Start(); err != nil {
			return "", err
		}
		m.child = cmd
		m.observation.MainPID = uint32(cmd.Process.Pid)
		m.childDone = make(chan struct{})
		go func() { cmd.Wait(); close(m.childDone) }()
	}
	if m.afterStart != nil {
		m.afterStart()
	}
	return "/org/freedesktop/systemd1/job/1", nil
}
func (m *fakeContainmentManager) WaitJob(context.Context, string, string) error {
	if m.afterWait != nil {
		m.afterWait()
	}
	return m.waitErr
}
func (m *fakeContainmentManager) Observe(context.Context, string) (unitObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observation, m.observeErr
}
func (m *fakeContainmentManager) UnitForPIDFD(context.Context, int) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observation.Unit, m.observation.InvocationID, nil
}
func (m *fakeContainmentManager) Stop(context.Context, string) (string, error) {
	return "/org/freedesktop/systemd1/job/2", m.terminateOriginal()
}
func (m *fakeContainmentManager) terminateOriginal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	if m.child != nil {
		m.child.Process.Kill()
		<-m.childDone
	}
	m.observation.MainPID = 0
	m.observation.ActiveState = "inactive"
	if m.terminalState != "" {
		m.observation.ActiveState = m.terminalState
	}
	if m.changedInvocation {
		m.observation.InvocationID = strings.Repeat("e", 32)
	}
	m.kernel.mu.Lock()
	m.kernel.populated = m.holdPopulated
	m.kernel.mu.Unlock()
	return nil
}
func TestSystemdSupervisorStubChild(t *testing.T) {
	if os.Getenv("SFL_UNIT_TEST_CHILD") != "1" {
		return
	}
	var binding AuthBinding
	if json.Unmarshal([]byte(os.Getenv("SFL_UNIT_BINDING")), &binding) != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ContainmentStubFake(ctx, os.Getenv("SFL_UNIT_ENDPOINT"), binding, strings.Repeat("d", 32), os.Getenv("SFL_UNIT_MANIFEST"), os.Getppid(), os.Stdout) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func systemdSupervisorFixture(t *testing.T) (*SystemdSupervisor, *fakeContainmentManager, FakeGateManifest) {
	t.Helper()
	root, err := os.MkdirTemp("", "sfl-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	stub := filepath.Join(root, "pinned-stub")
	raw := []byte("synthetic fixed fake executable")
	if err = os.WriteFile(stub, raw, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	binding := authTestBinding()
	unit := "sfl-alpha-" + binding.LaunchID + ".service"
	boot, _ := currentBootID()
	kernel := &fakeContainmentKernel{boot: boot, group: cgroupBinding{Path: "/user.slice/user-1000.slice/" + unit, Inode: 17}, populated: true}
	manager := &fakeContainmentManager{owner: "fake-bus/:1.2", kernel: kernel, observation: unitObservation{Unit: unit, Path: dbus.ObjectPath("/org/freedesktop/systemd1/unit/fake"), InvocationID: strings.Repeat("d", 32), MainPID: uint32(os.Getpid()), ControlGroup: kernel.group.Path, ActiveState: "active", Job: unitJob{0, "/"}, MemoryMax: 1 << 30, MemorySwapMax: 0, TasksMax: 64, CPUQuotaPerSecUSec: 1000000, KillMode: "control-group", Restart: "no", CollectMode: "inactive", RemainAfterExit: true, NoNewPrivileges: true, Transient: true, Description: "SFL fake containment " + Digest(binding)}}
	kernel.terminate = manager.terminateOriginal
	supervisor := newSystemdSupervisor(root, stub, hex.EncodeToString(hash[:]), manager, kernel)
	t.Cleanup(func() {
		supervisor.Close()
		manager.mu.Lock()
		if manager.child != nil {
			manager.child.Process.Kill()
			<-manager.childDone
		}
		manager.mu.Unlock()
	})
	manifest := FakeGateManifest{1, "synthetic systemd-supervisor marker"}
	if err = supervisor.PrepareFake(binding, manifest); err != nil {
		t.Fatal(err)
	}
	return supervisor, manager, manifest
}
func TestSystemdSupervisorActualChildGrantStopRecoveryAndAuth(t *testing.T) {
	s, m, manifest := systemdSupervisorFixture(t)
	m.spawn = true
	binding := authTestBinding()
	auth := authTestStore(t, Codex)
	profile, err := auth.LeaseProfile(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	if err = s.StartFake(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if err = s.GrantFake(context.Background(), binding, manifest); err != nil {
		t.Fatal(err)
	}
	<-m.childDone
	if strings.TrimSpace(m.output.String()) != Digest(manifest.Marker) {
		t.Fatal("actual child did not receive bounded grant", m.output.String())
	}
	if err = s.AbortAndWait(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	// Reopen through a fresh supervisor object, as after controller restart.
	recovered := newSystemdSupervisor(s.root, s.stub, s.stubHash, m, s.kernel)
	if err = profile.Complete(context.Background(), recovered); err != nil {
		t.Fatal("synthetic auth lease could not complete after bound stop", err)
	}
	if m.starts != 1 || m.stops != 1 {
		t.Fatal("recovery started work or repeated settled stop", m.starts, m.stops)
	}
	if err = recovered.StartFake(context.Background(), binding); err == nil {
		t.Fatal("recovery relaunched spent unit")
	}
}
func TestSystemdSupervisorUnknownStartAndMissingUnitDoNotProveStop(t *testing.T) {
	s, m, _ := systemdSupervisorFixture(t)
	m.startErr = errors.New("accepted reply lost")
	m.observeErr = errors.New("unit absent")
	if err := s.StartFake(context.Background(), authTestBinding()); err == nil {
		t.Fatal("unknown start accepted")
	}
	if err := s.AbortAndWait(context.Background(), authTestBinding()); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("missing unit became stop proof", err)
	}
	r, err := s.snapshotUnit(context.Background(), authTestBinding())
	if err != nil || !r.Started || r.StartSettled || r.Stopped || !r.Aborted {
		t.Fatal("uncertainty lost", r, err)
	}
	if m.stops != 0 || m.starts != 1 {
		t.Fatal("unknown launch retried or unrelated unit stopped")
	}
}
func TestSystemdSupervisorAbortDrainsStartAndNeverGrantsLater(t *testing.T) {
	s, m, manifest := systemdSupervisorFixture(t)
	m.holdStart = make(chan struct{})
	m.entered = make(chan struct{})
	started := make(chan error, 1)
	go func() { started <- s.StartFake(context.Background(), authTestBinding()) }()
	<-m.entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.AbortAndWait(ctx, authTestBinding()); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("undrained start became stop proof", err)
	}
	close(m.holdStart)
	if err := <-started; err == nil {
		t.Fatal("aborted launch returned ready")
	}
	if err := s.GrantFake(context.Background(), authTestBinding(), manifest); err == nil {
		t.Fatal("late start received grant")
	}
	if err := s.AbortAndWait(context.Background(), authTestBinding()); err != nil {
		t.Fatal("settled inert start could not recover", err)
	}
	if m.starts != 1 || m.stops != 1 {
		t.Fatal(m.starts, m.stops)
	}
}
func TestSystemdSupervisorRejectsChangedOrPopulatedContainment(t *testing.T) {
	for _, kind := range []string{"populated", "invocation", "manager", "inode", "resource"} {
		t.Run(kind, func(t *testing.T) {
			s, m, manifest := systemdSupervisorFixture(t)
			binding := authTestBinding()
			if err := s.StartFake(context.Background(), binding); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "populated":
				m.holdPopulated = true
			case "invocation":
				m.changedInvocation = true
			case "manager":
				m.owner = "replacement-bus/:1.2"
			case "inode":
				m.kernel.changed = true
			case "resource":
				m.observation.TasksMax = 65
			}
			if kind == "resource" {
				if err := s.GrantFake(context.Background(), binding, manifest); err == nil {
					t.Fatal("changed launch granted", kind)
				}
				return
			}
			if err := s.ConfirmAuthStopped(context.Background(), binding); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("unproven whole-unit stop", kind, err)
			}
		})
	}
}
func TestSystemdSupervisorPreparedWithoutStartAndChangedBoot(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(strconv.FormatBool(started), func(t *testing.T) {
			s, m, _ := systemdSupervisorFixture(t)
			if started {
				m.startErr = errors.New("unknown start")
				s.StartFake(context.Background(), authTestBinding())
				m.kernel.boot = "synthetic different boot"
			}
			if err := s.ConfirmAuthStopped(context.Background(), authTestBinding()); err != nil {
				t.Fatal(err)
			}
			if m.stops != 0 {
				t.Fatal("new boot or unstarted launch stopped unrelated unit")
			}
		})
	}
}

func TestSystemdPostStartJournalWritesKeepRecoveryDeadline(t *testing.T) {
	for _, stage := range []string{"job", "settled"} {
		t.Run(stage, func(t *testing.T) {
			s, m, _ := systemdSupervisorFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			released := make(chan struct{})
			hook := func() {
				held, err := s.gate(authTestBinding()).lock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				go func() {
					select {
					case <-release:
					case <-time.After(3 * time.Second):
					}
					held.close()
					close(released)
				}()
			}
			if stage == "job" {
				m.afterStart = hook
			} else {
				m.afterWait = hook
			}
			started := time.Now()
			err := s.StartFake(ctx, authTestBinding())
			elapsed := time.Since(started)
			close(release)
			<-released
			if !errors.Is(err, ErrAuthRecoveryRequired) || elapsed > 2*time.Second {
				t.Fatal("post-start journal write lost deadline", stage, elapsed, err)
			}
			record, err := s.snapshotUnit(context.Background(), authTestBinding())
			if err != nil || !record.Started || record.StartSettled || record.Captured {
				t.Fatal("uncertain launched attempt lost", record, err)
			}
		})
	}
}

func TestSystemdWaitsForPinnedImageAfterTrustedBootstrap(t *testing.T) {
	s, m, _ := systemdSupervisorFixture(t)
	m.kernel.pendingImage = 2
	if err := s.StartFake(context.Background(), authTestBinding()); err != nil {
		t.Fatal("trusted bootstrap transition lost pending classification", err)
	}
	if m.kernel.imageChecks != 3 {
		t.Fatal("sealed image transition not checked", m.kernel.imageChecks)
	}
}

type pausedPrepareKernel struct {
	containmentKernel
	entered, release chan struct{}
}

func (k *pausedPrepareKernel) BootID() (string, error) {
	close(k.entered)
	<-k.release
	return k.containmentKernel.BootID()
}
func TestSystemdCloseDrainsPreparationAndAbortsLateGate(t *testing.T) {
	s, _, manifest := systemdSupervisorFixture(t)
	paused := &pausedPrepareKernel{containmentKernel: s.kernel, entered: make(chan struct{}), release: make(chan struct{})}
	s.kernel = paused
	binding := authTestBinding()
	binding.LaunchID = strings.Repeat("e", 32)
	prepared := make(chan error, 1)
	go func() { prepared <- s.PrepareFake(binding, manifest) }()
	<-paused.entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		closing := s.closed
		s.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(paused.release)
			t.Fatal("close did not fence supervisor")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		close(paused.release)
		<-prepared
		t.Fatal("Close returned before preparation drained", err)
	default:
	}
	close(paused.release)
	if err := <-prepared; !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("late preparation succeeded", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	gate := s.gate(binding)
	conn, err := net.DialTimeout("unix", gate.Endpoint(), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("late gate listener survived Close")
	}
	held, err := gate.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.close()
	record, err := gate.read(held)
	if err != nil || record.Phase != "aborted" {
		t.Fatal("late gate not durably revoked", record, err)
	}
	if s.PrepareFake(binding, manifest) == nil {
		t.Fatal("closed supervisor accepted preparation")
	}
}
func TestContainmentMissingChildRequiresOriginalCgroupHierarchy(t *testing.T) {
	group := "/user.slice/sfl-test-missing-hierarchy.service"
	for _, anchor := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		root, _, err := openContainmentCgroupAt(anchor, group, cgroupHierarchy{})
		if root != nil {
			root.Close()
		}
		if !errors.Is(err, ErrAuthRecoveryRequired) || os.IsNotExist(err) {
			t.Fatal("unavailable hierarchy accepted as removed child", anchor, err)
		}
	}
	root, hierarchy, err := openContainmentCgroupAt("/sys/fs/cgroup", group, cgroupHierarchy{})
	if root != nil {
		root.Close()
		t.Fatal("unexpected fixture cgroup")
	}
	if hierarchy == (cgroupHierarchy{}) {
		t.Skip("host does not expose cgroup v2")
	}
	if !os.IsNotExist(err) {
		t.Fatal("verified missing child not distinguished", err)
	}
	if err := (linuxContainmentKernel{}).Empty(cgroupBinding{Path: group, Inode: 1, Hierarchy: hierarchy}); err != nil {
		t.Fatal("removed child on verified hierarchy rejected", err)
	}
	for _, field := range []string{"device", "inode", "mount", "cgroup"} {
		changed := hierarchy
		switch field {
		case "device":
			changed.Device++
		case "inode":
			changed.Inode++
		case "mount":
			changed.MountNamespace++
		case "cgroup":
			changed.CgroupNamespace++
		}
		if err := (linuxContainmentKernel{}).Empty(cgroupBinding{Path: group, Inode: 1, Hierarchy: changed}); !errors.Is(err, ErrAuthRecoveryRequired) {
			t.Fatal("changed hierarchy accepted missing child", field, err)
		}
	}
	if err := (linuxContainmentKernel{}).Empty(cgroupBinding{Path: group, Inode: 1}); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("unbound legacy hierarchy accepted", err)
	}
}
