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

func TestGitInheritedDescriptorAuditChild(t *testing.T) {
	kind := os.Getenv("SFL_GIT_FD_AUDIT")
	if kind == "" {
		return
	}
	worker := os.Getenv("SFL_GIT_FD_WORKER") == "1"
	last := 4
	if worker {
		last = 6
	}
	if kind == "high-fd" {
		// A direct exec preserves an unexpected non-CLOEXEC descriptor even
		// when it is outside os/exec's explicitly supplied ExtraFiles range.
		if unix.Dup3(last+1, 64, 0) != nil {
			os.Exit(2)
		}
		unix.CloseOnExec(last + 1)
		env := []string{"SFL_GIT_FD_AUDIT=high-fd-ready"}
		if worker {
			env = append(env, "SFL_GIT_FD_WORKER=1")
		}
		if unix.Exec(os.Args[0], []string{os.Args[0], "-test.run=^TestGitInheritedDescriptorAuditChild$"}, env) != nil {
			os.Exit(2)
		}
	}
	if kind == "runtime-private" {
		file, err := os.Open("/dev/null")
		if err != nil {
			os.Exit(2)
		}
		defer file.Close()
	}
	err := gitProcessInheritedRoles(worker)
	wantSuccess := kind == "valid" || kind == "runtime-private"
	if (err == nil) != wantSuccess {
		fmt.Fprintln(os.Stderr, "inherited descriptor policy mismatch", kind, err)
		os.Exit(3)
	}
	// Refusal never takes ownership of unknown descriptors or runtime FDs.
	if !wantSuccess {
		fd := last + 1
		if kind == "high-fd-ready" {
			fd = 64
		}
		if kind == "std-socket" {
			fd = 0
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			os.Exit(4)
		}
	}
	os.Exit(0)
}

func TestGitInheritedDescriptorAuditActualExec(t *testing.T) {
	for _, worker := range []bool{false, true} {
		for _, kind := range []string{"valid", "runtime-private", "extra-file", "extra-socket", "extra-pipe", "high-fd", "std-socket"} {
			t.Run(fmt.Sprintf("worker-%v/%s", worker, kind), func(t *testing.T) {
				image, _ := gitSealedTestImage(t)
				defer image.Close()
				supervisor := gitTestPIDFD(t, os.Getpid())
				roles := []*os.File{image, supervisor}
				if worker {
					manifest := gitGrantSealedData(t, []byte("synthetic bounded manifest"))
					read, write, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					defer read.Close()
					defer write.Close()
					roles = []*os.File{supervisor, image, manifest, write}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitInheritedDescriptorAuditChild$")
				command.Env = []string{"SFL_GIT_FD_AUDIT=" + kind}
				if worker {
					command.Env = append(command.Env, "SFL_GIT_FD_WORKER=1")
				}
				switch kind {
				case "extra-file", "high-fd":
					roles = append(roles, gitPacketFile(t))
				case "extra-socket", "std-socket":
					pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
					if err != nil {
						t.Fatal(err)
					}
					left, right := os.NewFile(uintptr(pair[0]), "inert socket"), os.NewFile(uintptr(pair[1]), "inert peer")
					defer left.Close()
					defer right.Close()
					if kind == "std-socket" {
						command.Stdin = left
					} else {
						roles = append(roles, left)
					}
				case "extra-pipe":
					read, write, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					defer read.Close()
					defer write.Close()
					roles = append(roles, read)
				}
				command.ExtraFiles = roles
				if raw, err := command.CombinedOutput(); err != nil || strings.TrimSpace(string(raw)) != "" {
					t.Fatal("actual descriptor handoff", err, string(raw))
				}
			})
		}
	}
}
