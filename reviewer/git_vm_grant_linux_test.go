//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The captured inert child plays the original stub's grant role only. It
// never runs QEMU; the real stub and guest are exercised by the KVM fixture.
func TestGitVMStubGrantChild(t *testing.T) {
	if os.Getenv("SFL_GIT_VM_GRANT_CHILD") != "1" {
		return
	}
	os.Stdout.Write([]byte("ready\n"))
	var start [1]byte
	if _, err := os.Stdin.Read(start[:]); err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: os.Getenv("SFL_GIT_VM_GRANT_ENDPOINT"), Net: "unixpacket"})
	if err != nil {
		os.Exit(3)
	}
	defer conn.Close()
	own, err := processGateIdentity(os.Getpid(), os.Getenv("SFL_GIT_VM_GRANT_INVOCATION"))
	if err != nil {
		os.Exit(4)
	}
	hash := os.Getenv("SFL_GIT_VM_GRANT_REQUEST")
	if os.Getenv("SFL_GIT_VM_GRANT_MODE") == "foreign-hash" {
		hash = Digest("foreign request")
	}
	hello, _ := json.Marshal(gitVMStubHello{gitVMStubProtocol, hash, own})
	if writeGitControlPacket(ctx, conn, hello, nil) != nil {
		os.Exit(5)
	}
	if os.Getenv("SFL_GIT_VM_GRANT_MODE") == "lost-send" {
		conn.Close()
		os.Stdin.Read(start[:])
		os.Exit(0)
	}
	grantRaw, boot, err := readGitControlPacket(ctx, conn, gitVMStubBootFDs)
	if err != nil {
		os.Exit(6)
	}
	defer closeVMFiles(boot)
	grant, err := DecodeStrict[gitVMStubGrant](grantRaw, gitControlPacketBytes)
	if err != nil || !grant.valid(hash) || grant.Recipient != own {
		os.Exit(7)
	}
	channelRaw, channels, err := readGitControlPacket(ctx, conn, gitVMStubChannelFDs)
	if err != nil {
		os.Exit(8)
	}
	defer closeVMFiles(channels)
	binding, err := DecodeStrict[gitVMStubChannels](channelRaw, gitControlPacketBytes)
	if err != nil || binding != (gitVMStubChannels{gitVMStubProtocol, gitRawHash(grantRaw)}) || verifyGrantedKVM(boot[3]) != nil || verifyGitVMRuntimeRoot(boot[7]) != nil || nativeVMStreams(channels) != nil {
		os.Exit(9)
	}
	os.Stdout.Write([]byte("granted\n"))
	os.Stdin.Read(start[:])
	os.Exit(0)
}

type gitVMGrantFixture struct {
	gitVMOwnerFixture
	listener *net.UnixListener
	boot     []*os.File
	channels *nativeVMChannels
	input    io.WriteCloser
	output   io.Reader
	done     chan struct{}
}

func newGitVMGrantFixture(t *testing.T, mode string) gitVMGrantFixture {
	t.Helper()
	kvm, err := openVMKVM()
	if err != nil {
		t.Skip("KVM is required for source VM grant descriptors")
	}
	spec, source := gitVMAssetsFixture(t)
	assets, err := openGitVMAssets(context.Background(), spec)
	if err != nil {
		kvm.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { assets.Close() })
	read := gitVMReadTestBinding()
	read.Source = source.Binding
	q := gitVMGuestRequest{1, gitVMGuestPurpose, source, read}
	directory, err := os.MkdirTemp("/tmp", "sfl-vm-grant-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	endpoint := filepath.Join(directory, "stub.sock")
	listener, err := gitFactoryListen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	f := newGitVMOwnerFixture(t)
	f.request = q
	unit := "sfl-alpha-" + q.Source.Binding.Attempt + ".service"
	f.manager.observation.Unit = unit
	f.kernel.group.Path = "/user.slice/" + unit
	f.manager.observation.ControlGroup = f.kernel.group.Path
	// Replace the generic inert child with the grant-role child as MainPID.
	child := exec.Command(os.Args[0], "-test.run=^TestGitVMStubGrantChild$")
	child.Env = []string{"SFL_GIT_VM_GRANT_CHILD=1", "SFL_GIT_VM_GRANT_ENDPOINT=" + endpoint, "SFL_GIT_VM_GRANT_INVOCATION=" + f.manager.observation.InvocationID, "SFL_GIT_VM_GRANT_REQUEST=" + Digest(q), "SFL_GIT_VM_GRANT_MODE=" + mode}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		child.Process.Kill()
		child.Wait()
		t.Fatal("grant child not ready", err)
	}
	done := make(chan struct{})
	go func() { child.Wait(); close(done) }()
	t.Cleanup(func() { child.Process.Kill(); input.Close(); <-done })
	f.manager.mu.Lock()
	f.manager.child, f.manager.childDone = child, done
	f.manager.observation.MainPID = uint32(child.Process.Pid)
	f.manager.mu.Unlock()
	f.child = child
	if f.owner.prepare(context.Background(), q) != nil || f.owner.start(context.Background(), q.Source.Binding.Attempt, endpoint) != nil {
		t.Fatal("prepare/start")
	}
	duplicates, err := assets.duplicate(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(directory, "runtime-")
	if err != nil {
		t.Fatal(err)
	}
	if os.Chmod(root, 0555) != nil {
		t.Fatal("read-only runtime root")
	}
	t.Cleanup(func() { os.Chmod(root, 0700) })
	rootFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	boot := []*os.File{duplicates[0], duplicates[1], duplicates[2], kvm, duplicates[3], duplicates[4], duplicates[5], rootFile}
	t.Cleanup(func() { closeVMFiles(boot) })
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channels.Close() })
	return gitVMGrantFixture{f, listener, boot, channels, input, output, done}
}

func (f gitVMGrantFixture) grant(t *testing.T) error {
	t.Helper()
	if _, err := f.input.Write([]byte("g")); err != nil {
		t.Fatal(err)
	}
	conn, err := gitFactoryAccept(context.Background(), f.listener)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	guest, err := f.channels.duplicateGuest()
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(guest)
	return f.owner.grant(context.Background(), f.attempt(), conn, f.boot, guest)
}

func TestGitVMGrantIsOneUseToTheCapturedStub(t *testing.T) {
	f := newGitVMGrantFixture(t, "")
	if f.owner.live(context.Background(), f.attempt()) == nil {
		t.Fatal("ungranted attempt admitted reads")
	}
	if err := f.grant(t); err != nil {
		t.Fatal("grant", err)
	}
	reply := make([]byte, 8)
	if _, err := io.ReadFull(f.output, reply); err != nil || string(reply) != "granted\n" {
		t.Fatal("stub did not receive the exact grant", err)
	}
	r, err := f.owner.read(f.attempt())
	if err != nil || !validAuthHash(r.GrantHash) || r.Revoked || f.owner.grantEligible[f.attempt()] {
		t.Fatal("grant not durable or still eligible", r, err)
	}
	if err := f.owner.live(context.Background(), f.attempt()); err != nil {
		t.Fatal("granted original not live", err)
	}
	// The same original stub cannot be granted twice.
	conn, peer := gitVMGrantSocketPair(t)
	defer peer.Close()
	if f.owner.grant(context.Background(), f.attempt(), conn, f.boot, nil) == nil {
		t.Fatal("second grant accepted")
	}
	f.kernel.mu.Lock()
	f.kernel.changed = true
	f.kernel.mu.Unlock()
	if f.owner.live(context.Background(), f.attempt()) == nil {
		t.Fatal("replacement cgroup admitted reads")
	}
	f.kernel.mu.Lock()
	f.kernel.changed = false
	f.kernel.mu.Unlock()
	if f.owner.stop(context.Background(), f.attempt()) != nil {
		t.Fatal("stop")
	}
	if f.owner.live(context.Background(), f.attempt()) == nil {
		t.Fatal("stopped attempt admitted reads")
	}
}

func gitVMGrantSocketPair(t *testing.T) (*net.UnixConn, *os.File) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	local := os.NewFile(uintptr(pair[0]), "grant-local")
	conn, err := net.FileConn(local)
	local.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*net.UnixConn), os.NewFile(uintptr(pair[1]), "grant-peer")
}

func TestGitVMGrantRefusesBeforeDurableAuthority(t *testing.T) {
	for _, kind := range []string{"foreign-hash", "wrong-boot", "impostor"} {
		t.Run(kind, func(t *testing.T) {
			mode := ""
			if kind == "foreign-hash" {
				mode = kind
			}
			f := newGitVMGrantFixture(t, mode)
			var err error
			switch kind {
			case "wrong-boot":
				f.boot[1], f.boot[2] = f.boot[2], f.boot[1]
				err = f.grant(t)
			case "impostor":
				// The test process, not the captured original stub, connects.
				conn, peer := gitVMGrantSocketPair(t)
				defer peer.Close()
				// It claims the captured stub's exact identity; only kernel
				// peer credentials can refuse it.
				captured, _ := f.owner.read(f.attempt())
				hello, _ := json.Marshal(gitVMStubHello{gitVMStubProtocol, Digest(f.request), captured.Identity})
				peerConn, _ := net.FileConn(peer)
				defer peerConn.Close()
				if writeGitControlPacket(context.Background(), peerConn.(*net.UnixConn), hello, nil) != nil {
					t.Fatal("impostor hello")
				}
				guest, _ := f.channels.duplicateGuest()
				defer closeVMFiles(guest)
				err = f.owner.grant(context.Background(), f.attempt(), conn, f.boot, guest)
			default:
				err = f.grant(t)
			}
			if err == nil {
				t.Fatal("refused grant accepted")
			}
			r, readErr := f.owner.read(f.attempt())
			if readErr != nil || r.GrantHash != "" || r.Revoked {
				t.Fatal("refusal wrote grant authority", r, readErr)
			}
			if f.owner.live(context.Background(), f.attempt()) == nil {
				t.Fatal("refused attempt admitted reads")
			}
		})
	}
}

func TestGitVMGrantLostSendRevokesWithoutReplay(t *testing.T) {
	f := newGitVMGrantFixture(t, "lost-send")
	if f.grant(t) == nil {
		t.Fatal("lost send accepted")
	}
	r, err := f.owner.read(f.attempt())
	if err != nil || !validAuthHash(r.GrantHash) || !r.Revoked || r.Stopped || f.owner.grantEligible[f.attempt()] {
		t.Fatal("lost send not durably revoked", r, err)
	}
	if f.owner.live(context.Background(), f.attempt()) == nil {
		t.Fatal("revoked attempt admitted reads")
	}
	if f.owner.stop(context.Background(), f.attempt()) != nil {
		t.Fatal("revoked original stop")
	}
}

// One source of truth for the measured launch: argv and descriptor roles.
func TestGitVMSourceLaunchLayoutIsFixed(t *testing.T) {
	q := gitVMGuestTestRequest(t)
	args := strings.Join(gitVMSourceQEMUArgs(q), " ")
	for _, want := range []string{"-nic none", "-monitor none", "-m 1536M", "rdinit=/source-init", "sfl-source=" + Digest(q), "fd=6,set=1,opaque=sfl-kvm", "-bios /proc/self/fd/10", "-kernel /proc/self/fd/4", "-initrd /proc/self/fd/5", "socket,id=sflbootdiag,fd=13", "socket,id=sfl0,fd=7", "name=sfl.source-control", "socket,id=sfl1,fd=8", "name=sfl.source-read", "socket,id=sfl2,fd=9", "name=sfl.source-export", "spawn=deny"} {
		if !strings.Contains(args, want) {
			t.Fatal("missing launch element", want)
		}
	}
	for _, forbidden := range []string{"-netdev", "virtfs", "-drive", "user,", "-qmp", "-gdb"} {
		if strings.Contains(args, forbidden) {
			t.Fatal("forbidden launch element", forbidden)
		}
	}
	files := make([]*os.File, 12)
	boot, channels := make([]*os.File, gitVMStubBootFDs), make([]*os.File, gitVMStubChannelFDs)
	for i := range boot {
		boot[i] = os.NewFile(uintptr(100+i), "boot-"+strconv.Itoa(i))
	}
	for i := range channels {
		channels[i] = os.NewFile(uintptr(200+i), "channel-"+strconv.Itoa(i))
	}
	serial := os.NewFile(300, "serial")
	copy(files, gitVMLaunchFiles(boot, channels, serial))
	// ExtraFiles index i becomes descriptor 3+i in QEMU's helper.
	want := []*os.File{boot[0], boot[1], boot[2], boot[3], channels[0], channels[1], channels[2], boot[4], boot[5], boot[6], serial, boot[7]}
	for i := range want {
		if files[i] != want[i] {
			t.Fatal("launch descriptor role moved", i+3)
		}
	}
}

func TestGitVMStubRequestArgumentIsCanonical(t *testing.T) {
	q := gitVMGuestTestRequest(t)
	raw, _ := json.Marshal(q)
	if got, err := parseGitVMStubRequest(string(raw), Digest(q)); err != nil || Digest(got) != Digest(q) {
		t.Fatal("canonical request refused", err)
	}
	var indented bytes.Buffer
	json.Indent(&indented, raw, "", " ")
	for _, bad := range []struct{ raw, hash string }{
		{string(raw), Digest("other")},
		{indented.String(), Digest(q)},
		{strings.Replace(string(raw), `"Version":1`, `"Version":1,"Extra":1`, 1), Digest(q)},
		{"", Digest(q)},
	} {
		if _, err := parseGitVMStubRequest(bad.raw, bad.hash); err == nil {
			t.Fatal("non-canonical stub request accepted")
		}
	}
	// Outside its own source unit the stub refuses before dialing anything.
	if GitVMSourceStub(context.Background(), []string{"/private/stub.sock", string(raw), Digest(q), strconv.Itoa(os.Getppid())}, strings.Repeat("a", 32)) == nil {
		t.Fatal("stub ran outside its source unit")
	}
}
