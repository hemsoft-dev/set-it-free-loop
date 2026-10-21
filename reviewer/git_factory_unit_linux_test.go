//go:build linux

package reviewer

import (
	"context"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestGitFactoryUnitRechecksBirthAndLiveOwnership(t *testing.T) {
	f := newGitUnitFixture(t)
	f.prepareStart(t)
	check := func() error {
		if !f.owner.enter(context.Background()) {
			t.Fatal("operation lock")
		}
		defer f.owner.leave()
		_, err := f.owner.factoryUnit(context.Background(), f.binding)
		return err
	}
	if check() != nil {
		t.Fatal("fresh captured fake unit refused")
	}
	for _, mode := range []string{"unit-path", "main-pid", "invocation", "inactive", "memory", "swap", "tasks", "description", "job", "owner", "eligibility", "group"} {
		t.Run(mode, func(t *testing.T) {
			f.manager.mu.Lock()
			before, owner := f.manager.observation, f.manager.owner
			switch mode {
			case "unit-path":
				f.manager.observation.Path = dbus.ObjectPath("/org/freedesktop/systemd1/unit/replacement")
			case "main-pid":
				f.manager.observation.MainPID++
			case "invocation":
				f.manager.observation.InvocationID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			case "inactive":
				f.manager.observation.ActiveState = "inactive"
			case "memory":
				f.manager.observation.MemoryMax++
			case "swap":
				f.manager.observation.MemorySwapMax = 1
			case "tasks":
				f.manager.observation.TasksMax++
			case "description":
				f.manager.observation.Description = "unrelated"
			case "job":
				f.manager.observation.Job.ID = 99
			case "owner":
				f.manager.owner = "replacement manager"
			case "eligibility":
				f.owner.grantEligible[f.binding.Attempt] = false
			case "group":
				f.manager.kernel.changed = true
			}
			f.manager.mu.Unlock()
			if check() == nil {
				t.Fatal("changed live unit qualified")
			}
			f.manager.mu.Lock()
			f.manager.observation, f.manager.owner = before, owner
			f.manager.mu.Unlock()
			f.manager.kernel.changed = false
			f.owner.grantEligible[f.binding.Attempt] = true
		})
	}
	if check() != nil {
		t.Fatal("restored fixture did not requalify")
	}
}
