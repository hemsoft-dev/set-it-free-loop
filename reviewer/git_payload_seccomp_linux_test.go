//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestGitPayloadSeccompInertChild(t *testing.T) {
	if os.Getenv("SFL_GIT_FILTER_CHILD") != "1" {
		return
	}
	runtime.LockOSThread()
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		t.Fatal(err)
	}
	filter, err := gitPayloadSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		t.Fatal(errno)
	}
	if os.Getenv("SFL_GIT_FILTER_ABI") == "x32" {
		unix.RawSyscall(0x40000000|39, 0, 0, 0)
		t.Fatal("x32 ABI entry survived")
	}
	for _, family := range []int{unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK, unix.AF_PACKET, unix.AF_UNSPEC} {
		fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		if err != unix.EAFNOSUPPORT {
			t.Fatal("forbidden socket", family, err)
		}
		pair, err := unix.Socketpair(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			unix.Close(pair[0])
			unix.Close(pair[1])
		}
		if err != unix.EAFNOSUPPORT {
			t.Fatal("forbidden socketpair", family, err)
		}
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal("required Unix socket", err)
	}
	unix.Close(fd)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(pair[0])
	unix.Close(pair[1])
	for _, number := range []uintptr{101, 310, 311, 312, 424, 434, 438, 440, 448, 425, 426, 427, 155, 161, 165, 166, 272, 308, 428, 429, 430, 431, 432, 433, 442, 248, 249, 250, 298, 300, 303, 304, 321, 323} {
		_, _, errno := unix.Syscall6(number, 0, 0, 0, 0, 0, 0)
		if errno != unix.EPERM {
			t.Fatal("escape syscall", number, errno)
		}
	}
	_, _, errno = unix.Syscall(435, 0, 0, 0)
	if errno != unix.ENOSYS {
		t.Fatal("clone3 did not request safe fallback", errno)
	}
	for _, flag := range []uintptr{unix.CLONE_NEWNS, unix.CLONE_NEWCGROUP, unix.CLONE_NEWUTS, unix.CLONE_NEWIPC, unix.CLONE_NEWUSER, unix.CLONE_NEWPID, unix.CLONE_NEWNET} {
		_, _, errno = unix.RawSyscall6(unix.SYS_CLONE, flag, 0, 0, 0, 0, 0)
		if errno != unix.EPERM {
			t.Fatal("namespace clone", flag, errno)
		}
	}
	// Normal process creation still works; its child must inherit this policy.
	command := exec.Command("/usr/bin/true")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatal("ordinary fork/exec denied", err, string(data))
	}
	if !t.Run("actual-git-sha1-sha256", TestGitMaterializationActualGitNoHostParserAndSealedReads) {
		os.Exit(1)
	}
	os.Stdout.WriteString("fixed-filter-enforced\n")
	os.Exit(0)
}

func TestGitPayloadSeccompSealedAndKernelEnforced(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("fixed x86-64 policy")
	}
	file, err := retainGitPayloadSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	filter, filterErr := gitPayloadSeccomp()
	var want bytes.Buffer
	binary.Write(&want, binary.LittleEndian, filter)
	if err != nil || filterErr != nil || !bytes.Equal(raw, want.Bytes()) || len(raw)%8 != 0 {
		t.Fatal("filter wire mismatch")
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	flags, flagErr := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	info, statErr := file.Stat()
	if err != nil || flagErr != nil || statErr != nil || seals&requiredStubSeals != requiredStubSeals || seals&unix.F_SEAL_EXEC == 0 || flags&unix.FD_CLOEXEC == 0 || info.Mode().Perm() != 0400 {
		t.Fatal("unsafe filter descriptor")
	}
	if _, err := file.WriteAt([]byte{0}, 0); err == nil {
		t.Fatal("mutable filter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitPayloadSeccompInertChild$")
	command.Env = []string{"SFL_GIT_FILTER_CHILD=1", "PATH=/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "fixed-filter-enforced\n" {
		t.Fatal("kernel filter", err, string(output))
	}
	abi := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitPayloadSeccompInertChild$")
	abi.Env = []string{"SFL_GIT_FILTER_CHILD=1", "SFL_GIT_FILTER_ABI=x32"}
	output, err = abi.CombinedOutput()
	if abi.ProcessState == nil {
		t.Fatal("ABI probe did not start", err, string(output))
	}
	status, exited := abi.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !exited || !status.Signaled() || status.Signal() != unix.SIGSYS || len(output) != 0 {
		t.Fatal("x32 entry was not killed", err, string(output))
	}
}

func TestGitPayloadSeccompActualBubblewrap(t *testing.T) {
	if os.Getenv("SFL_GIT_BOOTSTRAP_NAMESPACE") != "1" {
		t.Skip("explicit inert namespace compatibility check")
	}
	file, err := retainGitPayloadSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	parent := map[string]string{}
	for _, name := range []string{"net", "user", "mnt", "pid"} {
		parent[name], err = os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			t.Fatal(err)
		}
	}
	payload := `import ctypes,json,os,socket
r={'ns':{k:os.readlink('/proc/self/ns/'+k) for k in ('net','user','mnt','pid')},'sockets':{},'io':{}}
for n in ('AF_UNIX','AF_INET','AF_INET6','AF_NETLINK','AF_PACKET'):
 try:s=socket.socket(getattr(socket,n),socket.SOCK_DGRAM);s.close();r['sockets'][n]=0
 except OSError as e:r['sockets'][n]=e.errno
c=ctypes.CDLL(None,use_errno=True)
for n in (425,426,427):
 c.syscall(n,-1,0,0,0,0,0);r['io'][str(n)]=ctypes.get_errno()
print(json.dumps(r))`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/bwrap", "--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--clearenv", "--ro-bind", "/usr", "/usr", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chdir", "/", "--seccomp", "3", "--", "/usr/bin/python3", "-I", "-S", "-c", payload)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{file}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("actual measured helper compatibility", err, string(output))
	}
	var result struct {
		NS      map[string]string `json:"ns"`
		Sockets map[string]int    `json:"sockets"`
		IO      map[string]int    `json:"io"`
	}
	if json.Unmarshal(output, &result) != nil || len(result.NS) != 4 || len(result.Sockets) != 5 || len(result.IO) != 3 {
		t.Fatal("invalid probe output", string(output))
	}
	for name, before := range parent {
		if result.NS[name] == "" || result.NS[name] == before {
			t.Fatal("namespace unchanged", name)
		}
	}
	for name, errno := range result.Sockets {
		want := int(unix.EAFNOSUPPORT)
		if name == "AF_UNIX" {
			want = 0
		}
		if errno != want {
			t.Fatal("socket boundary", name, errno)
		}
	}
	for name, errno := range result.IO {
		if errno != int(unix.EPERM) {
			t.Fatal("io_uring escaped", name, errno)
		}
	}
}
