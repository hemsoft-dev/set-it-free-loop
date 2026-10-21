//go:build linux

package reviewer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var ErrGitReadBroker = errors.New("private Git broker refused")

const gitReadLocalURL = "http://sfl.invalid/hemsoft-dev/set-it-free-loop.git"

type GitReadExchanger interface {
	Exchange(context.Context, GitHubGitReadRequest) (io.ReadCloser, error)
}
type GitReadPeer struct {
	PID      int
	UID, GID uint32
}
type GitReadEndpoint struct{ Socket, URL string }
type GitReadBrokerOptions struct {
	Directory string
	Source    GitReadExchanger
	// Admit must independently bind this kernel peer to the measured helper and
	// the exact protected acquisition unit. No default peer policy is supplied.
	// It is a trusted bounded read-only check, called before SOCKS and dispatch.
	Admit func(context.Context, GitReadPeer) error
}
type GitReadBrokerAudit struct {
	Version                         int
	Connections, Requests, Rejected int
	RequestBytes, ResponseBytes     int64
	Stopped, Failed                 bool
}

type gitBrokerState struct {
	mu       sync.Mutex
	active   map[*net.UnixConn]bool
	audit    GitReadBrokerAudit
	outputMu sync.Mutex
	output   int64
}

// ServeGitReadSocket terminates SOCKS5h locally; it never connects to the
// requested target. One HTTP request is served per admitted Unix connection.
// Connection handlers, owned callbacks and sockets stop before return. Cancellation stops
// intake and active work; it returns a stopped audit with the context error.
// The existing empty private directory is marked used even on failure and
// cannot be reused. Its parent and the service identity must be protected.
func ServeGitReadSocket(ctx context.Context, o GitReadBrokerOptions, ready func(GitReadEndpoint) error) (audit GitReadBrokerAudit, serveErr error) {
	audit.Version = 1
	if ctx == nil || ctx.Err() != nil || o.Source == nil || o.Admit == nil || ready == nil || !filepath.IsAbs(o.Directory) || filepath.Clean(o.Directory) != o.Directory || privateDirectory(o.Directory) != nil {
		return audit, ErrGitReadBroker
	}
	resolved, err := filepath.EvalSymlinks(o.Directory)
	if err != nil || resolved != o.Directory {
		return audit, ErrGitReadBroker
	}
	entries, err := os.ReadDir(o.Directory)
	if err != nil || len(entries) != 0 {
		return audit, ErrGitReadBroker
	}
	endpoint := GitReadEndpoint{filepath.Join(o.Directory, "git.sock"), gitReadLocalURL}
	if len(endpoint.Socket) > 107 {
		return audit, ErrGitReadBroker
	}
	root, err := os.OpenRoot(o.Directory)
	if err != nil {
		return audit, ErrGitReadBroker
	}
	defer root.Close()
	marker, err := root.OpenFile("git-read.used", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return audit, ErrGitReadBroker
	}
	_, writeErr := marker.WriteString("sfl-git-read-broker-1\n")
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return audit, ErrGitReadBroker
	}
	directoryFile, dirErr := root.Open(".")
	if dirErr != nil {
		return audit, ErrGitReadBroker
	}
	dirSyncErr := directoryFile.Sync()
	dirCloseErr := directoryFile.Close()
	if dirSyncErr != nil || dirCloseErr != nil {
		return audit, ErrGitReadBroker
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		return audit, ErrGitReadBroker
	}
	listener.SetUnlinkOnClose(false)
	socketInfo, err := root.Lstat("git.sock")
	if err != nil || os.Chmod(endpoint.Socket, 0600) != nil {
		listener.Close()
		return audit, ErrGitReadBroker
	}
	defer func() {
		_ = listener.Close()
		current, err := root.Lstat("git.sock")
		if err != nil || !os.SameFile(current, socketInfo) || root.Remove("git.sock") != nil {
			audit.Failed = true
			serveErr = ErrGitReadBroker
		}
	}()
	phase, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	state := &gitBrokerState{active: map[*net.UnixConn]bool{}, audit: GitReadBrokerAudit{Version: 1}}
	closeAll := func() {
		_ = listener.Close()
		state.mu.Lock()
		for conn := range state.active {
			_ = conn.Close()
		}
		state.mu.Unlock()
	}
	stop := gitBrokerAfterFunc(phase, closeAll)
	defer stop()
	if phase.Err() != nil {
		closeAll()
		audit.Stopped = true
		return audit, errors.Join(ErrGitReadBroker, phase.Err())
	}
	if ready(endpoint) != nil {
		closeAll()
		audit.Stopped = true
		return audit, ErrGitReadBroker
	}
	var workers sync.WaitGroup
	for count := 0; count < 64; count++ {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if phase.Err() == nil {
				serveErr = ErrGitReadBroker
			}
			break
		}
		state.mu.Lock()
		state.audit.Connections++
		if phase.Err() != nil || len(state.active) >= 8 {
			state.audit.Rejected++
			state.mu.Unlock()
			conn.Close()
			if phase.Err() != nil {
				break
			}
			continue
		}
		state.active[conn] = true
		state.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { conn.Close(); state.mu.Lock(); delete(state.active, conn); state.mu.Unlock() }()
			if serveGitBrokerConnection(phase, o, conn, state) != nil {
				state.mu.Lock()
				state.audit.Rejected++
				state.mu.Unlock()
			}
		}()
	}
	_ = listener.Close()
	if phase.Err() != nil {
		closeAll()
	}
	workers.Wait()
	closeAll()
	state.mu.Lock()
	audit = state.audit
	state.mu.Unlock()
	state.outputMu.Lock()
	audit.ResponseBytes = state.output
	state.outputMu.Unlock()
	audit.Stopped = true
	audit.Failed = audit.Rejected > 0 || serveErr != nil
	if phase.Err() != nil {
		return audit, errors.Join(ErrGitReadBroker, phase.Err())
	}
	if audit.Failed {
		return audit, ErrGitReadBroker
	}
	return audit, nil
}

// A stopped callback cannot keep operating on a returned broker or connection.
func gitBrokerAfterFunc(ctx context.Context, work func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); work() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

func gitBrokerPeer(conn *net.UnixConn) (GitReadPeer, int, error) {
	pid, uid, fd, err := gatePeer(conn)
	if err != nil {
		return GitReadPeer{}, -1, ErrGitReadBroker
	}
	raw, err := conn.SyscallConn()
	var cred *unix.Ucred
	var lookup error
	if err == nil {
		err = raw.Control(func(socket uintptr) {
			cred, lookup = unix.GetsockoptUcred(int(socket), unix.SOL_SOCKET, unix.SO_PEERCRED)
		})
	}
	if err != nil || lookup != nil || cred == nil || int(cred.Pid) != pid || cred.Uid != uid {
		unix.Close(fd)
		return GitReadPeer{}, -1, ErrGitReadBroker
	}
	return GitReadPeer{pid, uid, cred.Gid}, fd, nil
}
func serveGitBrokerConnection(ctx context.Context, o GitReadBrokerOptions, conn *net.UnixConn, state *gitBrokerState) error {
	phase, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stop := gitBrokerAfterFunc(phase, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	peer, fd, err := gitBrokerPeer(conn)
	if err != nil {
		return ErrGitReadBroker
	}
	defer unix.Close(fd)
	check := func() error {
		checkCtx, cancel := context.WithTimeout(phase, 5*time.Second)
		defer cancel()
		if unix.PidfdSendSignal(fd, 0, nil, 0) != nil || o.Admit(checkCtx, peer) != nil || checkCtx.Err() != nil || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
			return ErrGitReadBroker
		}
		return nil
	}
	if check() != nil {
		return ErrGitReadBroker
	}
	if gitBrokerSOCKS(conn) != nil {
		return ErrGitReadBroker
	}
	q, err := gitBrokerRequest(conn)
	if err != nil {
		return ErrGitReadBroker
	}
	defer func() { _ = conn.Close(); _ = q.Body.Close() }()
	operation := GitHubGitAdvertisement
	switch q.Method + " " + q.RequestURI {
	case "GET /hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack":
		if q.ContentLength != 0 || len(q.TransferEncoding) != 0 || q.Header.Get("Expect") != "" || q.Header.Get("Content-Encoding") != "" {
			return ErrGitReadBroker
		}
	case "POST /hemsoft-dev/set-it-free-loop.git/git-upload-pack":
		operation = GitHubGitUploadPack
		if q.Header.Get("Content-Type") != "application/x-git-upload-pack-request" || q.ContentLength > githubGitRequestLimit {
			return ErrGitReadBroker
		}
	default:
		return ErrGitReadBroker
	}
	protocol := 0
	switch q.Header.Get("Git-Protocol") {
	case "":
	case "version=2":
		protocol = 2
	default:
		return ErrGitReadBroker
	}
	switch q.Header.Get("Expect") {
	case "":
	case "100-continue":
		if _, err := io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return ErrGitReadBroker
		}
	default:
		return ErrGitReadBroker
	}
	var data []byte
	var encoded int64
	if operation == GitHubGitUploadPack {
		limited := &io.LimitedReader{R: q.Body, N: githubGitRequestLimit + 1}
		var input io.Reader = limited
		switch q.Header.Get("Content-Encoding") {
		case "", "identity":
		case "gzip":
			zip, err := gzip.NewReader(limited)
			if err != nil {
				return ErrGitReadBroker
			}
			defer zip.Close()
			input = zip
		default:
			return ErrGitReadBroker
		}
		data, err = io.ReadAll(io.LimitReader(input, githubGitRequestLimit+1))
		encoded = githubGitRequestLimit + 1 - limited.N
		if err != nil || len(data) == 0 || int64(len(data)) > githubGitRequestLimit || encoded > githubGitRequestLimit || len(q.Trailer) != 0 {
			return ErrGitReadBroker
		}
	}
	if check() != nil {
		return ErrGitReadBroker
	}
	state.mu.Lock()
	state.audit.Requests++
	state.audit.RequestBytes += encoded
	state.mu.Unlock()
	request := GitHubGitReadRequest{Operation: operation, Protocol: protocol}
	if operation == GitHubGitUploadPack {
		request.Body = io.NopCloser(bytes.NewReader(data))
	}
	output, err := o.Source.Exchange(phase, request)
	if err != nil || output == nil {
		if output != nil {
			output.Close()
		}
		return ErrGitReadBroker
	}
	bounded := &gitBrokerOutput{source: output, state: state}
	defer bounded.Close()
	stopOutput := gitBrokerAfterFunc(phase, func() { _ = bounded.Close() })
	defer stopOutput()
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
	media := "application/x-git-upload-pack-advertisement"
	if operation == GitHubGitUploadPack {
		media = "application/x-git-upload-pack-result"
	}
	response := &http.Response{StatusCode: 200, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {media}}, ContentLength: -1, TransferEncoding: []string{"chunked"}, Close: true, Body: bounded}
	if response.Write(conn) != nil {
		return ErrGitReadBroker
	}
	return nil
}
func gitBrokerSOCKS(conn io.ReadWriter) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] < 1 || greeting[1] > 8 {
		return ErrGitReadBroker
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return ErrGitReadBroker
	}
	allowed := false
	for _, method := range methods {
		allowed = allowed || method == 0
	}
	if !allowed {
		return ErrGitReadBroker
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return ErrGitReadBroker
	}
	var request [5]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil || request[0] != 5 || request[1] != 1 || request[2] != 0 || request[3] != 3 || request[4] != byte(len("sfl.invalid")) {
		return ErrGitReadBroker
	}
	target := make([]byte, len("sfl.invalid")+2)
	if _, err := io.ReadFull(conn, target); err != nil || string(target[:len(target)-2]) != "sfl.invalid" || target[len(target)-2] != 0 || target[len(target)-1] != 80 {
		return ErrGitReadBroker
	}
	_, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

type gitBrokerHeaderReader struct {
	source    io.Reader
	remaining int64
	headers   bool
}

func (r *gitBrokerHeaderReader) Read(p []byte) (int, error) {
	if !r.headers {
		return r.source.Read(p)
	}
	if r.remaining <= 0 {
		return 0, ErrGitReadBroker
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	return n, err
}
func gitBrokerRequest(conn io.Reader) (*http.Request, error) {
	limited := &gitBrokerHeaderReader{conn, 32 << 10, true}
	buffered := bufio.NewReaderSize(limited, 4096)
	var header bytes.Buffer
	names := map[string]string{}
	for lineNumber := 0; lineNumber < 66; lineNumber++ {
		line, err := buffered.ReadString('\n')
		if err != nil || !strings.HasSuffix(line, "\r\n") || header.Len()+len(line) > 32<<10 {
			return nil, ErrGitReadBroker
		}
		header.WriteString(line)
		if line == "\r\n" {
			if strings.TrimSpace(names["host"]) != "sfl.invalid" {
				return nil, ErrGitReadBroker
			}
			if _, te := names["transfer-encoding"]; te {
				if _, cl := names["content-length"]; cl || strings.TrimSpace(names["transfer-encoding"]) != "chunked" {
					return nil, ErrGitReadBroker
				}
			}
			limited.headers = false
			request, err := http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(header.Bytes()), buffered)))
			if err != nil {
				return nil, ErrGitReadBroker
			}
			if request.Host != "sfl.invalid" || request.Proto != "HTTP/1.1" || request.URL.IsAbs() {
				request.Body.Close()
				return nil, ErrGitReadBroker
			}
			return request, nil
		}
		if lineNumber == 0 {
			parts := strings.Split(strings.TrimSuffix(line, "\r\n"), " ")
			if len(parts) != 3 || parts[2] != "HTTP/1.1" || (parts[0] != "GET" && parts[0] != "POST") || !strings.HasPrefix(parts[1], "/") || strings.HasPrefix(parts[1], "//") {
				return nil, ErrGitReadBroker
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, ErrGitReadBroker
		}
		name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		name = strings.ToLower(name)
		if !ok || name == "" || strings.TrimSpace(name) != name {
			return nil, ErrGitReadBroker
		}
		if _, duplicate := names[name]; duplicate {
			return nil, ErrGitReadBroker
		}
		switch name {
		case "host", "accept", "accept-encoding", "content-type", "content-length", "content-encoding", "transfer-encoding", "git-protocol", "user-agent", "pragma", "expect":
		case "connection":
			if strings.TrimSpace(value) != "close" && strings.TrimSpace(value) != "keep-alive" {
				return nil, ErrGitReadBroker
			}
		default:
			return nil, ErrGitReadBroker
		}
		names[name] = value
	}
	return nil, ErrGitReadBroker
}

type gitBrokerOutput struct {
	source   io.ReadCloser
	state    *gitBrokerState
	closed   sync.Once
	closeErr error
}

func (r *gitBrokerOutput) Close() error {
	r.closed.Do(func() {
		if r.source.Close() != nil {
			r.closeErr = ErrGitReadBroker
		}
	})
	return r.closeErr
}
func (r *gitBrokerOutput) Read(p []byte) (int, error) {
	r.state.outputMu.Lock()
	defer r.state.outputMu.Unlock()
	remaining := (64 << 20) - r.state.output
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n != 0 {
			return 0, ErrGitReadBroker
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.source.Read(p)
	r.state.output += int64(n)
	return n, err
}
