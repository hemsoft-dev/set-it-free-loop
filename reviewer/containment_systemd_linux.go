//go:build linux

package reviewer

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const systemdManagerPath dbus.ObjectPath = "/org/freedesktop/systemd1"
const systemdManagerInterface = "org.freedesktop.systemd1.Manager"

type unitProperty struct {
	Name  string
	Value dbus.Variant
}
type unitExec struct {
	Path          string
	Arguments     []string
	IgnoreFailure bool
}
type auxiliaryUnit struct {
	Name       string
	Properties []unitProperty
}
type unitJob struct {
	ID   uint32
	Path dbus.ObjectPath
}

type unitObservation struct {
	Unit                                                   string
	Path                                                   dbus.ObjectPath
	InvocationID                                           string
	MainPID                                                uint32
	ControlGroup                                           string
	ActiveState                                            string
	Job                                                    unitJob
	MemoryMax, MemorySwapMax, TasksMax, CPUQuotaPerSecUSec uint64
	KillMode, Restart, CollectMode                         string
	RemainAfterExit, NoNewPrivileges, Transient            bool
	Description                                            string
}

type containmentManager interface {
	Owner() string
	Start(context.Context, string, string, []string, string) (string, error)
	WaitJob(context.Context, string, string) error
	Observe(context.Context, string) (unitObservation, error)
	UnitForPIDFD(context.Context, int) (string, string, error)
	Stop(context.Context, string) (string, error)
	Close() error
}

type systemdCalls interface {
	Call(context.Context, dbus.ObjectPath, string, ...any) ([]any, error)
}

type ownedSystemdCalls struct {
	conn  *dbus.Conn
	owner string
}

func (b ownedSystemdCalls) Call(ctx context.Context, p dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := b.conn.Object(b.owner, p).CallWithContext(ctx, method, 0, args...)
	return call.Body, call.Err
}

type systemdJobs struct {
	mu        sync.Mutex
	owner     string
	completed map[string]jobResult
	changed   chan struct{}
	invalid   bool
}
type jobResult struct{ Unit, Result string }

func (j *systemdJobs) DeliverSignal(iface, name string, s *dbus.Signal) {
	if iface != systemdManagerInterface || name != "JobRemoved" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if s == nil || s.Sender != j.owner || s.Path != systemdManagerPath || len(s.Body) != 4 {
		j.invalid = true
		j.notify()
		return
	}
	var id uint32
	var p dbus.ObjectPath
	var unit, result string
	if dbus.Store(s.Body, &id, &p, &unit, &result) != nil || id == 0 || !p.IsValid() {
		j.invalid = true
		j.notify()
		return
	}
	// Bounded storage, no deferred goroutines or unbounded signal queue. On
	// overflow the caller retains recovery state instead of inventing job success.
	if old, ok := j.completed[string(p)]; ok && old != (jobResult{unit, result}) {
		j.invalid = true
	}
	if len(j.completed) >= 128 {
		j.invalid = true
	} else {
		j.completed[string(p)] = jobResult{unit, result}
	}
	j.notify()
}
func (j *systemdJobs) notify() { close(j.changed); j.changed = make(chan struct{}) }
func (j *systemdJobs) wait(ctx context.Context, p, unit string) error {
	if !dbus.ObjectPath(p).IsValid() || !strings.HasPrefix(p, "/org/freedesktop/systemd1/job/") {
		return ErrAuthRecoveryRequired
	}
	for {
		j.mu.Lock()
		if j.invalid {
			j.mu.Unlock()
			return ErrAuthRecoveryRequired
		}
		result, ok := j.completed[p]
		if ok {
			delete(j.completed, p)
		}
		changed := j.changed
		j.mu.Unlock()
		if ok {
			if result.Unit == unit && result.Result == "done" {
				return nil
			}
			return ErrAuthRecoveryRequired
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrAuthRecoveryRequired, ctx.Err())
		case <-changed:
		}
	}
}

type dbusContainmentManager struct {
	calls systemdCalls
	owner string
	jobs  *systemdJobs
	close func() error
}

func (m *dbusContainmentManager) Owner() string { return m.owner }
func (m *dbusContainmentManager) Close() error {
	if m.close != nil {
		return m.close()
	}
	return nil
}

// openUserContainmentManager uses a fixed local user bus and EXTERNAL UID only.
// It never reads HOME cookies, spawns dbus-launch, or selects a system manager.
// No unit is created by opening the transport. Every operation targets the
// captured unique owner, so manager replacement cannot redirect a launch/stop.
func openUserContainmentManager(ctx context.Context) (*dbusContainmentManager, error) {
	return openContainmentManagerAt(ctx, "/run/user/"+strconv.Itoa(os.Geteuid())+"/bus")
}

// Internal address seam exists for the private fake-bus transport fixture only.
// Production construction always supplies the fixed user bus above.
func openContainmentManagerAt(ctx context.Context, socket string) (*dbusContainmentManager, error) {
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, ErrAuthRecoveryRequired
	}
	jobs := &systemdJobs{completed: map[string]jobResult{}, changed: make(chan struct{})}
	conn, err := dbus.Dial("unix:path="+socket, dbus.WithSignalHandler(jobs))
	if err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	if err = conn.Hello(); err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	if !conn.SupportsUnixFDs() {
		return nil, ErrAuthRecoveryRequired
	}
	var owner string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.freedesktop.systemd1").Store(&owner); err != nil || !strings.HasPrefix(owner, ":") {
		return nil, ErrAuthRecoveryRequired
	}
	var busID string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&busID); err != nil || len(busID) != 32 {
		return nil, ErrAuthRecoveryRequired
	}
	if decoded, err := hex.DecodeString(busID); err != nil || len(decoded) != 16 {
		return nil, ErrAuthRecoveryRequired
	}
	jobs.mu.Lock()
	jobs.owner = owner
	jobs.mu.Unlock()
	if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(owner), dbus.WithMatchInterface(systemdManagerInterface), dbus.WithMatchMember("JobRemoved"), dbus.WithMatchObjectPath(systemdManagerPath)); err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	calls := ownedSystemdCalls{conn, owner}
	if _, err = calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".Subscribe"); err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	closeOnFailure = false
	return &dbusContainmentManager{calls: calls, owner: busID + "/" + owner, jobs: jobs, close: conn.Close}, nil
}

func validContainmentUnit(unit string) bool {
	if len(unit) != len("sfl-alpha-")+32+len(".service") || !strings.HasPrefix(unit, "sfl-alpha-") || !strings.HasSuffix(unit, ".service") {
		return false
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(unit, "sfl-alpha-"), ".service")
	raw, err := hex.DecodeString(nonce)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == nonce
}
func prop(name string, value any) unitProperty { return unitProperty{name, dbus.MakeVariant(value)} }
func fixedFakeUnitProperties(unit, stub string, args []string, description string) ([]unitProperty, error) {
	if !validContainmentUnit(unit) || !path.IsAbs(stub) || path.Clean(stub) != stub || len(args) != 5 || args[0] != stub || len(description) != len("SFL fake containment ")+64 || !strings.HasPrefix(description, "SFL fake containment ") {
		return nil, ErrAuthRecoveryRequired
	}
	return []unitProperty{
		prop("Description", description), prop("Type", "exec"), prop("ExecStart", []unitExec{{stub, args, false}}),
		prop("Restart", "no"), prop("RemainAfterExit", true), prop("CollectMode", "inactive"), prop("AddRef", true),
		prop("KillMode", "control-group"), prop("SendSIGKILL", true), prop("NoNewPrivileges", true),
		prop("MemoryAccounting", true), prop("MemoryMax", uint64(1<<30)), prop("MemorySwapMax", uint64(0)),
		prop("TasksAccounting", true), prop("TasksMax", uint64(64)), prop("CPUAccounting", true), prop("CPUQuotaPerSecUSec", uint64(1000000)),
		prop("TimeoutStartUSec", uint64(5000000)), prop("TimeoutStopUSec", uint64(5000000)), prop("RuntimeMaxUSec", uint64(120000000)),
		prop("StandardInput", "null"), prop("StandardOutput", "null"), prop("StandardError", "null"),
	}, nil
}
func (m *dbusContainmentManager) Start(ctx context.Context, unit, stub string, args []string, description string) (string, error) {
	properties, err := fixedFakeUnitProperties(unit, stub, args, description)
	if err != nil {
		return "", err
	}
	body, err := m.calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".StartTransientUnit", unit, "fail", properties, []auxiliaryUnit{})
	if err != nil {
		return "", errors.Join(ErrAuthRecoveryRequired, err)
	}
	var job dbus.ObjectPath
	if dbus.Store(body, &job) != nil || !job.IsValid() || !strings.HasPrefix(string(job), "/org/freedesktop/systemd1/job/") {
		return "", ErrAuthRecoveryRequired
	}
	return string(job), nil
}
func (m *dbusContainmentManager) WaitJob(ctx context.Context, job, unit string) error {
	return m.jobs.wait(ctx, job, unit)
}
func (m *dbusContainmentManager) Stop(ctx context.Context, unit string) (string, error) {
	if !validContainmentUnit(unit) {
		return "", ErrAuthRecoveryRequired
	}
	body, err := m.calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".StopUnit", unit, "replace")
	if err != nil {
		return "", errors.Join(ErrAuthRecoveryRequired, err)
	}
	var job dbus.ObjectPath
	if dbus.Store(body, &job) != nil || !job.IsValid() || !strings.HasPrefix(string(job), "/org/freedesktop/systemd1/job/") {
		return "", ErrAuthRecoveryRequired
	}
	return string(job), nil
}
func (m *dbusContainmentManager) UnitForPIDFD(ctx context.Context, fd int) (string, string, error) {
	body, err := m.calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".GetUnitByPIDFD", dbus.UnixFD(fd))
	if err != nil {
		return "", "", ErrAuthRecoveryRequired
	}
	var p dbus.ObjectPath
	var unit string
	var invocation []byte
	if dbus.Store(body, &p, &unit, &invocation) != nil || !p.IsValid() || !validContainmentUnit(unit) || len(invocation) != 16 {
		return "", "", ErrAuthRecoveryRequired
	}
	return unit, hex.EncodeToString(invocation), nil
}
func (m *dbusContainmentManager) properties(ctx context.Context, p dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	body, err := m.calls.Call(ctx, p, "org.freedesktop.DBus.Properties.GetAll", iface)
	if err != nil {
		return nil, err
	}
	var values map[string]dbus.Variant
	if dbus.Store(body, &values) != nil || len(values) > 512 {
		return nil, ErrAuthRecoveryRequired
	}
	return values, nil
}
func (m *dbusContainmentManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	var o unitObservation
	o.Unit = unit
	if !validContainmentUnit(unit) {
		return o, ErrAuthRecoveryRequired
	}
	body, err := m.calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".GetUnit", unit)
	if err != nil || dbus.Store(body, &o.Path) != nil || !o.Path.IsValid() {
		return o, ErrAuthRecoveryRequired
	}
	// Recovery acquires its own reference before stop so GC cannot erase the
	// exact unit's final identity between JobRemoved and post-stop observation.
	if _, err = m.calls.Call(ctx, systemdManagerPath, systemdManagerInterface+".RefUnit", unit); err != nil {
		return o, ErrAuthRecoveryRequired
	}
	u, err := m.properties(ctx, o.Path, "org.freedesktop.systemd1.Unit")
	if err != nil {
		return o, ErrAuthRecoveryRequired
	}
	s, err := m.properties(ctx, o.Path, "org.freedesktop.systemd1.Service")
	if err != nil {
		return o, ErrAuthRecoveryRequired
	}
	var invocation []byte
	reads := []struct {
		m   map[string]dbus.Variant
		key string
		dst any
	}{
		{u, "InvocationID", &invocation}, {u, "ControlGroup", &o.ControlGroup}, {u, "ActiveState", &o.ActiveState}, {u, "Job", &o.Job}, {u, "Description", &o.Description}, {u, "Transient", &o.Transient}, {u, "CollectMode", &o.CollectMode},
		{s, "MainPID", &o.MainPID}, {s, "MemoryMax", &o.MemoryMax}, {s, "MemorySwapMax", &o.MemorySwapMax}, {s, "TasksMax", &o.TasksMax}, {s, "CPUQuotaPerSecUSec", &o.CPUQuotaPerSecUSec},
		{s, "KillMode", &o.KillMode}, {s, "Restart", &o.Restart}, {s, "RemainAfterExit", &o.RemainAfterExit}, {s, "NoNewPrivileges", &o.NoNewPrivileges},
	}
	for _, r := range reads {
		v, ok := r.m[r.key]
		if !ok || dbus.Store([]any{v.Value()}, r.dst) != nil {
			return o, ErrAuthRecoveryRequired
		}
	}
	if len(invocation) != 16 {
		return o, ErrAuthRecoveryRequired
	}
	o.InvocationID = hex.EncodeToString(invocation)
	return o, nil
}

func boundedManagerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}
func currentBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(raw))
	if len(v) != 36 {
		return "", ErrAuthRecoveryRequired
	}
	return v, nil
}
func unitProcessIdentity(ctx context.Context, m containmentManager, o unitObservation) (GateIdentity, error) {
	if o.MainPID == 0 {
		return GateIdentity{}, ErrAuthRecoveryRequired
	}
	fd, err := unix.PidfdOpen(int(o.MainPID), 0)
	if err != nil {
		return GateIdentity{}, ErrAuthRecoveryRequired
	}
	defer unix.Close(fd)
	unit, invocation, err := m.UnitForPIDFD(ctx, fd)
	if err != nil || unit != o.Unit || invocation != o.InvocationID {
		return GateIdentity{}, ErrAuthRecoveryRequired
	}
	identity, err := processGateIdentity(int(o.MainPID), o.InvocationID)
	if err != nil || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return GateIdentity{}, ErrAuthRecoveryRequired
	}
	return identity, nil
}
