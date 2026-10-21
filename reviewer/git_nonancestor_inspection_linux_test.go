//go:build linux

package reviewer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// This launcher exits before inspection. The inert namespace helper reparents,
// making the inspector a nonancestor without a service or host policy change.
func TestGitDescriptorNonancestorLauncher(t *testing.T) {
	raw := os.Getenv("SFL_GIT_NONANCESTOR_LAUNCHER")
	if raw == "" {
		return
	}
	var args []string
	if json.Unmarshal([]byte(raw), &args) != nil || len(args) > 128 || len(args) < 2 || args[len(args)-1] != "-test.run=^TestGitMountNamespaceChild$" {
		os.Exit(2)
	}
	files := make([]*os.File, 8)
	for i := range files {
		files[i] = os.NewFile(uintptr(i+3), "inert-fixed-role")
		unix.CloseOnExec(i + 3)
	}
	unix.CloseOnExec(11)
	command := exec.Command("/proc/self/fd/11", args...)
	command.Env = []string{}
	command.ExtraFiles = files
	if command.Start() != nil || command.Process.Release() != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func gitAssertNonancestorInspector(t *testing.T, identity GateIdentity, original int) {
	t.Helper()
	if gitCheckProcessPIDFD(identity, original) != nil {
		t.Fatal("original detached worker identity")
	}
	pid := identity.PID
	chain := []int{}
	for steps := 0; steps < 32; steps++ {
		if pid == os.Getpid() || pid <= 0 {
			t.Fatal("inspector still an ancestor", chain)
		}
		chain = append(chain, pid)
		if pid == 1 {
			break
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		end := strings.LastIndexByte(string(raw), ')')
		if err != nil || end < 0 {
			t.Fatal("detached ancestry", err)
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 2 {
			t.Fatal("detached ancestry fields")
		}
		pid, err = strconv.Atoi(fields[1])
		if err != nil {
			t.Fatal(err)
		}
	}
	if chain[len(chain)-1] != 1 || gitCheckProcessPIDFD(identity, original) != nil {
		t.Fatal("ancestry or original identity not proven", chain)
	}
	namespace, err := os.Open(fmt.Sprintf("/proc/%d/ns/user", identity.PID))
	if err != nil {
		t.Fatal("target user namespace", err)
	}
	defer namespace.Close()
	owner, err := unix.IoctlGetUint32(int(namespace.Fd()), unix.NS_GET_OWNER_UID)
	if err != nil || owner != uint32(os.Geteuid()) {
		t.Fatal("inspector does not own target user namespace", owner, err)
	}
	inspector, err := os.Open("/proc/self/ns/user")
	if err != nil {
		t.Fatal(err)
	}
	defer inspector.Close()
	inspectorID, err := gitStatFileID(inspector)
	if err != nil {
		t.Fatal(err)
	}
	current := namespace
	parentOwned := false
	for depth := 0; depth < 8; depth++ {
		fd, err := unix.IoctlRetInt(int(current.Fd()), unix.NS_GET_PARENT)
		if err != nil {
			t.Fatal("target user namespace ancestry", err)
		}
		parent := os.NewFile(uintptr(fd), "inert-user-namespace-parent")
		defer parent.Close()
		parentID, err := gitStatFileID(parent)
		if err != nil {
			t.Fatal(err)
		}
		if parentID == inspectorID {
			uid, err := unix.IoctlGetUint32(int(current.Fd()), unix.NS_GET_OWNER_UID)
			if err != nil || uid != uint32(os.Geteuid()) {
				t.Fatal("inspector's child user namespace owner", uid, err)
			}
			parentOwned = true
			break
		}
		current = parent
	}
	if !parentOwned {
		t.Fatal("target user namespace is not a descendant owned by inspector")
	}
	t.Logf("inspector=%d nonancestor worker chain=%v target user namespace owner=%d", os.Getpid(), chain, owner)
}

func TestGitDescriptorActualNonancestorNondumpableWorker(t *testing.T) {
	if os.Getenv("SFL_GIT_DESCRIPTOR_KERNEL") != "1" {
		t.Skip("explicit inert nonancestor inspection qualification")
	}
	gitMountPrivateFixture(t, "clean", true)
}
