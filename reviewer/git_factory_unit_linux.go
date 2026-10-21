//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Keep these checks under the unit owner's operation lock. They confer no
// serialized qualification and do not arm or replay a grant after recovery.
func (o *gitUnitOwner) factoryUnit(ctx context.Context, binding gitMaterializationBinding) (gitUnitRecord, error) {
	r, err := o.read(binding.Attempt)
	if err != nil || o.closed || ctx.Err() != nil || r.Binding != binding || !r.Captured || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() || !o.grantEligible[binding.Attempt] || o.grantFailed[binding.Attempt] {
		return r, errGitUnit
	}
	boot, err := o.kernel.BootID()
	root, held := o.rootPIDFDs[binding.Attempt]
	if err != nil || boot != r.BootID || !held || gitCheckProcessPIDFD(r.Identity, root) != nil {
		return r, errGitUnit
	}
	current, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitUnit(r, current) || current.InvocationID != r.Identity.InvocationID || current.MainPID != uint32(r.Identity.PID) || current.ActiveState != "active" || r.Birth == nil || current.Path != r.Birth.Unit.UnitPath {
		return r, errGitUnit
	}
	group, err := o.kernel.Capture(current.ControlGroup, r.Unit)
	if err != nil || group != r.Cgroup || o.kernel.Executable(r.Identity.PID, r.StubHash) != nil {
		return r, errGitUnit
	}
	return r, nil
}

func (o *gitUnitOwner) checkFactoryReadPeer(ctx context.Context, binding gitMaterializationBinding, worker GateIdentity, peer GitReadPeer) error {
	if ctx == nil || !binding.valid() || !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	r, err := o.factoryUnit(bounded, binding)
	if err != nil || r.WorkerGrantHash == "" || r.WorkerIdentity != worker {
		return errGitUnit
	}
	identity, fd, err := retainGitBrokerPeer(peer, r.Identity.InvocationID)
	if err != nil {
		return errGitUnit
	}
	defer unix.Close(fd)
	unit, invocation, err := o.manager.UnitForPIDFD(bounded, fd)
	if err != nil || unit != r.Unit || invocation != r.Identity.InvocationID || gitCheckProcessPIDFD(identity, fd) != nil {
		return errGitUnit
	}
	after, err := o.factoryUnit(bounded, binding)
	if err != nil || after.Identity != r.Identity || after.Cgroup != r.Cgroup || bounded.Err() != nil {
		return errGitUnit
	}
	return nil
}

func gitFactoryParent(ctx context.Context, identity GateIdentity, original int) (int, error) {
	if ctx.Err() != nil || gitCheckProcessPIDFD(identity, original) != nil {
		return 0, errGitUnit
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/stat", identity.PID))
	if err != nil {
		return 0, errGitUnit
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	end := strings.LastIndexByte(string(raw), ')')
	if readErr != nil || closeErr != nil || len(raw) > 4096 || end < 0 {
		return 0, errGitUnit
	}
	parts := strings.Fields(string(raw[end+1:]))
	if len(parts) < 20 {
		return 0, errGitUnit
	}
	parent, err := strconv.Atoi(parts[1])
	start, startErr := strconv.ParseUint(parts[19], 10, 64)
	if err != nil || startErr != nil || parent <= 0 || start != identity.StartTicks || ctx.Err() != nil || gitCheckProcessPIDFD(identity, original) != nil {
		return 0, errGitUnit
	}
	return parent, nil
}

// Bind a worker to the original live unit root through retained, measured
// namespace-helper ancestors. No worker-supplied parent or filter flag enters
// this check. The trusted fixed bootstrap passes only the sealed fixed filter
// to that measured helper; process Seccomp=2 alone is never its provenance.
func (o *gitUnitOwner) checkFactoryLaunch(ctx context.Context, binding gitMaterializationBinding, worker GateIdentity, original int, helper *os.File, pin VMAssetPin) error {
	if ctx == nil || !binding.valid() || helper == nil || !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	r, err := o.factoryUnit(bounded, binding)
	if err != nil || r.BootstrapGrantHash == "" || r.WorkerGrantHash != "" || worker.PID == r.Identity.PID || worker.BootID != r.BootID || worker.InvocationID != r.Identity.InvocationID || verifyNativeBootstrap(bounded, helper, pin) != nil {
		return errGitUnit
	}
	helperID, err := gitStatFileID(helper)
	if err != nil {
		return errGitUnit
	}
	first, err := gitRetainProcessPIDFD(worker, original)
	if err != nil {
		return errGitUnit
	}
	type edge struct {
		identity   GateIdentity
		fd, parent int
	}
	chain := []edge{{identity: worker, fd: first}}
	defer func() {
		for _, item := range chain {
			unix.Close(item.fd)
		}
	}()
	reached := false
	for len(chain) <= 8 {
		index := len(chain) - 1
		item := &chain[index]
		parent, err := gitFactoryParent(bounded, item.identity, item.fd)
		if err != nil {
			return errGitUnit
		}
		item.parent = parent
		unit, invocation, err := o.manager.UnitForPIDFD(bounded, item.fd)
		if err != nil || unit != r.Unit || invocation != r.Identity.InvocationID {
			return errGitUnit
		}
		if parent == r.Identity.PID {
			if len(chain) < 2 {
				return errGitUnit
			}
			reached = true
			break
		}
		if parent <= 1 || len(chain) == 8 {
			return errGitUnit
		}
		for _, prior := range chain {
			if prior.identity.PID == parent {
				return errGitUnit
			}
		}
		identity, err := processGateIdentity(parent, r.Identity.InvocationID)
		if err != nil || identity.BootID != r.BootID {
			return errGitUnit
		}
		fd, err := unix.PidfdOpen(parent, 0)
		if err != nil {
			return errGitUnit
		}
		chain = append(chain, edge{identity: identity, fd: fd})
		executable, err := os.Open(fmt.Sprintf("/proc/%d/exe", parent))
		if err != nil {
			return errGitUnit
		}
		id, statErr := gitStatFileID(executable)
		hash, hashErr := gitBootstrapImageHash(bounded, executable, pin.Bytes)
		executable.Close()
		if statErr != nil || hashErr != nil || id != helperID || hash != pin.SHA256 || gitCheckProcessPIDFD(identity, fd) != nil {
			return errGitUnit
		}
	}
	if !reached {
		return errGitUnit
	}
	for _, item := range chain {
		parent, err := gitFactoryParent(bounded, item.identity, item.fd)
		if err != nil || parent != item.parent {
			return errGitUnit
		}
		if item.identity != worker {
			executable, err := os.Open(fmt.Sprintf("/proc/%d/exe", item.identity.PID))
			if err != nil {
				return errGitUnit
			}
			id, statErr := gitStatFileID(executable)
			executable.Close()
			if statErr != nil || id != helperID {
				return errGitUnit
			}
		}
	}
	after, err := o.factoryUnit(bounded, binding)
	if err != nil || after.Identity != r.Identity || after.Cgroup != r.Cgroup || verifyNativeBootstrap(bounded, helper, pin) != nil || bounded.Err() != nil {
		return errGitUnit
	}
	return nil
}
