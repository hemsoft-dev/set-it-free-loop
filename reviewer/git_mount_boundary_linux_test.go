//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Synthetic kernel-format data tests parsing/policy only, never qualification.
func gitMountTableFixture(t *testing.T) (gitMaterializationBinding, string) {
	t.Helper()
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testJob().Snapshot}
	work := "/git-work-" + binding.Attempt
	raw := "100 99 8:1 /private/runtime / ro,relatime - ext4 /dev/root rw\n" +
		"101 100 8:1 /private/broker /broker ro,relatime - ext4 /dev/root rw\n" +
		"102 100 0:42 " + work + " /work rw,nosuid,nodev,noexec - tmpfs tmpfs rw,noswap,size=262144k,nr_inodes=65536\n" +
		"103 100 0:42 " + work + " /tmp rw,nosuid,nodev,noexec - tmpfs tmpfs rw,noswap,size=262144k,nr_inodes=65536\n" +
		"104 100 0:43 / /proc ro,nosuid,nodev,noexec - proc proc rw\n" +
		"105 100 0:5 /null /dev/null rw,nosuid,noexec - devtmpfs udev rw\n" +
		"106 100 0:5 /urandom /dev/urandom ro,nosuid,noexec - devtmpfs udev rw\n"
	return binding, raw
}

func TestGitMountTableFixedLayoutAndRefusals(t *testing.T) {
	binding, raw := gitMountTableFixture(t)
	rows, err := parseGitMountTable([]byte(raw))
	if err != nil || len(rows) != 7 || validateGitMountLayout(rows, binding) != nil || rows["/work"].Device != unix.Mkdev(0, 42) {
		t.Fatal("fixed mount policy", err)
	}
	for _, kind := range []string{"extra", "missing", "duplicate-point", "duplicate-id", "propagation", "root-writable", "broker-writable", "proc-writable", "wrong-proc", "random-writable", "wrong-parent", "different-alias", "different-device", "noexec", "nosuid", "nodev", "noswap", "fake-noswap", "duplicate-option", "conflicting-flags", "bad-escape", "trailing", "nul", "bounds"} {
		t.Run(kind, func(t *testing.T) {
			bad := raw
			switch kind {
			case "extra":
				bad += "107 100 8:1 /home /home rw - ext4 /dev/root rw\n"
			case "missing":
				bad = strings.Split(raw, "106 ")[0]
			case "duplicate-point":
				bad = strings.Replace(raw, "/tmp rw,", "/work rw,", 1)
			case "duplicate-id":
				bad = strings.Replace(raw, "103 100", "102 100", 1)
			case "propagation":
				bad = strings.Replace(raw, "ro,relatime -", "ro,relatime shared:9 -", 1)
			case "root-writable":
				bad = strings.Replace(raw, "/ ro,relatime", "/ rw,relatime", 1)
			case "broker-writable":
				bad = strings.Replace(raw, "/broker ro,", "/broker rw,", 1)
			case "proc-writable":
				bad = strings.Replace(raw, "/proc ro,", "/proc rw,", 1)
			case "wrong-proc":
				bad = strings.Replace(raw, "- proc proc rw", "- ext4 /dev/root rw", 1)
			case "random-writable":
				bad = strings.Replace(raw, "/dev/urandom ro,", "/dev/urandom rw,", 1)
			case "wrong-parent":
				bad = strings.Replace(raw, "102 100", "102 101", 1)
			case "different-alias":
				bad = strings.Replace(raw, "103 100 0:42 /git-work-", "103 100 0:42 /other-", 1)
			case "different-device":
				bad = strings.Replace(raw, "103 100 0:42", "103 100 0:44", 1)
			case "noexec", "nosuid", "nodev":
				bad = strings.Replace(raw, ","+kind, "", 1)
			case "noswap":
				bad = strings.Replace(raw, ",noswap,", ",", 1)
			case "fake-noswap":
				bad = strings.Replace(raw, ",noswap,", ",noswap=1,", 1)
			case "duplicate-option":
				bad = strings.Replace(raw, "ro,relatime", "ro,ro,relatime", 1)
			case "conflicting-flags":
				bad = strings.Replace(raw, "ro,relatime", "ro,rw,relatime", 1)
			case "bad-escape":
				bad = strings.Replace(raw, "/private/runtime", `/private\057runtime`, 1)
			case "trailing":
				bad = strings.TrimSuffix(raw, "\n")
			case "nul":
				bad += "\x00"
			case "bounds":
				bad = strings.Repeat(raw, 256)
			}
			rows, err := parseGitMountTable([]byte(bad))
			if err == nil && validateGitMountLayout(rows, binding) == nil {
				t.Fatal("unsafe or ambiguous mount layout accepted")
			}
		})
	}
}

func TestGitMountWorkFilesystemIndependentBounds(t *testing.T) {
	fs := unix.Statfs_t{Type: unix.TMPFS_MAGIC, Bsize: 4096, Blocks: 65536, Files: 65536, Flags: unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC}
	if !gitBoundedWorkFilesystem(fs) {
		t.Fatal("fixed scalar filesystem policy")
	}
	for _, kind := range []string{"filesystem", "zero-page", "capacity", "overflow", "inodes", "readonly", "exec", "devices", "suid"} {
		bad := fs
		switch kind {
		case "filesystem":
			bad.Type = unix.EXT4_SUPER_MAGIC
		case "zero-page":
			bad.Bsize = 0
		case "capacity":
			bad.Blocks++
		case "overflow":
			bad.Blocks += 1 << 52
		case "inodes":
			bad.Files++
		case "readonly":
			bad.Flags |= unix.ST_RDONLY
		case "exec":
			bad.Flags &^= unix.ST_NOEXEC
		case "devices":
			bad.Flags &^= unix.ST_NODEV
		case "suid":
			bad.Flags &^= unix.ST_NOSUID
		}
		if gitBoundedWorkFilesystem(bad) {
			t.Fatal("wrong independent filesystem policy accepted", kind)
		}
	}
}

func TestGitMountReadUsesOriginalLivePIDFD(t *testing.T) {
	child := exec.Command("/usr/bin/sleep", "10")
	child.Env = []string{}
	if child.Start() != nil {
		t.Fatal("inert mount observation child")
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	identity, err := processGateIdentity(child.Process.Pid, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	pidfd := gitTestPIDFD(t, child.Process.Pid)
	before := gitPacketFDCount(t)
	got, err := gitReadMountInfo(context.Background(), identity, int(pidfd.Fd()))
	want, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", child.Process.Pid))
	if err != nil || readErr != nil || len(got) == 0 || string(got) != string(want) || gitPacketFDCount(t) != before {
		t.Fatal("actual mount observation changed bytes or leaked descriptors", err, readErr)
	}
	wrong := identity
	wrong.StartTicks++
	if raw, err := gitReadMountInfo(context.Background(), wrong, int(pidfd.Fd())); err == nil || raw != nil {
		t.Fatal("wrong process identity borrowed mount view")
	}
	child.Process.Kill()
	child.Wait()
	if raw, err := gitReadMountInfo(context.Background(), identity, int(pidfd.Fd())); err == nil || raw != nil {
		t.Fatal("dead original process retained mount authority")
	}
}
