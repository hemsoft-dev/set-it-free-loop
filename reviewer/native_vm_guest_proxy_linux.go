//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type nativeGuestStart struct {
	Version int
	Kind    string
}
type nativeGuestProxyReady struct {
	Ready nativeVMReady
	Port  int
}

func readNativeGuestProxyReady(input io.Reader, q nativeVMRequest, progress func(string) error) (nativeGuestProxyReady, error) {
	for range 8 {
		raw, err := readVMRecord[json.RawMessage](input, 4096)
		if err != nil {
			return nativeGuestProxyReady{}, ErrVMBoot
		}
		var stage nativeVMProgress
		if json.Unmarshal(raw, &stage) == nil && stage.Version == 1 && stage.Kind == "guest-stage" && strings.HasPrefix(stage.Stage, "client-") && nativeVMGuestStage(stage.Stage) {
			canonical, _ := json.Marshal(stage)
			if !bytes.Equal(raw, canonical) {
				return nativeGuestProxyReady{}, ErrVMBoot
			}
			if progress != nil && progress(stage.Stage) != nil {
				return nativeGuestProxyReady{}, ErrVMBoot
			}
			continue
		}
		var ready nativeGuestProxyReady
		if json.Unmarshal(raw, &ready) != nil || !ready.Ready.valid(q) || ready.Port <= 0 || ready.Port > 65535 {
			return nativeGuestProxyReady{}, ErrVMBoot
		}
		canonical, _ := json.Marshal(ready)
		if !bytes.Equal(raw, canonical) {
			return nativeGuestProxyReady{}, ErrVMBoot
		}
		return ready, nil
	}
	return nativeGuestProxyReady{}, ErrVMBoot
}

type nativeGuestBuffer struct {
	mu    sync.Mutex
	data  bytes.Buffer
	limit int
}

func (b *nativeGuestBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data.Len()+len(data) > b.limit {
		return 0, ErrVMBoot
	}
	return b.data.Write(data)
}
func (b *nativeGuestBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte{}, b.data.Bytes()...)
}

// The character device is a private, already connected virtio byte stream.
// Deadline support must be established by the guest kernel; it never dials.
type nativeGuestConn struct{ *os.File }
type nativeGuestAddr struct{}

func nativeGuestPrivateFile(fd uintptr, name string) *os.File {
	file := os.NewFile(fd, name)
	if file == nil {
		return nil
	}
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err == nil {
		_, err = unix.FcntlInt(fd, unix.F_SETFD, flags|unix.FD_CLOEXEC)
	}
	if err != nil {
		file.Close()
		return nil
	}
	return file
}

func (nativeGuestAddr) Network() string      { return "private-virtio" }
func (nativeGuestAddr) String() string       { return "private-provider" }
func (nativeGuestConn) LocalAddr() net.Addr  { return nativeGuestAddr{} }
func (nativeGuestConn) RemoteAddr() net.Addr { return nativeGuestAddr{} }

func nativeGuestLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return ErrVMBoot
	}
	defer unix.Close(fd)
	request, err := unix.NewIfreq("lo")
	if err != nil {
		return ErrVMBoot
	}
	if unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, request) != nil {
		return ErrVMBoot
	}
	request.SetUint16(request.Uint16() | unix.IFF_UP)
	if unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, request) != nil {
		return ErrVMBoot
	}
	return nil
}

func nativeGuestInvocation(q nativeVMRequest, base string) (Invocation, error) {
	reader, err := NewReader(nativeGuestNoObjects{}, q.Session.Snapshot, q.Paths, q.Session.Limits)
	if err != nil {
		return Invocation{}, ErrVMBoot
	}
	inv, err := (Adapter{Provider: q.Session.Identity.Provider, AuthDirectory: "/profile/" + string(q.Session.Identity.Provider), Reader: reader, SourceBridge: &SourceBridge{Program: "/sourceproxy", Directory: nativeVMSourceRoot, ReaderUID: nativeVMReaderUID}}).Invocation(q.Session, nil)
	if err != nil {
		return Invocation{}, ErrVMBoot
	}
	inv.Environment = append([]string{"HOME=/profile", "PATH=/usr/bin:/bin", "GOMAXPROCS=1"}, inv.Environment...)
	if nativeGuestRefreshEnabled(q.Policy) {
		inv.Environment = append(inv.Environment, "HTTPS_PROXY="+base, "NO_PROXY=127.0.0.1,localhost,::1", "SSL_CERT_FILE="+nativeGuestCAPath, "NODE_EXTRA_CA_CERTS="+nativeGuestCAPath, "CODEX_CA_CERTIFICATE="+nativeGuestCAPath)
	}
	if q.Session.Identity.Provider == Codex {
		extra := []string{"-c", `cli_auth_credentials_store="file"`, "-c", `openai_base_url="` + base + `/v1"`, "-c", `chatgpt_base_url="` + base + `/backend-api/"`, "-c", `features.enable_request_compression=false`}
		inv.Args = append(append(inv.Args[:len(inv.Args)-1:len(inv.Args)-1], extra...), "-")
	} else {
		inv.Environment = append(inv.Environment, "ANTHROPIC_BASE_URL="+base, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}
	return inv, nil
}

type nativeGuestNoObjects struct{}

func (nativeGuestNoObjects) Read(context.Context, string, string, int) (Object, error) {
	return Object{}, ErrVMObjects
}

// This mode runs as the trusted root sidecar inside the client PID/net/mount
// namespace. The actual CLI has the unprivileged provider UID. Only this root
// sidecar receives the private provider device and readiness/start descriptors.
func nativeGuestClientProxyMain() (result int) {
	if os.Getpid() != 1 || os.Getuid() != 0 {
		return 2
	}
	config := nativeGuestPrivateFile(3, "private-client-control")
	provider := nativeGuestPrivateFile(4, "private-provider")
	ready := nativeGuestPrivateFile(5, "private-client-ready")
	if config == nil || provider == nil || ready == nil {
		return 2
	}
	defer config.Close()
	defer provider.Close()
	defer ready.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { config.Close(); provider.Close(); os.Stdin.Close() })
	defer stop()
	progress := func(stage string) error { return writeVMRecord(ready, nativeVMProgress{1, "guest-stage", stage}, 4096) }
	if progress("client-config") != nil {
		return 2
	}
	q, err := readVMRecord[nativeVMRequest](config, 4<<20)
	if err != nil || !q.valid() {
		return 2
	}
	if progress("client-loopback") != nil || nativeGuestLoopback() != nil {
		return 2
	}
	if progress("client-proc") != nil || unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "") != nil {
		return 2
	}
	if unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil {
		return 2
	}
	// All external traffic is impossible in this fresh loopback-only namespace.
	addresses, err := net.InterfaceAddrs()
	if err != nil || len(addresses) == 0 {
		return 2
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || !ip.IsLoopback() {
			return 2
		}
	}
	if progress("client-listener") != nil {
		return 2
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 2
	}
	defer listener.Close()
	transport := nativeProviderTransport(nativeGuestConn{provider})
	defer transport.CloseIdleConnections()
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: q.Session.Limits.Timeout, WriteTimeout: q.Session.Limits.Timeout, IdleTimeout: q.Session.Limits.Timeout, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	server.Handler = nativeGuestProviderHandler(q, transport, cancel)
	var refreshProxy *nativeGuestTLSProxy
	if nativeGuestRefreshEnabled(q.Policy) {
		proxy, certificate, err := newNativeGuestTLSProxy(ctx, q.Policy, server.Handler, cancel)
		if err != nil {
			return 2
		}
		defer func() {
			if proxy.Close() != nil {
				result = 2
			}
		}()
		file, err := os.OpenFile(nativeGuestCAPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0444)
		if err != nil {
			return 2
		}
		n, writeErr := file.Write(certificate)
		modeErr, syncErr, closeErr := file.Chmod(0444), file.Sync(), file.Close()
		if writeErr != nil || n != len(certificate) || modeErr != nil || syncErr != nil || closeErr != nil {
			return 2
		}
		refreshProxy = proxy
		server.Handler = proxy
	}
	go server.Serve(listener)
	defer server.Close()
	inv, err := nativeGuestInvocation(q, "http://"+listener.Addr().String())
	if err != nil {
		return 2
	}
	uid := nativeVMCodexUID
	binary := "/client/codex"
	if q.Session.Identity.Provider == Claude {
		uid = nativeVMClaudeUID
		binary = "/client/claude"
	}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return 2
	}
	// The profile is still empty: no auth/status probe or inference is run here.
	if progress("client-version") != nil {
		return 2
	}
	probeCtx, stopProbe := context.WithTimeout(ctx, 5*time.Second)
	version := exec.CommandContext(probeCtx, binary, "--version")
	version.Dir = "/work"
	version.Env = inv.Environment
	version.Stderr = io.Discard
	version.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: nativeVMIPCGID, Groups: []uint32{}}, Pdeathsig: syscall.SIGKILL}
	version.WaitDelay = 2 * time.Second
	versionOutput := &nativeGuestBuffer{limit: 4096}
	version.Stdout = versionOutput
	err = version.Run()
	stopProbe()
	output := versionOutput.Bytes()
	if err != nil || len(output) > 4096 || !nativeGuestVersion(q.Session.Identity, string(output)) {
		return 2
	}
	if progress("client-probe-clean") != nil || !nativeGuestOnlySupervisor() {
		return 2
	}
	if writeVMRecord(ready, nativeGuestProxyReady{nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}, listener.Addr().(*net.TCPAddr).Port}, 4096) != nil {
		return 2
	}
	ready.Close()
	start, err := readVMRecord[nativeGuestStart](config, 256)
	if err != nil || start.Version != 1 || start.Kind != "start" {
		return 2
	}
	config.Close()
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return 2
	}
	client := exec.CommandContext(ctx, binary, inv.Args...)
	client.Dir = "/work"
	client.Env = inv.Environment
	client.Stdin = os.Stdin
	client.Stdout = os.Stdout
	diagnostics := &nativeGuestClientDiagnostics{}
	client.Stderr = diagnostics
	client.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: nativeVMIPCGID, Groups: []uint32{}}, Pdeathsig: syscall.SIGKILL}
	client.WaitDelay = 2 * time.Second
	if runErr := client.Run(); runErr != nil {
		if refreshProxy != nil && refreshProxy.failureCode() != 0 {
			return refreshProxy.failureCode()
		}
		return diagnostics.exitCode(runErr)
	}
	return 0
}

func nativeGuestProviderHandler(q nativeVMRequest, transport http.RoundTripper, stop func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, in *http.Request) {
		failed := true
		defer func() {
			if failed {
				// A denied response cannot become a review. Stop this owned CLI
				// now so guest PID1 can reap it and return refreshed auth, rather
				// than waiting for an opaque client's retry loop to time out.
				stop()
			}
		}()
		var authority string
		for _, route := range q.Policy.Routes {
			if route.Method == in.Method && route.Path == in.URL.Path && route.RawQuery == in.URL.RawQuery {
				if authority != "" && authority != route.Authority {
					http.Error(w, "ambiguous provider route", 403)
					return
				}
				authority = route.Authority
			}
		}
		if authority == "" || in.URL.IsAbs() || in.URL.User != nil || in.URL.RawPath != "" || len(in.Trailer) != 0 {
			http.Error(w, "provider route denied", 403)
			return
		}
		request := in.Clone(in.Context())
		request.URL = &url.URL{Scheme: "http", Host: "sfl-private.invalid", Path: in.URL.Path, RawQuery: in.URL.RawQuery}
		request.Host = authority
		request.RequestURI = ""
		request.GetBody = nil
		request.Close = false
		response, err := transport.RoundTrip(request)
		if err != nil {
			http.Error(w, "provider unavailable", 502)
			return
		}
		defer response.Body.Close()
		if content := response.Header.Get("Content-Type"); content != "" {
			w.Header().Set("Content-Type", content)
		}
		w.WriteHeader(response.StatusCode)
		n, err := io.Copy(w, io.LimitReader(response.Body, int64(q.Policy.Limits.ResponseBytes)+1))
		// Only a latched host guard failure is fatal. Ordinary upstream auth or
		// quota responses still reach the CLI's refresh/error handling, and the
		// local WebSocket refusal still permits the pinned HTTP fallback.
		failed = response.Header.Get(nativeProviderFatalHeader) == "1" || err != nil || n > int64(q.Policy.Limits.ResponseBytes)
	})
}

func nativeGuestVersion(identity Identity, output string) bool {
	line := strings.TrimSpace(output)
	if identity.Provider == Codex {
		return line == "codex-cli "+identity.ClientVersion
	}
	return line == identity.ClientVersion+" (Claude Code)" || line == identity.ClientVersion
}

var _ net.Conn = nativeGuestConn{}

func nativeGuestOnlySupervisor() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil || os.Getpid() != 1 {
		return false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid != 1 {
			return false
		}
	}
	return true
}
