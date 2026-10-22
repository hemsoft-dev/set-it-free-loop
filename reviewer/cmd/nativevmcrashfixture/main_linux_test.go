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
	root := t.TempDir()
	os.Chmod(root, 0700)
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		c := config{Version: 1, Purpose: "native-process-loss-fake-only", Root: root, ImageSourceHead: strings.Repeat("c", 40), Provider: provider}
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
		for _, mutate := range []func(*crashPoint){func(p *crashPoint) { p.SessionHash = strings.Repeat("e", 64) }, func(p *crashPoint) { p.PolicyHash = strings.Repeat("e", 64) }, func(p *crashPoint) { p.RootInode++ }, func(p *crashPoint) { p.RootDevice++ }, func(p *crashPoint) { p.Counts.Refreshes = 0 }, func(p *crashPoint) { p.Counts.Inference = 2 }, func(p *crashPoint) { p.Counts.Failures++ }, func(p *crashPoint) { p.ImageSourceHead = strings.Repeat("e", 40) }, func(p *crashPoint) { p.RunnerSourceTree = strings.Repeat("e", 40) }} {
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
