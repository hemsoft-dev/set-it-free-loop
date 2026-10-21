package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gitReadFixture(t *testing.T, role GitHubCredentialRole, handler http.Handler) (*GitHubInstallationTransport, *atomic.Int32) {
	t.Helper()
	_, options := installationHTTPFixture(t, role)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls.Add(1)
		user, pass, ok := q.BasicAuth()
		if !ok || user != "x-access-token" || pass != "synthetic-installation-reader" {
			t.Error("wrong upstream Git authority")
		}
		if q.Header.Get("Accept-Encoding") != "identity" || q.Header.Get("Cookie") != "" || q.Header.Get("Proxy-Authorization") != "" {
			t.Error("unexpected upstream header")
		}
		q.Header.Del("Authorization") // The actual Git producer never receives a token.
		handler.ServeHTTP(w, q)
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	api := options.Transport
	options.Transport = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		if q.URL.Host == "api.github.com" {
			return api.RoundTrip(q)
		}
		if q.URL.Scheme != "https" || q.URL.Host != "github.com" || q.URL.User != nil || q.GetBody != nil {
			t.Error("wrong Git destination or replayable request")
		}
		wantPath := "/hemsoft-dev/set-it-free-loop.git/info/refs"
		if q.Method == "POST" {
			wantPath = "/hemsoft-dev/set-it-free-loop.git/git-upload-pack"
		}
		if q.URL.Path != wantPath {
			t.Error("wrong Git path")
		}
		clone := q.Clone(q.Context())
		u := *q.URL
		u.Scheme, u.Host = base.Scheme, base.Host
		clone.URL, clone.Host = &u, base.Host
		return server.Client().Transport.RoundTrip(clone)
	})
	installation, err := AcquireGitHubInstallation(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { installation.Close() })
	return installation, &calls
}
func gitReadMedia(operation GitHubGitOperation) string {
	if operation == GitHubGitAdvertisement {
		return "application/x-git-upload-pack-advertisement"
	}
	return "application/x-git-upload-pack-result"
}
func TestGitHubGitReaderActualTLSRequestsAndOpaqueReturn(t *testing.T) {
	for _, protocol := range []int{0, 2} {
		for _, operation := range []GitHubGitOperation{GitHubGitAdvertisement, GitHubGitUploadPack} {
			t.Run(fmt.Sprintf("%d/%s", protocol, operation), func(t *testing.T) {
				payload := []byte("0008NAK\n0000\x00\xff")
				installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					expectedProtocol := ""
					if protocol == 2 {
						expectedProtocol = "version=2"
					}
					if q.Header.Get("Git-Protocol") != expectedProtocol || q.Header.Get("Accept") != gitReadMedia(operation) {
						t.Error("protocol/media not pinned")
					}
					if operation == GitHubGitAdvertisement {
						if q.Method != "GET" || q.URL.RawQuery != "service=git-upload-pack" || q.ContentLength != 0 {
							t.Error("bad advertisement")
						}
					} else {
						raw, err := io.ReadAll(q.Body)
						if err != nil || !bytes.Equal(raw, payload) || q.Header.Get("Content-Type") != "application/x-git-upload-pack-request" || q.URL.RawQuery != "" {
							t.Error("upload bytes changed", err)
						}
					}
					w.Header().Set("Content-Type", gitReadMedia(operation))
					w.Header().Set("Set-Cookie", "synthetic-private-header")
					w.Header().Set("WWW-Authenticate", "synthetic-private-header")
					w.Write(payload)
				}))
				reader, err := NewGitHubGitReader(installation)
				if err != nil {
					t.Fatal(err)
				}
				request := GitHubGitReadRequest{Operation: operation, Protocol: protocol}
				if operation == GitHubGitUploadPack {
					request.Body = io.NopCloser(bytes.NewReader(payload))
				}
				body, err := reader.Exchange(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(body)
				body.Close()
				if err != nil || !bytes.Equal(raw, payload) || calls.Load() != 1 {
					t.Fatal("opaque Git bytes lost", err)
				}
				for _, value := range []any{reader, *reader, body, *body.(*githubGitReadBody)} {
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					for _, s := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(raw)} {
						if !strings.Contains(s, "redacted") || strings.Contains(s, "synthetic-") {
							t.Fatal("private handle formatting exposed")
						}
					}
				}
			})
		}
	}
}
func TestGitHubGitReaderRejectsPublisherInvalidOperationsAndClosedCredential(t *testing.T) {
	publisher, calls := gitReadFixture(t, GitHubPublisher, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("publisher Git HTTP") }))
	if reader, err := NewGitHubGitReader(publisher); reader != nil || err == nil || calls.Load() != 0 {
		t.Fatal("publisher gained Git authority")
	}
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid operation reached HTTP") }))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []GitHubGitReadRequest{{Operation: "receive-pack"}, {Operation: GitHubGitAdvertisement, Protocol: 1}, {Operation: GitHubGitUploadPack}, {Operation: ""}} {
		if body, err := reader.Exchange(context.Background(), request); body != nil || err == nil {
			t.Fatal("invalid operation accepted")
		}
	}
	probe := &installationBodyProbe{}
	if body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement, Body: probe}); body != nil || err == nil || probe.closed != 1 {
		t.Fatal("rejected body not closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx, httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{})} {
		if body, err := reader.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
			t.Fatal("invalid context accepted")
		}
	}
	installation.state.now = func() time.Time { return controlTime.Add(59 * time.Minute) }
	if body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
		t.Fatal("expiring credential accepted")
	}
	installation.state.now = func() time.Time { return controlTime.Add(-time.Second) }
	if body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
		t.Fatal("backward clock accepted")
	}
	installation.state.now = func() time.Time { return controlTime }
	installation.Close()
	if body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
		t.Fatal("closed credential accepted")
	}
	if fresh, err := NewGitHubGitReader(installation); fresh != nil || err == nil {
		t.Fatal("closed credential created capability")
	}
	for _, installation := range []*GitHubInstallationTransport{nil, {}} {
		if r, err := NewGitHubGitReader(installation); r != nil || err == nil {
			t.Fatal("nil constructor accepted")
		}
	}
	for _, r := range []*GitHubGitReader{nil, {}} {
		probe := &installationBodyProbe{}
		if body, err := r.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: probe}); body != nil || err == nil || probe.closed != 1 {
			t.Fatal("nil capability leaked body")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("denied Git operation reached upstream")
	}
}
func TestGitHubGitReaderRejectsResponsesWithoutRedirectOrErrorLeak(t *testing.T) {
	for _, kind := range []string{"redirect", "auth", "html", "encoding", "oversize", "duplicate-type"} {
		t.Run(kind, func(t *testing.T) {
			installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				w.Header().Set("Content-Type", gitReadMedia(GitHubGitAdvertisement))
				switch kind {
				case "redirect":
					w.Header().Set("Location", "https://outside.invalid/synthetic-installation-reader")
					w.WriteHeader(307)
				case "auth":
					w.Header().Set("WWW-Authenticate", "synthetic-installation-reader")
					w.WriteHeader(401)
				case "html":
					w.Header().Set("Content-Type", "text/html")
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "oversize":
					w.Header().Set("Content-Length", strconv.FormatInt(githubGitAdvertisementLimit+1, 10))
				case "duplicate-type":
					w.Header().Add("Content-Type", gitReadMedia(GitHubGitAdvertisement))
				}
				io.WriteString(w, "synthetic-installation-reader private upstream body")
			}))
			reader, err := NewGitHubGitReader(installation)
			if err != nil {
				t.Fatal(err)
			}
			body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
			if body != nil || !errors.Is(err, ErrGitHubGitRead) || strings.Contains(err.Error(), "synthetic-") || calls.Load() != 1 {
				t.Fatal("bad response leaked or redirected", body, err, calls.Load())
			}
		})
	}
}
func TestGitHubGitReaderRequestAndStreamingResponseBounds(t *testing.T) {
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", gitReadMedia(GitHubGitAdvertisement))
		w.(http.Flusher).Flush()
		// Actual TLS response with unknown length exceeds the advertised byte bound.
		io.Copy(w, io.LimitReader(gitReadZeros{}, githubGitAdvertisementLimit+1))
	}))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, githubGitRequestLimit + 1} {
		request := GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: io.NopCloser(io.LimitReader(gitReadZeros{}, size))}
		if body, err := reader.Exchange(context.Background(), request); body != nil || err == nil {
			t.Fatal("request bound bypassed")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("oversize request dispatched")
	}
	body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, body)
	body.Close()
	if n != githubGitAdvertisementLimit || !errors.Is(err, ErrGitHubGitRead) {
		t.Fatal("streaming response exceeded bound", n, err)
	}
}

type gitReadZeros struct{}

func (gitReadZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestGitHubGitReaderCancellationClosesInputAndResponse(t *testing.T) {
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", gitReadMedia(GitHubGitAdvertisement))
		w.(http.Flusher).Flush()
		<-q.Context().Done()
	}))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	body, err := reader.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: input})
	cancel()
	if body != nil || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatal("canceled request body reached HTTP", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	body, err = reader.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, body); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrGitHubGitRead) {
			t.Fatal("canceled response did not fail", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response cancellation did not unblock read")
	}
	body.Close()
}

func TestGitHubGitReaderActualGitV0V2ThroughTLSBackend(t *testing.T) {
	fixture := newSnapshotGitFixture(t)
	execPath := fixture.run(t, "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("Git HTTP backend required")
	}
	root, home := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "hemsoft-dev", "set-it-free-loop.git")
	if err := os.MkdirAll(filepath.Dir(repo), 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1"}
	runBytes := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, fixture.git, append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
		command.Env = env
		var stderr bytes.Buffer
		command.Stderr = &stderr
		raw, err := command.Output()
		if err != nil {
			t.Fatalf("actual Git %v: %s %v", args, stderr.Bytes(), err)
		}
		return raw
	}
	run := func(args ...string) string { return strings.TrimSpace(string(runBytes(args...))) }
	t.Logf("actual producer %s; executable=%s; backend=%s", run("--version"), fixture.git, backend)
	run("clone", "--bare", "--no-hardlinks", fixture.dir, repo)
	var seen atomic.Int32
	installation, _ := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "" {
			t.Error("token passed to Git backend")
		}
		cgiHandler := &cgi.Handler{Path: backend, Env: append(append([]string{}, env...), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL="+q.Header.Get("Git-Protocol")), Stderr: io.Discard}
		cgiHandler.ServeHTTP(w, q)
	}))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	// This plain loopback server is a test bridge, not the future Unix peer boundary.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var operation GitHubGitOperation
		switch q.Method + " " + q.URL.RequestURI() {
		case "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack":
			operation = GitHubGitAdvertisement
		case "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack":
			operation = GitHubGitUploadPack
		default:
			t.Error("unexpected actual Git route")
			w.WriteHeader(403)
			return
		}
		protocol := 0
		if q.Header.Get("Git-Protocol") == "version=2" {
			protocol = 2
		}
		request := GitHubGitReadRequest{Operation: operation, Protocol: protocol}
		if operation == GitHubGitUploadPack {
			request.Body = q.Body
		}
		body, err := reader.Exchange(q.Context(), request)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer body.Close()
		seen.Add(1)
		w.Header().Set("Content-Type", gitReadMedia(operation))
		if _, err := io.Copy(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer bridge.Close()
	for _, protocol := range []int{0, 2} {
		destination := filepath.Join(t.TempDir(), "fetched.git")
		run("init", "--bare", destination)
		before := seen.Load()
		run("-C", destination, "-c", "protocol.version="+strconv.Itoa(protocol), "-c", "http.proxy=", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.postBuffer=1048576", "fetch", "--no-tags", "--no-recurse-submodules", bridge.URL+"/hemsoft-dev/set-it-free-loop.git", "HEAD:refs/heads/fetched")
		if seen.Load()-before < 2 {
			t.Fatal("actual Git did not use both routes")
		}
		if run("--git-dir="+destination, "rev-parse", "refs/heads/fetched") != fixture.snapshot.Head {
			t.Fatal("actual fetched tip mismatch")
		}
		run("--git-dir="+destination, "fsck", "--full", "--strict")
		objects := strings.Fields(run("--git-dir="+destination, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"))
		t.Logf("protocol=%d exchanges=%d exact-objects=%d full-strict-fsck=pass", protocol, seen.Load()-before, len(objects))
		for _, object := range objects {
			typ := run("--git-dir="+destination, "cat-file", "-t", object)
			original := runBytes("--git-dir="+repo, "cat-file", typ, object)
			fetched := runBytes("--git-dir="+destination, "cat-file", typ, object)
			if !bytes.Equal(fetched, original) {
				t.Fatal("actual fetched object bytes differ", object)
			}
		}
	}
}

type gitReadFaultBody struct {
	reader     io.Reader
	closed     atomic.Int32
	closeError bool
}

func (b *gitReadFaultBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *gitReadFaultBody) Close() error {
	b.closed.Add(1)
	if b.closeError {
		return errors.New("synthetic-private-close-error")
	}
	return nil
}

type gitReadFaultReader struct{}

func (gitReadFaultReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic-private-read-error")
}
func TestGitHubGitReaderRedactsTransportAndBodyErrors(t *testing.T) {
	for _, kind := range []string{"transport", "nil-response", "nil-body", "duplicate-case-type", "read-error", "close-error"} {
		t.Run(kind, func(t *testing.T) {
			installation, _ := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("fault test reached TLS") }))
			reader, err := NewGitHubGitReader(installation)
			if err != nil {
				t.Fatal(err)
			}
			probe := &gitReadFaultBody{reader: strings.NewReader("0000")}
			installation.state.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
				if deadline, ok := q.Context().Deadline(); !ok || time.Until(deadline) > 90*time.Second {
					t.Error("operation deadline absent")
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {gitReadMedia(GitHubGitAdvertisement)}}, Body: probe, ContentLength: -1, Request: q}
				switch kind {
				case "transport":
					return response, errors.New("synthetic-installation-reader upstream private")
				case "nil-response":
					return nil, nil
				case "nil-body":
					response.Body = nil
				case "duplicate-case-type":
					response.Header["content-type"] = []string{gitReadMedia(GitHubGitAdvertisement)}
				case "read-error":
					probe.reader = gitReadFaultReader{}
				case "close-error":
					probe.closeError = true
				}
				return response, nil
			})
			body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
			switch kind {
			case "read-error":
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.ReadAll(body)
				if !errors.Is(err, ErrGitHubGitRead) || strings.Contains(err.Error(), "synthetic-") {
					t.Fatal("private read error escaped", err)
				}
			case "close-error":
				if err != nil {
					t.Fatal(err)
				}
				err = body.Close()
				if !errors.Is(err, ErrGitHubGitRead) || strings.Contains(err.Error(), "synthetic-") {
					t.Fatal("private close error escaped", err)
				}
			default:
				if body != nil || !errors.Is(err, ErrGitHubGitRead) || strings.Contains(err.Error(), "synthetic-") {
					t.Fatal("private transport error escaped", err)
				}
			}
			if kind != "nil-response" && kind != "nil-body" && probe.closed.Load() != 1 {
				t.Fatal("response body not closed exactly once", probe.closed.Load())
			}
		})
	}
}
func TestGitHubGitReaderNoRegularRESTAuthorityExpansion(t *testing.T) {
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("ordinary transport gained Git authority") }))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader
	for _, q := range []*http.Request{
		installationQuery(t, "POST", githubRepoPath+"/git-upload-pack"),
		func() *http.Request {
			q, _ := http.NewRequest("GET", githubGitRepository+"/info/refs?service=git-upload-pack", nil)
			return q
		}(),
	} {
		if response, err := installation.RoundTrip(q); response != nil || err == nil {
			t.Fatal("ordinary transport widened")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("denied ordinary request reached Git upstream")
	}
}
func TestGitHubGitReaderExactStreamingBoundAndRequestBytePreservation(t *testing.T) {
	var expected []byte
	for i := 0; i < 256; i++ {
		expected = append(expected, byte(i))
	}
	installation, _ := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		raw, err := io.ReadAll(q.Body)
		if err != nil || !bytes.Equal(raw, expected) {
			t.Error("binary upload request changed", err)
		}
		w.Header().Set("Content-Type", gitReadMedia(GitHubGitUploadPack))
		w.Write(raw)
	}))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Protocol: 2, Body: io.NopCloser(bytes.NewReader(expected))})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	body.Close()
	if err != nil || !bytes.Equal(raw, expected) {
		t.Fatal("binary response changed", err)
	}
	// Exercise the larger POST response bound without materializing 64 MiB.
	for _, excess := range []int64{0, 1} {
		probe := &gitReadFaultBody{reader: io.LimitReader(gitReadZeros{}, (64<<20)+excess)}
		installation.state.inner = githubResponseTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {gitReadMedia(GitHubGitUploadPack)}}, Body: probe, ContentLength: -1}, nil
		})
		body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: io.NopCloser(strings.NewReader("0000"))})
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, body)
		body.Close()
		if n != 64<<20 || (excess == 0 && err != nil) || (excess == 1 && !errors.Is(err, ErrGitHubGitRead)) || probe.closed.Load() != 1 {
			t.Fatal("POST streaming bound incorrect", n, err, probe.closed.Load())
		}
	}
}
