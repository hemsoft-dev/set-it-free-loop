//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func vmGrantAssets(t *testing.T) (*VMBootImages, VMFixtureManifest, []*os.File) {
	t.Helper()
	spec := vmSpec(t)
	spec.Binding = authTestBinding()
	images, err := OpenVMBootImages(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { images.Close() })
	m, err := images.FixtureManifest()
	if err != nil {
		t.Fatal(err)
	}
	files, err := images.duplicate()
	if err != nil {
		t.Fatal(err)
	}
	inert, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, inert)
	t.Cleanup(func() { closeVMFiles(files) })
	return images, m, files
}
func vmSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := []*net.UnixConn{}
	for _, fd := range fds {
		file := os.NewFile(uintptr(fd), "vm-pair")
		connection, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		c := connection.(*net.UnixConn)
		c.SetDeadline(time.Now().Add(3 * time.Second))
		t.Cleanup(func() { c.Close() })
		connections = append(connections, c)
	}
	return connections[0], connections[1]
}
func rawVMGrant(t *testing.T, m VMFixtureManifest) []byte {
	t.Helper()
	identity, err := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(vmGateGrant{2, m.Binding, identity, m})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVMGrantActualUnixPeerTransfersSealedDescriptorsAfterDurableGrant(t *testing.T) {
	_, manifest, files := vmGrantAssets(t)
	root, err := os.MkdirTemp("", "sfl-vmg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	gate, err := prepareVMFixtureGate(root, manifest.Binding, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Abort(context.Background())
	identity, err := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	type received struct {
		grant vmGateGrant
		files []*os.File
		err   error
	}
	done := make(chan received, 1)
	helloSent := make(chan struct{})
	go func() {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: gate.Endpoint(), Net: "unix"})
		if err != nil {
			done <- received{err: err}
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		raw, _ := json.Marshal(gateHello{2, manifest.Binding, identity.InvocationID, Digest(manifest)})
		c.Write(raw)
		c.CloseWrite()
		close(helloSent)
		grant, fds, err := receiveVMGrant(c)
		done <- received{grant, fds, err}
	}()
	select {
	case <-helloSent:
	case got := <-done:
		t.Fatal("hello failed", got.err)
	}
	select {
	case <-done:
		t.Fatal("payload arrived before grant")
	default:
	}
	if err := gate.grant(context.Background(), identity, Digest(manifest), 2, rawVMGrant(t, manifest), files); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer closeVMFiles(got.files)
	if !reflect.DeepEqual(got.grant.Manifest, manifest) || got.grant.Identity != identity {
		t.Fatal(got.grant)
	}
	for index, file := range got.files {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("received FD not CLOEXEC", err)
		}
		if index < 3 {
			if err := verifyGrantedVMAsset(context.Background(), file, manifest.Assets[index], index); err != nil {
				t.Fatal(err)
			}
		}
	}
	if verifyGrantedKVM(got.files[3]) == nil {
		t.Fatal("ordinary test inert device accepted as KVM")
	}
	held, err := gate.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := gate.read(held)
	held.close()
	if err != nil || record.Phase != "granted" || record.Protocol != 2 || record.Identity != identity {
		t.Fatal(record, err)
	}
	if _, err := prepareVMFixtureGate(root, manifest.Binding, manifest); err == nil {
		t.Fatal("grant nonce reused")
	}
}

func TestVMGrantFragmentedFramesAndHostileAncillaryData(t *testing.T) {
	_, manifest, files := vmGrantAssets(t)
	raw := rawVMGrant(t, manifest)
	for _, kind := range []string{"fragmented", "missing-fd", "extra-fd", "truncated-rights", "oversized", "bad-json", "duplicate", "wrong-version", "extra-asset"} {
		t.Run(kind, func(t *testing.T) {
			server, client := vmSocketPair(t)
			before, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			body := append([]byte(nil), raw...)
			fds := []int{}
			for _, f := range files {
				fds = append(fds, int(f.Fd()))
			}
			switch kind {
			case "missing-fd":
				fds = fds[:3]
			case "extra-fd":
				fds = append(fds, fds[0])
			case "truncated-rights":
				for len(fds) < 30 {
					fds = append(fds, fds[0])
				}
			case "oversized":
				body = []byte(strings.Repeat("x", 4097))
			case "bad-json":
				body = []byte("null")
			case "duplicate":
				body = append([]byte(`{"Version":2,`), body[1:]...)
			case "wrong-version":
				body = []byte(strings.Replace(string(body), `"Version":2`, `"Version":1`, 1))
			case "extra-asset":
				m := manifest
				m.Assets = append(append([]VMAssetPin{}, m.Assets...), m.Assets[0])
				body = rawVMGrant(t, m)
			}
			sent := make(chan error, 1)
			go func() {
				if kind == "fragmented" {
					half := len(body) / 2
					_, _, err := server.WriteMsgUnix(body[:half], unix.UnixRights(fds[:2]...), nil)
					if err == nil {
						_, _, err = server.WriteMsgUnix(body[half:], unix.UnixRights(fds[2:]...), nil)
					}
					server.CloseWrite()
					sent <- err
					return
				}
				_, _, err := server.WriteMsgUnix(body, unix.UnixRights(fds...), nil)
				server.CloseWrite()
				sent <- err
			}()
			grant, received, err := receiveVMGrant(client)
			if kind == "fragmented" {
				if err != nil || !reflect.DeepEqual(grant.Manifest, manifest) {
					t.Fatal(grant, err)
				}
				closeVMFiles(received)
			} else if err == nil || received != nil {
				closeVMFiles(received)
				t.Fatal("hostile grant accepted", kind, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadDir("/proc/self/fd")
			if err != nil || len(after) != len(before) {
				t.Fatal("rejected grant leaked descriptors", len(before), len(after), err)
			}
		})
	}
}

func TestVMGrantRejectsChangedAssetsAndOrdinaryStubInvocation(t *testing.T) {
	images, manifest, files := vmGrantAssets(t)
	if VMFixtureStub(context.Background(), "/absent/gate.sock", manifest.Binding, strings.Repeat("d", 32), Digest(manifest), os.Getpid()) == nil {
		t.Fatal("ordinary host invocation launched")
	}
	for _, kind := range []string{"pin", "size", "swapped", "unsealed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			pin, file, ctx := manifest.Assets[1], files[1], context.Background()
			switch kind {
			case "pin":
				pin.SHA256 = Digest("wrong")
			case "size":
				pin.Bytes++
			case "swapped":
				file = files[0]
			case "unsealed":
				f, err := os.Open(images.spec.Kernel.Path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				file = f
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if verifyGrantedVMAsset(ctx, file, pin, 1) == nil {
				t.Fatal("invalid received asset accepted")
			}
		})
	}
}

func TestVMFixtureSupervisorSeparatesPayloadRecoveryAndAuthAuthority(t *testing.T) {
	s, manager, _ := systemdSupervisorFixture(t)
	s.payloadKind = "vm-fixture"
	v := &VMFixtureSupervisor{s}
	if _, ok := any(v).(AuthStopVerifier); ok {
		t.Fatal("VM fixture claims auth shutdown authority")
	}
	spec := vmSpec(t)
	binding := authTestBinding()
	binding.LaunchID = strings.Repeat("a", 32)
	spec.Binding = binding
	images, err := OpenVMBootImages(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	manifest, err := images.FixtureManifest()
	if err != nil {
		t.Fatal(err)
	}
	manager.kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + binding.LaunchID + ".service"
	manager.observation.ControlGroup = manager.kernel.group.Path
	if err := v.Prepare(binding, manifest); err != nil {
		t.Fatal(err)
	}
	if err := v.Grant(context.Background(), binding, images); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("unstarted VM was grantable", err)
	}
	if err := s.PrepareFake(binding, FakeGateManifest{1, "inert"}); err == nil {
		t.Fatal("fake method accepted VM supervisor")
	}
	if err := s.StartFake(context.Background(), binding); err == nil {
		t.Fatal("fake start accepted VM supervisor")
	}
	if err := v.Start(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	journal, err := s.snapshotUnit(context.Background(), binding)
	if err != nil || journal.PayloadKind != "vm-fixture" || !journal.Captured {
		t.Fatal(journal, err)
	}
	if expectedFakeObservation(journal, manager.observation) {
		t.Fatal("VM masqueraded as fake-marker payload")
	}
	if s.ConfirmAuthStopped(context.Background(), binding) == nil {
		t.Fatal("VM fixture vouched for auth")
	}
	recovered := newSystemdSupervisor(s.root, s.stub, s.stubHash, manager, s.kernel)
	recovered.payloadKind = "vm-fixture"
	defer recovered.Close()
	if err := (&VMFixtureSupervisor{recovered}).AbortAndWait(context.Background(), binding); err != nil {
		t.Fatal("durable VM stop failed", err)
	}
	journal, err = recovered.snapshotUnit(context.Background(), binding)
	if err != nil || !journal.Aborted || !journal.Stopped || manager.starts != 1 || manager.stops != 1 {
		t.Fatal(journal, err)
	}
	if err := v.Grant(context.Background(), binding, images); err == nil {
		t.Fatal("old supervisor granted recovered VM")
	}
}

func TestContainmentLegacyJournalMigrationIsFakeOnlyAndStrict(t *testing.T) {
	s, _, _ := systemdSupervisorFixture(t)
	binding := authTestBinding()
	for name, field := range map[string]string{"containment.json": "protocol", "unit.json": "PayloadKind"} {
		path := filepath.Join(s.root, binding.LaunchID, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			t.Fatal("fixture journal")
		}
		delete(fields, field)
		legacy, _ := json.Marshal(fields)
		if os.WriteFile(path, legacy, 0600) != nil {
			t.Fatal("write legacy")
		}
	}
	if _, err := s.snapshotUnit(context.Background(), binding); err != nil {
		t.Fatal("legacy fake unit lost recovery", err)
	}
	g := s.gate(binding)
	held, err := g.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := g.read(held)
	held.close()
	if err != nil || record.Protocol != 0 {
		t.Fatal(record, err)
	}
	for _, raw := range []string{`{"protocol":null}`, `{"protocol":0,"protocol":2}`, `null`} {
		if normalized, err := legacyContainmentField([]byte(raw), "protocol", json.RawMessage("0")); err == nil {
			if _, err := DecodeStrict[gateRecord](normalized, 65536); err == nil {
				t.Fatal("bad journal normalized", raw)
			}
		}
	}
	s.payloadKind = "vm-fixture"
	if _, err := s.snapshotUnit(context.Background(), binding); err == nil {
		t.Fatal("legacy fake record became VM")
	}
}

func TestVMGrantReceiverDeadlineClosesWithoutPayload(t *testing.T) {
	_, client := vmSocketPair(t)
	client.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, fds, err := receiveVMGrant(client); err == nil || fds != nil {
		closeVMFiles(fds)
		t.Fatal("partial grant accepted", err)
	}
}
