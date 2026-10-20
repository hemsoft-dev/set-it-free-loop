package reviewer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func readerAuditFixture(t *testing.T) (SourceReaderConfig, SourceReaderEvent, SourceReaderEvent) {
	t.Helper()
	s := testSession(Codex)
	c := SourceReaderConfig{LaunchID: strings.Repeat("a", 32), Session: s, Paths: []string{"worker.go"}, Root: "/run/sfl", ReaderUID: 1201, WorkerUID: 1202, WorkerGID: 1301}
	ready := SourceReaderEvent{Version: 1, Kind: "ready", SessionHash: Digest(s), ManifestHash: Digest(c), LaunchID: c.LaunchID, Socket: filepath.Join(c.Root, Digest(s), "source.sock"), ReaderUID: c.ReaderUID}
	a := &SourceAccess{Version: 1, SessionHash: Digest(s), Reads: []ReadRecord{}, Coverage: []Coverage{}}
	stopped := SourceReaderEvent{Version: 1, Kind: "stopped", SessionHash: Digest(s), ManifestHash: Digest(c), LaunchID: c.LaunchID, Access: a}
	return c, ready, stopped
}

func encodeReaderAudit(events ...SourceReaderEvent) []byte {
	var raw []byte
	for _, event := range events {
		data, _ := json.Marshal(event)
		raw = append(append(raw, data...), '\n')
	}
	return raw
}

func TestSourceReaderAuditRequiresExactLaunchManifestAndLifecycle(t *testing.T) {
	c, ready, stopped := readerAuditFixture(t)
	got, err := DecodeSourceReaderAudit(c, encodeReaderAudit(ready, stopped))
	if err != nil || !got.Ready || got.Failed || got.Access == nil || got.Access.ToolCalls != 0 {
		t.Fatal("complete zero-read audit rejected", got, err)
	}
	for _, mutate := range []func(*SourceReaderEvent){
		func(v *SourceReaderEvent) { v.Version++ },
		func(v *SourceReaderEvent) { v.LaunchID = strings.Repeat("b", 32) },
		func(v *SourceReaderEvent) { v.ManifestHash = strings.Repeat("b", 64) },
		func(v *SourceReaderEvent) { v.SessionHash = strings.Repeat("b", 64) },
		func(v *SourceReaderEvent) { v.Socket = "/other-job/source.sock" },
		func(v *SourceReaderEvent) { v.ReaderUID++ },
	} {
		bad := ready
		mutate(&bad)
		if _, err := DecodeSourceReaderAudit(c, encodeReaderAudit(bad, stopped)); err == nil {
			t.Fatal("mismatched reader identity accepted", bad)
		}
	}
	for _, raw := range [][]byte{encodeReaderAudit(ready), encodeReaderAudit(stopped), encodeReaderAudit(stopped, ready), encodeReaderAudit(ready, ready, stopped), append(encodeReaderAudit(ready, stopped), []byte("trailing")...), []byte(strings.Repeat("x", (2<<20)+1))} {
		if _, err := DecodeSourceReaderAudit(c, raw); err == nil {
			t.Fatal("incomplete/repeated/unbounded audit accepted")
		}
	}
	stopped.Failed = true
	got, err = DecodeSourceReaderAudit(c, encodeReaderAudit(stopped))
	if err != nil || got.Ready || !got.Failed || got.Access == nil {
		t.Fatal("failed-before-ready zero-read receipt rejected", got, err)
	}
}

func TestSourceReaderAuditRejectsOutOfScopeReadsAndAmbiguity(t *testing.T) {
	c, ready, stopped := readerAuditFixture(t)
	a := *stopped.Access
	a.ToolCalls, a.ReaderCalls, a.Bytes = 1, 1, 1
	a.Reads = []ReadRecord{{Revision: c.Session.Snapshot.Head, Path: "outside.go", Hash: strings.Repeat("a", 64), Bytes: 1}}
	stopped.Access = &a
	if _, err := DecodeSourceReaderAudit(c, encodeReaderAudit(ready, stopped)); err == nil {
		t.Fatal("receipt widened source manifest")
	}
	_, ready, stopped = readerAuditFixture(t)
	raw := string(encodeReaderAudit(ready, stopped))
	for _, bad := range []string{
		strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(raw, `"failed":false`, `"failed":true,"failed":false`, 1),
		strings.Replace(raw, `"kind":"stopped"`, `"kind":"stopped","unknown":"private-canary"`, 1),
	} {
		if _, err := DecodeSourceReaderAudit(c, []byte(bad)); err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("ambiguous audit accepted/disclosed", err)
		}
	}
}

func TestSourceReaderAuditRejectsTruncatedFinalFrame(t *testing.T) {
	c, ready, stopped := readerAuditFixture(t)
	complete := encodeReaderAudit(ready, stopped)
	// Scanner accepts a final JSON object at EOF without its delimiter. The
	// writer treats that short write as failure, so no prefix may qualify.
	for length := len(encodeReaderAudit(ready)); length < len(complete); length++ {
		got, err := DecodeSourceReaderAudit(c, complete[:length])
		if err == nil || got.Access != nil || got.Ready {
			t.Fatalf("truncated audit accepted at %d of %d bytes: %+v, %v", length, len(complete), got, err)
		}
	}
	if got, err := DecodeSourceReaderAudit(c, complete); err != nil || got.Access == nil || got.Failed || !got.Ready {
		t.Fatal("complete framed audit rejected", got, err)
	}
}
