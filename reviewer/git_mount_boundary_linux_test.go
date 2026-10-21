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
	slave := strings.Replace(raw, "ro,relatime -", "ro,relatime master:9 propagate_from:8 -", 1)
	rows, err = parseGitMountTable([]byte(slave))
	if err != nil || rows["/"].Master != 9 || rows["/"].Propagate != 8 || validateGitMountLayout(rows, binding) != nil {
		t.Fatal("kernel slave propagation fields were confused with shared propagation", err)
	}
	for _, kind := range []string{"extra", "missing", "duplicate-point", "duplicate-id", "propagation", "duplicate-master", "unknown-propagation", "invalid-propagation", "root-writable", "broker-writable", "proc-writable", "wrong-proc", "random-writable", "wrong-parent", "different-alias", "different-device", "noexec", "nosuid", "nodev", "noswap", "fake-noswap", "duplicate-option", "conflicting-flags", "bad-escape", "trailing", "nul", "bounds"} {
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
			case "duplicate-master":
				bad = strings.Replace(raw, "ro,relatime -", "ro,relatime master:9 master:8 -", 1)
			case "unknown-propagation":
				bad = strings.Replace(raw, "ro,relatime -", "ro,relatime unknown:9 -", 1)
			case "invalid-propagation":
				bad = strings.Replace(raw, "ro,relatime -", "ro,relatime propagate_from:9 -", 1)
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

func TestGitMountNamespaceChild(t *testing.T) {
	if os.Getenv("SFL_GIT_MOUNT_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "/broker/worker.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if mode := os.Getenv("SFL_GIT_DESCRIPTOR_CHILD"); mode != "" {
		supervisor := os.NewFile(3, "fixture-supervisor")
		peer, err := authenticateGitSupervisor(ctx, conn, supervisor)
		supervisor.Close()
		if err != nil {
			t.Fatal("fixture supervisor", err)
		}
		defer peer.Close()
		unix.CloseOnExec(4)
		if mode == "clean" {
			unix.Close(5)
		} else if mode != "inheritable" {
			unix.CloseOnExec(5)
		}
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil || writeGitControlPacket(ctx, conn, []byte("ready"), nil) != nil {
		t.Fatal("inert mount readiness")
	}
	packet, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(packet) != "finish" {
		t.Fatal("inert mount completion", err)
	}
	os.Exit(0)
}

func gitMountInertRuntime(t *testing.T, ctx context.Context) (*os.File, *os.File, GitRuntimePins) {
	t.Helper()
	build := actualGitRuntime(t)
	var manifest gitRuntimeManifest
	if json.Unmarshal(build.Manifest, &manifest) != nil {
		t.Fatal("runtime manifest")
	}
	prepared, err := prepareGitRuntime(ctx, nativeRuntimePrivateParent(t), bytes.NewReader(build.Artifact), build.Pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{}
	for _, file := range manifest.Files {
		bodies[file.Path], err = prepared.root.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if prepared.Close() != nil {
		t.Fatal("original runtime disposal")
	}
	image, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	manifest.Git = VMAssetPin{gitRawHash(image), int64(len(image))}
	manifest.HTTP = manifest.Git
	for i := range manifest.Files {
		if manifest.Files[i].Path == gitRuntimeProgram || manifest.Files[i].Path == gitRuntimeHTTP {
			manifest.Files[i].SHA256, manifest.Files[i].Bytes = manifest.Git.SHA256, manifest.Git.Bytes
			bodies[manifest.Files[i].Path] = image
		}
	}
	artifact, pins := gitRuntimeFrame(t, manifest, bodies)
	root, err := prepareGitRuntime(ctx, nativeRuntimePrivateParent(t), bytes.NewReader(artifact), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	fd, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	exe, err := root.root.Open(gitRuntimeProgram)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exe.Close() })
	return fd, exe, pins
}

func TestGitMountActualPrivateViewRefusesUnqualifiedWorkspace(t *testing.T) {
	gitMountPrivateFixture(t, "")
}

func TestGitDescriptorActualPrivateWorker(t *testing.T) {
	if os.Getenv("SFL_GIT_DESCRIPTOR_KERNEL") != "1" {
		t.Skip("explicit aggregate descriptor qualification using actual private worker roles")
	}
	for _, mode := range []string{"clean", "host-file", "host-directory", "internet-socket", "inheritable", "extra-eventfd", "extra-epoll"} {
		t.Run(mode, func(t *testing.T) { gitMountPrivateFixture(t, mode) })
	}
}

func gitMountPrivateFixture(t *testing.T, descriptorMode string) {
	t.Helper()
	if os.Getenv("SFL_GIT_BOOTSTRAP_NAMESPACE") != "1" {
		t.Skip("explicit inert private-namespace mount observation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, exe, pins := gitMountInertRuntime(t, ctx)
	listener, _, directory := gitSupervisorListener(t)
	gitSocket := filepath.Join(directory, "git.sock")
	unusedBroker, err := net.ListenUnix("unix", &net.UnixAddr{Name: gitSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer unusedBroker.Close()
	if os.Chmod(gitSocket, 0600) != nil || os.WriteFile(filepath.Join(directory, "git-read.used"), []byte("sfl-git-read-broker-1\n"), 0600) != nil {
		t.Fatal("inert broker directory")
	}
	broker, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	// This trusted empty fixture deliberately uses an unqualified host
	// directory. It must NEVER pass the production workspace policy.
	workspace := t.TempDir()
	if os.Chmod(workspace, 0700) != nil {
		t.Fatal("private empty workspace fixture")
	}
	work, err := os.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer work.Close()
	alias, err := duplicateGitControlFile(work)
	if err != nil {
		t.Fatal(err)
	}
	defer alias.Close()
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
	args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--setenv", "SFL_GIT_MOUNT_CHILD", "1", "--ro-bind-fd", "3", "/", "--ro-bind-fd", "4", "/broker", "--bind-fd", "6", "/work", "--bind-fd", "7", "/tmp", "--proc", "/proc", "--remount-ro", "/proc", "--dev-bind", "/dev/null", "/dev/null", "--dev-bind", "/dev/urandom", "/dev/urandom", "--remount-ro", "/dev/urandom", "--chdir", "/", "--seccomp", "5", "--", "/usr/bin/git", "-test.run=^TestGitMountNamespaceChild$"}
	command := exec.CommandContext(ctx, fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), helper.Fd()), args...)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{root, broker, filter, work, alias}
	var exportRead, supervisor *os.File
	if descriptorMode != "" {
		var exportWrite *os.File
		exportRead, exportWrite, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer exportRead.Close()
		defer exportWrite.Close()
		fd, err := unix.PidfdOpen(os.Getpid(), 0)
		if err != nil {
			t.Fatal(err)
		}
		supervisor = os.NewFile(uintptr(fd), "fixture-supervisor")
		defer supervisor.Close()
		var extra *os.File
		switch descriptorMode {
		case "extra-eventfd", "extra-epoll":
			var fd int
			if descriptorMode == "extra-eventfd" {
				fd, err = unix.Eventfd(0, unix.EFD_CLOEXEC)
			} else {
				fd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC)
			}
			if err == nil {
				extra = os.NewFile(uintptr(fd), "fixture-extra-poller")
			}
		case "host-directory":
			extra, err = os.Open(t.TempDir())
		case "internet-socket":
			var fd int
			fd, err = unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				extra = os.NewFile(uintptr(fd), "fixture-inherited-internet")
			}
		default:
			extra, err = os.CreateTemp(t.TempDir(), "outside-worker-")
		}
		if err != nil {
			t.Fatal(err)
		}
		defer extra.Close()
		args = []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--setenv", "SFL_GIT_MOUNT_CHILD", "1", "--setenv", "SFL_GIT_DESCRIPTOR_CHILD", descriptorMode, "--ro-bind-fd", "6", "/", "--ro-bind-fd", "7", "/broker", "--bind-fd", "9", "/work", "--bind-fd", "10", "/tmp", "--proc", "/proc", "--remount-ro", "/proc", "--dev-bind", "/dev/null", "/dev/null", "--dev-bind", "/dev/urandom", "/dev/urandom", "--remount-ro", "/dev/urandom", "--chdir", "/", "--seccomp", "8", "--", "/usr/bin/git", "-test.run=^TestGitMountNamespaceChild$"}
		command = exec.CommandContext(ctx, fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), helper.Fd()), args...)
		command.Env = []string{}
		command.ExtraFiles = []*os.File{supervisor, exportWrite, extra, root, broker, filter, work, alias}
		// The fixed production launcher uses null standard streams. Preserve
		// that actual descriptor policy in this inert fixture as well.
	}
	var output bytes.Buffer
	if descriptorMode == "" {
		command.Stdout, command.Stderr = &output, &output
	}
	if command.Start() != nil {
		t.Fatal("inert mount namespace launch")
	}
	waited := false
	defer func() {
		if !waited {
			command.Process.Kill()
			command.Wait()
		}
		if t.Failed() {
			t.Log("inert mount fixture:", output.String())
		}
	}()
	listener.SetDeadline(time.Now().Add(8 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal("inert namespace control", err)
	}
	defer conn.Close()
	packet, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil || string(packet) != "ready" {
		t.Fatal("inert namespace readiness", err)
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.kernel.Close()
	identity, original, err := retainGitBrokerPeer(peer, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(original)
	raw, err := gitReadMountInfo(ctx, identity, original)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseGitMountTable(raw)
	if err != nil || len(rows) != 7 {
		t.Fatalf("actual fixed mount structure: %v\n%s", err, raw)
	}
	for _, point := range []string{"/", "/broker", "/work", "/tmp", "/proc", "/dev/null", "/dev/urandom"} {
		if rows[point].ID == 0 {
			t.Fatal("expected actual private mount missing", point)
		}
	}
	view, err := os.OpenRoot(fmt.Sprintf("/proc/%d/root", identity.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	insideWork, err := view.Stat("work")
	insideTmp, aliasErr := view.Stat("tmp")
	outside, outsideErr := work.Stat()
	if err != nil || aliasErr != nil || outsideErr != nil || !os.SameFile(insideWork, insideTmp) || !os.SameFile(insideWork, outside) {
		t.Fatal("original worker root did not expose its actual mounted aliases", err, aliasErr, outsideErr)
	}
	boundary, err := retainGitProcessBoundary(ctx, identity, original, root, exe, pins.Git, pins)
	if err != nil {
		t.Fatal("actual process boundary", err)
	}
	defer boundary.Close()
	if descriptorMode != "" {
		rows, err := observeGitDescriptors(ctx, identity, original)
		if err != nil {
			duplicate, duplicateErr := unix.PidfdGetfd(original, 0, 0)
			if duplicateErr == nil {
				unix.Close(duplicate)
			}
			t.Log("actual original-pidfd standard-input duplication", duplicateErr)
			names, readErr := os.ReadDir(fmt.Sprintf("/proc/%d/fd", identity.PID))
			t.Log("actual fd directory access", readErr)
			for _, name := range names {
				fd, parseErr := strconv.Atoi(name.Name())
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				duplicate, duplicateErr := unix.PidfdGetfd(original, fd, 0)
				if duplicateErr == nil {
					unix.Close(duplicate)
				}
				t.Log("actual original-pidfd descriptor duplication", fd, duplicateErr)
			}
			t.Fatal("actual descriptor observation", err)
		}
		result := checkGitDescriptorBoundary(ctx, boundary, exportRead, supervisor, conn)
		if descriptorMode == "clean" && result != nil {
			t.Fatalf("fixed descriptor roles refused: %v\n%+v", result, rows)
		}
		if descriptorMode != "clean" && result == nil {
			t.Fatalf("inherited %s accepted: %+v", descriptorMode, rows)
		}
	}
	binding, _ := gitMountTableFixture(t)
	if checkGitMountBoundary(ctx, boundary, broker, binding) == nil {
		t.Fatal("unqualified host workspace became a source admission proof")
	}
	if writeGitControlPacket(ctx, conn, []byte("finish"), nil) != nil || command.Wait() != nil {
		t.Fatal("inert namespace completion")
	}
	waited = true
}
