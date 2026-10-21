//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGitDescriptorFlagsCurrentCloexecAndMalformedData(t *testing.T) {
	for _, input := range []struct {
		raw  string
		want uint64
	}{
		{"pos:\t0\nflags:\t02000002\nmnt_id:\t1\nino:\t42\n", unix.O_CLOEXEC | unix.O_RDWR},
		{"pos:\t0\nflags:\t02\n", unix.O_RDWR},
	} {
		actual, err := gitDescriptorFlags([]byte(input.raw))
		if err != nil || actual != input.want {
			t.Fatalf("current kernel flags %o: %v", actual, err)
		}
	}
	for _, raw := range []string{"", "flags:\t02000002", "pos:\t0\n", "flags:\t08\n", "flags:\t-1\n", "flags:\t02000002\nflags:\t02\n", "flags:\t02\n\x00", strings.Repeat("x", 16385)} {
		if _, err := gitDescriptorFlags([]byte(raw)); err == nil {
			t.Fatalf("malformed fdinfo accepted: %q", raw[:min(30, len(raw))])
		}
	}
}

// Static observations prove the private policy only. The separate private
// namespace fixture must obtain every descriptor from the real original pidfd.
func TestGitDescriptorPolicyRefusesExtraCapabilities(t *testing.T) {
	export := gitKernelFileID{1, 42}
	supervisor := gitPIDFDIdentity{2, 43}
	rows := []gitDescriptorObservation{
		{Number: 0, Flags: unix.O_RDONLY, Mode: unix.S_IFCHR, Device: unix.Mkdev(1, 3)},
		{Number: 1, Flags: unix.O_WRONLY, Mode: unix.S_IFCHR, Device: unix.Mkdev(1, 3)},
		{Number: 2, Flags: unix.O_WRONLY, Mode: unix.S_IFCHR, Device: unix.Mkdev(1, 3)},
		{Number: 6, Flags: unix.O_CLOEXEC | unix.O_WRONLY, Mode: unix.S_IFIFO, File: export},
		{Number: 7, Flags: unix.O_CLOEXEC | unix.O_RDWR, Mode: unix.S_IFSOCK, SocketDomain: unix.AF_UNIX, SocketType: unix.SOCK_SEQPACKET, SocketPeer: "/broker/worker.sock", Credentials: unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())}},
		{Number: 8, Flags: unix.O_CLOEXEC, File: gitKernelFileID(supervisor)},
		{Number: 9, Flags: unix.O_CLOEXEC, Target: "anon_inode:[eventpoll]"},
		{Number: 10, Flags: unix.O_CLOEXEC, Target: "anon_inode:[eventfd]"},
	}
	if validateGitDescriptors(rows, export, supervisor, "/broker/worker.sock") != nil {
		t.Fatal("fixed static descriptor policy")
	}
	for _, mode := range []string{"missing-standard", "stdio-socket", "stdio-read", "inheritable", "wrong-export", "export-read", "wrong-supervisor", "wrong-peer", "internet", "wrong-control-path", "unknown-anon", "extra-regular", "extra-directory", "extra-socket", "duplicate", "duplicate-control", "extra-eventfd", "extra-epoll", "replace-poller"} {
		t.Run(mode, func(t *testing.T) {
			bad := append([]gitDescriptorObservation(nil), rows...)
			switch mode {
			case "extra-eventfd", "extra-epoll":
				extra := bad[6]
				if mode == "extra-eventfd" {
					extra = bad[7]
				}
				extra.Number = 11
				bad = append(bad, extra)
			case "replace-poller":
				bad[6].Target = "anon_inode:[eventfd]"
			case "missing-standard":
				bad = bad[1:]
			case "stdio-socket":
				bad[0].Mode = unix.S_IFSOCK
			case "stdio-read":
				bad[1].Flags = unix.O_RDONLY
			case "inheritable":
				bad[3].Flags &^= unix.O_CLOEXEC
			case "wrong-export":
				bad[3].File.Inode++
			case "export-read":
				bad[3].Flags = unix.O_CLOEXEC | unix.O_RDONLY
			case "wrong-supervisor":
				bad[5].File.Inode++
			case "wrong-peer":
				bad[4].Credentials.Pid++
			case "internet":
				bad[4].SocketDomain = unix.AF_INET
			case "wrong-control-path":
				bad[4].SocketPeer = "/broker/git.sock"
			case "unknown-anon":
				bad[6].Target = "anon_inode:[io_uring]"
			case "extra-regular":
				bad = append(bad, gitDescriptorObservation{Number: 11, Flags: unix.O_CLOEXEC, Mode: unix.S_IFREG})
			case "extra-directory":
				bad = append(bad, gitDescriptorObservation{Number: 11, Flags: unix.O_CLOEXEC, Mode: unix.S_IFDIR})
			case "extra-socket":
				bad = append(bad, gitDescriptorObservation{Number: 11, Flags: unix.O_CLOEXEC, Mode: unix.S_IFSOCK})
			case "duplicate":
				bad[7].Number = bad[6].Number
			case "duplicate-control":
				extra := bad[4]
				extra.Number = 11
				bad = append(bad, extra)
			}
			if validateGitDescriptors(bad, export, supervisor, "/broker/worker.sock") == nil {
				t.Fatal("extra/missing capability accepted")
			}
		})
	}
}

func TestGitDescriptorOriginalPidfdAndNoLocalDescriptorMutation(t *testing.T) {
	// Self-observation is deliberately refused if the descriptor list changes
	// due to inspection's own temporary handles. It is not a worker fixture.
	child := exec.Command("/usr/bin/sleep", "10")
	child.Env = []string{}
	if child.Start() != nil {
		t.Fatal("inert descriptor child")
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	// Start returns after exec, before the dynamic loader necessarily closes
	// its temporary library handles. Inspect only the inert sleeping state.
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := os.ReadFile(fmt.Sprintf("/proc/%d/wchan", child.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(state), "nanosleep") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inert child never reached its kernel sleep")
		}
		time.Sleep(time.Millisecond)
	}
	identity, err := processGateIdentity(child.Process.Pid, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(child.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	before := gitPacketFDCount(t)
	rows, err := observeGitDescriptors(context.Background(), identity, pidfd)
	if err != nil || len(rows) != 3 {
		t.Fatalf("actual inert child descriptor read: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.Number > 2 || row.Mode&unix.S_IFMT != unix.S_IFCHR || row.Device != unix.Mkdev(1, 3) {
			t.Fatalf("actual null standard stream: %+v", row)
		}
	}
	bad := identity
	bad.StartTicks++
	if rows, err := observeGitDescriptors(context.Background(), bad, pidfd); err == nil || rows != nil {
		t.Fatal("substituted process accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := observeGitDescriptors(ctx, identity, pidfd); err == nil || rows != nil {
		t.Fatal("canceled observation accepted")
	}
	if after := gitPacketFDCount(t); before != after {
		t.Fatalf("descriptor leak: %d -> %d", before, after)
	}
}
