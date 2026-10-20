//go:build linux

package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type systemdCallFunc func(context.Context, dbus.ObjectPath, string, ...any) ([]any, error)

func (f systemdCallFunc) Call(c context.Context, p dbus.ObjectPath, m string, a ...any) ([]any, error) {
	return f(c, p, m, a...)
}
func TestSystemdFixedStartWireAndNoShell(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	stub := retainedStubExec
	args := []string{stub, "/private/gate.sock", "binding-json", Digest("manifest"), "123"}
	calls := 0
	m := &dbusContainmentManager{jobs: &systemdJobs{completed: map[string]jobResult{}, changed: make(chan struct{})}, calls: systemdCallFunc(func(_ context.Context, p dbus.ObjectPath, method string, a ...any) ([]any, error) {
		calls++
		if p != systemdManagerPath || method != systemdManagerInterface+".StartTransientUnit" || a[0] != unit || a[1] != "fail" {
			t.Fatal("wrong manager target", p, method, a)
		}
		properties := a[2].([]unitProperty)
		if dbus.SignatureOf(properties).String() != "a(sv)" || dbus.SignatureOf(a[3]).String() != "a(sa(sv))" {
			t.Fatal("incorrect D-Bus wire signature")
		}
		values := map[string]any{}
		for _, p := range properties {
			if _, exists := values[p.Name]; exists {
				t.Fatal("duplicate property")
			}
			values[p.Name] = p.Value.Value()
		}
		for k, want := range map[string]any{"Type": "exec", "Restart": "no", "RemainAfterExit": true, "CollectMode": "inactive", "AddRef": true, "KillMode": "control-group", "NoNewPrivileges": true, "MemoryMax": uint64(1 << 30), "TasksMax": uint64(64), "MemorySwapMax": uint64(0), "CPUQuotaPerSecUSec": uint64(1000000)} {
			if values[k] != want {
				t.Fatal("changed fixed restriction", k, values[k])
			}
		}
		execs := values["ExecStart"].([]unitExec)
		bootstrap, _ := trustedStubBootstrap()
		if len(execs) != 1 || execs[0].Path != bootstrap || len(execs[0].Arguments) != 9 || execs[0].Arguments[1] != "-I" || execs[0].Arguments[2] != "-S" || execs[0].Arguments[4] != stubBootstrapCode || execs[0].IgnoreFailure {
			t.Fatal("arbitrary or ignored command")
		}
		return []any{dbus.ObjectPath("/org/freedesktop/systemd1/job/1")}, nil
	})}
	if _, err := m.Start(context.Background(), unit, stub, args, "SFL fake containment "+Digest(authTestBinding()), 17); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	for _, bad := range []string{"sshd.service", "sfl-alpha-../other.service", "sfl-alpha-" + strings.Repeat("b", 32) + ".service;true"} {
		if _, err := m.Start(context.Background(), bad, stub, args, "bad", 17); err == nil {
			t.Fatal("unsafe unit")
		}
	}
	if calls != 1 {
		t.Fatal("invalid config contacted manager")
	}
}
func jobSignal(owner, p, unit, result string) *dbus.Signal {
	return &dbus.Signal{Sender: owner, Path: systemdManagerPath, Body: []any{uint32(1), dbus.ObjectPath(p), unit, result}}
}
func TestSystemdJobResultBeforeReplyAndUnknownFailures(t *testing.T) {
	for _, kind := range []string{"done", "wrong-unit", "failed", "sender", "overflow", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			j := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
			unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
			p := "/org/freedesktop/systemd1/job/1"
			if err := j.begin(unit); err != nil {
				t.Fatal(err)
			}
			if kind != "timeout" {
				s := jobSignal(":1.2", p, unit, "done")
				switch kind {
				case "wrong-unit":
					s.Body[2] = "other.service"
				case "failed":
					s.Body[3] = "failed"
				case "sender":
					s.Sender = ":1.3"
				case "overflow":
					for i := 0; i < 128; i++ {
						j.completed[p+strings.Repeat("x", i+1)] = jobResult{unit, "done"}
					}
				}
				j.DeliverSignal(systemdManagerInterface, "JobRemoved", s)
			}
			if err := j.bind(unit, p); err != nil && kind == "done" {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			err := j.wait(ctx, p, unit)
			if kind == "done" {
				if err != nil || len(j.completed) != 0 {
					t.Fatal("lost early result or leaked cache", err)
				}
			} else if !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("unknown job became success", kind, err)
			}
		})
	}
}
func TestSystemdObserveRequiresTypedIdentityAndPIDFDWire(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	p := dbus.ObjectPath("/org/freedesktop/systemd1/unit/fake")
	props := map[string]dbus.Variant{"InvocationID": dbus.MakeVariant(make([]byte, 16)), "ControlGroup": dbus.MakeVariant("/user.slice/" + unit), "ActiveState": dbus.MakeVariant("active"), "Job": dbus.MakeVariant(unitJob{0, "/"}), "Description": dbus.MakeVariant("fixed"), "Transient": dbus.MakeVariant(true), "CollectMode": dbus.MakeVariant("inactive"), "MainPID": dbus.MakeVariant(uint32(1)), "MemoryMax": dbus.MakeVariant(uint64(1 << 30)), "MemorySwapMax": dbus.MakeVariant(uint64(0)), "TasksMax": dbus.MakeVariant(uint64(64)), "CPUQuotaPerSecUSec": dbus.MakeVariant(uint64(1000000)), "KillMode": dbus.MakeVariant("control-group"), "Restart": dbus.MakeVariant("no"), "RemainAfterExit": dbus.MakeVariant(true), "NoNewPrivileges": dbus.MakeVariant(true)}
	m := &dbusContainmentManager{calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, a ...any) ([]any, error) {
		switch method {
		case systemdManagerInterface + ".GetUnit":
			return []any{p}, nil
		case systemdManagerInterface + ".RefUnit":
			return nil, nil
		case "org.freedesktop.DBus.Properties.GetAll":
			return []any{props}, nil
		case systemdManagerInterface + ".GetUnitByPIDFD":
			if _, ok := a[0].(dbus.UnixFD); !ok {
				t.Fatal("numeric PID substituted")
			}
			return []any{p, unit, make([]byte, 16)}, nil
		}
		return nil, errors.New("unexpected method")
	})}
	if o, err := m.Observe(context.Background(), unit); err != nil || o.MainPID != 1 || o.Unit != unit {
		t.Fatal(o, err)
	}
	if u, i, err := m.UnitForPIDFD(context.Background(), 17); err != nil || u != unit || len(i) != 32 {
		t.Fatal(u, i, err)
	}
	delete(props, "Job")
	if _, err := m.Observe(context.Background(), unit); err == nil {
		t.Fatal("missing field silently defaulted")
	}
}

func TestSystemdUnrelatedJobsDoNotPoisonFutureStartOrStop(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	jobs := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
	flood := func() {
		for i := 0; i < 1000; i++ {
			jobs.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", fmt.Sprintf("/org/freedesktop/systemd1/job/%d", i+100), "unrelated.service", "done"))
		}
	}
	flood()
	calls := 0
	manager := &dbusContainmentManager{jobs: jobs, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, a ...any) ([]any, error) {
		calls++
		flood()
		job := fmt.Sprintf("/org/freedesktop/systemd1/job/%d", calls)
		jobs.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", job, unit, "done"))
		return []any{dbus.ObjectPath(job)}, nil
	})}
	args := []string{retainedStubExec, "endpoint", "binding", "manifest", "123"}
	job, err := manager.Start(context.Background(), unit, retainedStubExec, args, "SFL fake containment "+Digest(authTestBinding()), 17)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.WaitJob(context.Background(), job, unit); err != nil {
		t.Fatal("unrelated traffic poisoned start", err)
	}
	flood()
	job, err = manager.Stop(context.Background(), unit)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.WaitJob(context.Background(), job, unit); err != nil || len(jobs.completed) != 0 || len(jobs.pending) != 0 || len(jobs.waiting) != 0 {
		t.Fatal("unrelated traffic poisoned stop or leaked tracker", err)
	}
}

func TestSystemdAbandonedStartResultDoesNotPreventRecoveryStop(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	jobs := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
	n := 0
	manager := &dbusContainmentManager{jobs: jobs, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, _ string, _ ...any) ([]any, error) {
		n++
		p := fmt.Sprintf("/org/freedesktop/systemd1/job/%d", n)
		jobs.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", p, unit, "done"))
		return []any{dbus.ObjectPath(p)}, nil
	})}
	args := []string{retainedStubExec, "endpoint", "binding", "manifest", "123"}
	if _, err := manager.Start(context.Background(), unit, retainedStubExec, args, "SFL fake containment "+Digest(authTestBinding()), 17); err != nil {
		t.Fatal(err)
	}
	// Simulate cancellation/storage failure before the caller waits on its start.
	p, err := manager.Stop(context.Background(), unit)
	if err != nil {
		t.Fatal("abandoned start prevented stop", err)
	}
	if err = manager.WaitJob(context.Background(), p, unit); err != nil || len(jobs.completed) != 0 || len(jobs.waiting) != 0 {
		t.Fatal("recovery stop or tracker retirement failed", err)
	}
}
