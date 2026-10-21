//go:build linux

package reviewer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gitExchangeFunc func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error)

func (f gitExchangeFunc) Exchange(ctx context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
	return f(ctx, q)
}

type gitBrokerTestResult struct {
	audit GitReadBrokerAudit
	err   error
}

func startGitBrokerTest(t *testing.T, source GitReadExchanger, admit func(context.Context, GitReadPeer) error) (GitReadEndpoint, func() gitBrokerTestResult) {
	t.Helper()
	directory, err := os.MkdirTemp("", "sfl-git-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan GitReadEndpoint, 1)
	done := make(chan gitBrokerTestResult, 1)
	go func() {
		audit, err := ServeGitReadSocket(ctx, GitReadBrokerOptions{directory, source, admit}, func(e GitReadEndpoint) error { ready <- e; return nil })
		done <- gitBrokerTestResult{audit, err}
	}()
	finished := false
	finish := func() gitBrokerTestResult {
		if finished {
			t.Fatal("test broker already finished")
		}
		finished = true
		cancel()
		select {
		case result := <-done:
			if !result.audit.Stopped {
				t.Fatal("broker did not stop", result)
			}
			return result
		case <-time.After(2 * time.Second):
			t.Fatal("broker did not drain")
			return gitBrokerTestResult{}
		}
	}
	t.Cleanup(func() {
		if !finished {
			finish()
		}
	})
	select {
	case endpoint := <-ready:
		return endpoint, finish
	case result := <-done:
		finished = true
		cancel()
		t.Fatal("broker failed startup", result)
	case <-time.After(time.Second):
		cancel()
		t.Fatal("broker startup timed out")
	}
	return GitReadEndpoint{}, finish
}
func ownGitBrokerPeer(ctx context.Context, peer GitReadPeer) error {
	if ctx.Err() != nil || peer.PID != os.Getpid() || peer.UID != uint32(os.Getuid()) || peer.GID != uint32(os.Getgid()) {
		return ErrGitReadBroker
	}
	return nil
}
func gitBrokerConnect(t *testing.T, endpoint GitReadEndpoint) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply != [2]byte{5, 0} {
		t.Fatal("SOCKS greeting", reply, err)
	}
	request := append([]byte{5, 1, 0, 3, byte(len("sfl.invalid"))}, []byte("sfl.invalid")...)
	request = append(request, 0, 80)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var success [10]byte
	if _, err := io.ReadFull(conn, success[:]); err != nil || success != [10]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0} {
		t.Fatal("SOCKS target", success, err)
	}
	return conn
}
func gitBrokerReply(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 {
		t.Fatal("Git HTTP response", response.StatusCode, err)
	}
	return raw
}
func TestGitReadBrokerChunkedGzipContinueAndExactPayload(t *testing.T) {
	for _, kind := range []string{"advertisement", "length", "chunked", "gzip", "continue"} {
		t.Run(kind, func(t *testing.T) {
			payload := []byte("0000\x00\xff\r\n")
			var calls atomic.Int32
			source := gitExchangeFunc(func(ctx context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				if q.Protocol != 2 {
					t.Error("protocol lost")
				}
				if kind == "advertisement" {
					if q.Operation != GitHubGitAdvertisement || q.Body != nil {
						t.Error("advertisement changed")
					}
				} else {
					raw, err := io.ReadAll(q.Body)
					q.Body.Close()
					if err != nil || !bytes.Equal(raw, payload) || q.Operation != GitHubGitUploadPack {
						t.Error("Git request bytes changed", err)
					}
				}
				return io.NopCloser(bytes.NewReader(payload)), nil
			})
			endpoint, finish := startGitBrokerTest(t, source, ownGitBrokerPeer)
			conn := gitBrokerConnect(t, endpoint)
			if kind == "advertisement" {
				fmt.Fprintf(conn, "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nGit-Protocol: version=2\r\n\r\n")
			} else {
				headers := ""
				data := payload
				switch kind {
				case "chunked":
					headers = "Transfer-Encoding: chunked\r\n"
					data = []byte(fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(payload), payload))
				case "gzip":
					var compressed bytes.Buffer
					zip := gzip.NewWriter(&compressed)
					zip.Write(payload)
					zip.Close()
					data = compressed.Bytes()
					headers = fmt.Sprintf("Content-Encoding: gzip\r\nContent-Length: %d\r\n", len(data))
				default:
					headers = fmt.Sprintf("Content-Length: %d\r\n", len(data))
				}
				if kind == "continue" {
					headers += "Expect: 100-continue\r\n"
				}
				fmt.Fprintf(conn, "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nGit-Protocol: version=2\r\nContent-Type: application/x-git-upload-pack-request\r\n%s\r\n", headers)
				if kind == "continue" {
					var interim [25]byte
					if _, err := io.ReadFull(conn, interim[:]); err != nil || string(interim[:]) != "HTTP/1.1 100 Continue\r\n\r\n" {
						t.Fatal("100 continue", string(interim[:]), err)
					}
				}
				conn.Write(data)
			}
			if raw := gitBrokerReply(t, conn); !bytes.Equal(raw, payload) {
				t.Fatal("Git output bytes changed")
			}
			conn.Close()
			result := finish()
			if calls.Load() != 1 || result.audit.Requests != 1 || result.audit.ResponseBytes != int64(len(payload)) {
				t.Fatal("broker counters incorrect", calls.Load(), result)
			}
			if _, err := os.Lstat(endpoint.Socket); !os.IsNotExist(err) {
				t.Fatal("broker socket survived stop", err)
			}
			if _, err := os.Stat(strings.TrimSuffix(endpoint.Socket, "git.sock") + "git-read.used"); err != nil {
				t.Fatal("used-attempt marker missing", err)
			}
		})
	}
}
func TestGitReadBrokerRejectsAmbiguousAndUnauthorizedHTTPBeforeSource(t *testing.T) {
	get := "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\n"
	post := "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nContent-Type: application/x-git-upload-pack-request\r\n"
	for name, request := range map[string]string{
		"auth":             get + "Authorization: synthetic-private-token\r\n\r\n",
		"cookie":           get + "Cookie: synthetic-private-cookie\r\n\r\n",
		"host":             get + "host: outside.invalid\r\n\r\n",
		"receive-pack":     "POST /hemsoft-dev/set-it-free-loop.git/git-receive-pack HTTP/1.1\r\nHost: sfl.invalid\r\nContent-Length: 4\r\n\r\n0000",
		"other-repo":       "GET /hemsoft-dev/other.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\n\r\n",
		"absolute-uri":     "GET http://sfl.invalid/hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\n\r\n",
		"duplicate-length": post + "Content-Length: 4\r\ncontent-length: 4\r\n\r\n0000",
		"te-and-length":    post + "Transfer-Encoding: chunked\r\nContent-Length: 4\r\n\r\n4\r\n0000\r\n0\r\n\r\n",
		"folded":           get + "Pragma: no-cache\r\n continued\r\n\r\n",
		"protocol":         get + "Git-Protocol: version=3\r\n\r\n",
		"trailer":          post + "Transfer-Encoding: chunked\r\n\r\n4\r\n0000\r\n0\r\nX-Private: synthetic-private\r\n\r\n",
		"oversize":         post + "Content-Length: 4194305\r\nExpect: 100-continue\r\n\r\n",
		"content-type":     strings.Replace(post, "application/x-git-upload-pack-request", "text/plain", 1) + "Content-Length: 4\r\n\r\n0000",
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			endpoint, finish := startGitBrokerTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				return io.NopCloser(strings.NewReader("forbidden")), nil
			}), ownGitBrokerPeer)
			conn := gitBrokerConnect(t, endpoint)
			conn.Write([]byte(request))
			raw, _ := io.ReadAll(conn)
			conn.Close()
			result := finish()
			if calls.Load() != 0 || result.audit.Requests != 0 || result.audit.Rejected != 1 || bytes.Contains(raw, []byte("synthetic-private")) || bytes.Contains(raw, []byte("100 Continue")) {
				t.Fatal("denied request dispatched or leaked", calls.Load(), result, string(raw))
			}
		})
	}
}
func TestGitReadBrokerPeerRefusalAndCancellationDrain(t *testing.T) {
	t.Run("peer-refused-before-SOCKS", func(t *testing.T) {
		var calls atomic.Int32
		endpoint, finish := startGitBrokerTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
			calls.Add(1)
			return nil, ErrGitReadBroker
		}), func(context.Context, GitReadPeer) error { return errors.New("private peer reason") })
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte{5, 1, 0})
		raw, _ := io.ReadAll(conn)
		conn.Close()
		result := finish()
		if len(raw) != 0 || calls.Load() != 0 || result.audit.Rejected != 1 {
			t.Fatal("peer refused after disclosure", result, string(raw))
		}
	})
	for _, stage := range []string{"SOCKS", "headers", "input", "exchange", "output"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			pipe, writer := io.Pipe()
			defer writer.Close()
			source := gitExchangeFunc(func(ctx context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
				close(entered)
				if stage == "exchange" {
					<-ctx.Done()
					return nil, errors.New("private exchange cancellation")
				}
				return pipe, nil
			})
			endpoint, finish := startGitBrokerTest(t, source, ownGitBrokerPeer)
			var conn *net.UnixConn
			var err error
			if stage == "SOCKS" {
				conn, err = net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				conn.Write([]byte{5})
			} else {
				conn = gitBrokerConnect(t, endpoint)
				switch stage {
				case "headers":
					io.WriteString(conn, "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost:")
				case "input":
					io.WriteString(conn, "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nContent-Type: application/x-git-upload-pack-request\r\nContent-Length: 4\r\n\r\n0")
				default:
					io.WriteString(conn, "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\n\r\n")
					select {
					case <-entered:
					case <-time.After(time.Second):
						t.Fatal("source not reached")
					}
				}
			}
			result := finish()
			conn.Close()
			pipe.Close()
			if !errors.Is(result.err, context.Canceled) {
				t.Fatal("cancellation result lost", result)
			}
		})
	}
}

func TestGitReadBrokerInetDeniedExec(t *testing.T) {
	if os.Getenv("SFL_GIT_BROKER_TEST_EXEC") != "1" {
		return
	}
	runtime.LockOSThread()
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		t.Fatal("unsupported test filter architecture")
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(unix.SYS_SOCKET), Jf: 4},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET, Jt: 1},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET6, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil || unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0) != nil {
		t.Fatal("test socket filter failed")
	}
	runtime.KeepAlive(filters)
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		fd, err := unix.Socket(family, unix.SOCK_STREAM, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		if err != unix.EPERM {
			t.Fatal("Internet socket was not denied", family, err)
		}
	}
	split := -1
	for i, arg := range os.Args {
		if arg == "--" {
			split = i + 1
			break
		}
	}
	if split < 0 || split >= len(os.Args) || !filepath.IsAbs(os.Args[split]) {
		t.Fatal("explicit Git executable required")
	}
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "SFL_GIT_BROKER_TEST_EXEC=") {
			env = append(env, value)
		}
	}
	if err := unix.Exec(os.Args[split], os.Args[split:], env); err != nil {
		t.Fatal(err)
	}
}
func TestGitReadBrokerActualGitUnixSOCKSWithoutInternetSockets(t *testing.T) {
	fixture := newSnapshotGitFixture(t)
	fixture.run(t, "update-ref", "refs/heads/base", fixture.snapshot.Base)
	execPath := fixture.run(t, "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	helper := filepath.Join(execPath, "git-remote-http")
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Skip("Git HTTP helper required")
	}
	helperHash := fmt.Sprintf("%x", sha256.Sum256(helperBytes))
	root, home := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "hemsoft-dev", "set-it-free-loop.git")
	if err := os.MkdirAll(filepath.Dir(repo), 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "NO_PROXY=", "no_proxy="}
	run := func(args ...string) []byte {
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
	run("clone", "--bare", "--no-hardlinks", fixture.dir, repo)
	installation, upstreamCalls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "" {
			t.Error("credential reached backend")
		}
		handler := &cgi.Handler{Path: backend, Env: append(append([]string{}, env...), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL="+q.Header.Get("Git-Protocol")), Stderr: io.Discard}
		handler.ServeHTTP(w, q)
	}))
	source, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	var rootPID atomic.Int64
	var admitted atomic.Int32
	admit := func(ctx context.Context, peer GitReadPeer) error {
		observed, observeErr := os.ReadFile(fmt.Sprintf("/proc/%d/exe", peer.PID))
		executableInfo, statErr := os.Stat(fmt.Sprintf("/proc/%d/exe", peer.PID))
		expectedInfo, expectedErr := os.Stat(helper)
		if peer.UID != uint32(os.Getuid()) || peer.GID != uint32(os.Getgid()) || observeErr != nil || statErr != nil || expectedErr != nil || !os.SameFile(executableInfo, expectedInfo) || fmt.Sprintf("%x", sha256.Sum256(observed)) != helperHash {
			return ErrGitReadBroker
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", peer.PID))
		if err != nil || !bytes.Contains(status, []byte("Seccomp:\t2")) {
			return ErrGitReadBroker
		}
		for ctx.Err() == nil && rootPID.Load() == 0 {
			select {
			case <-ctx.Done():
				return ErrGitReadBroker
			case <-time.After(time.Millisecond):
			}
		}
		pid := peer.PID
		for range 8 {
			if pid == int(rootPID.Load()) {
				admitted.Add(1)
				return nil
			}
			raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err != nil {
				return ErrGitReadBroker
			}
			end := strings.LastIndexByte(string(raw), ')')
			if end < 0 {
				return ErrGitReadBroker
			}
			fields := strings.Fields(string(raw[end+1:]))
			if len(fields) < 20 {
				return ErrGitReadBroker
			}
			pid, err = strconv.Atoi(fields[1])
			if err != nil || pid <= 1 {
				return ErrGitReadBroker
			}
		}
		return ErrGitReadBroker
	}
	endpoint, finish := startGitBrokerTest(t, source, admit)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []int{0, 2} {
		destination := filepath.Join(t.TempDir(), "fetched.git")
		run("init", "--bare", destination)
		args := []string{"-test.run=^TestGitReadBrokerInetDeniedExec$", "--", fixture.git, "-c", "core.hooksPath=/dev/null", "-C", destination, "-c", "protocol.version=" + strconv.Itoa(protocol), "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.postBuffer=1048576", "-c", "http.proxy=socks5h://localhost" + endpoint.Socket, "fetch", "--no-tags", "--no-recurse-submodules", endpoint.URL, "HEAD:refs/heads/head", "refs/heads/base:refs/heads/base"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, executable, args...)
		command.Env = append(append([]string{}, env...), "SFL_GIT_BROKER_TEST_EXEC=1")
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		rootPID.Store(0)
		if err := command.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		rootPID.Store(int64(command.Process.Pid))
		err := command.Wait()
		cancel()
		if err != nil {
			t.Fatalf("Unix actual Git v%d failed: %v %s %s", protocol, err, stdout.Bytes(), stderr.Bytes())
		}
		for ref, want := range map[string]string{"head": fixture.snapshot.Head, "base": fixture.snapshot.Base} {
			if got := strings.TrimSpace(string(run("--git-dir="+destination, "rev-parse", "refs/heads/"+ref))); got != want {
				t.Fatal("fetched tip differs", ref, got, want)
			}
		}
		if got := strings.TrimSpace(string(run("--git-dir="+destination, "merge-base", "--all", "refs/heads/base", "refs/heads/head"))); got != fixture.snapshot.MergeBase {
			t.Fatal("merge-base differs", got)
		}
		run("--git-dir="+destination, "fsck", "--full", "--strict")
		objects := strings.Fields(string(run("--git-dir="+destination, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")))
		for _, object := range objects {
			kind := strings.TrimSpace(string(run("--git-dir="+destination, "cat-file", "-t", object)))
			if !bytes.Equal(run("--git-dir="+repo, "cat-file", kind, object), run("--git-dir="+destination, "cat-file", kind, object)) {
				t.Fatal("fetched raw object differs", object)
			}
		}
		t.Logf("actual Git v%d, inherited AF_INET/AF_INET6 socket denial, pinned helper peer, two exact tips, unique merge-base, %d raw objects and full strict fsck passed", protocol, len(objects))
	}
	result := finish()
	if result.audit.Failed || admitted.Load() < 4 || result.audit.Requests < 4 || int32(result.audit.Requests) != upstreamCalls.Load() {
		t.Fatal("actual broker evidence incomplete", result, admitted.Load(), upstreamCalls.Load())
	}
}

func TestGitReadBrokerAggregateOverflowIsAnHTTPTransferFailure(t *testing.T) {
	source := gitExchangeFunc(func(ctx context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
		if q.Body != nil {
			io.Copy(io.Discard, q.Body)
			q.Body.Close()
		}
		return io.NopCloser(io.LimitReader(gitReadZeros{}, 40<<20)), nil
	})
	endpoint, finish := startGitBrokerTest(t, source, ownGitBrokerPeer)
	for index := range 2 {
		conn := gitBrokerConnect(t, endpoint)
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(conn, "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nContent-Type: application/x-git-upload-pack-request\r\nContent-Length: 4\r\n\r\n0000")
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		conn.Close()
		if index == 0 {
			if n != 40<<20 || err != nil {
				t.Fatal("first response failed", n, err)
			}
		} else if n != 24<<20 || err == nil {
			t.Fatal("aggregate overflow looked like successful HTTP EOF", n, err)
		}
	}
	result := finish()
	if result.audit.ResponseBytes != 64<<20 || result.audit.Rejected != 1 {
		t.Fatal("aggregate accounting incorrect", result)
	}
}

func TestGitReadBrokerGzipExpansionAndTruncationNeverDispatch(t *testing.T) {
	for _, kind := range []string{"expansion", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			var compressed bytes.Buffer
			zip := gzip.NewWriter(&compressed)
			if kind == "expansion" {
				io.Copy(zip, io.LimitReader(gitReadZeros{}, (4<<20)+1))
			} else {
				zip.Write([]byte("0000"))
			}
			zip.Close()
			data := compressed.Bytes()
			if kind == "truncated" {
				data = data[:len(data)-3]
			}
			var calls atomic.Int32
			endpoint, finish := startGitBrokerTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				return nil, ErrGitReadBroker
			}), ownGitBrokerPeer)
			conn := gitBrokerConnect(t, endpoint)
			fmt.Fprintf(conn, "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack HTTP/1.1\r\nHost: sfl.invalid\r\nContent-Type: application/x-git-upload-pack-request\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n", len(data))
			conn.Write(data)
			raw, _ := io.ReadAll(conn)
			conn.Close()
			result := finish()
			if calls.Load() != 0 || len(raw) != 0 || result.audit.Rejected != 1 {
				t.Fatal("gzip fault reached source or returned data", calls.Load(), result)
			}
		})
	}
}
func TestGitReadBrokerAdmissionCapacityAndInterruptedSocketsDrain(t *testing.T) {
	admitted := make(chan struct{}, 8)
	endpoint, finish := startGitBrokerTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		t.Error("partial peer reached source")
		return nil, ErrGitReadBroker
	}), func(ctx context.Context, p GitReadPeer) error {
		if err := ownGitBrokerPeer(ctx, p); err != nil {
			return err
		}
		admitted <- struct{}{}
		return nil
	})
	conns := []*net.UnixConn{}
	for range 8 {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		defer conn.Close()
		conn.Write([]byte{5})
		select {
		case <-admitted:
		case <-time.After(time.Second):
			t.Fatal("pending admission not counted")
		}
	}
	extra, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	extra.SetDeadline(time.Now().Add(time.Second))
	extra.Write([]byte{5, 1, 0})
	raw, _ := io.ReadAll(extra)
	extra.Close()
	if len(raw) != 0 {
		t.Fatal("ninth active connection admitted")
	}
	result := finish()
	if result.audit.Connections != 9 || result.audit.Requests != 0 || result.audit.Rejected != 9 {
		t.Fatal("capacity or shutdown accounting differs", result)
	}
}
func TestGitReadBrokerUsedDirectoryAndForeignSocketArePreserved(t *testing.T) {
	source := gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		t.Error("constructor/ready used source")
		return nil, ErrGitReadBroker
	})
	endpoint, finish := startGitBrokerTest(t, source, ownGitBrokerPeer)
	finish()
	directory := filepath.Dir(endpoint.Socket)
	before, err := os.ReadFile(filepath.Join(directory, "git-read.used"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ServeGitReadSocket(context.Background(), GitReadBrokerOptions{directory, source, ownGitBrokerPeer}, func(GitReadEndpoint) error { t.Error("used directory reopened"); return nil }); err == nil {
		t.Fatal("used attempt reused")
	}
	after, err := os.ReadFile(filepath.Join(directory, "git-read.used"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("used marker modified")
	}
	fresh, err := os.MkdirTemp("", "sfl-git-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fresh)
	audit, err := ServeGitReadSocket(context.Background(), GitReadBrokerOptions{fresh, source, ownGitBrokerPeer}, func(e GitReadEndpoint) error {
		if err := os.Remove(e.Socket); err != nil {
			return err
		}
		if err := os.WriteFile(e.Socket, []byte("synthetic foreign replacement"), 0600); err != nil {
			return err
		}
		return errors.New("private readiness failure")
	})
	if !audit.Stopped || !audit.Failed || !errors.Is(err, ErrGitReadBroker) || strings.Contains(err.Error(), "private readiness failure") {
		t.Fatal("failed readiness cleanup state incorrect", audit, err)
	}
	foreign, err := os.ReadFile(filepath.Join(fresh, "git.sock"))
	if err != nil || string(foreign) != "synthetic foreign replacement" {
		t.Fatal("foreign path removed or overwritten")
	}
}
