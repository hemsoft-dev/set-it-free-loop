//go:build linux

package reviewer

import (
	"context"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Immutable unit identity shared by a start birth record and a terminal
// receipt. Its presence alone proves neither termination nor source authority.
type gitEarlyStopEvidence struct {
	InvocationID string
	UnitPath     dbus.ObjectPath
	Cgroup       cgroupBinding
}

type gitUnitBirthEvidence struct {
	StartJob string
	Unit     gitEarlyStopEvidence
}

func (e gitUnitBirthEvidence) valid(r gitUnitRecord) bool {
	id := strings.TrimPrefix(e.StartJob, "/org/freedesktop/systemd1/job/")
	n, err := strconv.ParseUint(id, 10, 32)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == id && e.StartJob == r.StartJob && e.Unit.valid(r.Unit)
}

func gitBirthObservation(r gitUnitRecord, v unitObservation) bool {
	job := v.Job
	v.Job = unitJob{}
	if !expectedGitUnit(r, v) {
		return false
	}
	return job.ID == 0 || string(job.Path) == r.StartJob && strings.HasSuffix(r.StartJob, "/"+strconv.FormatUint(uint64(job.ID), 10))
}

// Fresh-start only, before WaitJob/root capture. The accepted job and original
// manager bind the observed unit; the durable receipt survives a later failed
// job or crash. A missing receipt is never reconstructed during recovery.
func (o *gitUnitOwner) captureBirth(ctx context.Context, r gitUnitRecord) (gitUnitRecord, error) {
	if ctx.Err() != nil || !r.Started || r.StartJob == "" || r.Birth != nil || r.Captured || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return r, errGitUnit
	}
	before, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !gitBirthObservation(r, before) {
		return r, errGitUnit
	}
	group, err := o.kernel.Capture(before.ControlGroup, r.Unit)
	if err != nil || group.Path != before.ControlGroup {
		return r, errGitUnit
	}
	birth := gitUnitBirthEvidence{r.StartJob, gitEarlyStopEvidence{before.InvocationID, before.Path, group}}
	if !birth.valid(r) {
		return r, errGitUnit
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !gitBirthObservation(r, after) || after.Path != before.Path || after.InvocationID != before.InvocationID || after.ControlGroup != before.ControlGroup {
		return r, errGitUnit
	}
	current, err := o.kernel.Capture(after.ControlGroup, r.Unit)
	if err != nil || current != group {
		return r, errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID || r.ManagerOwner != o.manager.Owner() || ctx.Err() != nil {
		return r, errGitUnit
	}
	r.Birth = &birth
	return r, nil
}

func (e gitEarlyStopEvidence) valid(unit string) bool {
	h := e.Cgroup.Hierarchy
	return gitQuarantineHex(e.InvocationID, 32) && e.UnitPath.IsValid() && strings.HasPrefix(string(e.UnitPath), "/org/freedesktop/systemd1/unit/") &&
		validUnitCgroup(e.Cgroup.Path, unit) && e.Cgroup.Inode != 0 && h.Device != 0 && h.Inode != 0 && h.MountNamespace != 0 && h.CgroupNamespace != 0
}

// Called only under the unit owner's operation token after durable revocation.
// A missing unit or missing uncaptured cgroup is uncertainty, never a proof.
// A live root falls back to the existing original-pidfd capture/stop path.
func (o *gitUnitOwner) stopBeforeCapture(ctx context.Context, r gitUnitRecord) (gitUnitRecord, bool, error) {
	if ctx.Err() != nil || r.Captured || !r.Started || !r.Revoked || r.BootstrapGrantHash != "" || r.WorkerGrantHash != "" || o.grantEligible[r.Binding.Attempt] || r.ManagerOwner != o.manager.Owner() {
		return r, true, errGitUnit
	}
	before, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitUnit(r, before) {
		return r, true, errGitUnit
	}
	if before.MainPID != 0 {
		if r.EarlyStop != nil {
			return r, true, errGitUnit
		}
		return r, false, nil
	}
	if before.ActiveState != "inactive" && before.ActiveState != "failed" {
		return r, true, errGitUnit
	}
	if r.Birth == nil || !r.Birth.valid(r) {
		return r, true, errGitUnit
	}
	proof := r.Birth.Unit
	if proof.InvocationID != before.InvocationID || proof.UnitPath != before.Path || proof.Cgroup.Path != before.ControlGroup || r.EarlyStop != nil && *r.EarlyStop != proof {
		return r, true, errGitUnit
	}
	if !proof.valid(r.Unit) || o.kernel.Empty(proof.Cgroup) != nil {
		return r, true, errGitUnit
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || after != before || o.kernel.Empty(proof.Cgroup) != nil {
		return r, true, errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID || r.ManagerOwner != o.manager.Owner() || ctx.Err() != nil {
		return r, true, errGitUnit
	}
	r.StartSettled = true
	r.Stopped = true
	r.EarlyStop = &proof
	return r, true, nil
}
