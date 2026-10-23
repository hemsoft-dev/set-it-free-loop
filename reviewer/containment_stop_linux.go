//go:build linux

package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Terminate targets the captured cgroup instance, never a reusable unit name.
// The anchored original directory is checked before opening cgroup.kill. If
// systemd removes it and reuses its path, this descriptor cannot select the
// replacement. Unknown identity, permission or kernel failures remain pending.
func (k linuxContainmentKernel) Terminate(ctx context.Context, binding cgroupBinding) error {
	if ctx == nil || ctx.Err() != nil || binding.Inode == 0 || binding.Hierarchy == (cgroupHierarchy{}) || !strings.HasPrefix(binding.Path, "/user.slice/") || filepath.Clean(binding.Path) != binding.Path {
		return ErrAuthRecoveryRequired
	}
	root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", binding.Path, binding.Hierarchy)
	if os.IsNotExist(err) {
		return k.Empty(binding)
	}
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer root.Close()
	return k.terminateOpened(ctx, root, binding)
}

func (k linuxContainmentKernel) terminateOpened(ctx context.Context, root *os.Root, binding cgroupBinding) error {
	// The original may empty itself and be removed right after it was opened.
	inode, err := cgroupInode(root)
	if err != nil {
		return cgroupRemovedAfterOpen(err, binding)
	}
	if inode != binding.Inode {
		return ErrAuthRecoveryRequired
	}
	// An already empty RemainAfterExit unit needs no manager mutation. Its name
	// may stay active; only physical termination of this instance is required.
	if k.Empty(binding) == nil {
		return nil
	}
	if err = killOpenedCgroup(ctx, root, binding); err != nil {
		return err
	}
	// SIGKILL delivery is asynchronous. A successful write cannot release auth
	// or source ownership before the original whole subtree is independently empty.
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return ErrAuthRecoveryRequired
		}
		if k.Empty(binding) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrAuthRecoveryRequired
		case <-timer.C:
		}
	}
}

func killOpenedCgroup(ctx context.Context, root *os.Root, binding cgroupBinding) error {
	// An original that emptied itself after the check above may be removed.
	kill, err := root.OpenFile("cgroup.kill", os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return cgroupRemovedAfterOpen(err, binding)
	}
	defer kill.Close()
	if ctx.Err() != nil {
		return ErrAuthRecoveryRequired
	}
	n, err := kill.Write([]byte("1"))
	if err != nil {
		return cgroupRemovedAfterOpen(err, binding)
	}
	if n != 1 {
		return ErrAuthRecoveryRequired
	}
	return nil
}

// The kernel can finish killing before systemd reaps its original main PID.
// Wait only for that same invocation's metadata to settle. Never mutate its name.
func waitForOriginalUnitStop(ctx context.Context, manager containmentManager, kernel containmentKernel, before unitObservation, group cgroupBinding) (unitObservation, error) {
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return unitObservation{}, ErrAuthRecoveryRequired
		}
		current, err := manager.Observe(ctx, before.Unit)
		if err != nil || current.Unit != before.Unit || current.Path != before.Path || current.InvocationID != before.InvocationID || current.Description != before.Description || current.ControlGroup != group.Path && current.ControlGroup != "" {
			return unitObservation{}, ErrAuthRecoveryRequired
		}
		if current.Job.ID == 0 && current.MainPID == 0 && (current.ActiveState == "inactive" || current.ActiveState == "failed" || current.ActiveState == "active") {
			// systemd clears ControlGroup after successful whole-group exit. The
			// retained invocation plus a fresh original kernel proof still applies.
			if kernel.Empty(group) != nil {
				return unitObservation{}, ErrAuthRecoveryRequired
			}
			return current, nil
		}
		select {
		case <-ctx.Done():
			return unitObservation{}, ErrAuthRecoveryRequired
		case <-timer.C:
		}
	}
}
