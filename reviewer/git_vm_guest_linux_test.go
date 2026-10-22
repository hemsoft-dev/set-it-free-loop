//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func gitVMGuestTestRequest(t *testing.T) gitVMGuestRequest {
	t.Helper()
	read := gitVMReadTestBinding()
	source := gitVMRequestFixture(t, read.Source)
	read.Source = source.Binding
	q := gitVMGuestRequest{1, gitVMGuestPurpose, source, read}
	if !q.valid() {
		t.Fatal("invalid synthetic request")
	}
	return q
}

func TestGitVMGuestHostEntryRefuses(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("host-only refusal test")
	}
	for _, args := range [][]string{nil, {"--source-worker", Digest("request")}, {"--native-worker"}, {"--source-worker"}} {
		if GitVMGuestMain(args) != 2 {
			t.Fatalf("host entry accepted %q", args)
		}
	}
	if _, err := gitVMGuestWorkerArgs(gitVMGuestRequest{}); err == nil {
		t.Fatal("empty request accepted")
	}
}

func TestGitVMGuestReleaseAndPhasesBindRequest(t *testing.T) {
	q := gitVMGuestTestRequest(t)
	r := gitVMGuestRelease{Version: 1, Purpose: gitVMGuestPurpose, SourceHead: strings.Repeat("a", 40), Worker: q.Source.Manifest.Worker, NamespaceHelper: q.Source.Manifest.NamespaceHelper, Runtime: q.Source.Manifest.Runtime, BootstrapLibraries: append([]nativeRuntimeFile(nil), q.Source.Manifest.BootstrapLibraries...)}
	if !r.matches(q) {
		t.Fatal("matching release refused")
	}
	r.Worker.SHA256 = Digest("other worker")
	if r.matches(q) {
		t.Fatal("foreign worker accepted")
	}
	r.Worker = q.Source.Manifest.Worker
	r.Purpose = "native-review"
	if r.matches(q) {
		t.Fatal("foreign purpose accepted")
	}
	p := gitVMGuestPhase{1, gitVMGuestPurpose, Digest(q), "host-grant"}
	if !p.valid(q, "host-grant") || p.valid(q, "worker-grant") {
		t.Fatal("phase binding failed")
	}
	foreign := q
	foreign.Read.ChannelID = strings.Repeat("c", 32)
	if !foreign.valid() || p.valid(foreign, "host-grant") {
		t.Fatal("request binding failed")
	}
}

type gitVMGuestShortWriter struct{}

func (gitVMGuestShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestGitVMGuestExportWriterBounds(t *testing.T) {
	var sink bytes.Buffer
	w := gitVMGuestExportWriter{ctx: context.Background(), target: &sink, remaining: 3}
	if n, err := w.Write([]byte("four")); n != 0 || err == nil || sink.Len() != 0 {
		t.Fatal("oversize write reached target")
	}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil || w.bytes != 3 || w.remaining != 0 {
		t.Fatal("bounded write failed")
	}
	if n, err := w.Write([]byte("d")); n != 0 || err == nil {
		t.Fatal("aggregate bound exceeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = gitVMGuestExportWriter{ctx: ctx, target: &sink, remaining: 9}
	if n, err := w.Write([]byte("x")); n != 0 || err == nil || sink.String() != "abc" {
		t.Fatal("cancelled write reached target")
	}
	w = gitVMGuestExportWriter{ctx: context.Background(), target: gitVMGuestShortWriter{}, remaining: 9}
	if n, err := w.Write([]byte("xyz")); n != 2 || !errors.Is(err, io.ErrShortWrite) || w.bytes != 2 || w.remaining != 7 {
		t.Fatal("short write accounting failed")
	}
}
