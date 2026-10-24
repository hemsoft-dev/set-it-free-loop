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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// gitProcessLineage lists from and its ancestors up to and including to: every
// process a helper ancestry check may retain or observe.
func gitProcessLineage(t *testing.T, from, to int) []int {
	t.Helper()
	var lineage []int
	for pid := from; ; {
		lineage = append(lineage, pid)
		if pid == to {
			return lineage
		}
		if pid <= 1 || len(lineage) > 8 {
			t.Fatal("process is not below the expected ancestor", from, to)
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		end := bytes.LastIndexByte(stat, ')')
		if err != nil || end < 0 {
			t.Fatal("process ancestry", pid, err)
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 2 {
			t.Fatal("process ancestry", pid)
		}
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			t.Fatal("process ancestry", pid, err)
		}
	}
}

func TestGitProcessObservationUsesActualKernelIdentityAndBorrowedFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "/usr/bin/sleep", "9")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	identity, err := processGateIdentity(child.Process.Pid, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	pidfd := gitTestPIDFD(t, child.Process.Pid)
	root, err := os.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	exe, err := os.Open("/usr/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()
	body, err := os.ReadFile("/usr/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	pin := VMAssetPin{gitRawHash(body), int64(len(body))}
	wantRoot, _ := gitStatFileID(root)
	wantExe, _ := gitStatFileID(exe)
	watch := gitWatchFDs(t, gitFDFiles(t, root, exe), gitFDProcfs(t, child.Process.Pid), gitFDPIDFDs(child.Process.Pid))
	got, err := observeGitProcess(ctx, identity, int(pidfd.Fd()), root, exe, pin)
	if err != nil {
		t.Fatal("live executable observation", err)
	}
	if got.Identity != identity || got.ParentPID != os.Getpid() || got.Root != wantRoot || got.Executable != wantExe || len(got.Namespaces) != 6 || len(got.Capabilities) != 5 || got.Cgroup == "" {
		t.Fatal("incomplete actual kernel observation", got)
	}
	for _, id := range got.UIDs {
		if id != uint32(os.Getuid()) {
			t.Fatal("actual child UID", got.UIDs)
		}
	}
	for _, id := range got.GIDs {
		if id != uint32(os.Getgid()) {
			t.Fatal("actual child GID", got.GIDs)
		}
	}
	mapped := false
	for _, entry := range got.ExecutableMaps {
		if entry.File == wantExe {
			mapped = true
		}
	}
	if !mapped {
		t.Fatal("actual executable is absent from mapped executable inventory", got.ExecutableMaps)
	}
	for name, id := range got.Namespaces {
		file, err := os.Open("/proc/self/ns/" + name)
		if err != nil {
			t.Fatal(err)
		}
		actual, statErr := gitStatFileID(file)
		file.Close()
		if statErr != nil || gitNamespaceIdentity(actual) != id {
			t.Fatal("ordinary child unexpectedly changed namespace", name, statErr)
		}
	}
	if leaked := watch.leaked(t); len(leaked) != 0 {
		t.Fatal("observation leaked descriptors", leaked)
	}
	if _, err := root.Stat(); err != nil {
		t.Fatal("borrowed root was closed", err)
	}
	if _, err := exe.Stat(); err != nil {
		t.Fatal("borrowed executable was closed", err)
	}
	if gitCheckProcessPIDFD(identity, int(pidfd.Fd())) != nil {
		t.Fatal("borrowed original pidfd was closed")
	}

	copyPath := filepath.Join(t.TempDir(), "same-bytes")
	if os.WriteFile(copyPath, body, 0500) != nil {
		t.Fatal("package copied executable")
	}
	copied, err := os.Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	wrongRoot, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer wrongRoot.Close()
	wrongPIDFD := gitTestPIDFD(t, os.Getpid())
	cancelled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	for _, kind := range []string{"copied-inode", "root", "hash", "size", "identity", "pidfd", "not-pidfd", "nil-root", "nil-executable", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			identity, fd, root, exe, pin, ctx := identity, int(pidfd.Fd()), root, exe, pin, ctx
			switch kind {
			case "copied-inode":
				exe = copied
			case "root":
				root = wrongRoot
			case "hash":
				pin.SHA256 = Digest("different executable")
			case "size":
				pin.Bytes++
			case "identity":
				identity.StartTicks++
			case "pidfd":
				fd = int(wrongPIDFD.Fd())
			case "not-pidfd":
				fd = int(exe.Fd())
			case "nil-root":
				root = nil
			case "nil-executable":
				exe = nil
			case "cancelled":
				ctx = cancelled
			}
			watch := gitWatchFDs(t, gitFDFiles(t, root, exe, copied, wrongRoot, wrongPIDFD), gitFDProcfs(t, child.Process.Pid), gitFDPIDFDs(child.Process.Pid, os.Getpid()))
			got, err := observeGitProcess(ctx, identity, fd, root, exe, pin)
			if err == nil || !reflect.DeepEqual(got, gitProcessObservation{}) {
				t.Fatal("bad process evidence returned an observation", got, err)
			}
			if leaked := watch.leaked(t); len(leaked) != 0 {
				t.Fatal("refusal leaked descriptors", leaked)
			}
		})
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if n, err := unix.Poll([]unix.PollFd{{Fd: int32(pidfd.Fd()), Events: unix.POLLIN}}, 3000); err != nil || n != 1 {
		t.Fatal("child did not terminate", err)
	}
	if got, err := observeGitProcess(ctx, identity, int(pidfd.Fd()), root, exe, pin); err == nil || !reflect.DeepEqual(got, gitProcessObservation{}) {
		t.Fatal("dead process accepted", got, err)
	}
}

func TestGitProcessObservationInertNamespaceChild(t *testing.T) {
	if os.Getenv("SFL_GIT_OBSERVATION_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "/broker/worker.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if writeGitControlPacket(ctx, conn, []byte("ready"), nil) != nil {
		t.Fatal("ready")
	}
	stage, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "seal" {
		t.Fatal("seal instruction", err)
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		t.Fatal("clear dumpability")
	}
	if writeGitControlPacket(ctx, conn, []byte("sealed"), nil) != nil {
		t.Fatal("sealed")
	}
	for _, role := range []string{"git", "helper"} {
		stage, files, err = readGitControlPacket(ctx, conn, 0)
		closeVMFiles(files)
		if err != nil || string(stage) != role {
			t.Fatal("probe instruction", role, err)
		}
		path := gitRuntimeProgram
		if role == "helper" {
			path = gitRuntimeHTTP
		}
		command := exec.CommandContext(ctx, "/"+path, "-test.run=^TestGitProcessObservationChainChild$")
		command.Env = []string{"SFL_GIT_OBSERVATION_CHAIN=" + role}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("inert chain", role, err, string(output))
		}
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "anonymous" {
		t.Fatal("mapping instruction", err)
	}
	mapping, err := unix.Mmap(-1, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil || unix.Mprotect(mapping, unix.PROT_READ|unix.PROT_EXEC) != nil {
		t.Fatal("inert anonymous executable mapping", err)
	}
	if writeGitControlPacket(ctx, conn, []byte("mapped"), nil) != nil {
		t.Fatal("mapping readiness")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "unmap" || unix.Munmap(mapping) != nil {
		t.Fatal("mapping removal", err)
	}
	if writeGitControlPacket(ctx, conn, []byte("unmapped"), nil) != nil {
		t.Fatal("unmapped readiness")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "finish" {
		t.Fatal("finish instruction", err)
	}
	os.Exit(0)
}

func TestGitProcessObservationChainChild(t *testing.T) {
	role := os.Getenv("SFL_GIT_OBSERVATION_CHAIN")
	if role == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if role == "git" {
		command := exec.CommandContext(ctx, "/"+gitRuntimeHTTP, "-test.run=^TestGitProcessObservationChainChild$")
		command.Env = []string{"SFL_GIT_OBSERVATION_CHAIN=helper"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("helper", err, string(output))
		}
		os.Exit(0)
	}
	if role != "helper" {
		t.Fatal("unknown inert role")
	}
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "/broker/worker.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if writeGitControlPacket(ctx, conn, []byte("helper"), nil) != nil {
		t.Fatal("helper readiness")
	}
	stage, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "finish" {
		t.Fatal("helper completion", err)
	}
	os.Exit(0)
}

func TestGitProcessObservationActualPrivateRootAndFinalPolicy(t *testing.T) {
	if os.Getenv("SFL_GIT_BOOTSTRAP_NAMESPACE") != "1" {
		t.Skip("explicit inert cross-namespace fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	build := actualGitRuntime(t)
	var manifest gitRuntimeManifest
	if json.Unmarshal(build.Manifest, &manifest) != nil {
		t.Fatal("runtime manifest")
	}
	bodies := map[string][]byte{}
	// This is an explicitly synthetic executable fixture, not a real Git
	// admission. Keep the measured runtime's real loader/library inventory.
	prepared, err := prepareGitRuntime(ctx, nativeRuntimePrivateParent(t), bytes.NewReader(build.Artifact), build.Pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		body, err := prepared.root.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		bodies[file.Path] = body
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bodies[gitRuntimeProgram] = image
	bodies[gitRuntimeHTTP] = image
	manifest.Git = VMAssetPin{gitRawHash(image), int64(len(image))}
	manifest.HTTP = manifest.Git
	for i := range manifest.Files {
		if manifest.Files[i].Path == gitRuntimeProgram || manifest.Files[i].Path == gitRuntimeHTTP {
			manifest.Files[i].SHA256 = manifest.Git.SHA256
			manifest.Files[i].Bytes = manifest.Git.Bytes
		}
	}
	artifact, pins := gitRuntimeFrame(t, manifest, bodies)
	root, err := prepareGitRuntime(ctx, nativeRuntimePrivateParent(t), bytes.NewReader(artifact), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rootFD, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	defer rootFD.Close()
	exe, err := root.root.Open(gitRuntimeProgram)
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()
	listener, _, directory := gitSupervisorListener(t)
	broker, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	filter, err := retainGitPayloadSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()
	helperBytes, err := os.ReadFile("/usr/bin/bwrap")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := retainNativeBootstrap(ctx, VMBootAsset{Path: "/usr/bin/bwrap", SHA256: gitRawHash(helperBytes), Bytes: int64(len(helperBytes))})
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--setenv", "SFL_GIT_OBSERVATION_CHILD", "1", "--ro-bind-fd", "3", "/", "--ro-bind-fd", "4", "/broker", "--proc", "/proc", "--remount-ro", "/proc", "--chdir", "/", "--seccomp", "5", "--", "/usr/bin/git", "-test.run=^TestGitProcessObservationInertNamespaceChild$"}
	command := exec.CommandContext(ctx, fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), helper.Fd()), args...)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{rootFD, broker, filter}
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
			t.Log("inert namespace output:", output.String())
		}
	}()
	listener.SetDeadline(time.Now().Add(8 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal("inert namespace connection", err)
	}
	defer conn.Close()
	stage, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "ready" {
		t.Fatal("readiness", err)
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.kernel.Close()
	identity, pidfd, err := retainGitBrokerPeer(peer, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	check := func(stage string) gitProcessObservation {
		t.Helper()
		got, err := observeGitProcess(ctx, identity, pidfd, rootFD, exe, pins.Git)
		if err != nil {
			t.Fatal(stage, "actual namespace observation", err)
		}
		if got.RootFlags&unix.ST_RDONLY == 0 || got.NoNewPrivileges != 1 || got.Seccomp != 2 {
			t.Fatal(stage, "actual final-policy evidence", got)
		}
		for name, value := range got.Capabilities {
			if value != 0 {
				t.Fatal(stage, "retained capability", name, value)
			}
		}
		for name, id := range got.Namespaces {
			file, err := os.Open("/proc/self/ns/" + name)
			if err != nil {
				t.Fatal(err)
			}
			actual, statErr := gitStatFileID(file)
			file.Close()
			if statErr != nil || id == gitNamespaceIdentity(actual) {
				t.Fatal(stage, "host namespace retained", name, statErr)
			}
		}
		return got
	}
	before := check("ready")
	if writeGitControlPacket(ctx, conn, []byte("seal"), nil) != nil {
		t.Fatal("seal dispatch")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "sealed" {
		t.Fatal("seal readiness", err)
	}
	after := check("nondumpable")
	if before.Executable != after.Executable || before.Root != after.Root || !reflect.DeepEqual(before.Namespaces, after.Namespaces) {
		t.Fatal("identity changed across dumpability transition")
	}
	boundary, err := retainGitProcessBoundary(ctx, identity, pidfd, rootFD, exe, pins.Git, pins)
	if err != nil {
		routes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/net/ipv6_route", identity.PID))
		t.Log("inert private-namespace IPv6 route table:", string(routes))
		t.Fatal("retain actual process boundary", err, "kernel network", after.Network)
	}
	defer boundary.Close()
	for _, role := range []string{"git", "helper"} {
		if writeGitControlPacket(ctx, conn, []byte(role), nil) != nil {
			t.Fatal("chain dispatch", role)
		}
		listener.SetDeadline(time.Now().Add(5 * time.Second))
		helperConn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal("helper connection", err)
		}
		func() {
			defer helperConn.Close()
			stage, files, err := readGitControlPacket(ctx, helperConn, 0)
			closeVMFiles(files)
			if err != nil || string(stage) != "helper" {
				t.Fatal("helper ready", err)
			}
			helper, err := gitBrokerPeer(helperConn)
			if err != nil {
				t.Fatal(err)
			}
			defer helper.kernel.Close()
			lineage := gitProcessLineage(t, helper.PID, identity.PID)
			watch := gitWatchFDs(t, gitFDFiles(t, boundary.root, boundary.image, boundary.git, boundary.http), gitFDProcfs(t, lineage...), gitFDPIDFDs(lineage...))
			err = boundary.checkHelper(ctx, helper)
			if role == "git" && err != nil || role == "helper" && err == nil {
				t.Fatal("actual helper ancestry policy", role, err)
			}
			if leaked := watch.leaked(t); len(leaked) != 0 {
				t.Fatal("ancestry leaked descriptors", leaked)
			}
			for _, forged := range []GitReadPeer{{PID: helper.PID, UID: helper.UID, GID: helper.GID}, {PID: helper.PID + 1, UID: helper.UID, GID: helper.GID, kernel: helper.kernel}} {
				if boundary.checkHelper(ctx, forged) == nil {
					t.Fatal("forged peer entered process boundary")
				}
			}
			if role == "git" {
				// The exact measured helper bytes at the Git inode are still a
				// different executable. Hash equality cannot replace role identity.
				original := boundary.http
				boundary.http = boundary.git
				wrong := boundary.checkHelper(ctx, helper)
				boundary.http = original
				if wrong == nil {
					t.Fatal("same bytes at wrong executable inode accepted")
				}
				if boundary.checkHelper(ctx, helper) != nil {
					t.Fatal("valid retained peer was damaged by refusal")
				}
			}
			if writeGitControlPacket(ctx, helperConn, []byte("finish"), nil) != nil {
				t.Fatal("helper finish")
			}
		}()
	}
	if writeGitControlPacket(ctx, conn, []byte("anonymous"), nil) != nil {
		t.Fatal("mapping dispatch")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "mapped" {
		t.Fatal("mapping readiness", err)
	}
	changed, err := observeGitProcess(ctx, identity, pidfd, rootFD, exe, pins.Git)
	if err != nil {
		t.Fatal("actual mapped process observation", err)
	}
	anonymous := false
	for _, mapping := range changed.ExecutableMaps {
		if mapping.File == (gitKernelFileID{}) && mapping.KernelName == "" {
			anonymous = true
		}
	}
	if !anonymous || boundary.checkWorker(ctx) == nil {
		t.Fatal("actual unexpected executable mapping accepted")
	}
	if writeGitControlPacket(ctx, conn, []byte("unmap"), nil) != nil {
		t.Fatal("unmap dispatch")
	}
	stage, files, err = readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(stage) != "unmapped" || boundary.checkWorker(ctx) != nil {
		t.Fatal("clean process did not recover after removing test mapping", err)
	}
	if boundary.Close() != nil || boundary.checkHelper(ctx, peer) == nil {
		t.Fatal("closed boundary admitted a peer")
	}
	if writeGitControlPacket(ctx, conn, []byte("finish"), nil) != nil {
		t.Fatal("finish dispatch")
	}
	err = command.Wait()
	waited = true
	if err != nil {
		t.Fatal("inert namespace exit", err, output.String())
	}
}
