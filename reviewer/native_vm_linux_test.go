//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeFirmwareExecChild(t *testing.T) {
	if os.Getenv("SFL_NATIVE_BIOS_TEST") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "-bios" && index+1 < len(os.Args) {
			file, err := os.Open(os.Args[index+1])
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			raw, err := io.ReadAll(io.LimitReader(file, 1025))
			if err != nil || len(raw) > 1024 || authHash(raw) != os.Getenv("SFL_NATIVE_BIOS_HASH") {
				t.Fatal("boot argv did not read the retained pinned BIOS")
			}
			return
		}
	}
	t.Fatal("BIOS argument absent")
}

func TestNativeFirmwareExecUsesSealedBytesAfterSourceReplacement(t *testing.T) {
	spec := vmSpec(t)
	s := testSession(Codex)
	spec.Binding.SessionHash = Digest(s)
	firmware := vmAsset(t, filepath.Join(t.TempDir(), "bios"), []byte("synthetic firmware pin"))
	images, err := OpenNativeVMBootImages(context.Background(), spec, firmware, nativeTestRuntimeSpec(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	manifest, err := images.NativeManifest(s)
	if err != nil || len(manifest.Assets) != 6 || manifest.Assets[3].SHA256 != firmware.SHA256 {
		t.Fatal("firmware absent from native manifest", err)
	}
	if os.Chmod(firmware.Path, 0700) != nil || os.WriteFile(firmware.Path, []byte("replacement ambient firmware"), 0600) != nil {
		t.Fatal("firmware replacement fixture failed")
	}
	sealed, err := images.duplicateFirmware()
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	if verifyGrantedVMAsset(context.Background(), sealed, manifest.Assets[3], 3) != nil {
		t.Fatal("source replacement changed retained firmware")
	}
	inert, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer inert.Close()
	args := append([]string{"-test.run=^TestNativeFirmwareExecChild$", "--"}, nativeVMArgs(manifest.Binding.LaunchID)...)
	command := exec.Command(os.Args[0], args...)
	command.Env = []string{"SFL_NATIVE_BIOS_TEST=1", "SFL_NATIVE_BIOS_HASH=" + firmware.SHA256}
	command.ExtraFiles = []*os.File{inert, inert, inert, inert, inert, inert, inert, sealed}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("firmware argv probe failed: %s %v", output, err)
	}
}

func openNativeBootTestImages(t *testing.T, ctx context.Context, spec VMBootSpec) (*VMBootImages, error) {
	t.Helper()
	firmware := vmAsset(t, filepath.Join(t.TempDir(), "bios"), []byte("synthetic pinned BIOS"))
	return OpenNativeVMBootImages(ctx, spec, firmware, nativeTestRuntimeSpec(t, spec))
}

func TestNativeVMConcreteKernelLimitsCannotUseLegacyOrMixedProfile(t *testing.T) {
	for _, profile := range []struct {
		memory, tasks  string
		native, legacy bool
	}{
		{"3221225472", "128", true, false},
		{"1073741824", "64", false, true},
		{"3221225472", "64", false, false},
		{"1073741824", "128", false, false},
	} {
		dir := t.TempDir()
		for name, value := range map[string]string{"memory.max": profile.memory, "memory.swap.max": "0", "pids.max": profile.tasks, "cpu.max": "100000 100000"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if (verifyContainmentLimits(root, "3221225472", "128") == nil) != profile.native || (verifyContainmentLimits(root, "1073741824", "64") == nil) != profile.legacy {
			t.Fatal("concrete payload limit profiles confused", profile)
		}
		root.Close()
	}
}

func TestNativeVMManifestRejectsFixtureAndForeignSession(t *testing.T) {
	s := testSession(Codex)
	spec := vmSpec(t)
	spec.Binding.SessionHash = Digest(s)
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	m, err := images.NativeManifest(s)
	if err != nil || !m.valid() || m.Purpose != "native-review" {
		t.Fatal("native manifest unavailable", err)
	}
	unpinned := m
	unpinned.Version = 1
	unpinned.Assets = unpinned.Assets[:3]
	if unpinned.valid() {
		t.Fatal("legacy unpinned BIOS manifest accepted")
	}
	fixture := m
	fixture.Purpose = "synthetic-role-fixture"
	if fixture.valid() {
		t.Fatal("fixture purpose promoted to native")
	}
	foreign := s
	foreign.Round++
	if _, err := images.NativeManifest(foreign); err == nil {
		t.Fatal("foreign session reuses pinned boot binding")
	}
	changed := m
	changed.SessionHash = Digest("another-session")
	if changed.valid() {
		t.Fatal("binding/session mismatch accepted")
	}
	images.Close()
	if _, err := images.NativeManifest(s); err == nil {
		t.Fatal("closed images reused")
	}
}

func TestNativeVMChannelsActualSocketsSeparateAndOneUse(t *testing.T) {
	c, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	files, err := c.duplicateGuest()
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(files)
	if _, err := c.duplicateGuest(); err == nil {
		t.Fatal("guest descriptors exported twice")
	}
	for index, file := range files {
		conn, err := net.FileConn(file)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		c.host[index].SetDeadline(time.Now().Add(time.Second))
		want := []byte{byte(index + 1)}
		if n, err := conn.Write(want); err != nil || n != 1 {
			t.Fatal(err)
		}
		var got [1]byte
		if _, err := io.ReadFull(c.host[index], got[:]); err != nil || got[0] != want[0] {
			t.Fatal("channel crossed descriptor positions", index, err)
		}
		if _, err := c.host[index].Write([]byte{9}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, got[:]); err != nil || got[0] != 9 {
			t.Fatal(err)
		}
		conn.Close()
		file.Close()
		// The original guest end was closed when its one-use duplicate was
		// exported, so child closure must produce EOF rather than a self-held peer.
		if n, err := c.host[index].Read(got[:]); n != 0 || err != io.EOF {
			t.Fatal("retained guest duplicate hides EOF", index, n, err)
		}
	}
	c.Close()
	if _, err := c.duplicateGuest(); err == nil {
		t.Fatal("closed channels duplicated")
	}
}

func TestNativeVMArgvExposesOnlyThreePrivateDescriptorPorts(t *testing.T) {
	args := nativeVMArgs(strings.Repeat("1", 32))
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"tcg", "-netdev", "-virtfs", "-fsdev", "-drive", "stdio", "guest-agent", "-monitor stdio"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("unapproved VM capability", forbidden)
		}
	}
	for _, required := range []string{"-nic none", "-monitor none", "-serial chardev:sflbootdiag", "socket,id=sflbootdiag,fd=13", "kvm,device=/dev/fdset/1", "-m 2048M", "rdinit=/native-init", "initramfs_options=size=1024m,nr_inodes=4096", "socket,id=sflcontrol,fd=7", "socket,id=sflobjects,fd=8", "socket,id=sflprovider,fd=9", "name=sfl.control", "name=sfl.objects", "name=sfl.provider"} {
		if !strings.Contains(joined, required) {
			t.Fatal("missing fixed VM boundary", required)
		}
	}
}

func TestNativeVMGrantRejectsWrongPurposeWithoutOpeningKVM(t *testing.T) {
	// This test uses only sealed inert assets and no host process/unit/device.
	s := testSession(Codex)
	spec := vmSpec(t)
	spec.Binding.SessionHash = Digest(s)
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	m, err := images.NativeManifest(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g := &ContainmentGate{binding: m.Binding}
	bad := m
	bad.Purpose = "synthetic-role-fixture"
	if g.grantNativeVM(context.Background(), GateIdentity{}, bad, images, c) == nil {
		t.Fatal("fixture grant accepted")
	}
	bad = m
	bad.Assets = append([]VMAssetPin{}, m.Assets...)
	bad.Assets[3].SHA256 = strings.Repeat("f", 64)
	if g.grantNativeVM(context.Background(), GateIdentity{}, bad, images, c) == nil {
		t.Fatal("mismatched retained BIOS accepted before KVM/channel access")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.grantNativeVM(ctx, GateIdentity{}, m, images, c) == nil {
		t.Fatal("cancelled grant accepted")
	}
	if c.guestSpent {
		t.Fatal("invalid grant consumed private channels")
	}
	// Descriptor ownership remains intact after pre-grant failures.
	for _, file := range c.guest {
		if _, err := file.Stat(); err != nil {
			t.Fatal("pre-grant rejection closed owned channel", err)
		}
	}
}

func nativeGrantFixture(t *testing.T) (NativeVMManifest, []*os.File, *nativeVMChannels) {
	t.Helper()
	s := testSession(Codex)
	spec := vmSpec(t)
	spec.Binding.SessionHash = Digest(s)
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { images.Close() })
	m, err := images.NativeManifest(s)
	if err != nil {
		t.Fatal(err)
	}
	files, err := images.duplicate()
	if err != nil {
		t.Fatal(err)
	}
	inert, err := os.Open(os.DevNull)
	if err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	files = append(files, inert)
	channels, err := openNativeVMChannels()
	if err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	t.Cleanup(func() { channels.Close() })
	private, err := channels.duplicateGuest()
	if err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	files = append(files, private...)
	firmware, err := images.duplicateFirmware()
	if err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	files = append(files, firmware)
	runtimeFiles, err := images.duplicateNativeRuntime(context.Background())
	if err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	files = append(files, runtimeFiles...)
	t.Cleanup(func() { closeVMFiles(files) })
	return m, files, channels
}

func TestNativeVMActualAuthenticatedTenDescriptorGrant(t *testing.T) {
	m, files, channels := nativeGrantFixture(t)
	root, err := os.MkdirTemp("", "sflnat-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	gate, err := prepareContainmentGateHash(root, m.Binding, Digest(m), nativeVMGrantProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Abort(context.Background())
	identity, err := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(nativeVMGrant{nativeVMGrantProtocol, m.Binding, identity, m})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: gate.Endpoint(), Net: "unix"})
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		hello, _ := json.Marshal(gateHello{nativeVMGrantProtocol, m.Binding, identity.InvocationID, Digest(m)})
		if _, err = conn.Write(hello); err != nil {
			done <- err
			return
		}
		conn.CloseWrite()
		grant, received, err := receiveNativeVMGrant(conn)
		if err != nil {
			done <- err
			return
		}
		defer closeVMFiles(received)
		if Digest(grant.Manifest) != Digest(m) || len(received) != nativeVMDescriptorCount {
			done <- ErrVMBoot
			return
		}
		for index, file := range received {
			flags, e := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
			if e != nil || flags&unix.FD_CLOEXEC == 0 {
				done <- ErrVMBoot
				return
			}
			if index < 3 {
				if verifyGrantedVMAsset(context.Background(), file, m.Assets[index], index) != nil {
					done <- ErrVMBoot
					return
				}
			}
			if index >= 4 && index < 7 {
				if nativeVMStream(file) != nil {
					done <- ErrVMBoot
					return
				}
			}
		}
		if _, err = received[4].Write([]byte("private")); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = gate.grant(ctx, identity, Digest(m), nativeVMGrantProtocol, raw, files); err != nil {
		t.Fatal(err)
	}
	channels.host[0].SetDeadline(time.Now().Add(2 * time.Second))
	var got [7]byte
	if _, err = io.ReadFull(channels.host[0], got[:]); err != nil || string(got[:]) != "private" {
		t.Fatal("control descriptor changed", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	held, err := gate.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	record, err := gate.read(held)
	held.close()
	if err != nil || record.Phase != "granted" || record.Protocol != nativeVMGrantProtocol {
		t.Fatal("native authorization not durable", record, err)
	}
}

func TestNativeVMReceiverRefusesFixturePurposeAndDescriptorCounts(t *testing.T) {
	for _, kind := range []string{"fixture", "version", "missing", "extra"} {
		t.Run(kind, func(t *testing.T) {
			m, files, _ := nativeGrantFixture(t)
			grant := nativeVMGrant{Version: nativeVMGrantProtocol, Binding: m.Binding, Manifest: m}
			if kind == "fixture" {
				grant.Manifest.Purpose = "synthetic-role-fixture"
			}
			if kind == "version" {
				grant.Version = 2
			}
			if kind == "missing" {
				files = files[:6]
			}
			if kind == "extra" {
				files = append(append([]*os.File{}, files...), files[0])
			}
			sender, receiver := vmSocketPair(t)
			raw, err := json.Marshal(grant)
			if err != nil {
				t.Fatal(err)
			}
			fds := make([]int, len(files))
			for i, f := range files {
				fds[i] = int(f.Fd())
			}
			if _, _, err = sender.WriteMsgUnix(raw, unix.UnixRights(fds...), nil); err != nil {
				t.Fatal(err)
			}
			sender.CloseWrite()
			_, received, err := receiveNativeVMGrant(receiver)
			if !errors.Is(err, ErrContainmentGate) || len(received) != 0 {
				closeVMFiles(received)
				t.Fatal("unrelated authority accepted", kind, err)
			}
		})
	}
}

func TestNativeVMSupervisorRequiresNativeRecoveryAndResourceProfile(t *testing.T) {
	s, manager, _ := systemdSupervisorFixture(t)
	s.payloadKind = "native-vm"
	v := &NativeVMSupervisor{s}
	session := testSession(Codex)
	spec := vmSpec(t)
	binding := authTestBinding()
	binding.SessionHash = Digest(session)
	binding.LaunchID = strings.Repeat("a", 32)
	spec.Binding = binding
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	m, err := images.NativeManifest(session)
	if err != nil {
		t.Fatal(err)
	}
	manager.observation.MemoryMax = 3 << 30
	manager.observation.TasksMax = 128
	manager.kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + binding.LaunchID + ".service"
	manager.observation.ControlGroup = manager.kernel.group.Path
	if err = v.Prepare(m); err != nil {
		t.Fatal(err)
	}
	if err = s.StartFake(context.Background(), binding); err == nil {
		t.Fatal("fake API launched native payload")
	}
	if err = v.Start(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	journal, err := s.snapshotUnit(context.Background(), binding)
	if err != nil || !journal.Captured || journal.PayloadKind != "native-vm" {
		t.Fatal(journal, err)
	}
	weaker := manager.observation
	weaker.MemoryMax = 1 << 30
	weaker.TasksMax = 64
	if expectedPayloadObservation(journal, weaker) {
		t.Fatal("fixture resource profile vouched for native VM")
	}
	// Fresh recovery owns the durable whole-unit stop. No QEMU, KVM, provider,
	// host service or credential is started by the fake manager in this test.
	recovered := newSystemdSupervisor(s.root, s.stub, s.stubHash, manager, s.kernel)
	recovered.payloadKind = "native-vm"
	defer recovered.Close()
	if err = (&NativeVMSupervisor{recovered}).ConfirmAuthStopped(context.Background(), binding); err != nil {
		t.Fatal("fresh native stop failed", err)
	}
	journal, err = recovered.snapshotUnit(context.Background(), binding)
	if err != nil || !journal.Aborted || !journal.Stopped || manager.starts != 1 || manager.stops != 1 {
		t.Fatal(journal, err)
	}
}

func TestNativeVMFixedUnitResourcesDoNotChangeFixtureBudget(t *testing.T) {
	binding := authTestBinding()
	unit := "sfl-alpha-" + binding.LaunchID + ".service"
	args := []string{retainedStubExec, "gate.sock", "binding", "manifest", "pid"}
	for _, native := range []bool{false, true} {
		kind := "vm-fixture"
		if native {
			kind = "native-vm"
		}
		var properties []unitProperty
		var err error
		if native {
			properties, err = fixedNativeVMUnitProperties(unit, retainedStubExec, args, payloadDescription(kind, binding), 3)
		} else {
			properties, err = fixedVMUnitProperties(unit, retainedStubExec, args, payloadDescription(kind, binding), 3)
		}
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]any{}
		for _, p := range properties {
			values[p.Name] = p.Value.Value()
		}
		wantMemory, wantTasks, wantRuntime := uint64(1<<30), uint64(64), uint64(120000000)
		if native {
			wantMemory, wantTasks, wantRuntime = 3<<30, 128, 420000000
		}
		if values["MemoryMax"] != wantMemory || values["TasksMax"] != wantTasks || values["RuntimeMaxUSec"] != wantRuntime || values["MemorySwapMax"] != uint64(0) {
			t.Fatal("fixed resource contract changed", kind, values)
		}
	}
	if _, err := fixedVMUnitProperties(unit, retainedStubExec, args, payloadDescription("native-vm", binding), 3); err == nil {
		t.Fatal("fixture unit accepted native authority")
	}
}

func TestNativeVMChannelsCannotAliasCredentialAndObjectStreams(t *testing.T) {
	c, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	files, err := c.duplicateGuest()
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(files)
	if nativeVMStreams(files) != nil {
		t.Fatal("independent private channels rejected")
	}
	aliased := []*os.File{files[0], files[0], files[2]}
	if nativeVMStreams(aliased) == nil {
		t.Fatal("control/auth and object streams alias")
	}
	fd, err := unix.FcntlInt(files[0].Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	copy := os.NewFile(uintptr(fd), "aliased-channel")
	defer copy.Close()
	if nativeVMStreams([]*os.File{files[0], copy, files[2]}) == nil {
		t.Fatal("duplicated socket inode bypassed channel separation")
	}
	inert, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer inert.Close()
	if nativeVMStreams([]*os.File{files[0], inert, files[2]}) == nil {
		t.Fatal("device passed as private stream")
	}
}

func nativeRootOwnedTestAsset(t *testing.T, path string) VMBootAsset {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skip("root-owned native bootstrap fixture unavailable")
	}
	return VMBootAsset{path, authHash(raw), int64(len(raw))}
}
func nativeTestRuntimeSpec(t *testing.T, spec VMBootSpec) NativeVMRuntimeSpec {
	t.Helper()
	manifest, body := nativeRuntimeFixture(t)
	manifest.Program = VMAssetPin{spec.QEMU.SHA256, spec.QEMU.Bytes}
	artifact := vmAsset(t, filepath.Join(t.TempDir(), "runtime"), nativeRuntimeFrame(t, manifest, body))
	helperPath := "/usr/bin/bwrap"
	if _, err := os.Lstat(helperPath); err != nil {
		helperPath, err = filepath.EvalSymlinks("/bin/true")
		if err != nil {
			t.Skip("root-owned executable fixture unavailable")
		}
	}
	return NativeVMRuntimeSpec{Artifact: artifact, NamespaceHelper: nativeRootOwnedTestAsset(t, helperPath), BootstrapLibraries: []VMBootAsset{nativeRootOwnedTestAsset(t, "/lib64/ld-linux-x86-64.so.2"), nativeRootOwnedTestAsset(t, "/usr/lib/x86_64-linux-gnu/libc.so.6")}}
}
