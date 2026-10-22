//go:build linux

package reviewer

import (
	"context"
	"errors"
	"fmt"
	"github.com/godbus/dbus/v5"
	"strings"
	"testing"
	"time"
)

func TestSystemdReferenceCreatorCancellationDoesNotPoisonOtherObservers(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	m := &dbusContainmentManager{calls: systemdCallFunc(func(ctx context.Context, _ dbus.ObjectPath, _ string, _ ...any) ([]any, error) {
		calls++
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, nil
		}
	})}
	defer m.Close()
	creator, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	unit := "sfl-alpha-" + strings.Repeat("a", 32) + ".service"
	go func() { owner <- m.referenceUnit(creator, unit) }()
	<-entered
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatal("creator did not keep its own cancellation", err)
	}
	waiter, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- m.referenceUnit(waiter, unit) }()
	close(release)
	if err := <-result; err != nil {
		t.Fatal("creator cancellation poisoned another observer", err)
	}
	if m.referenceUnit(context.Background(), unit) != nil || calls != 1 {
		t.Fatal("shared reference was retried", calls)
	}
}
func TestSystemdCloseCancelsAndJoinsIndependentReference(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	m := &dbusContainmentManager{calls: systemdCallFunc(func(ctx context.Context, _ dbus.ObjectPath, _ string, _ ...any) ([]any, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	unit := "sfl-alpha-" + strings.Repeat("b", 32) + ".service"
	go func() { result <- m.referenceUnit(ctx, unit) }()
	<-entered
	cancel()
	<-result
	if m.Close() != nil {
		t.Fatal("independent reference was not drained")
	}
	select {
	case <-finished:
	default:
		t.Fatal("closed while reference still running")
	}
	if m.referenceUnit(context.Background(), unit) == nil {
		t.Fatal("closed reference owner rearmed")
	}
}
func TestSystemdRetireUnitPrunesAbandonedTrackingAndKeepsUnknownUnref(t *testing.T) {
	jobs := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
	calls, closed := 0, 0
	m := &dbusContainmentManager{client: ":1.77", jobs: jobs, close: func() error { closed++; return nil }, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, _ ...any) ([]any, error) {
		calls++
		if strings.HasSuffix(method, ".GetUnit") {
			return []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/test")}, nil
		}
		if strings.HasSuffix(method, ".GetAll") {
			return []any{map[string]dbus.Variant{"Refs": dbus.MakeVariant([]string{":1.77", ":1.other"})}}, nil
		}
		if strings.HasSuffix(method, ".UnrefUnit") {
			return nil, errors.New("lost unref reply")
		}
		return nil, nil
	})}
	defer m.Close()
	unit := "sfl-alpha-" + strings.Repeat("c", 32) + ".service"
	if m.referenceUnit(context.Background(), unit) != nil {
		t.Fatal("reference")
	}
	if jobs.begin(unit) != nil || jobs.bind(unit, "/org/freedesktop/systemd1/job/1") != nil {
		t.Fatal("abandoned start setup")
	}
	jobs.DeliverSignal(systemdManagerInterface, "JobRemoved", jobSignal(":1.2", "/org/freedesktop/systemd1/job/1", unit, "done"))
	if m.RetireUnit(context.Background(), unit) == nil || m.RetireUnit(context.Background(), unit) == nil || calls != 4 {
		t.Fatal("unknown retirement retried or hidden", calls)
	}
	if len(jobs.pending)+len(jobs.waiting)+len(jobs.completed) != 0 {
		t.Fatal("abandoned start tracking retained")
	}
	// More than the admission limit of abandoned entries can retire without
	// dispatching any manager job or consuming connection capacity.
	for n := 0; n < 140; n++ {
		name := fmt.Sprintf("sfl-alpha-%032x.service", n)
		path := fmt.Sprintf("/org/freedesktop/systemd1/job/%d", n+2)
		if jobs.begin(name) != nil || jobs.bind(name, path) != nil || m.RetireUnit(context.Background(), name) != nil {
			t.Fatal("retired jobs exhausted admission", n)
		}
	}
	if calls != 4 {
		t.Fatal("local tracking retirement dispatched another operation", calls)
	}
	if m.Close() != nil || m.RetireUnit(context.Background(), unit) != nil || closed != 1 {
		t.Fatal("closed connection did not settle owned references", closed)
	}
}
func TestPhysicalRecoveryRetiresStartOnlyAfterDurableProof(t *testing.T) {
	for _, kind := range []string{"provider", "git"} {
		t.Run(kind, func(t *testing.T) {
			jobs := &systemdJobs{owner: ":1.2", completed: map[string]jobResult{}, changed: make(chan struct{})}
			var unit, job string
			var stop func() error
			var check func() bool
			var manager *fakeContainmentManager
			if kind == "provider" {
				s, m, _ := systemdSupervisorFixture(t)
				manager = m
				if s.StartFake(context.Background(), authTestBinding()) != nil {
					t.Fatal("start")
				}
				r, e := s.snapshotUnit(context.Background(), authTestBinding())
				if e != nil {
					t.Fatal(e)
				}
				unit, job = r.Unit, r.StartJob
				stop = func() error { return s.AbortAndWait(context.Background(), authTestBinding()) }
				check = func() bool {
					x, e := s.snapshotUnit(context.Background(), authTestBinding())
					return e == nil && x.Stopped && x.Aborted
				}
			}
			if kind == "git" {
				f := newGitUnitFixture(t)
				f.prepareStart(t)
				manager = f.manager
				r, e := f.owner.read(f.binding.Attempt)
				if e != nil {
					t.Fatal(e)
				}
				unit, job = r.Unit, r.StartJob
				stop = func() error { return f.owner.stop(context.Background(), f.binding) }
				check = func() bool { x, e := f.owner.read(f.binding.Attempt); return e == nil && x.Stopped && x.Revoked }
			}
			if jobs.begin(unit) != nil || jobs.bind(unit, job) != nil {
				t.Fatal("abandoned original tracking")
			}
			manager.retireHook = func(actual string) error {
				if actual != unit || !check() {
					return errors.New("retirement before durable original proof")
				}
				jobs.end(actual)
				return nil
			}
			if stop() != nil || len(jobs.pending)+len(jobs.waiting)+len(jobs.completed) != 0 || manager.starts != 1 || manager.stops != 1 {
				t.Fatal("physical recovery retained start or dispatched new job")
			}
		})
	}
}
func TestLegacyUnitLifecycleIsRecoveryOnly(t *testing.T) {
	old := unitObservation{CollectMode: "inactive", ExitType: "main", RemainAfterExit: true}
	current := unitObservation{CollectMode: "inactive-or-failed", ExitType: "cgroup", RemainAfterExit: false}
	if expectedUnitLifecycle(old, false) || !expectedUnitLifecycle(old, true) || !expectedUnitLifecycle(current, false) {
		t.Fatal("legacy policy granted or fresh policy refused")
	}
	for _, bad := range []unitObservation{{CollectMode: "inactive-or-failed", RemainAfterExit: true}, {CollectMode: "inactive", RemainAfterExit: false}} {
		if expectedUnitLifecycle(bad, true) {
			t.Fatal("unknown lifecycle pair accepted")
		}
	}
}

func TestOriginalStoppedGroupCanLoseItsManagerPath(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		if s.StartFake(context.Background(), authTestBinding()) != nil {
			t.Fatal("start")
		}
		m.clearStoppedGroup = true
		if s.AbortAndWait(context.Background(), authTestBinding()) != nil {
			t.Fatal("original empty group rejected after manager path cleared")
		}
	})
	t.Run("git", func(t *testing.T) {
		f := newGitUnitFixture(t)
		f.prepareStart(t)
		f.manager.clearStoppedGroup = true
		if f.owner.stop(context.Background(), f.binding) != nil {
			t.Fatal("original empty group rejected after manager path cleared")
		}
	})
	t.Run("populated", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		if s.StartFake(context.Background(), authTestBinding()) != nil {
			t.Fatal("start")
		}
		m.clearStoppedGroup = true
		m.holdPopulated = true
		if s.AbortAndWait(context.Background(), authTestBinding()) == nil {
			t.Fatal("missing manager path hid a populated original group")
		}
	})
}

func TestRetirementDropsOnlyItsOwnCountedReferences(t *testing.T) {
	for _, initial := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			own, unrefs, other := initial, 0, 3
			m := &dbusContainmentManager{client: ":1.77", jobs: &systemdJobs{completed: map[string]jobResult{}, changed: make(chan struct{})}, calls: systemdCallFunc(func(_ context.Context, _ dbus.ObjectPath, method string, _ ...any) ([]any, error) {
				switch method {
				case systemdManagerInterface + ".RefUnit":
					return nil, nil
				case systemdManagerInterface + ".GetUnit":
					return []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/test")}, nil
				case "org.freedesktop.DBus.Properties.GetAll":
					refs := []string{}
					for n := 0; n < own; n++ {
						refs = append(refs, ":1.77")
					}
					for n := 0; n < other; n++ {
						refs = append(refs, ":1.other")
					}
					return []any{map[string]dbus.Variant{"Refs": dbus.MakeVariant(refs)}}, nil
				case systemdManagerInterface + ".UnrefUnit":
					if own <= 0 {
						return nil, errors.New("unowned reference")
					}
					own--
					unrefs++
					return nil, nil
				default:
					return nil, errors.New("unexpected mutation")
				}
			})}
			defer m.Close()
			unit := "sfl-alpha-" + strings.Repeat("e", 32) + ".service"
			if m.referenceUnit(context.Background(), unit) != nil {
				t.Fatal("reference")
			}
			err := m.RetireUnit(context.Background(), unit)
			if initial > 2 {
				if err == nil || unrefs != 0 || own != initial {
					t.Fatal("unknown reference count mutated", err, unrefs, own)
				}
				return
			}
			if err != nil || m.RetireUnit(context.Background(), unit) != nil || own != 0 || other != 3 || unrefs != initial {
				t.Fatal("owned reference accounting", err, own, other, unrefs)
			}
			if m.referenceUnit(context.Background(), unit) == nil {
				t.Fatal("retired unit re-referenced")
			}
			if _, err := m.startProperties(context.Background(), unit, nil); err == nil {
				t.Fatal("retired unit name restarted")
			}
		})
	}
}
