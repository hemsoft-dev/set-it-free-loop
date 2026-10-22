//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func gitVMRequestFixture(t *testing.T, binding gitMaterializationBinding) gitVMRequest {
	t.Helper()
	pin := VMAssetPin{Digest("synthetic asset pin, no execution qualification"), 1024}
	filter, err := gitFixedFilterPin()
	if err != nil {
		t.Fatal(err)
	}
	m := gitVMManifest{
		Version: 1, Purpose: gitVMSourcePurpose,
		QEMU: pin, Kernel: pin, Initrd: pin, BIOS: pin, QEMURuntime: pin, NamespaceHelper: pin,
		BootstrapLibraries: []nativeRuntimeFile{
			{"lib64/ld-linux-x86-64.so.2", pin.SHA256, pin.Bytes},
			{"usr/lib/x86_64-linux-gnu/libc.so.6", pin.SHA256, pin.Bytes},
		},
		Runtime: GitRuntimePins{pin, pin, pin}, Worker: pin, Filter: filter,
	}
	binding.RuntimeHash = Digest(m)
	return gitVMRequest{1, gitVMSourcePurpose, binding, m}
}

func TestGitVMExportActualGitProducer(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			_, binding, store := gitMaterializationFixture(t, format)
			q := gitVMRequestFixture(t, binding)
			var wire bytes.Buffer
			if err := writeGitVMExport(context.Background(), &wire, store, q); err != nil {
				t.Fatal(err)
			}
			m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), q)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			got, err := m.Read(context.Background(), q.Binding.Snapshot.Head, "safe.go", snapshotObjectBytes)
			want, wantErr := store.Read(context.Background(), q.Binding.Snapshot.Head, "safe.go", snapshotObjectBytes)
			if err != nil || wantErr != nil || got.Mode != want.Mode || !bytes.Equal(got.Data, want.Data) {
				t.Fatalf("export differs: %v %v", err, wantErr)
			}
			// The source envelope cannot be interpreted as the existing host frame.
			if value, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), q.Binding); err == nil {
				value.Close()
				t.Fatal("VM export accepted as host acquisition")
			}
		})
	}
}

func TestGitVMExportRequestAndFramingRefusals(t *testing.T) {
	_, binding, store := gitMaterializationFixture(t, "sha1")
	q := gitVMRequestFixture(t, binding)
	var export bytes.Buffer
	if err := writeGitVMExport(context.Background(), &export, store, q); err != nil {
		t.Fatal(err)
	}
	wire := export.Bytes()
	headerEnd := 4 + int(binary.BigEndian.Uint32(wire[:4]))
	header := gitVMExportHeader{1, gitVMSourcePurpose, Digest(q), q.Binding}
	wrap := func(value any, body []byte) []byte {
		var b bytes.Buffer
		if err := writeVMRecord(&b, value, gitVMRecordBytes); err != nil {
			t.Fatal(err)
		}
		b.Write(body)
		return b.Bytes()
	}
	badPurpose := header
	badPurpose.Purpose = "native-review"
	badRequest := header
	badRequest.RequestHash = Digest("other request")
	badAttempt := header
	badAttempt.Binding.Attempt = strings.Repeat("b", 32)
	unknown := map[string]any{}
	raw, _ := json.Marshal(header)
	_ = json.Unmarshal(raw, &unknown)
	unknown["AuthReturn"] = "forbidden"
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, gitVMRecordBytes+1)
	for name, data := range map[string][]byte{
		"provider receipt":         wrap(nativeVMFinished{Version: 1, State: "complete"}, wire[headerEnd:]),
		"wrong purpose":            wrap(badPurpose, wire[headerEnd:]),
		"wrong request":            wrap(badRequest, wire[headerEnd:]),
		"wrong attempt":            wrap(badAttempt, wire[headerEnd:]),
		"unknown credential field": wrap(unknown, wire[headerEnd:]),
		"oversize header":          oversize,
		"truncated header":         wire[:headerEnd-1],
		"truncated export":         wire[:len(wire)-1],
		"trailing bytes":           append(append([]byte{}, wire...), 0),
		"bare host export":         wire[headerEnd:],
	} {
		t.Run(name, func(t *testing.T) {
			m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(data)), q)
			if err == nil {
				m.Close()
				t.Fatal("accepted invalid export")
			}
		})
	}
	for name, mutate := range map[string]func(*gitVMRequest){
		"release":  func(v *gitVMRequest) { v.Binding.ReleaseHash = Digest("new release") },
		"snapshot": func(v *gitVMRequest) { v.Binding.Snapshot.PR++ },
		"kernel": func(v *gitVMRequest) {
			v.Manifest.Kernel.SHA256 = Digest("other kernel")
			v.Binding.RuntimeHash = Digest(v.Manifest)
		},
		"purpose": func(v *gitVMRequest) { v.Purpose = "native-review" },
		"version": func(v *gitVMRequest) { v.Version++ },
		"runtime": func(v *gitVMRequest) { v.Binding.RuntimeHash = Digest("old host runtime") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := q
			mutate(&changed)
			m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire)), changed)
			if err == nil {
				m.Close()
				t.Fatal("accepted mismatched request")
			}
		})
	}
}

func TestGitVMExportCancellationDuringHeader(t *testing.T) {
	q := gitVMRequestFixture(t, gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("unused"), testJob().Snapshot})
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := receiveGitVMExport(ctx, reader, q); done <- err }()
	// An accepted prefix guarantees the receiver is already reading the header.
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], 1024)
	if _, err := writer.Write(prefix[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled intake succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("header intake retained blocked reader")
	}
}

func TestGitVMManifestRejectsUnpinnedAssetsAndOtherPurposes(t *testing.T) {
	q := gitVMRequestFixture(t, gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("unused"), testJob().Snapshot})
	if !q.valid() {
		t.Fatal("fixture invalid")
	}
	for name, mutate := range map[string]func(*gitVMManifest){
		"native purpose":  func(m *gitVMManifest) { m.Purpose = "native-review" },
		"host purpose":    func(m *gitVMManifest) { m.Purpose = "isolated-git-acquisition" },
		"fixture purpose": func(m *gitVMManifest) { m.Purpose = "synthetic-role-fixture" },
		"version":         func(m *gitVMManifest) { m.Version++ },
		"missing loader":  func(m *gitVMManifest) { m.BootstrapLibraries = m.BootstrapLibraries[1:] },
		"oversize initrd": func(m *gitVMManifest) { m.Initrd.Bytes = (512 << 20) + 1 },
		"mutable filter":  func(m *gitVMManifest) { m.Filter.SHA256 = Digest("different filter") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := q
			mutate(&changed.Manifest)
			changed.Binding.RuntimeHash = Digest(changed.Manifest)
			if changed.valid() {
				t.Fatal("invalid manifest accepted even with matching digest")
			}
		})
	}
	for index := range 11 {
		changed := q
		pins := []*VMAssetPin{&changed.Manifest.QEMU, &changed.Manifest.Kernel, &changed.Manifest.Initrd,
			&changed.Manifest.BIOS, &changed.Manifest.QEMURuntime, &changed.Manifest.NamespaceHelper,
			&changed.Manifest.Worker, &changed.Manifest.Filter,
			&changed.Manifest.Runtime.Git, &changed.Manifest.Runtime.HTTP, &changed.Manifest.Runtime.Artifact}
		*pins[index] = VMAssetPin{}
		changed.Binding.RuntimeHash = Digest(changed.Manifest)
		if changed.valid() {
			t.Fatalf("missing asset %d accepted", index)
		}
	}
}
