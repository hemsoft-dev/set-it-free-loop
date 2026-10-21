//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitGrantSealedData(t *testing.T, data []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inert-asset")
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("asset write")
	}
	file, err := sealVMAsset(context.Background(), VMBootAsset{path, gitRawHash(data), int64(len(data))}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

type gitGrantFixture struct {
	unit     gitUnitFixture
	files    []*os.File
	manifest gitGrantManifest
	conn     *net.UnixConn
	listener *net.UnixListener
	path     string
	readPipe *os.File
}

func newGitGrantFixture(t *testing.T) gitGrantFixture {
	t.Helper()
	listener, path, directory := gitSupervisorListener(t)
	f := newGitUnitFixture(t, path)
	filter, err := retainGitPayloadSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { filter.Close() })
	workerInfo, err := f.owner.image.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	helperData, err := os.ReadFile("/usr/bin/bwrap")
	if err != nil {
		t.Fatal(err)
	}
	helperPin := VMAssetPin{gitRawHash(helperData), int64(len(helperData))}
	helper, err := retainNativeBootstrap(context.Background(), VMBootAsset{Path: "/usr/bin/bwrap", SHA256: helperPin.SHA256, Bytes: helperPin.Bytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Close() })
	runtimeManifest, bodies := gitRuntimeFixture(t)
	artifact, runtimePins := gitRuntimeFrame(t, runtimeManifest, bodies)
	runtimeRoot, err := prepareGitRuntime(context.Background(), nativeRuntimePrivateParent(t), bytes.NewReader(artifact), runtimePins, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtimeRoot.Close() })
	runtimeFD, err := runtimeRoot.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtimeFD.Close() })
	filterPin, err := gitFixedFilterPin()
	if err != nil {
		t.Fatal(err)
	}
	manifest := gitGrantManifest{2, "isolated-git-acquisition", runtimePins, helperPin, VMAssetPin{f.stubHash, workerInfo.Size()}, filterPin}
	f.binding.RuntimeHash = Digest(manifest)
	manifestRaw, _ := json.Marshal(manifest)
	gitPath := filepath.Join(directory, "git.sock")
	gitListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: gitPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gitListener.Close() })
	if os.Chmod(gitPath, 0600) != nil || os.WriteFile(filepath.Join(directory, "git-read.used"), []byte("sfl-git-read-broker-1\n"), 0600) != nil {
		t.Fatal("broker fixture")
	}
	broker, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.Close() })
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readPipe.Close(); writePipe.Close() })
	worker, err := duplicateGitControlFile(f.owner.image.file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })
	files := []*os.File{gitGrantSealedData(t, manifestRaw), runtimeFD, helper, worker, filter, broker, writePipe, gitTestPIDFD(t, os.Getpid())}
	f.prepareStart(t)
	if json.NewEncoder(f.input).Encode(f.binding) != nil {
		t.Fatal("binding fixture")
	}
	listener.SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return gitGrantFixture{f, files, manifest, conn, listener, path, readPipe}
}
func gitGrantAck(t *testing.T, conn *net.UnixConn) {
	t.Helper()
	raw, files, err := readGitControlPacket(context.Background(), conn, 0)
	closeVMFiles(files)
	if err != nil || string(raw) != "received" {
		t.Fatal("grant was not received", err, string(raw))
	}
}
func gitWorkerGrantClient(t *testing.T, path string, binding gitMaterializationBinding) (*exec.Cmd, func()) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestGitAcquisitionUnitInertChild$")
	child.Env = []string{"SFL_GIT_UNIT_INERT=1", "SFL_GIT_UNIT_GRANT_ENDPOINT=" + path, "SFL_GIT_UNIT_GRANT_STAGE=" + gitWorkerGrant}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if child.Start() != nil {
		t.Fatal("worker fixture")
	}
	done := make(chan struct{})
	go func() { child.Wait(); close(done) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			child.Process.Kill()
			input.Close()
			<-done
		}
	}
	t.Cleanup(stop)
	ready := make([]byte, 6)
	if _, err := io.ReadFull(output, ready); err != nil || string(ready) != "ready\n" {
		t.Fatal("worker not ready", err)
	}
	if json.NewEncoder(input).Encode(binding) != nil {
		t.Fatal("worker binding")
	}
	return child, stop
}
func TestGitUnitGrantPersistsBeforeAtomicHandoffAndConsumesEachStage(t *testing.T) {
	f := newGitGrantFixture(t)
	if err := f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files); err != nil {
		t.Fatal("bootstrap grant", err)
	}
	gitGrantAck(t, f.conn)
	record, err := f.unit.owner.read(f.unit.binding.Attempt)
	packet := gitUnitGrantPacket{1, gitBootstrapGrant, f.unit.binding, record.Identity, f.unit.binding.RuntimeHash}
	data, _ := json.Marshal(packet)
	if err != nil || record.BootstrapGrantHash != gitRawHash(data) || record.WorkerGrantHash != "" || record.Revoked {
		t.Fatal("wrong consumed bootstrap record", record, err)
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("bootstrap replayed")
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitWorkerGrant, f.conn, nil) == nil {
		t.Fatal("root masqueraded as inner worker")
	}
	worker, stopWorker := gitWorkerGrantClient(t, f.path, f.unit.binding)
	f.listener.SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := f.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := f.unit.owner.grant(context.Background(), f.unit.binding, gitWorkerGrant, conn, nil); err != nil {
		t.Fatal("worker grant", err)
	}
	gitGrantAck(t, conn)
	record, err = f.unit.owner.read(f.unit.binding.Attempt)
	if err != nil || record.WorkerGrantHash == "" || record.WorkerIdentity.PID != worker.Process.Pid || record.WorkerIdentity.InvocationID != record.Identity.InvocationID {
		t.Fatal("worker recipient not bound", record, err)
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitWorkerGrant, conn, nil) == nil {
		t.Fatal("worker replayed")
	}
	stopWorker()
	if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
		t.Fatal(err)
	}
}
func TestGitUnitGrantLostSendRevokesAndCannotReplay(t *testing.T) {
	f := newGitGrantFixture(t)
	if f.conn.CloseWrite() != nil {
		t.Fatal("close sending direction")
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("lost send succeeded")
	}
	record, err := f.unit.owner.read(f.unit.binding.Attempt)
	if err != nil || record.BootstrapGrantHash == "" || !record.Revoked || !f.unit.owner.grantFailed[f.unit.binding.Attempt] {
		t.Fatal("uncertain grant was discarded", record, err)
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("uncertain grant replayed")
	}
	if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
		t.Fatal("stop after uncertain grant", err)
	}
}
func TestGitUnitGrantRefusesWrongDescriptorRolesBeforeConsuming(t *testing.T) {
	for _, kind := range []string{"missing", "nil", "manifest", "runtime", "helper", "worker", "filter", "directory", "pipe", "supervisor"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitGrantFixture(t)
			bad := append([]*os.File(nil), f.files...)
			switch kind {
			case "missing":
				bad = bad[:7]
			case "nil":
				bad[0] = nil
			case "manifest":
				bad[0] = gitGrantSealedData(t, []byte("{}"))
			case "runtime":
				bad[1] = gitGrantSealedData(t, []byte("different artifact"))
			case "helper":
				bad[2] = gitPacketFile(t)
			case "worker":
				bad[3] = bad[1]
			case "filter":
				bad[4] = bad[1]
			case "directory":
				directory, err := os.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer directory.Close()
				bad[5] = directory
			case "pipe":
				bad[6] = f.readPipe
			case "supervisor":
				bad[7] = gitTestPIDFD(t, f.unit.child.Process.Pid)
			}
			if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, bad) == nil {
				t.Fatal("wrong role accepted")
			}
			record, err := f.unit.owner.read(f.unit.binding.Attempt)
			if err != nil || record.BootstrapGrantHash != "" || record.WorkerGrantHash != "" || record.Revoked {
				t.Fatal("invalid input consumed authority", record, err)
			}
			if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestGitUnitGrantRecoveryAndOrderingRefuseNewAuthority(t *testing.T) {
	f := newGitGrantFixture(t)
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitWorkerGrant, f.conn, nil) == nil {
		t.Fatal("worker before bootstrap")
	}
	rootFD := f.unit.owner.rootPIDFDs[f.unit.binding.Attempt]
	delete(f.unit.owner.rootPIDFDs, f.unit.binding.Attempt)
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("reopened owner without original root pidfd granted")
	}
	f.unit.owner.rootPIDFDs[f.unit.binding.Attempt] = rootFD
	if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
		t.Fatal(err)
	}
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("stopped attempt granted")
	}
}
func TestGitUnitGrantReopeningAndRecaptureCannotRearmOldAttempt(t *testing.T) {
	f := newGitGrantFixture(t)
	old := f.unit.owner
	// Release only the old controller's kernel handles while its independent
	// inert unit root remains live. No production API allows this abandonment;
	// the existing owner-crash test separately exercises an actual SIGKILL.
	for _, fd := range old.rootPIDFDs {
		unix.Close(fd)
	}
	old.image.Close()
	old.supervisor.Close()
	old.lock.Close()
	old.root.Close()
	old.closed = true
	reopened, err := newGitUnitOwner(context.Background(), f.unit.root, f.unit.stub, f.unit.stubHash, f.unit.manager, f.unit.manager.kernel)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, err := reopened.read(f.unit.binding.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	// A live manager observation is enough for recovery stop, not a fresh grant.
	record, err = reopened.captureStarted(context.Background(), record)
	if err != nil || reopened.write(record) != nil {
		t.Fatal("recovery capture", err)
	}
	if reopened.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("reopened controller rearmed an old attempt after capture")
	}
	if reopened.stop(context.Background(), f.unit.binding) != nil {
		t.Fatal("recovered stop")
	}
}

type gitGrantWriteFaultManager struct {
	gitUnitManager
	fail func()
}

func (m *gitGrantWriteFaultManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	value, err := m.gitUnitManager.Observe(ctx, unit)
	if m.fail != nil {
		fail := m.fail
		m.fail = nil
		fail()
	}
	return value, err
}
func TestGitUnitGrantPersistenceFailureSendsNothing(t *testing.T) {
	f := newGitGrantFixture(t)
	directory := filepath.Join(f.unit.root, gitUnitDirectory(f.unit.binding.Attempt))
	f.unit.owner.manager = &gitGrantWriteFaultManager{gitUnitManager: f.unit.manager, fail: func() {
		if os.Chmod(directory, 0500) != nil {
			t.Fatal("fault setup")
		}
	}}
	defer os.Chmod(directory, 0700)
	if f.unit.owner.grant(context.Background(), f.unit.binding, gitBootstrapGrant, f.conn, f.files) == nil {
		t.Fatal("failed intent persistence sent authority")
	}
	if os.Chmod(directory, 0700) != nil {
		t.Fatal("fault restore")
	}
	record, err := f.unit.owner.read(f.unit.binding.Attempt)
	if err != nil || record.BootstrapGrantHash != "" || record.Revoked {
		t.Fatal("failed intent changed prior record", record, err)
	}
	// The client emits this ACK only after receiving all eight descriptors.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, files, err := readGitControlPacket(ctx, f.conn, 0); err == nil {
		closeVMFiles(files)
		t.Fatal("client received a grant despite failed persistence")
	}
	if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
		t.Fatal(err)
	}
}
func TestGitUnitGrantRecordRejectsSubstitutedConsumptionAndIdentity(t *testing.T) {
	f := newGitGrantFixture(t)
	record, err := f.unit.owner.read(f.unit.binding.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(record)
	if bytes.Contains(legacy, []byte("GrantHash")) || bytes.Contains(legacy, []byte("WorkerIdentity")) {
		t.Fatal("zero grant fields changed pre-grant v1 canonical records")
	}
	good := record
	good.BootstrapGrantHash = Digest(gitUnitGrantPacket{1, gitBootstrapGrant, good.Binding, good.Identity, good.Binding.RuntimeHash})
	if !good.valid(good.Binding.Attempt, good.StubHash) {
		t.Fatal("canonical bootstrap consumption invalid")
	}
	for _, mutate := range []func(*gitUnitRecord){func(v *gitUnitRecord) { v.BootstrapGrantHash = strings.Repeat("a", 64) }, func(v *gitUnitRecord) { v.WorkerGrantHash = strings.Repeat("b", 64) }, func(v *gitUnitRecord) { v.WorkerIdentity = v.Identity }, func(v *gitUnitRecord) { v.Captured = false }} {
		bad := good
		mutate(&bad)
		if bad.valid(bad.Binding.Attempt, bad.StubHash) {
			t.Fatal("inconsistent grant record accepted")
		}
	}
	if err := f.unit.owner.stop(context.Background(), f.unit.binding); err != nil {
		t.Fatal(err)
	}
}
func TestGitUnitGrantCanonicalPacketsRejectMutationsAndDisposeRights(t *testing.T) {
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testJob().Snapshot}
	identity, err := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	good := gitUnitGrantPacket{1, gitWorkerGrant, binding, identity, binding.RuntimeHash}
	canonical, _ := json.Marshal(good)
	for _, kind := range []string{"valid", "space", "unknown", "duplicate", "case", "wrong-binding", "wrong-stage", "wrong-recipient", "rights"} {
		t.Run(kind, func(t *testing.T) {
			left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
			raw := append([]byte(nil), canonical...)
			var files []*os.File
			switch kind {
			case "space":
				raw = append(raw, '\n')
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(",\"unknown\":true}")...)
			case "duplicate":
				raw = append([]byte("{\"Version\":1,"), raw[1:]...)
			case "case":
				raw = bytes.Replace(raw, []byte("\"Version\""), []byte("\"version\""), 1)
			case "wrong-binding":
				changed := good
				changed.Binding.Snapshot.PR++
				raw, _ = json.Marshal(changed)
			case "wrong-stage":
				changed := good
				changed.Stage = gitBootstrapGrant
				raw, _ = json.Marshal(changed)
			case "wrong-recipient":
				changed := good
				changed.Recipient.StartTicks = 0
				raw, _ = json.Marshal(changed)
			case "rights":
				files = []*os.File{gitPacketFile(t)}
			}
			if writeGitControlPacket(context.Background(), left, raw, files) != nil {
				t.Fatal("actual packet producer")
			}
			before := gitPacketFDCount(t)
			packet, received, err := readGitUnitGrant(context.Background(), right, binding, gitWorkerGrant)
			if kind == "valid" {
				if err != nil || packet != good || len(received) != 0 {
					closeVMFiles(received)
					t.Fatal("canonical packet changed", err)
				}
			} else if err == nil || len(received) != 0 {
				closeVMFiles(received)
				t.Fatal("mutated grant accepted")
			}
			if gitPacketFDCount(t) != before {
				t.Fatal("grant decoder leaked descriptors")
			}
		})
	}
}
