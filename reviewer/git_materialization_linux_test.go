//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func gitMaterializationFixture(t *testing.T, format string) ([]byte, gitMaterializationBinding, GitStore) {
	t.Helper()
	store, snapshot := gitQuarantineFixture(t, format)
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("synthetic release"), Digest("synthetic runtime"), snapshot}
	var output bytes.Buffer
	if err := writeGitMaterialization(context.Background(), &output, store, binding, DefaultGitAcquisitionBounds()); err != nil {
		t.Fatal("actual synthetic Git export", err)
	}
	return output.Bytes(), binding, store
}
func TestGitMaterializationActualGitNoHostParserAndSealedReads(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			raw, binding, store := gitMaterializationFixture(t, format)
			original, err := CaptureSnapshotEvidence(context.Background(), store, binding.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]Object{}
			for _, revision := range gitMaterializationRevisions(binding.Snapshot) {
				for _, path := range original.Paths() {
					object, err := store.Read(context.Background(), revision, path, snapshotObjectBytes)
					if err == nil {
						want[revision+":"+path] = object
					}
				}
			}
			// Deleting the quarantine makes accidental host-side Git fallback fail.
			if err := os.RemoveAll(store.GitDir); err != nil {
				t.Fatal(err)
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(raw)), binding)
			if err != nil {
				t.Fatal("receive", err)
			}
			defer m.Close()
			job := testJob()
			job.Snapshot = binding.Snapshot
			job.Packet = m.packet
			bound, err := restoreGitMaterialization(context.Background(), m, job)
			if err != nil {
				t.Fatal("restore", err)
			}
			if !reflect.DeepEqual(bound.diffLines(), bindSnapshotEvidence(t, original).diffLines()) || !reflect.DeepEqual(bound.evidence.Coverage(), original.Coverage()) || bound.evidence.Packet() != original.Packet() {
				t.Fatal("packet/omissions/anchors changed")
			}
			for _, revision := range gitMaterializationRevisions(binding.Snapshot) {
				for _, path := range original.Paths() {
					object, err := m.Read(context.Background(), revision, path, snapshotObjectBytes)
					expected, exists := want[revision+":"+path]
					if exists && (err != nil || !reflect.DeepEqual(object, expected)) {
						t.Fatalf("plain lookup differs %s %s: %v", revision, path, err)
					}
					if !exists && err == nil {
						t.Fatalf("missing path granted: %s", path)
					}
				}
			}
			reader, err := bound.Reader(binding.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if value, err := reader.Read(context.Background(), binding.Snapshot.Head, "safe.go"); err != nil || !strings.Contains(value, "FOUR") {
				t.Fatal("reader", err)
			}
			if _, err := reader.Read(context.Background(), binding.Snapshot.Head, "link"); err == nil {
				t.Fatal("symlink reader grant")
			}
			if _, err := reader.Read(context.Background(), binding.Snapshot.Head, "binary"); err == nil {
				t.Fatal("binary reader grant")
			}
			if _, err := m.Read(context.Background(), strings.Repeat("f", len(binding.Snapshot.Head)), "safe.go", snapshotObjectBytes); err == nil {
				t.Fatal("foreign revision grant")
			}
			for _, file := range m.files {
				if _, err := file.WriteAt([]byte("mutate"), 0); err == nil {
					t.Fatal("sealed blob writable")
				}
				if err := file.Truncate(0); err == nil {
					t.Fatal("sealed blob truncatable")
				}
				if err := file.Chmod(0500); err == nil {
					t.Fatal("sealed blob executable")
				}
				seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
				if err != nil || seals&requiredStubSeals != requiredStubSeals {
					t.Fatal("missing data seals")
				}
				flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatal("blob inherited across exec")
				}
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal("idempotent close", err)
			}
			if _, err := reader.Read(context.Background(), binding.Snapshot.Head, "safe.go"); err == nil {
				t.Fatal("closed reader grant")
			}
		})
	}
}

type materializationWire struct {
	manifest gitMaterializationManifest
	packet   []byte
	data     []byte
}

func decodeMaterializationWire(t *testing.T, raw []byte) materializationWire {
	t.Helper()
	offset := len(gitMaterializationMagic)
	n := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
	offset += 4
	var out materializationWire
	if err := json.Unmarshal(raw[offset:offset+n], &out.manifest); err != nil {
		t.Fatal(err)
	}
	offset += n
	n = int(binary.BigEndian.Uint32(raw[offset : offset+4]))
	offset += 4
	out.packet = append([]byte(nil), raw[offset:offset+n]...)
	offset += n
	out.data = append([]byte(nil), raw[offset:]...)
	return out
}
func (w materializationWire) encode(t *testing.T) []byte {
	t.Helper()
	manifest, err := json.Marshal(w.manifest)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	raw.WriteString(gitMaterializationMagic)
	for _, part := range [][]byte{manifest, w.packet} {
		binary.Write(&raw, binary.BigEndian, uint32(len(part)))
		raw.Write(part)
	}
	raw.Write(w.data)
	return raw.Bytes()
}
func TestGitMaterializationRefusesMutatedActualProducerArtifacts(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	for _, kind := range []string{"binding", "inventory", "packet-hash", "path", "revision", "mode", "identity", "missing-entry", "duplicate-entry", "absent", "omitted-included", "missing-blob", "extra-blob", "blob-size", "blob-hash", "blob-oid", "blob-order", "blob-bytes", "blob-bytes-sha-updated", "patch", "coverage", "oversize-total"} {
		t.Run(kind, func(t *testing.T) {
			w := decodeMaterializationWire(t, raw)
			switch kind {
			case "binding":
				w.manifest.Binding.Attempt = strings.Repeat("b", 32)
			case "inventory":
				w.manifest.Inventory.SnapshotHash = Digest("wrong snapshot")
			case "packet-hash":
				w.manifest.PacketHash = Digest("wrong packet")
			case "path":
				w.manifest.Entries[0].Path = "../outside"
			case "revision":
				w.manifest.Entries[0].Revision = strings.Repeat("f", 40)
			case "mode":
				w.manifest.Entries[0].Mode = "120000"
			case "identity":
				for i := range w.manifest.Entries {
					if w.manifest.Entries[i].State == "blob" {
						w.manifest.Entries[i].Object = strings.Repeat("f", 40)
						break
					}
				}
			case "missing-entry":
				w.manifest.Entries = w.manifest.Entries[1:]
			case "duplicate-entry":
				w.manifest.Entries[1] = w.manifest.Entries[0]
			case "absent":
				for i := range w.manifest.Entries {
					if w.manifest.Entries[i].State == "absent" {
						w.manifest.Entries[i].Object = strings.Repeat("f", 40)
						break
					}
				}
			case "omitted-included":
				for i := range w.manifest.Entries {
					e := &w.manifest.Entries[i]
					if e.Path == "safe.go" && e.Revision == binding.Snapshot.Head {
						e.State = "omitted"
						break
					}
				}
			case "missing-blob":
				w.manifest.Blobs = w.manifest.Blobs[1:]
			case "extra-blob":
				w.manifest.Blobs = append(w.manifest.Blobs, gitMaterializationBlob{strings.Repeat("f", 40), gitRawHash(nil), 0})
			case "blob-size":
				w.manifest.Blobs[0].Bytes = snapshotObjectBytes + 1
			case "blob-hash":
				w.manifest.Blobs[0].SHA256 = Digest("wrong hash")
			case "blob-oid":
				old := w.manifest.Blobs[0].Object
				id := strings.Repeat("0", 40)
				w.manifest.Blobs[0].Object = id
				for i := range w.manifest.Entries {
					if w.manifest.Entries[i].Object == old {
						w.manifest.Entries[i].Object = id
					}
				}
			case "blob-order":
				w.manifest.Blobs[0], w.manifest.Blobs[1] = w.manifest.Blobs[1], w.manifest.Blobs[0]
			case "blob-bytes":
				w.data[len(w.data)-1] ^= 1
			case "blob-bytes-sha-updated":
				// Matching a tampered SHA-256 is insufficient: Git's canonical
				// blob header and object hash must still match the manifest OID.
				at := 0
				for i := range w.manifest.Blobs {
					blob := &w.manifest.Blobs[i]
					if blob.Bytes > 0 {
						w.data[at] ^= 1
						blob.SHA256 = gitRawHash(w.data[at : at+blob.Bytes])
						break
					}
					at += blob.Bytes
				}
			case "patch":
				var p snapshotPacket
				json.Unmarshal(w.packet, &p)
				for i := range p.Files {
					if p.Files[i].Path == "safe.go" {
						p.Files[i].Patch = strings.Replace(p.Files[i].Patch, "+FOUR", "+FORGED", 1)
					}
				}
				w.packet, _ = json.Marshal(p)
				w.manifest.PacketHash = gitRawHash(w.packet)
			case "coverage":
				var p snapshotPacket
				json.Unmarshal(w.packet, &p)
				p.Coverage = append(p.Coverage, Coverage{"outside", "binary or non-UTF-8 diff omitted"})
				w.packet, _ = json.Marshal(p)
				w.manifest.PacketHash = gitRawHash(w.packet)
			case "oversize-total":
				// Keep individual lengths valid, exceed only the aggregate limit.
				w.manifest.Blobs = nil
				for i := 0; i < 17; i++ {
					w.manifest.Blobs = append(w.manifest.Blobs, gitMaterializationBlob{strings.Repeat("0", 38) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16]), gitRawHash(nil), snapshotObjectBytes})
				}
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(w.encode(t))), binding)
			if kind == "patch" || kind == "coverage" {
				// Content authenticity comes from the protected worker; the bounded Go
				// recovery validator independently refuses forged patches/omissions.
				if err != nil {
					return
				}
				defer m.Close()
				job := testJob()
				job.Snapshot = binding.Snapshot
				job.Packet = m.packet
				if bound, err := restoreGitMaterialization(context.Background(), m, job); err == nil || bound != nil {
					t.Fatal("forged packet granted")
				}
				return
			}
			if m != nil {
				m.Close()
			}
			if err == nil || m != nil {
				t.Fatal("mutated wire admitted")
			}
		})
	}
}
func TestGitMaterializationRejectsFramingAndNoncanonicalMetadata(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	offset := len(gitMaterializationMagic)
	manifestLength := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
	manifest := raw[offset+4 : offset+4+manifestLength]
	for _, kind := range []string{"magic", "manifest-length", "packet-length", "truncated-magic", "truncated-manifest", "truncated-packet", "truncated-blob", "trailing-byte", "trailing-error", "duplicate-key", "case-alias", "unknown-key", "null", "whitespace", "packet-oversize"} {
		t.Run(kind, func(t *testing.T) {
			mutated := append([]byte(nil), raw...)
			switch kind {
			case "magic":
				mutated[0] ^= 1
			case "manifest-length":
				binary.BigEndian.PutUint32(mutated[offset:offset+4], gitMaterializationManifestBytes+1)
			case "packet-length":
				binary.BigEndian.PutUint32(mutated[offset+4+manifestLength:offset+8+manifestLength], snapshotPacketBytes+1)
			case "truncated-magic":
				mutated = mutated[:2]
			case "truncated-manifest":
				mutated = mutated[:offset+8]
			case "truncated-packet":
				mutated = mutated[:offset+8+manifestLength]
			case "truncated-blob":
				mutated = mutated[:len(mutated)-1]
			case "trailing-byte":
				mutated = append(mutated, 0)
			case "trailing-error":
				m, err := receiveGitMaterialization(context.Background(), &materializationTailError{Reader: bytes.NewReader(raw)}, binding)
				if m != nil {
					m.Close()
				}
				if err == nil || m != nil {
					t.Fatal("tail failure admitted")
				}
				return
			default:
				replacement := []byte{}
				switch kind {
				case "duplicate-key":
					replacement = append([]byte(`{"Version":1,`), manifest[1:]...)
				case "case-alias":
					replacement = bytes.Replace(manifest, []byte(`"Version"`), []byte(`"version"`), 1)
				case "unknown-key":
					replacement = append([]byte(`{"Unknown":1,`), manifest[1:]...)
				case "null":
					replacement = bytes.Replace(manifest, []byte(`"Version":1`), []byte(`"Version":null`), 1)
				case "whitespace":
					replacement = append([]byte(" "), manifest...)
				case "packet-oversize":
					replacement = manifest
				}
				var b bytes.Buffer
				b.WriteString(gitMaterializationMagic)
				binary.Write(&b, binary.BigEndian, uint32(len(replacement)))
				b.Write(replacement)
				b.Write(raw[offset+4+manifestLength:])
				mutated = b.Bytes()
				if kind == "packet-oversize" {
					binary.BigEndian.PutUint32(mutated[offset+4+len(replacement):offset+8+len(replacement)], 0xffffffff)
				}
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(mutated)), binding)
			if m != nil {
				m.Close()
			}
			if err == nil || m != nil {
				t.Fatal("malformed framing admitted")
			}
		})
	}
}

type materializationTailError struct{ *bytes.Reader }

func (r *materializationTailError) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("synthetic secret tail error")
	}
	return n, err
}
func (*materializationTailError) Close() error { return nil }

func TestGitMaterializationCancellationClosesPipeAtEveryPhase(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	offset := len(gitMaterializationMagic)
	manifestLength := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
	packetAt := offset + 4 + manifestLength
	packetLength := int(binary.BigEndian.Uint32(raw[packetAt : packetAt+4]))
	for _, cut := range []int{0, offset + 2, offset + 9, packetAt + 5, packetAt + 4 + packetLength + 1, len(raw)} {
		ctx, cancel := context.WithCancel(context.Background())
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			m, err := receiveGitMaterialization(ctx, reader, binding)
			if m != nil {
				m.Close()
			}
			done <- err
		}()
		if _, err := writer.Write(raw[:cut]); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled intake admitted")
			}
		case <-time.After(time.Second):
			t.Fatal("intake did not stop")
		}
		writer.Close()
	}
}
func TestGitMaterializationConcurrentReadCloseAndJobFencing(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(raw)), binding)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	job := testJob()
	job.Snapshot = binding.Snapshot
	job.Packet = m.packet
	for _, kind := range []string{"packet", "snapshot"} {
		bad := job
		if kind == "packet" {
			bad.Packet += " "
		} else {
			bad.Snapshot.PR++
		}
		if got, err := restoreGitMaterialization(context.Background(), m, bad); err == nil || got != nil {
			t.Fatal("different admitted job granted")
		}
	}
	if _, err := m.Read(context.Background(), binding.Snapshot.Head, "safe.go", 1); err == nil {
		t.Fatal("read budget ignored")
	}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 20; j++ {
				object, err := m.Read(context.Background(), binding.Snapshot.Head, "safe.go", snapshotObjectBytes)
				if err == nil && !bytes.Contains(object.Data, []byte("FOUR")) {
					t.Error("bad concurrent bytes")
				}
			}
		}()
	}
	m.Close()
	wait.Wait()
}

func TestGitMaterializationFileDirectoryTransitionsAndOmissions(t *testing.T) {
	f := newSnapshotGitFixture(t)
	f.snapshot.MergeBase = f.snapshot.Head
	f.snapshot.Base = f.run(t, "commit-tree", f.run(t, "rev-parse", "HEAD^{tree}"), "-p", f.snapshot.Head, "-m", "synthetic sibling base")
	// Both directions must preserve a complete raw inventory while keeping each
	// inline patch restricted to its exact path, rather than descendant patches.
	if err := os.Remove(filepath.Join(f.dir, "safe.go")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "safe.go/child.go", "new child\n")
	if err := os.RemoveAll(filepath.Join(f.dir, "dir")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "dir", "directory becomes an ordinary file\n")
	f.write(t, "empty", "")
	f.write(t, "large", strings.Repeat("x", snapshotObjectBytes+1))
	f.write(t, "executable", "plain script bytes, never executed\n")
	if err := os.Chmod(filepath.Join(f.dir, "executable"), 0700); err != nil {
		t.Fatal(err)
	}
	f.run(t, "add", "--all")
	f.run(t, "commit", "--quiet", "-m", "synthetic file directory transitions")
	f.snapshot.Head = f.run(t, "rev-parse", "HEAD")
	store, snapshot := gitQuarantineFromFixture(t, f, "sha1")
	if _, err := CaptureSnapshotEvidence(context.Background(), store, snapshot); err != nil {
		t.Fatal("actual exact-path packet capture", err)
	}
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("synthetic release"), Digest("synthetic runtime"), snapshot}
	var output bytes.Buffer
	if err := writeGitMaterialization(context.Background(), &output, store, binding, DefaultGitAcquisitionBounds()); err != nil {
		t.Fatal("file directory export", err)
	}
	m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(output.Bytes())), binding)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	job := testJob()
	job.Snapshot = snapshot
	job.Packet = m.packet
	bound, err := restoreGitMaterialization(context.Background(), m, job)
	if err != nil {
		t.Fatal(err)
	}
	object, err := m.Read(context.Background(), snapshot.Head, "safe.go", snapshotObjectBytes)
	if err != nil || object.Mode != "040000" || len(object.Data) != 0 {
		t.Fatal("directory source omitted incorrectly", object, err)
	}
	object, err = m.Read(context.Background(), snapshot.MergeBase, "dir", snapshotObjectBytes)
	if err != nil || object.Mode != "040000" {
		t.Fatal("old directory missing", err)
	}
	object, err = m.Read(context.Background(), snapshot.Head, "empty", 1)
	if err != nil || object.Mode != "100644" || len(object.Data) != 0 {
		t.Fatal("empty blob missing", err)
	}
	object, err = m.Read(context.Background(), snapshot.Head, "executable", snapshotObjectBytes)
	if err != nil || object.Mode != "100755" {
		t.Fatal("regular executable mode lost", err)
	}
	if _, err := m.Read(context.Background(), snapshot.Head, "large", snapshotObjectBytes); err == nil {
		t.Fatal("oversized plain source admitted")
	}
	if bound.diffLines()["safe.go"][1] || !bound.diffLines()["safe.go/child.go"][1] || !bound.diffLines()["dir"][1] {
		t.Fatal("directory transition anchors differ")
	}
	reader, err := bound.Reader(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(context.Background(), snapshot.Head, "large"); err == nil {
		t.Fatal("oversized reader grant")
	}
	_, coverage := reader.Evidence()
	found := false
	for _, c := range coverage {
		if c.Path == "large" {
			found = true
		}
	}
	if !found {
		t.Fatal("lost source omission")
	}
}

// gitMaterializationBlobTarget is the descriptor link of a sealed source blob.
const gitMaterializationBlobTarget = "/memfd:sfl-source-blob"

func TestGitMaterializationFailedIntakeClosesPreviouslySealedBlobs(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	// A last-record failure occurs after earlier records have acquired memfds.
	raw[len(raw)-1] ^= 1
	watch := gitWatchFDs(t, gitFDTargets(gitMaterializationBlobTarget))
	for i := 0; i < 12; i++ {
		m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(raw)), binding)
		if err == nil || m != nil {
			if m != nil {
				m.Close()
			}
			t.Fatal("late corruption admitted")
		}
		if leaked := watch.leaked(t); len(leaked) != 0 {
			t.Fatalf("retained failed intake descriptors: %+v", leaked)
		}
	}
}
func TestGitMaterializationWriterFailureAndInvalidBindings(t *testing.T) {
	raw, binding, store := gitMaterializationFixture(t, "sha1")
	bad := binding
	bad.Snapshot.Base = strings.Repeat("a", 64)
	for _, input := range []gitMaterializationBinding{bad, {}} {
		var output bytes.Buffer
		if err := writeGitMaterialization(context.Background(), &output, store, input, DefaultGitAcquisitionBounds()); err == nil || output.Len() != 0 {
			t.Fatal("invalid binding exported")
		}
		if m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(raw)), input); err == nil || m != nil {
			if m != nil {
				m.Close()
			}
			t.Fatal("invalid binding received")
		}
	}
	if err := writeGitMaterialization(context.Background(), materializationShortWriter{}, store, binding, DefaultGitAcquisitionBounds()); err == nil {
		t.Fatal("short write admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if err := writeGitMaterialization(ctx, &output, store, binding, DefaultGitAcquisitionBounds()); err == nil || output.Len() != 0 {
		t.Fatal("cancelled export admitted")
	}
}

type materializationShortWriter struct{}

func (materializationShortWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		return len(p) - 1, nil
	}
	return 0, nil
}
