//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type fakeWireManager struct {
	conn                        *dbus.Conn
	mu                          sync.Mutex
	unit                        string
	invocation                  []byte
	starts, stops, refs, pidfds int
}

func (m *fakeWireManager) Subscribe() *dbus.Error { return nil }
func (m *fakeWireManager) RefUnit(unit string) *dbus.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if unit != m.unit {
		return dbus.MakeFailedError(fmt.Errorf("wrong unit"))
	}
	m.refs++
	return nil
}
func (m *fakeWireManager) GetUnit(unit string) (dbus.ObjectPath, *dbus.Error) {
	if unit != m.unit {
		return "", dbus.MakeFailedError(fmt.Errorf("wrong unit"))
	}
	return "/org/freedesktop/systemd1/unit/fake", nil
}
func (m *fakeWireManager) StartTransientUnit(unit, mode string, properties []unitProperty, aux []auxiliaryUnit) (dbus.ObjectPath, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if unit != m.unit || mode != "fail" || len(aux) != 0 {
		return "", dbus.MakeFailedError(fmt.Errorf("wrong start"))
	}
	found := false
	for _, p := range properties {
		if p.Name == "ExtraFileDescriptors" {
			var descriptors []namedUnitFD
			if dbus.Store([]any{p.Value.Value()}, &descriptors) != nil || len(descriptors) != 1 {
				return "", dbus.MakeFailedError(fmt.Errorf("bad image descriptor"))
			}
			for _, descriptor := range descriptors {
				unix.Close(int(descriptor.FD))
			}
		}
		if p.Name == "ExecStart" {
			var calls []unitExec
			bootstrap, _ := trustedStubBootstrap()
			if dbus.Store([]any{p.Value.Value()}, &calls) != nil || len(calls) != 1 || calls[0].Path != bootstrap || len(calls[0].Arguments) != 9 || calls[0].Arguments[4] != stubBootstrapCode || calls[0].IgnoreFailure {
				return "", dbus.MakeFailedError(fmt.Errorf("bad argv"))
			}
			found = true
		}
	}
	if !found {
		return "", dbus.MakeFailedError(fmt.Errorf("missing command"))
	}
	m.starts++
	job := dbus.ObjectPath("/org/freedesktop/systemd1/job/101")
	if err := m.conn.Emit(systemdManagerPath, systemdManagerInterface+".JobRemoved", uint32(101), job, unit, "done"); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return job, nil
}
func (m *fakeWireManager) StopUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if unit != m.unit || mode != "replace" {
		return "", dbus.MakeFailedError(fmt.Errorf("wrong stop"))
	}
	m.stops++
	job := dbus.ObjectPath("/org/freedesktop/systemd1/job/102")
	if err := m.conn.Emit(systemdManagerPath, systemdManagerInterface+".JobRemoved", uint32(102), job, unit, "done"); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return job, nil
}
func (m *fakeWireManager) GetUnitByPIDFD(fd dbus.UnixFD) (dbus.ObjectPath, string, []byte, *dbus.Error) {
	defer unix.Close(int(fd))
	if unix.PidfdSendSignal(int(fd), 0, nil, 0) != nil {
		return "", "", nil, dbus.MakeFailedError(fmt.Errorf("not a live pidfd"))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pidfds++
	return "/org/freedesktop/systemd1/unit/fake", m.unit, m.invocation, nil
}

type fakeWireProperties struct{ manager *fakeWireManager }

func (p fakeWireProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	m := p.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if iface == "org.freedesktop.systemd1.Unit" {
		return map[string]dbus.Variant{"InvocationID": dbus.MakeVariant(m.invocation), "ActiveState": dbus.MakeVariant("active"), "Job": dbus.MakeVariant(unitJob{0, "/"}), "Description": dbus.MakeVariant("SFL fake containment " + Digest(authTestBinding())), "Transient": dbus.MakeVariant(true), "CollectMode": dbus.MakeVariant("inactive")}, nil
	}
	if iface == "org.freedesktop.systemd1.Service" {
		return map[string]dbus.Variant{"ControlGroup": dbus.MakeVariant("/user.slice/" + m.unit), "MainPID": dbus.MakeVariant(uint32(os.Getpid())), "MemoryMax": dbus.MakeVariant(uint64(1 << 30)), "MemorySwapMax": dbus.MakeVariant(uint64(0)), "TasksMax": dbus.MakeVariant(uint64(64)), "CPUQuotaPerSecUSec": dbus.MakeVariant(uint64(1000000)), "KillMode": dbus.MakeVariant("control-group"), "Restart": dbus.MakeVariant("no"), "RemainAfterExit": dbus.MakeVariant(true), "NoNewPrivileges": dbus.MakeVariant(true)}, nil
	}
	return nil, dbus.MakeFailedError(fmt.Errorf("unknown interface"))
}
func TestSystemdActualDBusTransportAgainstPrivateFakeManager(t *testing.T) {
	binary, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("explicit transport fixture needs dbus-daemon")
	}
	root, err := os.MkdirTemp("", "sfl-dbus-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "bus")
	config := filepath.Join(root, "bus.conf")
	xml := `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd"><busconfig><type>session</type><listen>unix:path=` + socket + `</listen><auth>EXTERNAL</auth><policy context="default"><allow user="` + strconv.Itoa(os.Geteuid()) + `"/><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>`
	if err = os.WriteFile(config, []byte(xml), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--nofork", "--nopidfile", "--config-file="+config, "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	defer func() { cmd.Process.Kill(); <-done }()
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	server, err := dbus.Dial(strings.TrimSpace(address))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	closeAuth := context.AfterFunc(ctx, func() { server.Close() })
	defer closeAuth()
	if err = server.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
		t.Fatal(err)
	}
	if err = server.Hello(); err != nil {
		t.Fatal(err)
	}
	invocation, _ := hex.DecodeString(strings.Repeat("d", 32))
	fake := &fakeWireManager{conn: server, unit: "sfl-alpha-" + authTestBinding().LaunchID + ".service", invocation: invocation}
	if err = server.Export(fake, systemdManagerPath, systemdManagerInterface); err != nil {
		t.Fatal(err)
	}
	if err = server.Export(fakeWireProperties{fake}, "/org/freedesktop/systemd1/unit/fake", "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	if reply, err := server.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal(reply, err)
	}
	manager, err := openContainmentManagerAt(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	// The real adapter sends real D-Bus variants/argv and receives an early
	// JobRemoved signal from an isolated fake manager, never the host manager.
	args := []string{retainedStubExec, "/fake/gate.sock", "binding", Digest("manifest"), "123"}
	fdimage, err := unix.MemfdCreate("fake-dbus-image", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fdimage)
	job, err := manager.Start(ctx, fake.unit, args[0], args, "SFL fake containment "+Digest(authTestBinding()), fdimage)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.WaitJob(ctx, job, fake.unit); err != nil {
		t.Fatal(err)
	}
	o, err := manager.Observe(ctx, fake.unit)
	if err != nil || o.Job.ID != 0 || o.TasksMax != 64 || o.InvocationID != strings.Repeat("d", 32) {
		t.Fatal(o, err)
	}
	// Concurrent shutdown polls must retain one unit reference, not one
	// reference for each observation. Exercise the real D-Bus adapter.
	var polls sync.WaitGroup
	for i := 0; i < 8; i++ {
		polls.Add(1)
		go func() {
			defer polls.Done()
			for n := 0; n < 12; n++ {
				if _, err := manager.Observe(ctx, fake.unit); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	polls.Wait()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if unit, inv, err := manager.UnitForPIDFD(ctx, fd); err != nil || unit != fake.unit || inv != strings.Repeat("d", 32) {
		t.Fatal(unit, inv, err)
	}
	fake.mu.Lock()
	if fake.starts != 1 || fake.stops != 0 || fake.refs != 1 || fake.pidfds != 1 {
		t.Fatal("wire counts", fake.starts, fake.stops, fake.refs, fake.pidfds)
	}
	fake.mu.Unlock()
	// Drain the address pipe without leaving an owned helper blocked on output.
	cmd.Process.Kill()
	io.Copy(io.Discard, stdout)
}
