//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func TestGitAcquisitionFixedUnitWireIsSeparateAndBounded(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("a", 32) + ".service"
	args := []string{retainedStubExec, "/private/git-gate.sock", "fixed binding", Digest("binding"), "123"}
	calls := 0
	manager := &dbusContainmentManager{jobs: &systemdJobs{completed: map[string]jobResult{}, changed: make(chan struct{})}, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, args ...any) ([]any, error) {
		calls++
		if method != systemdManagerInterface+".StartTransientUnit" {
			t.Fatal("wrong manager operation")
		}
		properties := args[2].([]unitProperty)
		values := map[string]dbus.Variant{}
		for _, property := range properties {
			if _, duplicate := values[property.Name]; duplicate {
				t.Fatal("duplicate property")
			}
			values[property.Name] = property.Value
		}
		if values["TemporaryFileSystem"].Signature().String() != "a(ss)" || values["RestrictAddressFamilies"].Signature().String() != "(bas)" || values["SystemCallArchitectures"].Signature().String() != "as" || values["SystemCallFilter"].Signature().String() != "(bas)" || values["SystemCallErrorNumber"].Signature().String() != "i" {
			t.Fatal("wrong typed policy wire")
		}
		if !reflect.DeepEqual(values["TemporaryFileSystem"].Value(), []gitUnitTemporaryFS{{"/tmp", gitUnitTmpfsOptions}}) || !reflect.DeepEqual(values["RestrictAddressFamilies"].Value(), gitUnitAddressFamilies{true, []string{"AF_UNIX", "AF_NETLINK"}}) || !reflect.DeepEqual(values["SystemCallArchitectures"].Value(), []string{"native"}) {
			t.Fatal("weakened unit policy")
		}
		if !reflect.DeepEqual(values["SystemCallFilter"].Value(), gitUnitSyscallFilter{false, []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}}) {
			t.Fatal("io_uring policy weakened")
		}
		for key, want := range map[string]any{"MemoryMax": uint64(1 << 30), "MemorySwapMax": uint64(0), "TasksMax": uint64(64), "CPUQuotaPerSecUSec": uint64(1000000), "RuntimeMaxUSec": uint64(120000000), "Restart": "no", "KillMode": "control-group", "NoNewPrivileges": true, "SystemCallErrorNumber": int32(unix.EPERM)} {
			if values[key].Value() != want {
				t.Fatal(key)
			}
		}
		return []any{dbus.ObjectPath("/org/freedesktop/systemd1/job/1")}, nil
	})}
	if _, err := manager.Start(context.Background(), unit, retainedStubExec, args, gitUnitDescriptionPrefix+Digest("binding"), 7); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := fixedFakeUnitProperties(unit, retainedStubExec, args, gitUnitDescriptionPrefix+Digest("binding"), 7); err == nil {
		t.Fatal("Git accepted as fake auth payload")
	}
	if _, err := fixedGitUnitProperties(unit, retainedStubExec, args, "SFL native VM containment "+Digest("binding"), 7); err == nil {
		t.Fatal("native identity accepted as Git")
	}
}
func TestGitAcquisitionUnitInertChild(t *testing.T) {
	if os.Getenv("SFL_GIT_UNIT_INERT") != "1" {
		return
	}
	os.Stdout.Write([]byte("ready\n"))
	var one [1]byte
	os.Stdin.Read(one[:])
	os.Exit(0)
}

type gitUnitFixture struct {
	owner                *gitUnitOwner
	manager              *fakeContainmentManager
	binding              gitMaterializationBinding
	root, stub, stubHash string
	child                *exec.Cmd
}

func newGitUnitFixture(t *testing.T) gitUnitFixture {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "unit-state")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(parent, "synthetic-stub")
	stubBytes := []byte("synthetic sealed Git worker bytes; not executed")
	if err := os.WriteFile(stub, stubBytes, 0700); err != nil {
		t.Fatal(err)
	}
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
		t.Fatal("child not ready", err)
	}
	done := make(chan struct{})
	go func() { child.Wait(); close(done) }()
	t.Cleanup(func() { child.Process.Kill(); input.Close(); <-done })
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testJob().Snapshot}
	unit := "sfl-alpha-" + binding.Attempt + ".service"
	boot, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	kernel := &fakeContainmentKernel{boot: boot, group: cgroupBinding{Path: "/user.slice/" + unit, Inode: 91, Hierarchy: cgroupHierarchy{Device: 1, Inode: 2, MountNamespace: 3, CgroupNamespace: 4}}}
	manager := &fakeContainmentManager{owner: "synthetic-bus/:1.77", kernel: kernel, child: child, childDone: done, observation: unitObservation{Unit: unit, Path: "/org/freedesktop/systemd1/unit/synthetic", InvocationID: strings.Repeat("b", 32), MainPID: uint32(child.Process.Pid), ControlGroup: kernel.group.Path, ActiveState: "active", MemoryMax: 1 << 30, TasksMax: 64, CPUQuotaPerSecUSec: 1000000, KillMode: "control-group", Restart: "no", CollectMode: "inactive", RemainAfterExit: true, NoNewPrivileges: true, Transient: true}}
	hash := gitRawHash(stubBytes)
	owner, err := newGitUnitOwner(context.Background(), root, stub, hash, manager, kernel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Preserve production Close behavior in assertions. Cleanup may release
		// synthetic ownership after explicitly killing this fixture's own child.
		child.Process.Kill()
		<-done
		if owner.Close() != nil {
			owner.image.Close()
			owner.lock.Close()
			owner.root.Close()
			for _, fd := range owner.rootPIDFDs {
				unix.Close(fd)
			}
		}
	})
	return gitUnitFixture{owner, manager, binding, root, stub, hash, child}
}
func (f gitUnitFixture) prepareStart(t *testing.T) {
	t.Helper()
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal("prepare", err)
	}
	if err := f.owner.start(context.Background(), f.binding, "/private/git-gate.sock"); err != nil {
		t.Fatal("start", err)
	}
}
func TestGitAcquisitionUnitDurableCaptureAndWholeStop(t *testing.T) {
	f := newGitUnitFixture(t)
	f.manager.kernel.pendingImage = 2
	f.prepareStart(t)
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || !r.Started || !r.StartSettled || !r.Captured || r.Revoked || r.Stopped || r.Identity.PID != f.child.Process.Pid || len(f.owner.rootPIDFDs) != 1 || f.manager.starts != 1 {
		t.Fatal("capture receipt", r, err)
	}
	if err := f.owner.Close(); err == nil {
		t.Fatal("live ownership discarded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if other, err := newGitUnitOwner(ctx, f.root, f.stub, f.stubHash, f.manager, f.manager.kernel); err == nil || other != nil {
		t.Fatal("second owner acquired live lock")
	}
	if err := f.owner.start(context.Background(), f.binding, "/private/git-gate.sock"); err == nil || f.manager.starts != 1 {
		t.Fatal("duplicate start")
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal("stop", err)
	}
	r, err = f.owner.read(f.binding.Attempt)
	if err != nil || !r.Stopped || !r.Revoked || r.StopJob == "" || f.manager.stops != 1 {
		t.Fatal("stop receipt", r, err)
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil || f.manager.stops != 1 {
		t.Fatal("rechecked stop", err)
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal("idempotent close", err)
	}
	reopened, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal("reopen", err)
	}
	defer reopened.Close()
	if err := reopened.stop(context.Background(), f.binding); err != nil {
		t.Fatal("reopened stop proof", err)
	}
	if err := reopened.prepare(context.Background(), f.binding); err == nil {
		t.Fatal("spent nonce reused")
	}
}
func TestGitAcquisitionUnitRefusesChangedIdentityAndLimits(t *testing.T) {
	for _, kind := range []string{"manager", "boot", "invocation", "cgroup", "root-image", "memory", "swap", "tasks", "cpu", "description", "pending-job", "root-pid"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitUnitFixture(t)
			if err := f.owner.prepare(context.Background(), f.binding); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "manager":
				f.manager.owner = "different-bus/:1.78"
			case "boot":
				f.manager.kernel.boot = "changed-boot"
			case "invocation":
				f.manager.observation.InvocationID = "not-an-invocation"
			case "cgroup":
				f.manager.kernel.changed = true
			case "root-image":
				f.manager.kernel.pendingImage = 99999
			case "memory":
				f.manager.observation.MemoryMax++
			case "swap":
				f.manager.observation.MemorySwapMax = 1
			case "tasks":
				f.manager.observation.TasksMax++
			case "cpu":
				f.manager.observation.CPUQuotaPerSecUSec++
			case "description":
				f.manager.afterStart = func() { f.manager.observation.Description = "different payload" }
			case "pending-job":
				f.manager.observation.Job.ID = 99
			case "root-pid":
				f.manager.observation.MainPID = 0
			}
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
			defer cancel()
			if err := f.owner.start(ctx, f.binding, "/private/git-gate.sock"); err == nil {
				t.Fatal("foreign/unsafe unit captured")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			if r.Captured || r.Stopped {
				t.Fatal("failed capture qualified")
			}
			if kind == "manager" || kind == "boot" {
				if f.manager.starts != 0 {
					t.Fatal("changed owner started unit")
				}
			}
		})
	}
}
func TestGitAcquisitionUnitStopRequiresRootDeathAndEmptyCapturedGroup(t *testing.T) {
	for _, kind := range []string{"populated", "changed-group", "changed-invocation", "root-still-alive", "manager-changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitUnitFixture(t)
			f.prepareStart(t)
			switch kind {
			case "populated":
				f.manager.holdPopulated = true
			case "changed-group":
				f.manager.kernel.changed = true
			case "changed-invocation":
				f.manager.changedInvocation = true
			case "root-still-alive":
				f.manager.child = nil
			case "manager-changed":
				f.manager.owner = "different-bus/:1.78"
			}
			if err := f.owner.stop(context.Background(), f.binding); err == nil {
				t.Fatal("incomplete stop accepted")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil || !r.Revoked || r.Stopped {
				t.Fatal("uncertainty lost", r, err)
			}
			if err := f.owner.Close(); err == nil {
				t.Fatal("uncertain ownership discarded")
			}
			// The fixture explicitly repairs its own simulated condition, then recovery
			// may stop the captured invocation; no new Start call is allowed.
			f.child.Process.Kill()
			<-f.manager.childDone
			f.manager.holdPopulated = false
			f.manager.kernel.populated = false
			f.manager.kernel.changed = false
			f.manager.owner = r.ManagerOwner
			f.manager.observation.InvocationID = r.Identity.InvocationID
			f.manager.observation.MainPID = 0
			f.manager.observation.ActiveState = "inactive"
			f.manager.changedInvocation = false
			if err := f.owner.stop(context.Background(), f.binding); err != nil {
				t.Fatal("explicit stop retry", err)
			}
			if f.manager.starts != 1 {
				t.Fatal("recovery restarted unit")
			}
		})
	}
}
func TestGitAcquisitionUnitLostStartReplyRecoveryNeverRestarts(t *testing.T) {
	f := newGitUnitFixture(t)
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	f.manager.startErr = errors.New("synthetic lost start reply")
	// Pretend systemd accepted the request but its reply disappeared. The actual
	// live test child and manager observation still identify the owned unit.
	f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
	if err := f.owner.start(context.Background(), f.binding, "/private/git-gate.sock"); err == nil {
		t.Fatal("lost reply succeeded")
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || !r.Started || r.StartSettled || r.Captured || r.Stopped {
		t.Fatal("lost intent", r, err)
	}
	if err := f.owner.start(context.Background(), f.binding, "/private/git-gate.sock"); err == nil || f.manager.starts != 1 {
		t.Fatal("uncertain start repeated")
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal("recover accepted invocation", err)
	}
	if f.manager.starts != 1 || f.manager.stops != 1 {
		t.Fatal("incorrect recovery operations")
	}
}
func TestGitAcquisitionUnitUnknownMissingStartRemainsOwned(t *testing.T) {
	f := newGitUnitFixture(t)
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	f.manager.startErr = errors.New("unknown start")
	f.manager.observeErr = errors.New("unit absent")
	if err := f.owner.start(context.Background(), f.binding, "/private/git-gate.sock"); err == nil {
		t.Fatal("unknown start succeeded")
	}
	if err := f.owner.stop(context.Background(), f.binding); err == nil {
		t.Fatal("missing unit treated as death")
	}
	if err := f.owner.Close(); err == nil {
		t.Fatal("unknown root abandoned")
	}
	f.manager.kernel.boot = "changed-boot-after-real-reboot"
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal("new boot did not prove prior processes dead", err)
	}
	if f.manager.stops != 0 || f.manager.starts != 1 {
		t.Fatal("reboot recovery contacted foreign unit")
	}
}
func TestGitAcquisitionUnitRejectsMutatedJournalAndBinding(t *testing.T) {
	f := newGitUnitFixture(t)
	f.prepareStart(t)
	path := filepath.Join(f.root, gitUnitDirectory(f.binding.Attempt), "git-unit.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"duplicate", "alias", "unknown", "null", "foreign-binding", "wrong-image", "wrong-cgroup", "unsafe-job", "uncaptured-identity"} {
		raw := append([]byte(nil), original...)
		switch kind {
		case "duplicate":
			raw = append([]byte(`{"Version":1,`), raw[1:]...)
		case "alias":
			raw = bytes.Replace(raw, []byte(`"Version"`), []byte(`"version"`), 1)
		case "unknown":
			raw = append([]byte(`{"Other":1,`), raw[1:]...)
		case "null":
			raw = bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version":null`), 1)
		default:
			var r gitUnitRecord
			json.Unmarshal(raw, &r)
			switch kind {
			case "foreign-binding":
				r.Binding.Attempt = strings.Repeat("f", 32)
			case "wrong-image":
				r.StubHash = Digest("wrong image")
			case "wrong-cgroup":
				r.Cgroup.Path = "/user.slice/foreign.service"
			case "unsafe-job":
				r.StartJob = "/outside/job"
			case "uncaptured-identity":
				r.Captured = false
			}
			raw, _ = json.Marshal(r)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		before := f.manager.stops
		if err := f.owner.stop(context.Background(), f.binding); err == nil || f.manager.stops != before {
			t.Fatal("mutated record selected a unit", kind)
		}
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	bad := f.binding
	bad.Snapshot.PR++
	if err := f.owner.stop(context.Background(), bad); err == nil {
		t.Fatal("different binding stopped unit")
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
}
func TestGitAcquisitionUnitStartStopAdmissionHonorsDeadline(t *testing.T) {
	f := newGitUnitFixture(t)
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	entered := make(chan struct{})
	f.manager.holdStart = hold
	f.manager.entered = entered
	started := make(chan error, 1)
	go func() { started <- f.owner.start(context.Background(), f.binding, "/private/git-gate.sock") }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	at := time.Now()
	if err := f.owner.stop(ctx, f.binding); err == nil || time.Since(at) > 500*time.Millisecond {
		t.Fatal("pending start ignored stop admission deadline", err)
	}
	if err := f.owner.Close(); err == nil {
		t.Fatal("in-flight start abandoned")
	}
	close(hold)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
}

func TestGitAcquisitionUnitCrashOwnerChild(t *testing.T) {
	if os.Getenv("SFL_GIT_OWNER_CRASH") != "1" {
		return
	}
	var binding gitMaterializationBinding
	if json.Unmarshal([]byte(os.Getenv("SFL_GIT_TEST_BINDING")), &binding) != nil {
		os.Exit(2)
	}
	boot, err := currentBootID()
	if err != nil {
		os.Exit(3)
	}
	manager := &fakeContainmentManager{owner: "synthetic-bus/:1.77", kernel: &fakeContainmentKernel{boot: boot}, startErr: errors.New("synthetic accepted start with lost reply")}
	owner, err := newGitUnitOwner(context.Background(), os.Getenv("SFL_GIT_TEST_ROOT"), os.Getenv("SFL_GIT_TEST_STUB"), os.Getenv("SFL_GIT_TEST_HASH"), manager, manager.kernel)
	if err != nil || owner.prepare(context.Background(), binding) != nil {
		os.Exit(4)
	}
	if owner.start(context.Background(), binding, "/private/git-gate.sock") == nil {
		os.Exit(5)
	}
	record, err := owner.read(binding.Attempt)
	if err != nil || !record.Started || record.Captured {
		os.Exit(6)
	}
	os.Stdout.Write([]byte("ready\n"))
	var one [1]byte
	os.Stdin.Read(one[:])
	os.Exit(7)
}
func TestGitAcquisitionUnitActualOwnerCrashPreservesUncertainStart(t *testing.T) {
	f := newGitUnitFixture(t)
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.binding)
	child := exec.Command(os.Args[0], "-test.run=^TestGitAcquisitionUnitCrashOwnerChild$")
	child.Env = []string{"SFL_GIT_OWNER_CRASH=1", "SFL_GIT_TEST_ROOT=" + f.root, "SFL_GIT_TEST_STUB=" + f.stub, "SFL_GIT_TEST_HASH=" + f.stubHash, "SFL_GIT_TEST_BINDING=" + string(raw)}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			child.Process.Kill()
			child.Wait()
		}
	}()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		t.Fatal("owner child failed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	other, err := newGitUnitOwner(ctx, f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	cancel()
	if err == nil || other != nil {
		t.Fatal("process owner lock bypassed")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	reaped = true
	reopened, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal("recover owner", err)
	}
	defer reopened.Close()
	record, err := reopened.read(f.binding.Attempt)
	if err != nil || !record.Started || record.Captured || record.Stopped {
		t.Fatal("crashed start intent lost", record, err)
	}
	if err := reopened.start(context.Background(), f.binding, "/private/git-gate.sock"); err == nil || f.manager.starts != 0 {
		t.Fatal("crashed start was retried")
	}
	// The external fake manager retained the actual inert root independently of
	// the killed controller process. Recovery captures and stops that exact root.
	f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
	if err := reopened.stop(context.Background(), f.binding); err != nil {
		t.Fatal("recover stop", err)
	}
	if f.manager.starts != 0 || f.manager.stops != 1 {
		t.Fatal("recovery operations changed")
	}
}
