//go:build linux

package reviewer

import (
	"context"
	"strings"

	"github.com/godbus/dbus/v5"
)

// A distinct terminal receipt for an attempt that never captured a live root
// and therefore never granted a bootstrap or worker capability. It does not
// fabricate a root identity, pidfd, successful start or source qualification.
type gitEarlyStopEvidence struct {
	InvocationID string
	UnitPath     dbus.ObjectPath
	Cgroup       cgroupBinding
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
	var proof gitEarlyStopEvidence
	if r.EarlyStop != nil {
		proof = *r.EarlyStop
		if proof.InvocationID != before.InvocationID || proof.UnitPath != before.Path || proof.Cgroup.Path != before.ControlGroup {
			return r, true, errGitUnit
		}
	} else {
		group, err := o.kernel.Capture(before.ControlGroup, r.Unit)
		if err != nil || group.Path != before.ControlGroup {
			return r, true, errGitUnit
		}
		proof = gitEarlyStopEvidence{before.InvocationID, before.Path, group}
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
