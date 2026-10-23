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
		for k, want := range map[string]any{"Type": "exec", "Restart": "no", "ExitType": "cgroup", "RemainAfterExit": false, "CollectMode": "inactive-or-failed", "AddRef": true, "KillMode": "control-group", "NoNewPrivileges": true, "MemoryMax": uint64(1 << 30), "TasksMax": uint64(64), "MemorySwapMax": uint64(0), "CPUQuotaPerSecUSec": uint64(1000000)} {
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
	props := map[string]dbus.Variant{"InvocationID": dbus.MakeVariant(make([]byte, 16)), "ControlGroup": dbus.MakeVariant("/user.slice/" + unit), "ActiveState": dbus.MakeVariant("active"), "Job": dbus.MakeVariant(unitJob{0, "/"}), "Description": dbus.MakeVariant("fixed"), "Transient": dbus.MakeVariant(true), "CollectMode": dbus.MakeVariant("inactive-or-failed"), "MainPID": dbus.MakeVariant(uint32(1)), "MemoryMax": dbus.MakeVariant(uint64(1 << 30)), "MemorySwapMax": dbus.MakeVariant(uint64(0)), "TasksMax": dbus.MakeVariant(uint64(64)), "CPUQuotaPerSecUSec": dbus.MakeVariant(uint64(1000000)), "KillMode": dbus.MakeVariant("control-group"), "Restart": dbus.MakeVariant("no"), "ExitType": dbus.MakeVariant("cgroup"), "RemainAfterExit": dbus.MakeVariant(false), "NoNewPrivileges": dbus.MakeVariant(true)}
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

func TestSystemdUnrelatedJobsDoNotPoisonFutureStarts(t *testing.T) {
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
	unit = "sfl-alpha-" + strings.Repeat("c", 32) + ".service"
	job, err = manager.Start(context.Background(), unit, retainedStubExec, args, "SFL fake containment "+Digest(authTestBinding()), 17)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.WaitJob(context.Background(), job, unit); err != nil || len(jobs.completed) != 0 || len(jobs.pending) != 0 || len(jobs.waiting) != 0 {
		t.Fatal("unrelated traffic poisoned next start or leaked tracker", err)
	}
}

func TestSystemdAbandonedStartTrackingCanBeRetiredWithoutStoppingByName(t *testing.T) {
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	jobs := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
	// This tests the job cache only, without starting or mutating any unit.
	if jobs.begin(unit) != nil || jobs.bind(unit, "/org/freedesktop/systemd1/job/1") != nil {
		t.Fatal("initial tracking")
	}
	jobs.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", "/org/freedesktop/systemd1/job/1", unit, "done"))
	// A caller that abandoned its wait can retire that cache before a fresh
	// trusted observation. No recovery path needs a name-targeted stop call.
	if jobs.begin(unit) != nil {
		t.Fatal("abandoned tracking poisoned later observation")
	}
	jobs.end(unit)
	if len(jobs.completed) != 0 || len(jobs.waiting) != 0 || len(jobs.pending) != 0 {
		t.Fatal("abandoned tracking retained")
	}
}

func TestSystemdUnitReferenceDoesNotRetryUnknownReplies(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			calls := 0
			m := &dbusContainmentManager{calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, _ ...any) ([]any, error) {
				if method != systemdManagerInterface+".RefUnit" {
					t.Fatal("unexpected operation", method)
				}
				calls++
				if failed {
					return nil, errors.New("synthetic lost reply after accepted reference")
				}
				return nil, nil
			})}
			unit := "sfl-alpha-" + strings.Repeat("a", 32) + ".service"
			for n := 0; n < 30; n++ {
				if err := m.referenceUnit(context.Background(), unit); (err != nil) != failed {
					t.Fatal("reference outcome changed", err)
				}
			}
			if calls != 1 {
				t.Fatal("reference reacquired", calls)
			}
		})
	}
}

func TestSystemdReferenceWaiterCancellationDoesNotWaitForOtherPoll(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := &dbusContainmentManager{calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, _ string, _ ...any) ([]any, error) {
		close(entered)
		<-release
		return nil, nil
	})}
	unit := "sfl-alpha-0123456789abcdef0123456789abcdef.service"
	owner := make(chan error, 1)
	go func() { owner <- m.referenceUnit(context.Background(), unit) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() { waiter <- m.referenceUnit(ctx, unit) }()
	var got error
	blocked := false
	select {
	case got = <-waiter:
	case <-time.After(200 * time.Millisecond):
		blocked = true
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if blocked {
		got = <-waiter
	}
	if err := m.referenceUnit(context.Background(), unit); err != nil {
		t.Fatal("waiter cancellation poisoned owner result", err)
	}
	if blocked || got != context.DeadlineExceeded {
		t.Fatalf("canceled waiter blocked=%v outcome=%v", blocked, got)
	}
}

// settled correlates a job-free first invocation only with our own completed
// job. Any other job removed on the unit refuses the correlation without
// failing WaitJob, and settled never consumes the cached result.
func TestSystemdJobSettledRequiresOwnDoneJobAndNoForeignJob(t *testing.T) {
	if (&dbusContainmentManager{}).JobSettled("/org/freedesktop/systemd1/job/1", "sfl-alpha-"+strings.Repeat("b", 32)+".service") == nil {
		t.Fatal("settled correlation without a job tracker")
	}
	for _, kind := range []string{"done", "done-before-bind", "unsettled", "failed", "foreign-after-bind", "foreign-before-bind", "other-unit"} {
		t.Run(kind, func(t *testing.T) {
			j := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
			unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
			p, foreign := "/org/freedesktop/systemd1/job/1", "/org/freedesktop/systemd1/job/9"
			deliver := func(job, u, result string) {
				j.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", job, u, result))
			}
			if j.begin(unit) != nil {
				t.Fatal("begin")
			}
			switch kind {
			case "done-before-bind":
				deliver(p, unit, "done")
			case "foreign-before-bind":
				deliver(foreign, unit, "done")
			}
			if j.bind(unit, p) != nil {
				t.Fatal("bind")
			}
			switch kind {
			case "done", "foreign-before-bind":
				deliver(p, unit, "done")
			case "failed":
				deliver(p, unit, "failed")
			case "foreign-after-bind":
				deliver(p, unit, "done")
				deliver(foreign, unit, "done")
			case "other-unit":
				deliver(p, unit, "done")
				deliver(foreign, "sfl-alpha-"+strings.Repeat("c", 32)+".service", "done")
			}
			err := j.settled(p, unit)
			want := kind == "done" || kind == "done-before-bind" || kind == "other-unit"
			if (err == nil) != want {
				t.Fatal("settled correlation", kind, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			waitErr := j.wait(ctx, p, unit)
			if (waitErr == nil) != (kind != "unsettled" && kind != "failed") {
				t.Fatal("settled changed WaitJob", kind, waitErr)
			}
			if len(j.completed) != 0 || len(j.waiting) != 0 || len(j.foreign) != 0 {
				t.Fatal("tracker leaked", j.completed, j.waiting, j.foreign)
			}
		})
	}
}
