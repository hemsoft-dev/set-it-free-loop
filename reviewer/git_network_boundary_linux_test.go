//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func gitBoundaryProcess(t *testing.T, pid int) (GateIdentity, int) {
	t.Helper()
	identity, err := processGateIdentity(pid, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return identity, fd
}
func TestGitNetworkBoundaryRejectsHostNamespaceAndWrongDescriptors(t *testing.T) {
	identity, fd := gitBoundaryProcess(t, os.Getpid())
	for range 12 {
		boundary, err := retainGitNetworkBoundary(identity, fd, identity, fd)
		if err == nil || boundary != nil {
			if boundary != nil {
				boundary.Close()
			}
			t.Fatal("host network accepted")
		}
		if gitCheckProcessPIDFD(identity, fd) != nil {
			t.Fatal("caller pidfd closed on refusal")
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "not-a-namespace")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := gitNetworkNamespaceIdentity(file); err == nil {
		t.Fatal("ordinary file accepted as namespace")
	}
	userNS, err := os.Open("/proc/self/ns/user")
	if err != nil {
		t.Fatal(err)
	}
	defer userNS.Close()
	if _, err := gitNetworkNamespaceIdentity(userNS); err == nil {
		t.Fatal("wrong namespace type accepted")
	}
	for _, bad := range []int{-1, int(file.Fd()), int(userNS.Fd())} {
		if retained, err := gitRetainProcessPIDFD(identity, bad); err == nil {
			unix.Close(retained)
			t.Fatal("wrong process descriptor accepted")
		}
	}
	for _, mutate := range []func(*GateIdentity){func(v *GateIdentity) { v.PID++ }, func(v *GateIdentity) { v.StartTicks++ }, func(v *GateIdentity) { v.BootID = "wrong" }, func(v *GateIdentity) { v.InvocationID = "invalid" }} {
		changed := identity
		mutate(&changed)
		if retained, err := gitRetainProcessPIDFD(changed, fd); err == nil {
			unix.Close(retained)
			t.Fatal("wrong process identity accepted")
		}
	}
	var missing *gitNetworkBoundary
	if missing.recheck() == nil || missing.matchesPeer(identity, fd) == nil || missing.Close() != nil {
		t.Fatal("nil boundary behavior")
	}
}
func TestGitNetworkBoundaryRejectsUnrelatedAndDeadPIDFD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "/usr/bin/sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	identity, fd := gitBoundaryProcess(t, child.Process.Pid)
	self, selfFD := gitBoundaryProcess(t, os.Getpid())
	if gitCheckProcessPIDFD(self, fd) == nil || gitCheckProcessPIDFD(identity, selfFD) == nil {
		t.Fatal("unrelated live pidfd accepted")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 3000); err != nil || n != 1 {
		t.Fatal("child did not terminate", err)
	}
	if gitCheckProcessPIDFD(identity, fd) == nil {
		t.Fatal("exited process accepted before reap")
	}
}
func TestGitNetworkBoundaryActualPrivateNamespace(t *testing.T) {
	if os.Getenv("SFL_GIT_BOOTSTRAP_NAMESPACE") != "1" {
		t.Skip("explicit inert namespace fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Deliberately retain the fixture's host PID namespace so its reported PID
	// can locate it. Production must also prove the separate PID/mount boundary.
	command := exec.CommandContext(ctx, "/usr/bin/bwrap", "--unshare-user", "--unshare-net", "--disable-userns", "--die-with-parent", "--new-session", "--clearenv", "--ro-bind", "/usr", "/usr", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chdir", "/", "--", "/usr/bin/python3", "-I", "-S", "-c", "import os,sys;print(os.getpid(),flush=True);sys.stdin.buffer.read(1)")
	command.Env = []string{}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); command.Process.Kill(); command.Wait() }()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	worker, fd := gitBoundaryProcess(t, pid)
	manager, managerFD := gitBoundaryProcess(t, os.Getpid())
	supplied, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := retainGitNetworkBoundary(worker, supplied, manager, managerFD)
	if err != nil {
		unix.Close(supplied)
		t.Fatal("private namespace could not be independently retained", err)
	}
	defer boundary.Close()
	if boundary.recheck() != nil || boundary.matchesPeer(worker, fd) != nil {
		t.Fatal("original private namespace lost")
	}
	if boundary.matchesPeer(manager, managerFD) == nil {
		t.Fatal("host peer accepted")
	}
	// The boundary owns duplicates; closing the supplied descriptors cannot
	// revoke the retained observations or cause descriptor-number substitution.
	if err := unix.Close(supplied); err != nil {
		t.Fatal(err)
	}
	if boundary.recheck() != nil {
		t.Fatal("caller close affected retained pidfd")
	}
	input.Close()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 3000); err != nil || n != 1 {
		t.Fatal("fixture did not terminate", err)
	}
	if boundary.recheck() == nil {
		t.Fatal("exited original worker retained admission")
	}
	if boundary.Close() != nil || boundary.recheck() == nil || boundary.matchesPeer(worker, fd) == nil || boundary.Close() != nil {
		t.Fatal("closed boundary remained usable")
	}
}
