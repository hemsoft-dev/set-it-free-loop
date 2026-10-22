//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

type oneByteReader struct{ io.Reader }

func (b oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return b.Reader.Read(p)
}
func TestPrivateCrashPointFramingAndMutations(t *testing.T) {
	good := crashPoint{Version: 1, SessionHash: strings.Repeat("a", 64)}
	raw, _ := json.Marshal(good)
	raw = append(raw, '\n')
	decoded, err := readCrashPoint(oneByteReader{bytes.NewReader(raw)})
	if err != nil || r.Digest(decoded) != r.Digest(good) {
		t.Fatal("actual framing rejected", err)
	}
	for _, bad := range [][]byte{raw[:len(raw)-1], []byte("{} garbage\n"), []byte("{\"Version\":1,\"version\":2}\n"), []byte("{\"Version\":1,\"unknown\":true}\n"), []byte(strings.Repeat(" ", 8193) + "\n")} {
		if _, err := readCrashPoint(bytes.NewReader(bad)); err == nil {
			t.Fatal("malformed crash point accepted")
		}
	}
}
func TestCrashPointBindsSourcePolicySessionAndOriginalRoot(t *testing.T) {
	sourceHead, sourceTree = strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		root := t.TempDir()
		os.Chmod(root, 0700)
		c := config{InstanceNonce: strings.Repeat("1", 32), Version: 1, Purpose: "native-process-loss-fake-only", Root: root, ImageSourceHead: strings.Repeat("c", 40), Provider: provider}
		if err := writeOwnedRoot(c); err != nil {
			t.Fatal(err)
		}
		s, err := fixedSession(c)
		if err != nil {
			t.Fatal(err)
		}
		_, policy, err := f.NewRefresh(s, false)
		if err != nil {
			t.Fatal(err)
		}
		dev, ino, err := rootIdentity(root)
		if err != nil {
			t.Fatal(err)
		}
		p := crashPoint{1, sourceHead, sourceTree, c.ImageSourceHead, r.Digest(s), r.Digest(policy), strings.Repeat("d", 64), dev, ino, f.Counts{Inference: 1, Refreshes: 1}}
		if !pointValid(c, p) {
			t.Fatal("valid private point rejected")
		}
		for _, mutate := range []func(*crashPoint){func(p *crashPoint) { p.SessionHash = strings.Repeat("e", 64) }, func(p *crashPoint) { p.PolicyHash = strings.Repeat("e", 64) }, func(p *crashPoint) { p.RootInode++ }, func(p *crashPoint) { p.RootDevice++ }, func(p *crashPoint) { p.Counts.Refreshes = 0 }, func(p *crashPoint) { p.Counts.Inference = 2 }, func(p *crashPoint) { p.Counts.Failures++ }, func(p *crashPoint) { p.Counts.Negotiation++ }, func(p *crashPoint) { p.ImageSourceHead = strings.Repeat("e", 40) }, func(p *crashPoint) { p.RunnerSourceTree = strings.Repeat("e", 40) }} {
			bad := p
			mutate(&bad)
			if pointValid(c, bad) {
				t.Fatal("substituted point accepted")
			}
		}
	}
}
func TestProcessLossNeverAdoptsExistingRootOrOpensManager(t *testing.T) {
	c := config{Root: t.TempDir(), Provider: r.Codex}
	if runParent(c, "/must-remain-unopened") == nil {
		t.Fatal("existing root adopted")
	}
	if runChild(c) == nil {
		t.Fatal("child adopted existing root")
	}
	if _, err := os.Stat(filepath.Join(c.Root, "auth")); !os.IsNotExist(err) {
		t.Fatal("existing root changed")
	}
}
func TestCrashTransportUsesOnlyOwnedLoopbackTLS(t *testing.T) {
	calls := 0
	transport, closeBackend, err := loopbackTransport(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { calls++; w.WriteHeader(204) }))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	for _, endpoint := range []string{"http://fixture.provider.example/v1/responses", "https://other.invalid/v1/responses"} {
		q, _ := http.NewRequestWithContext(context.Background(), "POST", endpoint, nil)
		if _, err := transport.RoundTrip(q); err == nil {
			t.Fatal("outside fake route accepted")
		}
	}
	q, _ := http.NewRequest("POST", "https://fixture.provider.example/v1/responses", nil)
	response, err := transport.RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 || response.StatusCode != 204 {
		t.Fatal("local TLS positive failed")
	}
}

// This is the actual native-unit producer JSON from the first synthetic
// supervisor-loss run at 8d78dc5, not a hand-built copy of consumer fields.
func TestUnitIdentityDecodesActualNativeProducer(t *testing.T) {
	raw, err := os.ReadFile("testdata/actual-native-unit.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "launches", "135983a22d31751fb89b31ff3e7259f3")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "unit.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	record, err := readUnit(root)
	if err != nil {
		t.Fatal(err)
	}
	if record.Identity.InvocationID != "1c651bc3829344ffb92b6a7c84d2dc0f" || !capturedUnitBound(record, record.Binding.SessionHash) {
		t.Fatal("lost actual invocation identity", record.Identity)
	}
}

func TestCapturedUnitRequiresCompleteOriginalIdentity(t *testing.T) {
	raw, err := os.ReadFile("testdata/actual-native-unit.json")
	if err != nil {
		t.Fatal(err)
	}
	var record unitRecord
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*unitRecord){func(u *unitRecord) { u.Identity.InvocationID = "" }, func(u *unitRecord) { u.Identity.BootID = "" }, func(u *unitRecord) { u.Identity.PID = 0 }, func(u *unitRecord) { u.Identity.StartTicks = 0 }, func(u *unitRecord) { u.Captured = false }, func(u *unitRecord) { u.Unit = "other.service" }, func(u *unitRecord) { u.PayloadKind = "fake" }, func(u *unitRecord) { u.ManifestHash = "" }} {
		bad := record
		mutate(&bad)
		if capturedUnitBound(bad, record.Binding.SessionHash) {
			t.Fatal("substituted or missing unit identity accepted")
		}
	}
}

func TestCrashPointRejectsAdditionalRecordsAndTrailingBytes(t *testing.T) {
	raw, _ := json.Marshal(crashPoint{Version: 1})
	raw = append(raw, '\n')
	for _, tail := range []string{"{}\n", "garbage", "\n", " "} {
		bad := append(append([]byte{}, raw...), []byte(tail)...)
		if _, err := readCrashPoint(bytes.NewReader(bad)); err == nil {
			t.Fatal("accepted bytes after one crash point", tail)
		}
	}
}

func TestOwnedRootRecoveryRefusesForeignOrReplacedState(t *testing.T) {
	sourceHead, sourceTree = strings.Repeat("a", 40), strings.Repeat("b", 40)
	c := config{InstanceNonce: strings.Repeat("1", 32), Version: 1, Purpose: "native-process-loss-fake-only", Provider: r.Codex, Root: t.TempDir(), ImageSourceHead: strings.Repeat("c", 40)}
	os.Chmod(c.Root, 0700)
	if err := writeOwnedRoot(c); err != nil {
		t.Fatal(err)
	}
	if err := verifyOwnedRoot(c); err != nil {
		t.Fatal(err)
	}
	foreign := c
	foreign.InstanceNonce = strings.Repeat("2", 32)
	if _, err := recoverOwned(foreign); err == nil {
		t.Fatal("foreign fixture was adopted")
	}
	original := c.Root
	c.Root = t.TempDir()
	os.Chmod(c.Root, 0700)
	if _, err := recoverOwned(c); err == nil {
		t.Fatal("unmarked existing root adopted")
	}
	c.Root = original
	if _, err := recoverOwned(c); err == nil {
		t.Fatal("missing original unit claimed stopped")
	}
}
func TestActualCrashPointProducerClosesItsPipe(t *testing.T) {
	incoming, outgoing, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer incoming.Close()
	want := crashPoint{Version: 1}
	if err = sendCrashPoint(outgoing, want); err != nil {
		t.Fatal(err)
	}
	got, err := readCrashPoint(incoming)
	if err != nil || r.Digest(got) != r.Digest(want) {
		t.Fatal("producer did not deliver exact EOF-bound record", err)
	}
}

func TestFixtureRecordReadsBoundAllocationAndRejectLinks(t *testing.T) {
	root := t.TempDir()
	for _, limit := range []int64{16384, 65536, 100 << 10} {
		file := filepath.Join(root, "record")
		if err := os.WriteFile(file, bytes.Repeat([]byte("x"), int(limit)+1), 0600); err != nil {
			t.Fatal(err)
		}
		if raw, err := readBoundedFixture(file, limit); err == nil || len(raw) != 0 {
			t.Fatal("oversized record was read")
		}
	}
	canary := filepath.Join(root, "canary")
	if err := os.WriteFile(canary, []byte("synthetic-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(canary, link); err != nil {
		t.Fatal(err)
	}
	if raw, err := readBoundedFixture(link, 100); err == nil || len(raw) != 0 {
		t.Fatal("fixture followed a record link")
	}
}
