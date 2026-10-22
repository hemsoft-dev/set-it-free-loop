//go:build linux

package reviewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// The fake manager shares the fixed source VM property validation but runs
// no systemd unit. It models the accepted job as pending until WaitJob.
func (m *fakeContainmentManager) StartGitVM(ctx context.Context, unit, stub string, args []string, description string, imageFD, supervisorFD int) (string, error) {
	if _, err := fixedGitVMUnitProperties(unit, stub, args, description, imageFD, supervisorFD); err != nil {
		return "", err
	}
	job, err := m.Start(ctx, unit, stub, args, description, imageFD)
	if err != nil {
		return job, err
	}
	m.mu.Lock()
	if m.observation.Job.ID == 0 {
		m.observation.Job = unitJob{1, "/org/freedesktop/systemd1/job/1"}
	}
	m.mu.Unlock()
	afterWait := m.afterWait
	m.afterWait = func() {
		m.mu.Lock()
		if m.observation.Job.ID == 1 {
			m.observation.Job = unitJob{}
		}
		m.mu.Unlock()
		if afterWait != nil {
			afterWait()
		}
	}
	return job, nil
}

type gitVMOwnerFixture struct {
	owner                *gitVMUnitOwner
	manager              *fakeContainmentManager
	kernel               *fakeContainmentKernel
	request              gitVMGuestRequest
	root, stub, stubHash string
	child                *exec.Cmd
}

func gitVMOwnerInertChild(t *testing.T) (*exec.Cmd, chan struct{}) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestGitAcquisitionUnitInertChild$")
	child.Env = []string{"SFL_GIT_UNIT_INERT=1"}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		child.Process.Kill()
		child.Wait()
		t.Fatal("inert child not ready", err)
	}
	done := make(chan struct{})
	go func() { child.Wait(); close(done) }()
	t.Cleanup(func() { child.Process.Kill(); input.Close(); <-done })
	return child, done
}

func newGitVMOwnerFixture(t *testing.T) gitVMOwnerFixture {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "source-vm-owner")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(parent, "synthetic-source-vm-stub")
	stubBytes := []byte("synthetic sealed source VM stub bytes; not executed")
	if err := os.WriteFile(stub, stubBytes, 0700); err != nil {
		t.Fatal(err)
	}
	child, done := gitVMOwnerInertChild(t)
	q := gitVMGuestTestRequest(t)
	unit := "sfl-alpha-" + q.Source.Binding.Attempt + ".service"
	boot, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	kernel := &fakeContainmentKernel{boot: boot, group: cgroupBinding{Path: "/user.slice/" + unit, Inode: 91, Hierarchy: cgroupHierarchy{Device: 1, Inode: 2, MountNamespace: 3, CgroupNamespace: 4}}}
	manager := &fakeContainmentManager{owner: "synthetic-bus/:1.88", kernel: kernel, child: child, childDone: done, observation: unitObservation{Unit: unit, Path: "/org/freedesktop/systemd1/unit/synthetic_2dsource", InvocationID: strings.Repeat("b", 32), MainPID: uint32(child.Process.Pid), ControlGroup: kernel.group.Path, ActiveState: "active", MemoryMax: gitVMUnitMemory, TasksMax: gitVMUnitTasks, CPUQuotaPerSecUSec: 1000000, KillMode: "control-group", Restart: "no", CollectMode: "inactive-or-failed", ExitType: "cgroup", NoNewPrivileges: true, Transient: true}}
	kernel.terminate = manager.terminateOriginal
	hash := gitRawHash(stubBytes)
	owner, err := newGitVMUnitOwner(context.Background(), root, stub, hash, manager, kernel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Process.Kill()
		<-done
		if owner.Close() != nil {
			for _, fd := range owner.rootPIDFDs {
				unix.Close(fd)
			}
			owner.journal.Close(context.Background())
			owner.image.Close()
			owner.supervisor.Close()
			owner.root.Close()
		}
	})
	return gitVMOwnerFixture{owner, manager, kernel, q, root, stub, hash, child}
}

func (f gitVMOwnerFixture) attempt() string { return f.request.Source.Binding.Attempt }

func TestGitVMOwnerFixedUnitWireIsSeparateAndBounded(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("a", 32) + ".service"
	args := []string{retainedStubExec, "/private/source-vm-gate.sock", "fixed request", Digest("request"), fmt.Sprint(os.Getpid())}
	supervisor := gitTestPIDFD(t, os.Getpid())
	image, _ := gitSealedTestImage(t)
	imageFD, supervisorFD := int(image.Fd()), int(supervisor.Fd())
	calls := 0
	manager := &dbusContainmentManager{jobs: &systemdJobs{completed: map[string]jobResult{}, changed: make(chan struct{})}, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, args ...any) ([]any, error) {
		calls++
		if method != systemdManagerInterface+".StartTransientUnit" {
			t.Fatal("wrong manager operation")
		}
		values := map[string]dbus.Variant{}
		for _, property := range args[2].([]unitProperty) {
			if _, duplicate := values[property.Name]; duplicate {
				t.Fatal("duplicate property")
			}
			values[property.Name] = property.Value
		}
		wantFDs := []namedUnitFD{{dbus.UnixFD(imageFD), "sfl-unitstub"}, {dbus.UnixFD(supervisorFD), "sfl-source-vm-supervisor"}}
		if !reflect.DeepEqual(values["ExtraFileDescriptors"].Value(), wantFDs) {
			t.Fatal("wrong original descriptor roles/order", values["ExtraFileDescriptors"])
		}
		if !reflect.DeepEqual(values["RestrictAddressFamilies"].Value(), gitUnitAddressFamilies{true, []string{"AF_UNIX", "AF_NETLINK"}}) || !reflect.DeepEqual(values["SystemCallFilter"].Value(), gitUnitSyscallFilter{false, []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}}) || !reflect.DeepEqual(values["SystemCallArchitectures"].Value(), []string{"native"}) {
			t.Fatal("weakened source VM unit policy")
		}
		if _, ok := values["TemporaryFileSystem"]; ok {
			t.Fatal("host Git work mount leaked into source VM unit")
		}
		for key, want := range map[string]any{"MemoryMax": gitVMUnitMemory, "MemorySwapMax": uint64(0), "TasksMax": gitVMUnitTasks, "CPUQuotaPerSecUSec": uint64(1000000), "RuntimeMaxUSec": gitVMUnitRuntime, "Restart": "no", "KillMode": "control-group", "NoNewPrivileges": true, "SystemCallErrorNumber": int32(unix.EPERM)} {
			if values[key].Value() != want {
				t.Fatal(key)
			}
		}
		return []any{dbus.ObjectPath("/org/freedesktop/systemd1/job/1")}, nil
	})}
	if _, err := manager.StartGitVM(context.Background(), unit, retainedStubExec, args, gitVMUnitDescriptionPrefix+Digest("request"), imageFD, supervisorFD); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := manager.Start(context.Background(), "sfl-alpha-"+strings.Repeat("b", 32)+".service", retainedStubExec, args, gitVMUnitDescriptionPrefix+Digest("request"), imageFD); err == nil || calls != 1 {
		t.Fatal("generic single-FD launch accepted source VM")
	}
	if _, err := fixedGitUnitProperties(unit, retainedStubExec, args, gitVMUnitDescriptionPrefix+Digest("request"), imageFD, supervisorFD); err == nil {
		t.Fatal("source VM accepted as host Git")
	}
	if _, err := fixedGitVMUnitProperties(unit, retainedStubExec, args, gitUnitDescriptionPrefix+Digest("request"), imageFD, supervisorFD); err == nil {
		t.Fatal("host Git accepted as source VM")
	}
	if _, err := fixedGitVMUnitProperties(unit, retainedStubExec, args, "SFL native VM containment "+Digest("request"), imageFD, supervisorFD); err == nil {
		t.Fatal("native provider VM accepted as source VM")
	}
	other := exec.Command("/usr/bin/sleep", "5")
	if other.Start() != nil {
		t.Fatal("inert other supervisor")
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	otherFD := gitTestPIDFD(t, other.Process.Pid)
	for index, badFD := range []int{-1, imageFD, int(otherFD.Fd())} {
		if _, err := manager.StartGitVM(context.Background(), fmt.Sprintf("sfl-alpha-%032x.service", index+1), retainedStubExec, args, gitVMUnitDescriptionPrefix+Digest("request"), imageFD, badFD); err == nil || calls != 1 {
			t.Fatal("wrong supervisor descriptor reached manager")
		}
	}
	wrong := append([]string(nil), args...)
	wrong[4] = fmt.Sprint(other.Process.Pid)
	if _, err := manager.StartGitVM(context.Background(), "sfl-alpha-"+strings.Repeat("c", 32)+".service", retainedStubExec, wrong, gitVMUnitDescriptionPrefix+Digest("request"), imageFD, supervisorFD); err == nil || calls != 1 {
		t.Fatal("substituted numeric supervisor reached manager")
	}
}

func TestGitVMOwnerDurableStartCaptureAndOriginalStop(t *testing.T) {
	f := newGitVMOwnerFixture(t)
	f.kernel.pendingImage = 2
	retired := 0
	f.manager.retireHook = func(string) error { retired++; return nil }
	if err := f.owner.prepare(context.Background(), f.request); err != nil {
		t.Fatal("prepare", err)
	}
	if f.owner.prepare(context.Background(), f.request) == nil {
		t.Fatal("duplicate attempt prepared")
	}
	if err := f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock"); err != nil {
		t.Fatal("start", err)
	}
	r, err := f.owner.read(f.attempt())
	if err != nil || !r.StartIntent || r.StartJob != "/org/freedesktop/systemd1/job/1" || !r.born() || !r.StartSettled || !r.Captured || r.Revoked || r.Stopped || r.Identity.PID != f.child.Process.Pid || r.Cgroup != f.kernel.group || r.Birth.InvocationID != r.Identity.InvocationID {
		t.Fatal("captured lifecycle not durable", r, err)
	}
	if !f.owner.grantEligible[f.attempt()] || f.manager.starts != 1 {
		t.Fatal("fresh start not grant eligible")
	}
	if f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") == nil || f.manager.starts != 1 {
		t.Fatal("started attempt restarted")
	}
	other := f.request
	other.Source.Binding.Attempt = strings.Repeat("e", 32)
	other.Read.Source.Attempt = other.Source.Binding.Attempt
	if f.owner.prepare(context.Background(), other) == nil {
		t.Fatal("second attempt prepared while first was live")
	}
	if f.owner.Close() == nil {
		t.Fatal("owner closed with a live unit")
	}
	if err := f.owner.stop(context.Background(), f.attempt()); err != nil {
		t.Fatal("stop", err)
	}
	r, err = f.owner.read(f.attempt())
	if err != nil || !r.Revoked || !r.Stopped || r.StopBoot != "" || f.manager.stops != 1 || retired != 1 || f.owner.grantEligible[f.attempt()] {
		t.Fatal("original stop not recorded", r, err, f.manager.stops, retired)
	}
	select {
	case <-f.manager.childDone:
	default:
		t.Fatal("original root survived stop")
	}
	if err := f.owner.prepare(context.Background(), other); err != nil {
		t.Fatal("stopped attempt still blocked new work", err)
	}
}

func TestGitVMOwnerCrashAfterStartIntentIsRecoveredNotRetried(t *testing.T) {
	f := newGitVMOwnerFixture(t)
	if f.owner.prepare(context.Background(), f.request) != nil {
		t.Fatal("prepare")
	}
	// The manager accepts the job but the reply is lost before it is recorded.
	f.manager.startErr = errors.New("lost reply")
	if f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") == nil {
		t.Fatal("lost reply accepted")
	}
	f.manager.startErr = nil
	r, err := f.owner.read(f.attempt())
	if err != nil || !r.StartIntent || r.StartJob != "" || r.born() {
		t.Fatal("start intent lost", r, err)
	}
	if f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") == nil || f.manager.starts != 1 {
		t.Fatal("uncertain start was retried")
	}
	// Without a birth there is no original instance to prove stopped on this boot.
	f.manager.observation.Description = gitVMUnitDescriptionPrefix + r.RequestHash
	if f.owner.recover(context.Background()) == nil {
		t.Fatal("unborn same-boot instance declared stopped")
	}
	if r, _ = f.owner.read(f.attempt()); r.Stopped {
		t.Fatal("uncertain attempt marked stopped")
	}
	// Only a changed boot proves that unborn instance gone.
	f.kernel.boot = "synthetic-next-boot"
	if err := f.owner.recover(context.Background()); err != nil {
		t.Fatal("reboot recovery", err)
	}
	r, err = f.owner.read(f.attempt())
	if err != nil || !r.Stopped || r.StopBoot != "synthetic-next-boot" || f.manager.stops != 0 {
		t.Fatal("reboot proof not recorded", r, err)
	}
}

func TestGitVMOwnerReopenRecoversOriginalWithoutRearmingGrant(t *testing.T) {
	f := newGitVMOwnerFixture(t)
	if f.owner.prepare(context.Background(), f.request) != nil || f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") != nil {
		t.Fatal("start")
	}
	// Simulate a crashed controller: drop process-local state, keep the journal.
	for _, fd := range f.owner.rootPIDFDs {
		unix.Close(fd)
	}
	f.owner.rootPIDFDs = map[string]int{}
	f.owner.journal.Close(context.Background())
	f.owner.image.Close()
	f.owner.supervisor.Close()
	f.owner.root.Close()
	f.owner.closed = true
	reopened, err := newGitVMUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.kernel)
	if err != nil {
		t.Fatal("reopen", err)
	}
	f.owner = reopened
	defer reopened.Close()
	if reopened.grantEligible[f.attempt()] {
		t.Fatal("recovery re-armed grant authority")
	}
	if reopened.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") == nil || f.manager.starts != 1 {
		t.Fatal("recovered attempt restarted")
	}
	if err := reopened.recover(context.Background()); err != nil {
		t.Fatal("recover", err)
	}
	r, err := reopened.read(f.attempt())
	if err != nil || !r.Stopped || f.manager.stops != 1 || reopened.grantEligible[f.attempt()] {
		t.Fatal("recovered stop", r, err)
	}
	if reopened.Close() != nil {
		t.Fatal("close after terminal receipts")
	}
}

func TestGitVMOwnerRefusesReplacementInstance(t *testing.T) {
	for _, kind := range []string{"invocation", "cgroup", "description"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitVMOwnerFixture(t)
			if f.owner.prepare(context.Background(), f.request) != nil || f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") != nil {
				t.Fatal("start")
			}
			// A replacement occupies the unit name between capture and stop.
			f.manager.mu.Lock()
			switch kind {
			case "invocation":
				f.manager.observation.InvocationID = strings.Repeat("e", 32)
			case "cgroup":
				f.kernel.changed = true
			case "description":
				f.manager.observation.Description = gitVMUnitDescriptionPrefix + Digest("other request")
			}
			f.manager.mu.Unlock()
			if f.owner.stop(context.Background(), f.attempt()) == nil || f.manager.stops != 0 {
				t.Fatal("replacement instance was stopped or accepted")
			}
			r, err := f.owner.read(f.attempt())
			if err != nil || !r.Revoked || r.Stopped {
				t.Fatal("replacement produced a stop receipt", r, err)
			}
			if f.owner.grantEligible[f.attempt()] {
				t.Fatal("revoked attempt still grant eligible")
			}
		})
	}
}

func TestGitVMOwnerEarlyExitUsesBirthProof(t *testing.T) {
	f := newGitVMOwnerFixture(t)
	f.manager.waitErr = errors.New("job failed")
	if f.owner.prepare(context.Background(), f.request) != nil || f.owner.start(context.Background(), f.attempt(), "/private/source-vm-gate.sock") == nil {
		t.Fatal("failed job accepted")
	}
	r, err := f.owner.read(f.attempt())
	if err != nil || !r.born() || r.StartSettled || r.Captured {
		t.Fatal("birth not retained", r, err)
	}
	// The original stub exits on its own before capture.
	f.manager.mu.Lock()
	f.child.Process.Kill()
	<-f.manager.childDone
	f.manager.observation.MainPID = 0
	f.manager.observation.ActiveState = "failed"
	f.manager.mu.Unlock()
	if err := f.owner.stop(context.Background(), f.attempt()); err != nil {
		t.Fatal("early stop", err)
	}
	r, err = f.owner.read(f.attempt())
	if err != nil || !r.Stopped || r.Captured || r.StopBoot != "" || f.manager.stops != 0 {
		t.Fatal("birth proof receipt", r, err)
	}
	// A populated original group is never proof.
	g := newGitVMOwnerFixture(t)
	g.manager.waitErr = errors.New("job failed")
	g.owner.prepare(context.Background(), g.request)
	g.owner.start(context.Background(), g.attempt(), "/private/source-vm-gate.sock")
	g.manager.mu.Lock()
	g.child.Process.Kill()
	<-g.manager.childDone
	g.manager.observation.MainPID = 0
	g.manager.mu.Unlock()
	g.kernel.populated = true
	if g.owner.stop(context.Background(), g.attempt()) == nil {
		t.Fatal("populated original group accepted as stopped")
	}
}

func TestGitVMOwnerJournalBoundAndForget(t *testing.T) {
	f := newGitVMOwnerFixture(t)
	if f.owner.prepare(context.Background(), f.request) != nil {
		t.Fatal("prepare")
	}
	if f.owner.forget(context.Background(), f.attempt()) == nil {
		t.Fatal("unstopped attempt forgotten")
	}
	if f.owner.stop(context.Background(), f.attempt()) != nil {
		t.Fatal("unstarted stop")
	}
	if r, _ := f.owner.read(f.attempt()); !r.Stopped || r.StartIntent || f.manager.stops != 0 {
		t.Fatal("unstarted attempt receipt", r)
	}
	for i := 1; i < gitVMOwnerRecords; i++ {
		q := f.request
		q.Source.Binding.Attempt = fmt.Sprintf("%032x", i)
		q.Read.Source.Attempt = q.Source.Binding.Attempt
		if f.owner.prepare(context.Background(), q) != nil || f.owner.stop(context.Background(), q.Source.Binding.Attempt) != nil {
			t.Fatal("bounded fill", i)
		}
	}
	extra := f.request
	extra.Source.Binding.Attempt = strings.Repeat("f", 32)
	extra.Read.Source.Attempt = extra.Source.Binding.Attempt
	if f.owner.prepare(context.Background(), extra) == nil {
		t.Fatal("journal bound exceeded")
	}
	if f.owner.forget(context.Background(), f.attempt()) != nil || f.owner.prepare(context.Background(), extra) != nil {
		t.Fatal("forget did not free a record")
	}
	if _, err := f.owner.read(f.attempt()); err == nil {
		t.Fatal("forgotten record still present")
	}
}

func TestGitVMOwnerRootRefusesForeignState(t *testing.T) {
	for _, name := range []string{"git-owner.lock", "native", "journal.txt"} {
		t.Run(name, func(t *testing.T) {
			f := newGitVMOwnerFixture(t)
			if f.owner.Close() != nil {
				t.Fatal("close")
			}
			path := filepath.Join(f.root, name)
			var err error
			if name == "native" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.WriteFile(path, []byte("foreign"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if owner, err := newGitVMUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.kernel); err == nil {
				owner.Close()
				t.Fatal("foreign owner state accepted")
			}
		})
	}
}
