//go:build linux

package reviewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Opt-in, model-free qualification on a delegated user cgroup hierarchy.
// This starts only fresh inert owned units. Cleanup uses original descriptors.
func TestContainmentKernelOriginalCgroupTermination(t *testing.T) {
	if os.Getenv("SFL_TEST_ORIGINAL_CGROUP") != "1" {
		t.Skip("explicit model-free native qualification only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	unit := "sfl-alpha-" + hex.EncodeToString(nonce[:]) + ".service"
	kernel := linuxContainmentKernel{}
	type captured struct {
		binding               cgroupBinding
		invocation            string
		rootPIDFD, childPIDFD int
		kill                  *os.File
	}
	var originals []captured
	t.Cleanup(func() {
		for _, old := range originals {
			old.kill.Write([]byte("1"))
			old.kill.Close()
			unix.Close(old.rootPIDFD)
			unix.Close(old.childPIDFD)
		}
	})
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("bounded native observation expired")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	show := func() map[string]string {
		command := exec.CommandContext(ctx, "/usr/bin/systemctl", "--user", "show", unit, "--property=LoadState,ActiveState,InvocationID,MainPID,ControlGroup")
		raw, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				fields[k] = v
			}
		}
		return fields
	}
	dead := func(fd int) bool {
		p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, e := unix.Poll(p, 0)
		return e == nil && n == 1 && p[0].Revents&unix.POLLIN != 0
	}
	alive := func(fd int) bool {
		return unix.PidfdSendSignal(fd, 0, nil, 0) == nil && func() bool {
			p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, e := unix.Poll(p, 0)
			return e == nil && n == 0
		}()
	}
	const code = `import os,sys,signal
assert len(os.environ.get("INVOCATION_ID",""))==32
if os.fork()==0:
 signal.signal(signal.SIGTERM,signal.SIG_IGN)
 with open(sys.argv[1],"x") as f:
  f.write(str(os.getpid()));f.flush();os.fsync(f.fileno())
 while True:signal.pause()
while True:signal.pause()
`
	python, err := trustedStubBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		record := filepath.Join(t.TempDir(), "owned-child.pid")
		args := []string{"--user", "--unit=" + unit, "--property=RuntimeMaxSec=10", "--property=TimeoutStopSec=1", "--property=KillMode=control-group", "--property=Restart=no", "--property=RemainAfterExit=yes", "--property=CollectMode=inactive-or-failed", "--property=NoNewPrivileges=yes", "--property=TasksMax=64", "--property=MemoryMax=1G", "--property=MemorySwapMax=0", "--property=CPUQuota=100%", python, "-I", "-S", "-B", "-c", code, record}
		if output, err := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...).CombinedOutput(); err != nil {
			t.Fatalf("inert start: %s: %v", output, err)
		}
		wait(func() bool { _, err := os.Stat(record); return err == nil })
		observed := show()
		pid, err := strconv.Atoi(observed["MainPID"])
		if err != nil || pid <= 0 {
			t.Fatal(observed, err)
		}
		childRaw, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		child, err := strconv.Atoi(string(childRaw))
		if err != nil {
			t.Fatal(err)
		}
		binding, err := kernel.Capture(observed["ControlGroup"], unit)
		if err != nil {
			t.Fatal(err)
		}
		root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", binding.Path, binding.Hierarchy)
		if err != nil {
			t.Fatal(err)
		}
		kill, err := root.OpenFile("cgroup.kill", os.O_WRONLY|unix.O_NOFOLLOW, 0)
		root.Close()
		if err != nil {
			t.Fatal(err)
		}
		rootFD, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			kill.Close()
			t.Fatal(err)
		}
		childFD, err := unix.PidfdOpen(child, 0)
		if err != nil {
			kill.Close()
			unix.Close(rootFD)
			t.Fatal(err)
		}
		current := captured{binding, observed["InvocationID"], rootFD, childFD, kill}
		originals = append(originals, current)
		if !alive(rootFD) || !alive(childFD) {
			t.Fatal("inert original processes already dead")
		}
		if index == 1 {
			old := originals[0]
			if old.invocation == current.invocation || old.binding.Inode == current.binding.Inode {
				t.Fatal("replacement was not a new instance")
			}
			if _, err := old.kill.Write([]byte("1")); !errors.Is(err, unix.ENODEV) {
				t.Fatal("removed original descriptor did not refuse", err)
			}
			if err := kernel.Terminate(ctx, old.binding); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("replacement path accepted as original", err)
			}
			if !alive(rootFD) || !alive(childFD) {
				t.Fatal("replacement was touched by old ownership")
			}
		}
		if err := kernel.Terminate(ctx, binding); err != nil {
			t.Fatal("actual original termination", err)
		}
		wait(func() bool { return dead(rootFD) && dead(childFD) && kernel.Empty(binding) == nil })
		wait(func() bool { return show()["LoadState"] == "not-found" })
		raw, _ := json.Marshal(map[string]any{"case": index, "invocation": current.invocation, "capturedCgroup": binding, "rootDead": true, "descendantDead": true, "empty": true, "replacementProtected": index == 1})
		t.Log(string(raw))
	}
}
