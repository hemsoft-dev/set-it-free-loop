//go:build linux

// claudefixture is an offline protocol test, never a live launcher.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"net"
	"os"
	"strings"
	"sync"
)

const source = "immutable source evidence\n"

var head = strings.Repeat("a", 40)
var logMu sync.Mutex

func record(name string, v any) {
	logMu.Lock()
	defer logMu.Unlock()
	f, e := os.OpenFile("/artifacts/"+name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		panic(e)
	}
	defer f.Close()
	if e = json.NewEncoder(f).Encode(v); e != nil {
		panic(e)
	}
}
func write(name string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		panic(e)
	}
	if e = os.WriteFile("/artifacts/"+name, append(b, '\n'), 0600); e != nil {
		panic(e)
	}
}

type store struct{}

func (store) Read(_ context.Context, revision, path string, max int) (r.Object, error) {
	if revision != head || path != "source.go" {
		return r.Object{}, errors.New("fixture object denied")
	}
	return r.Object{Mode: "100644", Data: []byte(source)}, nil
}

type sourceTools struct{ reader *r.Reader }

func (t sourceTools) Read(ctx context.Context, revision, path string) (string, error) {
	text, e := t.reader.Read(ctx, revision, path)
	record("source.jsonl", map[string]any{"tool": "read", "revision": revision, "path": path, "bytes": len(text), "success": e == nil})
	return text, e
}
func (t sourceTools) Search(ctx context.Context, revision, needle string, max int) ([]r.Anchor, error) {
	matches, e := t.reader.Search(ctx, revision, needle, max)
	record("source.jsonl", map[string]any{"tool": "search", "revision": revision, "matches": len(matches), "success": e == nil})
	return matches, e
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error) {
	n, e := os.Stdin.Read(p)
	if n > 0 {
		record("mcp-wire.jsonl", map[string]any{"direction": "in", "data": string(p[:n])})
	}
	return n, e
}
func (stdio) Write(p []byte) (int, error) {
	record("mcp-wire.jsonl", map[string]any{"direction": "out", "data": string(p)})
	return os.Stdout.Write(p)
}
func (stdio) Close() error { return errors.Join(os.Stdin.Close(), os.Stdout.Close()) }

type limited struct {
	buf bytes.Buffer
	max int
}

func (b *limited) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, errors.New("fixture output cap")
	}
	return b.buf.Write(p)
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--socket" {
		snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
		reader, e := r.NewReader(store{}, snap, []string{"source.go"}, r.DefaultLimits())
		if e != nil {
			panic(e)
		}
		if e = r.ServeSourceMCP(context.Background(), r.Invocation{Source: sourceTools{reader}, Limits: r.DefaultLimits()}, stdio{}); e != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 || os.Args[1] != "--fixture" || os.Getenv("SFL_FAKE_CLIENT_FIXTURE") != "1" {
		fmt.Fprintln(os.Stderr, "run only through scripts/claude-fixture.py")
		os.Exit(2)
	}
	addresses, e := net.InterfaceAddrs()
	if e != nil {
		panic(e)
	}
	for _, address := range addresses {
		ip, _, e := net.ParseCIDR(address.String())
		if e != nil || !ip.IsLoopback() {
			panic("offline fixture requires loopback-only namespace")
		}
	}
	if len(addresses) == 0 {
		panic("missing fixture loopback")
	}
	claudeMain()
}
