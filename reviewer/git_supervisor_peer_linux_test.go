//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func gitTestPIDFD(t *testing.T, pid int) *os.File {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "synthetic-pidfd")
	t.Cleanup(func() { file.Close() })
	return file
}
func gitSupervisorListener(t *testing.T) (*net.UnixListener, string, string) {
	t.Helper()
	directory, err := os.MkdirTemp("", "sfl-git-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	path := filepath.Join(directory, "worker.sock")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if os.Chmod(path, 0600) != nil {
		t.Fatal("socket permissions")
	}
	return listener, path, directory
}
func TestGitSupervisorPeerUsesPidfsIdentityAndRejectsAnonymousOrWrongPeers(t *testing.T) {
	listener, path, _ := gitSupervisorListener(t)
	left, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	expected := gitTestPIDFD(t, os.Getpid())
	peer, err := authenticateGitSupervisor(context.Background(), left, expected)
	if err != nil {
		t.Fatal("actual peer", err)
	}
	want, err := gitLivePIDFDIdentity(expected)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitLivePIDFDIdentity(peer)
	if err != nil || want != got {
		t.Fatal("pidfs identity mismatch", err)
	}
	peer.Close()
	otherOpen := gitTestPIDFD(t, os.Getpid())
	same, err := gitLivePIDFDIdentity(otherOpen)
	if err != nil || same != want {
		t.Fatal("same process pidfd opens have different identities", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "/usr/bin/sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	wrong := gitTestPIDFD(t, child.Process.Pid)
	wrongIdentity, err := gitLivePIDFDIdentity(wrong)
	if err != nil || wrongIdentity == want {
		t.Fatal("different live process shares pidfs identity", err)
	}
	for _, candidate := range []*os.File{nil, wrong, gitPacketFile(t)} {
		before := gitPacketFDCount(t)
		if peer, err := authenticateGitSupervisor(context.Background(), left, candidate); err == nil {
			peer.Close()
			t.Fatal("wrong trusted descriptor accepted")
		}
		if gitPacketFDCount(t) != before {
			t.Fatal("refusal leaked descriptors")
		}
	}
	unnamed, _ := gitPacketPair(t, unix.SOCK_SEQPACKET)
	if peer, err := authenticateGitSupervisor(context.Background(), unnamed, expected); err == nil {
		peer.Close()
		t.Fatal("inherited anonymous socketpair accepted")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	poll := []unix.PollFd{{Fd: int32(wrong.Fd()), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 3000); err != nil || n != 1 {
		t.Fatal("inert child did not stop", err)
	}
	if _, err := gitLivePIDFDIdentity(wrong); err == nil {
		t.Fatal("dead process accepted")
	}
	if _, err := gitLivePIDFDIdentity(nil); err == nil {
		t.Fatal("nil process descriptor accepted")
	}
}

func TestGitSupervisorPrivateChild(t *testing.T) {
	if os.Getenv("SFL_GIT_SUPERVISOR_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	expected := os.NewFile(3, "trusted-supervisor")
	unix.CloseOnExec(3)
	canary := os.NewFile(4, "inert-canary")
	unix.CloseOnExec(4)
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "/broker/worker.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	peer, authErr := authenticateGitSupervisor(ctx, conn, expected)
	var credentials *unix.Ucred
	raw, _ := conn.SyscallConn()
	var queryErr error
	raw.Control(func(fd uintptr) {
		credentials, queryErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if queryErr != nil || credentials == nil {
		t.Fatal("kernel credentials", queryErr)
	}
	if os.Getenv("SFL_GIT_SUPERVISOR_WRONG") == "1" {
		if authErr == nil {
			peer.Close()
			t.Fatal("wrong inherited pidfd authenticated supervisor")
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"trusted": false, "peer_pid": credentials.Pid})
		os.Exit(0)
	}
	if authErr != nil {
		t.Fatal("outside supervisor authentication", authErr)
	}
	if credentials.Pid != 0 {
		t.Fatal("fixture did not enter private PID namespace", credentials.Pid)
	}
	if writeGitControlPacket(ctx, conn, []byte("ready"), nil) != nil {
		t.Fatal("ready")
	}
	stage, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "seal" {
		t.Fatal("seal instruction", err)
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		t.Fatal("dumpability transition")
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || dumpable != 0 {
		t.Fatal("dumpability was not cleared", err)
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CapEff", "CapPrm", "CapAmb"} {
		if !strings.Contains(string(status), key+":\t0000000000000000\n") {
			t.Fatal("payload retained capabilities", key)
		}
	}
	// Git-equivalent child inherits the final policy and no private worker FDs.
	probe := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-S", "-c", `import errno,os,sys
try:
 fd=os.open('/proc/'+sys.argv[1]+'/fd/4',os.O_RDONLY)
 os.close(fd);sys.exit(2)
except OSError as e:
 if e.errno!=errno.EACCES:sys.exit(3)
try:
 os.fstat(4);sys.exit(4)
except OSError as e:
 if e.errno!=errno.EBADF:sys.exit(5)
`, strconv.Itoa(os.Getpid()))
	probe.Env = []string{}
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatal("child reached worker canary descriptor", err, string(output))
	}
	if writeGitControlPacket(ctx, conn, []byte("sealed"), nil) != nil {
		t.Fatal("sealed")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "finish" {
		t.Fatal("finish", err)
	}
	peer.Close()
	expected.Close()
	canary.Close()
	conn.Close()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"trusted": true, "peer_pid": credentials.Pid, "dumpable": dumpable, "child_proc_denied": true})
	os.Exit(0)
}

func TestGitSupervisorActualPrivatePIDNamespaceAndDumpableBoundary(t *testing.T) {
	if os.Getenv("SFL_GIT_BOOTSTRAP_NAMESPACE") != "1" {
		t.Skip("explicit inert cross-namespace fixture")
	}
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			listener, _, directory := gitSupervisorListener(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			expected := gitTestPIDFD(t, os.Getpid())
			if wrong {
				other := exec.CommandContext(ctx, "/usr/bin/sleep", "10")
				if other.Start() != nil {
					t.Fatal("inert other process")
				}
				defer func() { other.Process.Kill(); other.Wait() }()
				expected = gitTestPIDFD(t, other.Process.Pid)
			}
			filter, err := retainGitPayloadSeccomp()
			if err != nil {
				t.Fatal(err)
			}
			defer filter.Close()
			canary := gitPacketFile(t)
			args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--setenv", "SFL_GIT_SUPERVISOR_CHILD", "1", "--ro-bind", "/usr", "/usr", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--ro-bind", os.Args[0], "/fixture", "--ro-bind", directory, "/broker", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chdir", "/", "--seccomp", "5"}
			if wrong {
				args = append(args, "--setenv", "SFL_GIT_SUPERVISOR_WRONG", "1")
			}
			args = append(args, "--", "/fixture", "-test.run=^TestGitSupervisorPrivateChild$")
			command := exec.CommandContext(ctx, "/usr/bin/bwrap", args...)
			command.Env = []string{}
			command.ExtraFiles = []*os.File{expected, canary, filter}
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					command.Process.Kill()
					command.Wait()
				}
				if t.Failed() {
					t.Log("inert worker output:", output.String())
				}
			}()
			if !wrong {
				listener.SetDeadline(time.Now().Add(8 * time.Second))
				conn, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal("worker connection", err)
				}
				defer conn.Close()
				stage, files, err := readGitControlPacket(ctx, conn, 0)
				closeVMFiles(files)
				if err != nil || string(stage) != "ready" {
					t.Fatal("worker readiness", err, string(stage))
				}
				kernelPeer, err := gitBrokerPeer(conn)
				if err != nil {
					t.Fatal(err)
				}
				defer kernelPeer.kernel.Close()
				identity, fd, err := retainGitBrokerPeer(kernelPeer, strings.Repeat("e", 32))
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(fd)
				manager, err := processGateIdentity(os.Getpid(), identity.InvocationID)
				if err != nil {
					t.Fatal(err)
				}
				boundary, err := retainGitNetworkBoundary(identity, fd, manager, int(expected.Fd()))
				if err != nil {
					t.Fatal("pre-seal namespace capture", err)
				}
				defer boundary.Close()
				if writeGitControlPacket(ctx, conn, []byte("seal"), nil) != nil {
					t.Fatal("seal dispatch")
				}
				stage, files, err = readGitControlPacket(ctx, conn, 0)
				closeVMFiles(files)
				if err != nil || string(stage) != "sealed" {
					t.Fatal("worker seal", err, string(stage))
				}
				if boundary.recheck() != nil || boundary.matchesPeer(identity, fd) != nil {
					t.Fatal("host could not revalidate after dumpable zero")
				}
				if writeGitControlPacket(ctx, conn, []byte("finish"), nil) != nil {
					t.Fatal("finish dispatch")
				}
			}
			err = command.Wait()
			waited = true
			if err != nil {
				t.Fatal("private boundary fixture", err, output.String())
			}
			var result struct {
				Trusted  bool `json:"trusted"`
				PeerPID  int  `json:"peer_pid"`
				Dumpable int  `json:"dumpable"`
				Denied   bool `json:"child_proc_denied"`
			}
			if json.Unmarshal(output.Bytes(), &result) != nil || result.Trusted == wrong || result.PeerPID != 0 || (!wrong && (!result.Denied || result.Dumpable != 0)) {
				t.Fatal("wrong kernel boundary result", output.String())
			}
		})
	}
}
