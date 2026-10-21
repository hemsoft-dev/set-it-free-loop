//go:build linux

package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A real inert child terminates before any root pidfd or grant is captured.
// The manager and cgroup observations are fake, not host qualification.
func gitEarlyStopFixture(t *testing.T) gitUnitFixture {
	t.Helper()
	f := newGitUnitFixture(t)
	if err := f.owner.prepare(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	f.manager.startErr = errors.New("synthetic accepted start, lost reply")
	f.manager.observation.Description = gitUnitDescriptionPrefix + Digest(f.binding)
	if f.owner.start(context.Background(), f.binding, "/private/bootstrap.sock") == nil {
		t.Fatal("lost reply succeeded")
	}
	f.child.Process.Kill()
	<-f.manager.childDone
	f.manager.observation.MainPID = 0
	f.manager.observation.ActiveState = "failed"
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
	for _, kind := range []string{"captured", "not-started", "not-settled", "not-revoked", "not-stopped", "grant", "identity", "invocation", "unit-path", "group", "inode", "hierarchy"} {
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
			}
			if bad.valid(f.binding.Attempt, f.stubHash) {
				t.Fatal("terminal proof accepted an identity/qualification alias")
			}
		})
	}
}

func TestGitEarlyStopRefusesIncompleteTerminalEvidence(t *testing.T) {
	for _, kind := range []string{"missing-unit", "manager", "description", "invocation", "pending-job", "active", "populated", "changed-group", "missing-group", "limits"} {
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
			case "active":
				f.manager.observation.ActiveState = "active"
			case "populated":
				f.manager.kernel.populated = true
			case "changed-group":
				f.manager.kernel.changed = true
			case "missing-group":
				f.manager.observation.ControlGroup = ""
			case "limits":
				f.manager.observation.MemorySwapMax = 1
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
