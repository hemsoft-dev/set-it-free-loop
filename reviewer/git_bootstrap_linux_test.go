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
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type gitBootstrapTestRequest struct {
	Binding              gitMaterializationBinding
	Endpoint, Invocation string
	SupervisorPID        int
}

func TestGitBootstrapRootChild(t *testing.T) {
	if os.Getenv("SFL_GIT_BOOTSTRAP_ROOT") != "1" {
		return
	}
	var input gitBootstrapTestRequest
	if json.Unmarshal([]byte(os.Getenv("SFL_GIT_BOOTSTRAP_REQUEST")), &input) != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	image, supervisor := os.NewFile(3, "image"), os.NewFile(4, "supervisor")
	assets, err := receiveGitBootstrap(ctx, input.Endpoint, input.Binding, input.Invocation, input.SupervisorPID, image, supervisor)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap refused")
		os.Exit(1)
	}
	if _, err := image.Stat(); err == nil {
		os.Exit(3)
	}
	if _, err := supervisor.Stat(); err == nil {
		os.Exit(3)
	}
	if assets.manifest.Version != 2 || len(assets.files) != 8 {
		os.Exit(3)
	}
	for _, file := range assets.files {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			os.Exit(3)
		}
	}
	if assets.Close() != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("bootstrap-ok\n")
	os.Exit(0)
}

func TestGitBootstrapActualSealedRootAndDescriptorGrant(t *testing.T) {
	// These root receiver children never launch bwrap or Git. Their grants use
	// real kernel descriptors. One case uses the actual ELF runtime producer;
	// other runtime bytes are explicitly synthetic. No unit qualification.
	for _, kind := range []string{"success", "actual-cli-refusal", "wrong-supervisor", "wrong-diagnostic", "missing-grant", "wrong-binding", "wrong-recipient", "wrong-runtime", "wrong-granted-supervisor", "copied-worker", "ordinary-root", "legacy-manifest"} {
		t.Run(kind, func(t *testing.T) {
			source := os.Args[0]
			if kind == "actual-cli-refusal" {
				var fs unix.Statfs_t
				info, err := os.Stat("/tmp")
				if err != nil || unix.Statfs("/tmp", &fs) != nil {
					t.Fatal("host tmpfs observation", err)
				}
				if info.Mode().Perm() == 0700 && fs.Type == unix.TMPFS_MAGIC {
					t.Skip("requires a host with incompatible /tmp, no launch qualification")
				}
				source = buildGitAcquisitionTestCLI(t)
			}
			// The build has its own deadline. It must not consume the live
			// descriptor handshake's observation budget on a cold compiler cache.
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			image, imagePin := gitSealExecutableAt(t, source)
			var build GitRuntimeBuild
			if kind == "success" {
				build = actualGitRuntime(t)
			} else {
				m, bodies := gitRuntimeFixture(t)
				artifact, pins := gitRuntimeFrame(t, m, bodies)
				build = GitRuntimeBuild{Pins: pins, Artifact: artifact}
			}
			runtimeRoot, err := prepareGitRuntime(ctx, nativeRuntimePrivateParent(t), bytes.NewReader(build.Artifact), build.Pins, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeRoot.Close()
			runtimeFD, err := runtimeRoot.directoryFD()
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeFD.Close()
			parent, err := os.MkdirTemp("", "sfl-git-root-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(parent)
			endpoint := filepath.Join(parent, "bootstrap.sock")
			listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: endpoint, Net: "unixpacket"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			brokerPath := filepath.Join(parent, "broker")
			if os.Mkdir(brokerPath, 0700) != nil {
				t.Fatal("broker directory")
			}
			for _, name := range []string{"git.sock", "worker.sock"} {
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(brokerPath, name), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if os.Chmod(filepath.Join(brokerPath, name), 0600) != nil {
					t.Fatal("socket permissions")
				}
			}
			if os.WriteFile(filepath.Join(brokerPath, "git-read.used"), []byte("sfl-git-read-broker-1\n"), 0600) != nil {
				t.Fatal("marker")
			}
			broker, err := os.Open(brokerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			filter, err := retainGitPayloadSeccomp()
			if err != nil {
				t.Fatal(err)
			}
			defer filter.Close()
			filterPin, err := gitFixedFilterPin()
			if err != nil {
				t.Fatal(err)
			}
			helperRaw, err := os.ReadFile("/usr/bin/bwrap")
			if err != nil {
				t.Fatal(err)
			}
			helperPin := VMAssetPin{gitRawHash(helperRaw), int64(len(helperRaw))}
			helper, err := retainNativeBootstrap(ctx, VMBootAsset{"/usr/bin/bwrap", helperPin.SHA256, helperPin.Bytes})
			if err != nil {
				t.Fatal(err)
			}
			defer helper.Close()
			manifest := gitGrantManifest{2, "isolated-git-acquisition", build.Pins, helperPin, imagePin, filterPin}
			if kind == "legacy-manifest" {
				manifest.Version = 1
			}
			binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("synthetic release"), Digest(manifest), testJob().Snapshot}
			rawManifest, _ := json.Marshal(manifest)
			manifestFD := gitGrantSealedData(t, rawManifest)
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			supervisor := gitTestPIDFD(t, os.Getpid())
			other := exec.CommandContext(ctx, "/usr/bin/sleep", "10")
			if other.Start() != nil {
				t.Fatal("inert other process")
			}
			defer func() { other.Process.Kill(); other.Wait() }()
			otherSupervisor := gitTestPIDFD(t, other.Process.Pid)
			initial := supervisor
			if kind == "wrong-supervisor" {
				initial = otherSupervisor
			}
			request := gitBootstrapTestRequest{binding, endpoint, strings.Repeat("b", 32), os.Getpid()}
			if kind == "wrong-diagnostic" {
				request.SupervisorPID = other.Process.Pid
			}
			rawRequest, _ := json.Marshal(request)
			program := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), image.Fd())
			if kind == "ordinary-root" {
				program = os.Args[0]
			}
			arguments := []string{"-test.run=^TestGitBootstrapRootChild$"}
			environment := []string{"SFL_GIT_BOOTSTRAP_ROOT=1", "SFL_GIT_BOOTSTRAP_REQUEST=" + string(rawRequest)}
			if kind == "actual-cli-refusal" {
				raw, _ := jsonMarshalGitBinding(binding)
				arguments = []string{endpoint, raw, Digest(binding), fmt.Sprint(os.Getpid())}
				environment = []string{"INVOCATION_ID=" + request.Invocation}
			}
			child := exec.CommandContext(ctx, program, arguments...)
			child.Env = environment
			child.ExtraFiles = []*os.File{image, initial}
			var terminal bytes.Buffer
			child.Stdout = &terminal
			child.Stderr = &terminal
			if child.Start() != nil {
				t.Fatal("sealed root start")
			}
			waited := false
			defer func() {
				if !waited {
					child.Process.Kill()
					child.Wait()
				}
			}()
			listener.SetDeadline(time.Now().Add(3 * time.Second))
			conn, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			raw, extra, helloErr := readGitControlPacket(ctx, conn, 0)
			closeVMFiles(extra)
			if kind == "wrong-supervisor" || kind == "wrong-diagnostic" {
				if helloErr == nil {
					t.Fatal("unauthenticated bootstrap hello")
				}
			} else {
				var hello gitBootstrapHello
				if helloErr != nil || json.Unmarshal(raw, &hello) != nil || hello.Version != 1 || hello.Binding != binding || hello.Identity.PID != child.Process.Pid || hello.Identity.InvocationID != request.Invocation || hello.ManifestHash != binding.RuntimeHash {
					t.Fatal("root hello", helloErr, string(raw))
				}
				if kind == "missing-grant" {
					conn.Close()
				} else {
					packet := gitUnitGrantPacket{1, gitBootstrapGrant, binding, hello.Identity, binding.RuntimeHash}
					files := []*os.File{manifestFD, runtimeFD, helper, image, filter, broker, write, supervisor}
					switch kind {
					case "wrong-binding":
						packet.Binding.Attempt = strings.Repeat("c", 32)
					case "wrong-recipient":
						packet.Recipient.StartTicks++
					case "wrong-runtime":
						files[1] = gitGrantSealedData(t, build.Artifact)
					case "wrong-granted-supervisor":
						files[7] = otherSupervisor
					case "copied-worker":
						copy, _ := gitSealedTestImage(t)
						files[3] = copy
					}
					raw, _ := json.Marshal(packet)
					if writeGitControlPacket(ctx, conn, raw, files) != nil {
						t.Fatal("bootstrap send")
					}
				}
			}
			err = child.Wait()
			waited = true
			if kind == "success" {
				if err != nil || terminal.String() != "bootstrap-ok\n" {
					t.Fatal("sealed root grant", err, terminal.String())
				}
				if runtimeRoot.verify() != nil {
					t.Fatal("receiver deleted or changed caller runtime")
				}
			} else if kind == "actual-cli-refusal" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 || terminal.Len() != 0 {
					t.Fatal("fixed CLI did not refuse incompatible host", err, terminal.String())
				}
			} else if err == nil || !strings.Contains(terminal.String(), "bootstrap refused") {
				t.Fatal("bad root/grant accepted", err, terminal.String())
			}
		})
	}
}
