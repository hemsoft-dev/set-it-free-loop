//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real inert child terminates before any root pidfd or grant is captured.
// The manager and cgroup observations are fake, not host qualification.
func gitEarlyStopFixture(t *testing.T) gitUnitFixture {
	t.Helper()
	f := newGitUnitFixture(t)
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	f.manager.waitErr = errors.New("synthetic job failed before root capture")
	f.manager.afterWait = func() {
		r, err := f.owner.read(f.binding.Attempt)
		if err != nil || r.Birth == nil || !r.Birth.valid(r) || r.StartSettled || r.Captured {
			t.Fatal("birth identity was not durable before waiting for start", r, err)
		}
		f.child.Process.Kill()
		<-f.manager.childDone
		f.manager.observation.MainPID = 0
		f.manager.observation.ActiveState = "failed"
	}
	if f.owner.start(context.Background(), f.binding, "/private/bootstrap.sock") == nil {
		t.Fatal("failed start job succeeded")
	}
	return f
}

func TestGitEarlyStopRecoversTerminalUncapturedAttempt(t *testing.T) {
	f := gitEarlyStopFixture(t)
	if f.owner.Close() == nil {
		t.Fatal("unproven attempt discarded")
	}
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal("terminal uncaptured attempt could not recover", err)
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || !r.Stopped || !r.Revoked || !r.StartSettled || r.Captured || r.EarlyStop == nil || len(f.owner.rootPIDFDs) != 0 || f.owner.grantEligible[f.binding.Attempt] {
		t.Fatal("terminal recovery fabricated root or grant", r, err)
	}
	if f.manager.starts != 1 || f.manager.stops != 0 {
		t.Fatal("terminal recovery restarted or stopped another invocation")
	}
	if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
		t.Fatal("terminal recovery could not recheck and close")
	}
	reopened, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Rechecking the persisted proof uses the original captured hierarchy,
	// not a fresh capture of a potentially replaced cgroup.
	reopened.kernel = gitEarlyStopNoCapture{f.manager.kernel}
	if reopened.stop(context.Background(), f.binding) != nil || reopened.grantEligible[f.binding.Attempt] {
		t.Fatal("reopen lost proof or rearmed a grant")
	}
}

type gitEarlyStopNoCapture struct{ containmentKernel }

func (gitEarlyStopNoCapture) Capture(string, string) (cgroupBinding, error) {
	return cgroupBinding{}, errors.New("must not recapture a persisted terminal proof")
}

type gitEarlyStopChangingManager struct {
	gitUnitManager
	reads  int
	change func(*unitObservation)
}

func (m *gitEarlyStopChangingManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	v, err := m.gitUnitManager.Observe(ctx, unit)
	m.reads++
	if m.reads == 2 {
		m.change(&v)
	}
	return v, err
}

func TestGitEarlyStopRefusesChangedProofAndExpiredContext(t *testing.T) {
	for _, kind := range []string{"invocation", "unit-path", "live-root", "pending-job", "group", "state", "boot", "manager", "canceled", "repopulated"} {
		t.Run(kind, func(t *testing.T) {
			f := gitEarlyStopFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			changing := &gitEarlyStopChangingManager{gitUnitManager: f.manager, change: func(v *unitObservation) {
				switch kind {
				case "invocation":
					v.InvocationID = strings.Repeat("d", 32)
				case "unit-path":
					v.Path += "changed"
				case "live-root":
					v.MainPID = 99
				case "pending-job":
					v.Job.ID = 9
				case "group":
					v.ControlGroup += "changed"
				case "state":
					v.ActiveState = "activating"
				case "boot":
					f.manager.kernel.boot = "another-boot"
				case "manager":
					f.manager.owner = "different-bus/:1.99"
				case "canceled":
					cancel()
				case "repopulated":
					f.manager.kernel.populated = true
				}
			}}
			f.owner.manager = changing
			if f.owner.stop(ctx, f.binding) == nil || f.owner.Close() == nil {
				t.Fatal("changing terminal proof released ownership")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil || r.Stopped || r.EarlyStop != nil || f.manager.starts != 1 || f.manager.stops != 0 {
				t.Fatal("changed proof became a receipt or invoked another unit", r, err)
			}
			f.manager.kernel.boot = "new-boot-for-explicit-fixture-recovery"
			if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
				t.Fatal("explicit changed-boot fixture recovery failed")
			}
		})
	}
}

func TestGitEarlyStopJournalRefusesQualificationAliases(t *testing.T) {
	f := gitEarlyStopFixture(t)
	if f.owner.stop(context.Background(), f.binding) != nil {
		t.Fatal("terminal proof")
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || r.EarlyStop == nil || !r.valid(f.binding.Attempt, f.stubHash) {
		t.Fatal("valid terminal proof missing", err)
	}
	for _, kind := range []string{"captured", "not-started", "not-settled", "not-revoked", "not-stopped", "grant", "identity", "invocation", "unit-path", "group", "inode", "hierarchy", "missing-birth", "different-job", "different-birth"} {
		t.Run(kind, func(t *testing.T) {
			bad := r
			proof := *r.EarlyStop
			bad.EarlyStop = &proof
			switch kind {
			case "captured":
				bad.Captured = true
			case "not-started":
				bad.Started = false
			case "not-settled":
				bad.StartSettled = false
			case "not-revoked":
				bad.Revoked = false
			case "not-stopped":
				bad.Stopped = false
			case "grant":
				bad.BootstrapGrantHash = Digest("invented-grant")
			case "identity":
				bad.Identity.PID = 99
			case "invocation":
				proof.InvocationID = ""
			case "unit-path":
				proof.UnitPath = "/"
			case "group":
				proof.Cgroup.Path += "changed"
			case "inode":
				proof.Cgroup.Inode = 0
			case "hierarchy":
				proof.Cgroup.Hierarchy.MountNamespace = 0
			case "missing-birth":
				bad.Birth = nil
			case "different-job":
				bad.StartJob = "/org/freedesktop/systemd1/job/99"
			case "different-birth":
				birth := *r.Birth
				birth.Unit.InvocationID = strings.Repeat("d", 32)
				bad.Birth = &birth
			}
			if bad.valid(f.binding.Attempt, f.stubHash) {
				t.Fatal("terminal proof accepted an identity/qualification alias")
			}
		})
	}
}

type gitEarlyStopCrashInput struct {
	Root, Stub, StubHash, Boot string
	Binding                    gitMaterializationBinding
	Observation                unitObservation
	Group                      cgroupBinding
}

func TestGitEarlyStopCrashOwnerChild(t *testing.T) {
	if os.Getenv("SFL_GIT_EARLY_STOP_CRASH") != "1" {
		return
	}
	var input gitEarlyStopCrashInput
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		t.Fatal("explicit synthetic crash fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kernel := &fakeContainmentKernel{boot: input.Boot, group: input.Group}
	manager := &fakeContainmentManager{owner: "synthetic-bus/:1.77", kernel: kernel, observation: input.Observation}
	owner, err := newGitUnitOwner(ctx, input.Root, input.Stub, input.StubHash, manager, kernel)
	if err != nil || owner.prepare(ctx, input.Binding) != nil {
		t.Fatal("synthetic owner", err)
	}
	manager.afterWait = func() {
		r, err := owner.read(input.Binding.Attempt)
		if err != nil || r.Birth == nil || r.StartSettled || r.Captured {
			t.Fatal("pre-crash birth was not persisted", err)
		}
		fmt.Fprint(os.Stdout, "birth\n")
		<-ctx.Done() // Parent SIGKILLs the owner while its actual flock is held.
	}
	_ = owner.start(ctx, input.Binding, "/private/bootstrap.sock")
}

func TestGitEarlyStopRecoversPersistedBirthAfterActualOwnerCrash(t *testing.T) {
	f := newGitUnitFixture(t)
	// No attempt exists yet; transfer the otherwise empty synthetic service
	// directory to the separate owner process without discarding owned work.
	if f.owner.Close() != nil {
		t.Fatal("empty fixture owner did not close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitEarlyStopCrashOwnerChild$")
	command.Env = []string{"SFL_GIT_EARLY_STOP_CRASH=1"}
	input := gitEarlyStopCrashInput{f.root, f.stub, f.stubHash, f.manager.kernel.boot, f.binding, f.manager.observation, f.manager.kernel.group}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdin = strings.NewReader(string(raw))
	output, err := command.StdoutPipe()
	if err != nil || command.Start() != nil {
		t.Fatal("start synthetic crash owner", err)
	}
	var exitOnce sync.Once
	killOwner := func() { exitOnce.Do(func() { command.Process.Kill(); command.Wait() }) }
	defer killOwner()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "birth\n" {
		t.Fatal("owner did not reach durable birth", err, string(ready))
	}
	killOwner()
	f.child.Process.Kill()
	<-f.manager.childDone
	f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
	f.manager.observation.MainPID = 0
	f.manager.observation.ActiveState = "failed"
	reopened, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal("crashed ownership did not reopen", err)
	}
	defer reopened.Close()
	r, err := reopened.read(f.binding.Attempt)
	if err != nil || r.Birth == nil || r.StartSettled || r.Captured || r.Stopped || reopened.grantEligible[f.binding.Attempt] {
		t.Fatal("crash lost birth identity or invented settled/root/grant state", r, err)
	}
	if reopened.stop(context.Background(), f.binding) != nil || f.manager.starts != 0 || f.manager.stops != 0 || reopened.Close() != nil {
		t.Fatal("durable birth could not recover without restart/rebinding")
	}
}

type gitBirthInitializingManager struct {
	gitUnitManager
	remaining int
	reads     int
	observed  chan struct{}
	once      sync.Once
}

func (m *gitBirthInitializingManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	v, err := m.gitUnitManager.Observe(ctx, unit)
	m.reads++
	if m.remaining > 0 {
		m.remaining--
		v.InvocationID = strings.Repeat("0", 32)
		v.ControlGroup = ""
		v.ActiveState = "activating"
		v.Job = unitJob{1, "/org/freedesktop/systemd1/job/1"}
		if m.observed != nil {
			m.once.Do(func() { close(m.observed) })
		}
	}
	return v, err
}

type gitBirthDelayedCgroup struct {
	containmentKernel
	remaining int
	reads     int
}

type gitBirthDisappearingCgroup struct {
	containmentKernel
	manager *fakeContainmentManager
	reads   int
	missing error
}

func (k *gitBirthDisappearingCgroup) Capture(group, unit string) (cgroupBinding, error) {
	k.reads++
	if k.reads > 1 {
		return cgroupBinding{}, k.missing
	}
	value, err := k.containmentKernel.Capture(group, unit)
	k.manager.observation.MainPID = 0
	k.manager.observation.ActiveState = "failed"
	k.manager.observation.Job = unitJob{}
	return value, err
}

func TestGitBirthRetainsOriginalGroupWhenTerminalDirectoryDisappears(t *testing.T) {
	for _, kind := range []string{"removed", "permission", "populated", "replaced"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitUnitFixture(t)
			if f.owner.prepare(context.Background(), f.binding) != nil {
				t.Fatal("prepare")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			r.Started = true
			r.StartJob = "/org/freedesktop/systemd1/job/1"
			f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
			f.manager.observation.Job = unitJob{1, "/org/freedesktop/systemd1/job/1"}
			kernel := &gitBirthDisappearingCgroup{containmentKernel: f.manager.kernel, manager: f.manager, missing: os.ErrNotExist}
			if kind == "permission" {
				kernel.missing = os.ErrPermission
			}
			if kind == "populated" {
				f.manager.kernel.populated = true
			}
			// Replacement is introduced after the first capture, so the
			// original binding must be checked again by Empty.
			if kind == "replaced" {
				f.owner.manager = &gitEarlyStopChangingManager{gitUnitManager: f.manager, change: func(*unitObservation) { f.manager.kernel.changed = true }}
			}
			f.owner.kernel = kernel
			got, err := f.owner.captureBirth(context.Background(), r)
			if kind == "removed" {
				if err != nil || got.Birth == nil || got.Birth.Unit.Cgroup != f.manager.kernel.group || got.Captured || got.Stopped {
					t.Fatal("original removed group was discarded or termination fabricated", got, err)
				}
			} else if err == nil || got.Birth != nil {
				t.Fatal("uncertain group accepted", got, err)
			}
		})
	}
}

func (k *gitBirthDelayedCgroup) Capture(group, unit string) (cgroupBinding, error) {
	k.reads++
	if k.remaining > 0 {
		k.remaining--
		return cgroupBinding{}, os.ErrNotExist
	}
	return k.containmentKernel.Capture(group, unit)
}

func TestGitBirthWaitsForAcceptedJobInitializationWithoutRestart(t *testing.T) {
	for _, phase := range []string{"properties", "cgroup"} {
		t.Run(phase, func(t *testing.T) {
			f := newGitUnitFixture(t)
			if phase == "properties" {
				f.owner.manager = &gitBirthInitializingManager{gitUnitManager: f.manager, remaining: 3}
			} else {
				f.manager.observation.Job = unitJob{1, "/org/freedesktop/systemd1/job/1"}
				f.manager.observation.ActiveState = "activating"
				f.manager.afterWait = func() {
					if f.manager.observation.Job.ID == 1 {
						f.manager.observation.Job = unitJob{}
						f.manager.observation.ActiveState = "active"
					}
				}
				f.owner.kernel = &gitBirthDelayedCgroup{containmentKernel: f.manager.kernel, remaining: 3}
			}
			f.prepareStart(t)
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil || r.Birth == nil || !r.Captured || !r.StartSettled || f.manager.starts != 1 || r.Birth.Unit.InvocationID == strings.Repeat("0", 32) {
				t.Fatal("accepted pending job was lost or restarted", r, err)
			}
			if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
				t.Fatal("initialized unit cleanup")
			}
		})
	}
}

func TestGitBirthPendingInitializationHonorsCallerCancellation(t *testing.T) {
	f := newGitUnitFixture(t)
	if f.owner.prepare(context.Background(), f.binding) != nil {
		t.Fatal("prepare")
	}
	manager := &gitBirthInitializingManager{gitUnitManager: f.manager, remaining: 100000, observed: make(chan struct{})}
	f.owner.manager = manager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.owner.start(ctx, f.binding, "/private/bootstrap.sock") }()
	select {
	case <-manager.observed:
	case <-time.After(time.Second):
		t.Fatal("pending start not observed")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("uninitialized accepted job succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending initialization ignored cancellation")
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || !r.Started || r.StartJob == "" || r.Birth != nil || r.Captured || f.manager.starts != 1 || f.owner.Close() == nil {
		t.Fatal("canceled start lost uncertainty or repeated work", r, err)
	}
	f.manager.kernel.boot = "new-boot-for-canceled-initialization-fixture"
	if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
		t.Fatal("explicit fixture recovery")
	}
}

func TestGitBirthPendingInitializationRejectsChangedInvocation(t *testing.T) {
	f := newGitUnitFixture(t)
	f.manager.observation.Job = unitJob{1, "/org/freedesktop/systemd1/job/1"}
	f.manager.observation.ActiveState = "activating"
	manager := &gitEarlyStopChangingManager{gitUnitManager: f.manager, change: func(v *unitObservation) {
		v.InvocationID = strings.Repeat("d", 32)
	}}
	f.owner.manager = manager
	f.owner.kernel = &gitBirthDelayedCgroup{containmentKernel: f.manager.kernel, remaining: 3}
	if f.owner.prepare(context.Background(), f.binding) != nil || f.owner.start(context.Background(), f.binding, "/private/bootstrap.sock") == nil {
		t.Fatal("changed invocation admitted while waiting for its group")
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || r.Birth != nil || r.Captured || f.manager.starts != 1 || manager.reads != 2 || f.owner.Close() == nil {
		t.Fatal("pending identity was rebound or uncertain start released", r, err, manager.reads)
	}
	f.manager.kernel.boot = "new-boot-for-changed-pending-identity"
	if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
		t.Fatal("explicit changed-boot fixture recovery")
	}
}

func TestGitBirthRejectsSettledJobBeforeAnyInvocationWasCorrelated(t *testing.T) {
	for _, phase := range []string{"first-observation", "after-empty-properties"} {
		t.Run(phase, func(t *testing.T) {
			f := newGitUnitFixture(t)
			if f.owner.prepare(context.Background(), f.binding) != nil {
				t.Fatal("prepare")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			r.Started = true
			r.StartJob = "/org/freedesktop/systemd1/job/1"
			f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
			f.manager.observation.InvocationID = strings.Repeat("d", 32)
			if phase == "after-empty-properties" {
				f.owner.manager = &gitBirthInitializingManager{gitUnitManager: f.manager, remaining: 1}
			}
			got, err := f.owner.captureBirth(context.Background(), r)
			if err == nil || got.Birth != nil || got.Captured || f.owner.grantEligible[f.binding.Attempt] {
				t.Fatal("settled replacement invocation adopted as original", got, err)
			}
		})
	}
}

func TestGitEarlyStopRefusesIncompleteTerminalEvidence(t *testing.T) {
	for _, kind := range []string{"missing-unit", "manager", "description", "invocation", "pending-job", "activating", "populated", "changed-group", "missing-group", "limits", "replacement-before-recovery"} {
		t.Run(kind, func(t *testing.T) {
			f := gitEarlyStopFixture(t)
			switch kind {
			case "missing-unit":
				f.manager.observeErr = errors.New("unit absent")
			case "manager":
				f.manager.owner = "different-bus/:1.99"
			case "description":
				f.manager.observation.Description = "another payload"
			case "invocation":
				f.manager.observation.InvocationID = ""
			case "pending-job":
				f.manager.observation.Job.ID = 9
			case "activating":
				f.manager.observation.ActiveState = "activating"
			case "populated":
				f.manager.kernel.populated = true
			case "changed-group":
				f.manager.kernel.changed = true
			case "missing-group":
				f.manager.observation.ControlGroup = ""
			case "limits":
				f.manager.observation.MemorySwapMax = 1
			case "replacement-before-recovery":
				f.manager.observation.InvocationID = strings.Repeat("d", 32)
				f.manager.kernel.group.Inode++
			}
			if f.owner.stop(context.Background(), f.binding) == nil || f.owner.Close() == nil {
				t.Fatal("incomplete terminal evidence released ownership")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil || !r.Revoked || r.Stopped || r.Captured || f.manager.starts != 1 || f.manager.stops != 0 {
				t.Fatal("refusal lost uncertainty or contacted another unit", r, err)
			}
			// Only a changed kernel boot can retire these deliberately broken
			// fixture records without trusting another manager or missing unit.
			f.manager.kernel.boot = strings.Repeat("c", 32)
			if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
				t.Fatal("changed boot could not retire the old attempt")
			}
		})
	}
}

func TestGitEarlyStopRecoversActiveExitedBoundUnit(t *testing.T) {
	f := gitEarlyStopFixture(t)
	f.manager.observation.ActiveState = "active"
	f.manager.waitErr = nil
	f.manager.afterWait = nil
	if err := f.owner.stop(context.Background(), f.binding); err != nil {
		t.Fatal("active-exited original unit could not be stopped", err)
	}
	r, err := f.owner.read(f.binding.Attempt)
	if err != nil || !r.Stopped || r.EarlyStop == nil || r.Captured || r.StopJob != "/org/freedesktop/systemd1/job/2" || f.manager.stops != 1 || f.owner.Close() != nil {
		t.Fatal("active-exited recovery lost terminal proof", r, err, f.manager.stops)
	}
}

func TestGitEarlyStopActiveExitedRefusesReplacementOrNonemptyGroup(t *testing.T) {
	for _, kind := range []string{"invocation", "group", "populated", "post-stop-invocation", "stop-job-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := gitEarlyStopFixture(t)
			f.manager.observation.ActiveState = "active"
			f.manager.waitErr = nil
			f.manager.afterWait = nil
			wantStops := 0
			switch kind {
			case "invocation":
				f.manager.observation.InvocationID = strings.Repeat("d", 32)
			case "group":
				f.manager.kernel.changed = true
			case "populated":
				f.manager.kernel.populated = true
			case "post-stop-invocation":
				f.manager.changedInvocation = true
				wantStops = 1
			case "stop-job-failure":
				f.manager.waitErr = errors.New("synthetic stop job failed")
				wantStops = 1
			}
			if f.owner.stop(context.Background(), f.binding) == nil || f.owner.Close() == nil {
				t.Fatal("uncertain active-exited unit released")
			}
			r, err := f.owner.read(f.binding.Attempt)
			if err != nil || r.Stopped || r.EarlyStop != nil || f.manager.stops != wantStops {
				t.Fatal("failed recovery fabricated proof or stopped replacement", r, err, f.manager.stops)
			}
			f.manager.kernel.boot = "changed-boot-for-active-exited-fixture"
			if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
				t.Fatal("fixture recovery")
			}
		})
	}
}

func TestGitEarlyStopRefusesUnknownStartWithoutBirthIdentity(t *testing.T) {
	f := newGitUnitFixture(t)
	if f.owner.prepare(context.Background(), f.binding) != nil {
		t.Fatal("prepare")
	}
	f.manager.startErr = errors.New("synthetic lost accepted-start reply")
	f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
	if f.owner.start(context.Background(), f.binding, "/private/bootstrap.sock") == nil {
		t.Fatal("lost reply succeeded")
	}
	f.child.Process.Kill()
	<-f.manager.childDone
	f.manager.observation.MainPID = 0
	f.manager.observation.ActiveState = "failed"
	if f.owner.stop(context.Background(), f.binding) == nil || f.owner.Close() == nil {
		t.Fatal("terminal recovery invented a missing post-start identity")
	}
	f.manager.kernel.boot = "new-boot-for-unknown-start-fixture"
	if f.owner.stop(context.Background(), f.binding) != nil || f.owner.Close() != nil {
		t.Fatal("changed boot could not retire the unbound old attempt")
	}
}
