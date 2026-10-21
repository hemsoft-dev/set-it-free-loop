//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type gitWorkerTestRequest struct {
	Binding                                   gitMaterializationBinding
	InvocationID, ControlPath, Git, Workspace string
	Endpoint                                  GitReadEndpoint
	Timeout                                   time.Duration
}

func TestGitAcquisitionWorkerChild(t *testing.T) {
	if os.Getenv("SFL_GIT_WORKER_CHILD") != "1" {
		return
	}
	var request gitWorkerTestRequest
	if json.Unmarshal([]byte(os.Getenv("SFL_GIT_WORKER_REQUEST")), &request) != nil {
		os.Exit(2)
	}
	runtime.LockOSThread()
	filter, err := gitPayloadSeccomp()
	if err != nil || unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		os.Exit(2)
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		os.Exit(2)
	}
	timeout := 8 * time.Second
	if request.Timeout > 0 && request.Timeout < timeout {
		timeout = request.Timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = runGitAcquisitionWorker(ctx, gitWorkerInput{request.Binding, request.InvocationID, request.ControlPath, request.Git, request.Workspace, request.Endpoint,
		os.NewFile(3, "supervisor"), os.NewFile(4, "image"), os.NewFile(5, "manifest"), os.NewFile(6, "export")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker refused:", err, "context:", ctx.Err())
		os.Exit(1)
	}
	os.Stdout.WriteString("worker-ok\n")
	os.Exit(0)
}

func gitSealedTestImage(t *testing.T) (*os.File, VMAssetPin) {
	t.Helper()
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker")
	if os.WriteFile(path, raw, 0500) != nil {
		t.Fatal("package image")
	}
	pin := VMAssetPin{gitRawHash(raw), int64(len(raw))}
	file, err := sealVMAsset(context.Background(), VMBootAsset{path, pin.SHA256, pin.Bytes}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file, pin
}

func TestGitAcquisitionWorkerSealedProcessGrantAndExport(t *testing.T) {
	// These children run outside bubblewrap. They prove the actual sealed
	// worker/peer/grant/parser/export code, never mini's failed inner exec path
	// or the unfinished aggregate production unit/mount/helper policy.
	for _, kind := range []string{"success", "success-sha256", "blocked-export", "wrong-supervisor", "missing-grant", "wrong-binding", "wrong-recipient", "ordinary-image"} {
		t.Run(kind, func(t *testing.T) {
			format := "sha1"
			if kind == "success-sha256" {
				format = "sha256"
			}
			fixture := snapshotGitFixtureFormat(t, format)
			if kind == "blocked-export" {
				if os.WriteFile(filepath.Join(fixture.dir, "large.txt"), []byte(strings.Repeat("source line\n", 32768)), 0600) != nil {
					t.Fatal("large synthetic blob")
				}
				fixture.run(t, "add", "large.txt")
				fixture.run(t, "commit", "-m", "large inert source export")
				fixture.snapshot.Head = strings.TrimSpace(fixture.run(t, "rev-parse", "HEAD"))
			}
			endpoint, finish := gitAcquisitionBackend(t, fixture)
			image, imagePin := gitSealedTestImage(t)
			filterPin, err := gitFixedFilterPin()
			if err != nil {
				t.Fatal(err)
			}
			// Synthetic release metadata. The factory must qualify runtime
			// files separately; no test dummy is a production release receipt.
			pin := VMAssetPin{Digest("synthetic"), 1}
			manifest := gitGrantManifest{1, "isolated-git-acquisition", GitRuntimePins{pin, pin, pin}, pin, imagePin, filterPin}
			binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("synthetic release"), Digest(manifest), fixture.snapshot}
			rawManifest, _ := json.Marshal(manifest)
			manifestFile := gitGrantSealedData(t, rawManifest)
			listener, control, _ := gitSupervisorListener(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			supervisor := gitTestPIDFD(t, os.Getpid())
			if kind == "wrong-supervisor" {
				other := exec.CommandContext(ctx, "/usr/bin/sleep", "10")
				if other.Start() != nil {
					t.Fatal("inert other process")
				}
				defer func() { other.Process.Kill(); other.Wait() }()
				supervisor = gitTestPIDFD(t, other.Process.Pid)
			}
			workspace := t.TempDir()
			os.Chmod(workspace, 0700)
			request := gitWorkerTestRequest{binding, strings.Repeat("b", 32), control, fixture.git, workspace, endpoint, 0}
			if kind == "blocked-export" {
				request.Timeout = 2 * time.Second
			}
			rawRequest, _ := json.Marshal(request)
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), image.Fd())
			if kind == "ordinary-image" {
				path = os.Args[0]
			}
			// Clear inherited capabilities before Go creates any threads. The
			// installed host session carries CAP_WAKE_ALARM in CapInh. This
			// fixture-only wrapper changes its child, never host policy.
			child := exec.CommandContext(ctx, "/usr/bin/setpriv", "--inh-caps=-all", "--no-new-privs", "--", path, "-test.run=^TestGitAcquisitionWorkerChild$")
			child.Env = []string{"SFL_GIT_WORKER_CHILD=1", "SFL_GIT_WORKER_REQUEST=" + string(rawRequest)}
			child.ExtraFiles = []*os.File{supervisor, image, manifestFile, write}
			var terminal bytes.Buffer
			child.Stdout = &terminal
			child.Stderr = &terminal
			startedAt := time.Now()
			if child.Start() != nil {
				t.Fatal("start actual sealed worker")
			}
			write.Close()
			waited := false
			defer func() {
				if !waited {
					child.Process.Kill()
					child.Wait()
				}
			}()
			payload := make(chan []byte, 1)
			if kind != "blocked-export" {
				go func() {
					raw, _ := io.ReadAll(io.LimitReader(read, gitMaterializationBlobBytes+gitMaterializationManifestBytes+snapshotPacketBytes+1<<20))
					payload <- raw
				}()
			}
			if kind != "ordinary-image" {
				listener.SetDeadline(time.Now().Add(3 * time.Second))
				conn, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal("worker connection", err)
				}
				defer conn.Close()
				raw, files, readyErr := readGitControlPacket(ctx, conn, 0)
				closeVMFiles(files)
				if kind == "wrong-supervisor" {
					if readyErr == nil {
						t.Fatal("wrong peer received readiness")
					}
				} else {
					if readyErr != nil {
						err = child.Wait()
						waited = true
						t.Fatal("worker before readiness", readyErr, err, terminal.String())
					}
					var ready gitWorkerReady
					if readyErr != nil || json.Unmarshal(raw, &ready) != nil || ready.Version != 1 || ready.Binding != binding || ready.ManifestHash != binding.RuntimeHash {
						t.Fatal("worker readiness", readyErr, string(raw))
					}
					if _, err := os.Stat(filepath.Join(workspace, "acquisition.used")); !os.IsNotExist(err) {
						t.Fatal("fetch before grant", err)
					}
					if kind != "missing-grant" {
						identity, err := processGateIdentity(child.Process.Pid, request.InvocationID)
						if err != nil {
							t.Fatal(err)
						}
						packet := gitUnitGrantPacket{1, gitWorkerGrant, binding, identity, binding.RuntimeHash}
						if kind == "wrong-binding" {
							packet.Binding.Attempt = strings.Repeat("c", 32)
						}
						if kind == "wrong-recipient" {
							packet.Recipient.StartTicks++
						}
						raw, _ := json.Marshal(packet)
						if writeGitControlPacket(ctx, conn, raw, nil) != nil {
							t.Fatal("grant send")
						}
					} else {
						conn.Close()
					}
				}
			}
			err = child.Wait()
			waited = true
			var exported []byte
			if kind == "blocked-export" {
				exported, _ = io.ReadAll(io.LimitReader(read, 1<<20))
			} else {
				select {
				case exported = <-payload:
				case <-ctx.Done():
					t.Fatal("export failed to close")
				}
			}
			audit := finish().audit
			if kind == "blocked-export" {
				if err == nil || len(exported) == 0 || len(exported) >= 32768*12 || audit.Requests < 2 || !strings.Contains(terminal.String(), "context deadline exceeded") || time.Since(startedAt) > 5*time.Second {
					t.Fatal("full export pipe did not cancel", err, terminal.String(), len(exported), audit)
				}
				if m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(exported)), binding); err == nil {
					m.Close()
					t.Fatal("truncated export accepted")
				}
			} else if strings.HasPrefix(kind, "success") {
				if err != nil || terminal.String() != "worker-ok\n" || len(exported) == 0 || audit.Requests < 2 {
					t.Fatal("actual worker failed", err, terminal.String(), len(exported), audit)
				}
				m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(exported)), binding)
				if err != nil {
					t.Fatal("matching plain export", err)
				}
				m.Close()
			} else {
				if err == nil || len(exported) != 0 || audit.Requests != 0 {
					t.Fatal("rejected worker fetched or exported", err, terminal.String(), len(exported), audit)
				}
				if _, err := os.Stat(filepath.Join(workspace, "acquisition.used")); !os.IsNotExist(err) {
					t.Fatal("rejected grant spent workspace", err)
				}
			}
		})
	}
}
