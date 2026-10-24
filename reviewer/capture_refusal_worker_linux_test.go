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
	"strconv"
	"strings"
	"testing"
)

// workerRefusalCases are real Git histories that the acquisition worker's
// quarantine or capture definitely answers as a permanent refusal.
var workerRefusalCases = []struct{ name, reason string }{
	{"unrelated history", CaptureMissingHistory},
	{"absent merge base", CaptureMissingHistory},
	{"criss-cross merge base", CaptureAmbiguousHistory},
	{"differing merge base", CaptureAmbiguousHistory},
	{"inventory over limit", CaptureInventoryLimit},
	{"history over commit bound", CaptureHistoryLimit},
}

// refusedWorkerFixture moves the synthetic repository's HEAD and snapshot to a
// history whose complete fetch the worker refuses. Every case is actual Git.
func refusedWorkerFixture(t *testing.T, name string) snapshotGitFixture {
	t.Helper()
	f := newSnapshotGitFixture(t)
	s := f.snapshot
	tree := f.run(t, "rev-parse", s.Head+"^{tree}")
	switch name {
	case "unrelated history":
		// Head and base share no ancestor: merge-base exits 1.
		s.Base = f.run(t, "commit-tree", tree, "-m", "unrelated base")
	case "absent merge base":
		// The recorded merge base is not in the complete fetched history.
		s.MergeBase = f.run(t, "commit-tree", tree, "-m", "unreachable merge base")
	case "criss-cross merge base":
		one := f.run(t, "commit-tree", tree, "-p", s.MergeBase, "-m", "criss-cross one")
		two := f.run(t, "commit-tree", tree, "-p", s.MergeBase, "-m", "criss-cross two")
		s.Head = f.run(t, "commit-tree", tree, "-p", one, "-p", two, "-m", "merge one")
		s.Base = f.run(t, "commit-tree", tree, "-p", two, "-p", one, "-m", "merge two")
		s.MergeBase = one
	case "differing merge base":
		s.MergeBase = s.Head
	case "inventory over limit":
		for i := 0; i <= snapshotFileLimit; i++ {
			f.write(t, "many/"+strconv.Itoa(i), "x\n")
		}
		f.run(t, "add", "--all")
		f.run(t, "commit", "--quiet", "-m", "too many paths")
		s.Head = f.run(t, "rev-parse", "HEAD")
	case "history over commit bound":
		// More commits than the default quarantine commit bound.
		return historyLimitFixture(t, "commits over bound")
	default:
		t.Fatal("unknown refusal case", name)
	}
	f.run(t, "update-ref", "HEAD", s.Head)
	f.snapshot = s
	return f
}

// gitCaptureRefusalWireFixture is an independent encoding of the documented
// refusal record: marker, big-endian length and canonical JSON, then EOF.
func gitCaptureRefusalWireFixture(t *testing.T, binding gitMaterializationBinding, reason string) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Version int
		Binding gitMaterializationBinding
		Reason  string
	}{1, binding, reason})
	if err != nil {
		t.Fatal(err)
	}
	return gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-1\n", raw)
}

func gitCaptureRefusalFrame(magic string, raw []byte) []byte {
	var b bytes.Buffer
	b.WriteString(magic)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	b.Write(size[:])
	b.Write(raw)
	return b.Bytes()
}

func workerRefusalBinding(snapshot Snapshot) gitMaterializationBinding {
	return gitMaterializationBinding{strings.Repeat("1", 32), strings.Repeat("2", 64), strings.Repeat("3", 64), snapshot}
}

// Quarantine verification refuses a definite history answer only after every
// other check passed. A cancelled verification is never a refusal.
func TestGitQuarantineClassifiesDefiniteHistoryRefusals(t *testing.T) {
	for _, c := range workerRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			f := refusedWorkerFixture(t, c.name)
			store, snapshot := gitQuarantineFromFixture(t, f, "sha1")
			_, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds())
			if c.reason == CaptureInventoryLimit {
				// The history is complete and exact; only capture refuses it.
				if err != nil {
					t.Fatal("complete history refused before capture", err)
				}
				return
			}
			if err != (CaptureRefusal{c.reason}) {
				t.Fatal("definite history answer not refused", err)
			}
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := verifyGitQuarantine(cancelled, store, snapshot, DefaultGitAcquisitionBounds()); !errors.Is(err, ErrGitQuarantine) {
				t.Fatal("cancelled verification reported", err)
			}
			// A bound overrun is answered before any history question.
			bounds := DefaultGitAcquisitionBounds()
			bounds.Objects = 1
			if _, err := verifyGitQuarantine(context.Background(), store, snapshot, bounds); err != (CaptureRefusal{CaptureHistoryLimit}) {
				t.Fatal("bounded verification not refused as history_limit", err)
			}
		})
	}
}

// The actual worker fetch through the broker reports each refusal as the
// exact refusal record, never as source, and the host decodes it to the
// same unwrapped refusal the poll worker recognises.
func TestGitAcquisitionRefusalCrossesWorkerExport(t *testing.T) {
	for _, c := range workerRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			f := refusedWorkerFixture(t, c.name)
			endpoint, finish := gitAcquisitionBackend(t, f)
			workspace := t.TempDir()
			if os.Chmod(workspace, 0700) != nil {
				t.Fatal("workspace")
			}
			binding := workerRefusalBinding(f.snapshot)
			var exported bytes.Buffer
			if err := acquireGitMaterialization(context.Background(), &exported, f.git, workspace, endpoint, binding, DefaultGitAcquisitionBounds()); err != nil {
				t.Fatal("refused acquisition did not report its refusal", err)
			}
			if !bytes.Equal(exported.Bytes(), gitCaptureRefusalWireFixture(t, binding, c.reason)) {
				t.Fatalf("refusal export is not the exact record: %q", exported.Bytes())
			}
			marker, err := os.ReadFile(filepath.Join(workspace, "acquisition.used"))
			if err != nil || string(marker) != Digest(binding)+"\n" {
				t.Fatal("refused acquisition not spent", err)
			}
			var replay bytes.Buffer
			if acquireGitMaterialization(context.Background(), &replay, f.git, workspace, endpoint, binding, DefaultGitAcquisitionBounds()) == nil || replay.Len() != 0 {
				t.Fatal("refused acquisition replayed")
			}
			if result := finish(); !result.audit.Stopped || result.audit.Requests == 0 {
				t.Fatal("refusal without actual fetch evidence", result.audit)
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(exported.Bytes())), binding)
			if m != nil || err != (CaptureRefusal{c.reason}) {
				t.Fatal("host did not decode the unwrapped refusal", err)
			}
		})
	}
}

// The host accepts a refusal only as the exact canonical record for its own
// attempt with no data. Every other shape is an opaque materialization error.
func TestGitMaterializationReceiverAcceptsOnlyExactRefusal(t *testing.T) {
	_, binding, _ := gitMaterializationFixture(t, "sha1")
	valid := gitCaptureRefusalWireFixture(t, binding, CaptureInventoryLimit)
	m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(valid)), binding)
	if m != nil || err != (CaptureRefusal{CaptureInventoryLimit}) {
		t.Fatal("exact refusal record not accepted", err)
	}
	record := func(fields string) []byte {
		return gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-1\n", []byte(fields))
	}
	canonicalBinding, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	other := binding
	other.Attempt = strings.Repeat("b", 32)
	moved := binding
	moved.Snapshot.Head = moved.Snapshot.Base
	materialization, _, _ := gitMaterializationFixture(t, "sha1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, test := range map[string]struct {
		wire     []byte
		expected gitMaterializationBinding
		ctx      context.Context
	}{
		"unknown reason":       {wire: gitCaptureRefusalWireFixture(t, binding, "synthetic_reason")},
		"empty reason":         {wire: gitCaptureRefusalWireFixture(t, binding, "")},
		"cased reason":         {wire: gitCaptureRefusalWireFixture(t, binding, "Inventory_Limit")},
		"other attempt":        {wire: gitCaptureRefusalWireFixture(t, other, CaptureInventoryLimit)},
		"other snapshot":       {wire: gitCaptureRefusalWireFixture(t, moved, CaptureInventoryLimit)},
		"for another receiver": {wire: valid, expected: other},
		"version":              {wire: record(`{"Version":2,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit"}`)},
		"reason with packet":   {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit","Packet":"{}"}`)},
		"missing reason":       {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `}`)},
		"duplicate reason":     {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit","Reason":"inventory_limit"}`)},
		"field order":          {wire: record(`{"Binding":` + string(canonicalBinding) + `,"Version":1,"Reason":"inventory_limit"}`)},
		"whitespace":           {wire: record(`{"Version":1, "Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit"}`)},
		"escaped reason":       {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory\u005flimit"}`)},
		"second value":         {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit"}{}`)},
		"reason with data":     {wire: append(append([]byte(nil), valid...), materialization[len(gitMaterializationMagic):]...)},
		"trailing byte":        {wire: append(append([]byte(nil), valid...), 0)},
		"truncated":            {wire: valid[:len(valid)-1]},
		"empty frame":          {wire: gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-1\n", nil)},
		"oversized frame":      {wire: append([]byte("SFL-GIT-CAPTURE-REFUSAL-1\n"), 0, 1, 0, 0)},
		"refusal as source":    {wire: append([]byte(gitMaterializationMagic), valid[len(gitMaterializationMagic):]...)},
		"source as refusal":    {wire: append([]byte("SFL-GIT-CAPTURE-REFUSAL-1\n"), materialization[len(gitMaterializationMagic):]...)},
		"other marker":         {wire: gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-2\n", valid[len(gitMaterializationMagic)+4:])},
		"cancelled":            {wire: valid, ctx: ctx},
	} {
		t.Run(name, func(t *testing.T) {
			expected, receiveCtx := binding, context.Background()
			if test.expected.Attempt != "" {
				expected = test.expected
			}
			if test.ctx != nil {
				receiveCtx = test.ctx
			}
			m, err := receiveGitMaterialization(receiveCtx, io.NopCloser(bytes.NewReader(test.wire)), expected)
			if m != nil {
				m.Close()
			}
			var refusal CaptureRefusal
			if m != nil || errors.As(err, &refusal) || !errors.Is(err, errGitMaterialization) {
				t.Fatal("refusal-shaped wire accepted", err)
			}
		})
	}
}

// Host archive staging returns the refusal and keeps the attempt's intake
// incomplete, exactly like a failed intake, so it is discarded after stop and
// can never reopen as retained source.
func TestGitExportArchiveRefusalStaysIncompleteIntake(t *testing.T) {
	_, binding, _ := gitMaterializationFixture(t, "sha1")
	archive, _ := exportArchiveFixture(t)
	m, err := archive.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(gitCaptureRefusalWireFixture(t, binding, CaptureAmbiguousHistory))))
	if m != nil || err != (CaptureRefusal{CaptureAmbiguousHistory}) {
		t.Fatal("staged refusal not returned unwrapped", err)
	}
	archive.enter(context.Background())
	records, err := archive.records()
	archive.leave()
	if err != nil || len(records) != 1 || records[0].State != "receiving" || records[0].Binding != binding {
		t.Fatal("refusal became retained source", records, err)
	}
	if reopened, err := archive.reopen(context.Background(), binding); err == nil || reopened != nil {
		t.Fatal("refused intake reopened")
	}
	fresh, _ := exportArchiveFixture(t)
	unknown := gitCaptureRefusalWireFixture(t, binding, "synthetic_reason")
	if m, err := fresh.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(unknown))); m != nil || !errors.Is(err, errGitExportArchive) {
		t.Fatal("unknown refusal staged or reported", err)
	}
}

// The source VM envelope carries the same record after its request header.
// The VM archive keeps the request's intake receiving, like a failed one.
func TestGitVMExportCarriesRequestBoundRefusal(t *testing.T) {
	_, binding, _ := gitMaterializationFixture(t, "sha1")
	q := gitVMRequestFixture(t, binding)
	var wire bytes.Buffer
	if writeVMRecord(&wire, gitVMExportHeader{1, gitVMSourcePurpose, Digest(q), q.Binding}, gitVMRecordBytes) != nil {
		t.Fatal("header")
	}
	header := append([]byte(nil), wire.Bytes()...)
	wire.Write(gitCaptureRefusalWireFixture(t, q.Binding, CaptureMissingHistory))
	m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), q)
	if m != nil || err != (CaptureRefusal{CaptureMissingHistory}) {
		t.Fatal("VM refusal not decoded", err)
	}
	if m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), q.Binding); m != nil || !errors.Is(err, errGitMaterialization) {
		t.Fatal("VM refusal accepted without its envelope", err)
	}
	other := q
	other.Binding.Attempt = strings.Repeat("d", 32)
	other = gitVMRequestFixture(t, other.Binding)
	if m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), other); m != nil || !errors.Is(err, errGitVMProtocol) {
		t.Fatal("VM refusal accepted for another request", err)
	}
	withData := append(append(append([]byte(nil), header...), gitCaptureRefusalWireFixture(t, q.Binding, CaptureMissingHistory)...), 'x')
	if m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(withData)), q); m != nil || errors.As(err, new(CaptureRefusal)) {
		t.Fatal("VM refusal with data accepted", err)
	}
	archive, err := openGitVMExportArchive(context.Background(), gitVMArchiveTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close(context.Background())
	m, err = archive.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire.Bytes())))
	if m != nil || err != (CaptureRefusal{CaptureMissingHistory}) {
		t.Fatal("VM archive did not return the refusal", err)
	}
	j, err := archive.journal()
	if err != nil || len(j.Records) != 1 || j.Records[0].State != "receiving" {
		t.Fatal("VM refusal became retained source", j, err)
	}
}

// runGitFactoryAcquisition returns a refusal only after the proved stop and
// lease release that also guard published source. An unknown stop or a
// cancelled acquisition keeps the opaque error and every owned resource.
func TestGitFactoryRefusalReturnsOnlyAfterProvedStop(t *testing.T) {
	for _, mode := range []string{"stopped", "unknown stop", "cancelled", "close"} {
		t.Run(mode, func(t *testing.T) {
			_, binding, _ := gitMaterializationFixture(t, "sha1")
			archive, _ := exportArchiveFixture(t)
			owner, _, _, _, _ := credentialOwnerFixture(t)
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("fake pair qualification")
			}
			f := &fakeGitFactoryAcquisition{archive: archive, binding: binding, wire: gitCaptureRefusalWireFixture(t, binding, CaptureInventoryLimit)}
			ctx := context.Background()
			switch mode {
			case "unknown stop":
				f.stopError = errors.New("fake live broker/unit")
			case "close":
				f.closeError = errors.New("fake cleanup failure")
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				entered, allow := make(chan struct{}), make(chan struct{})
				f.stopEntered, f.allowStop = entered, allow
				go func() { <-entered; cancel(); close(allow) }()
				ctx = cancelled
			}
			m, err := runGitFactoryAcquisition(ctx, owner, binding, f)
			owner.state.mu.Lock()
			owned := len(owner.state.acquisitions)
			owner.state.mu.Unlock()
			if m != nil {
				m.Close()
				t.Fatal("refusal published source")
			}
			if mode == "stopped" {
				if err != (CaptureRefusal{CaptureInventoryLimit}) || !f.closed || owned != 0 {
					t.Fatal("proved stopped refusal not returned unwrapped", err, f.closed, owned)
				}
				return
			}
			if errors.As(err, new(CaptureRefusal)) || !errors.Is(err, errGitUnit) {
				t.Fatal("refusal escaped without proved stop", err)
			}
			if mode == "unknown stop" {
				if owned != 1 || f.closed {
					t.Fatal("unknown stop lost its lease", owned, f.closed)
				}
				f.stopError = nil
				if f.generation.stop(context.Background()) != nil || f.close() != nil {
					t.Fatal("explicit fake recovery failed")
				}
			}
		})
	}
}
