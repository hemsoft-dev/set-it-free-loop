//go:build linux

// isolationfixture is a credential-free guest fixture, not an activation or
// production launcher. Privileged setup runs only as PID1 in its marked guest.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"golang.org/x/sys/unix"
)

const readerUID, workerUID, deniedUID, ipcGID = 1201, 1202, 1203, 1301

type fixtureManifest struct {
	Version int    `json:"version"`
	Nonce   string `json:"nonce"`
	Commit  string `json:"commit"`
}

type roleReceipt struct {
	Role              string `json:"role"`
	UID               int    `json:"uid"`
	MemoryMax         string `json:"memory_max"`
	PidsMax           string `json:"pids_max"`
	CPUMax            string `json:"cpu_max"`
	Stopped           bool   `json:"stopped"`
	DescendantsKilled int    `json:"descendants_killed"`
}

type guestResult struct {
	Version                     int             `json:"version"`
	Nonce                       string          `json:"nonce"`
	Passed                      bool            `json:"passed"`
	FakeClients                 bool            `json:"fake_clients"`
	ProductionBoundaryQualified bool            `json:"production_boundary_qualified"`
	Error                       string          `json:"error,omitempty"`
	Access                      *r.SourceAccess `json:"source_access,omitempty"`
	Roles                       []roleReceipt   `json:"roles,omitempty"`
	ReapedOrphans               int             `json:"reaped_orphans"`
}

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "--guest-init":
		guestInit()
	case "--reader":
		if !unprivilegedRole(readerUID) {
			fmt.Fprintln(os.Stderr, "fixture reader identity/capability check failed")
			os.Exit(2)
		}
		if _, err := os.ReadFile("/worker-private-canary"); !os.IsPermission(err) {
			os.Exit(2)
		}
		if os.WriteFile("/source/must-not-write", []byte("inert"), 0600) == nil {
			os.Exit(2)
		}
		code := r.SourceReaderMain(context.Background(), os.Environ(), os.Stdin, os.Stdout)
		if code != 0 {
			fmt.Fprintln(os.Stderr, "fixture reader service failed", code)
		}
		os.Exit(code)
	case "--worker":
		if !unprivilegedRole(workerUID) {
			fmt.Fprintln(os.Stderr, "fixture worker identity/capability check failed")
			os.Exit(2)
		}
		if err := worker(); err != nil {
			fmt.Fprintln(os.Stderr, "fixture worker failed:", err)
			os.Exit(2)
		}
	case "--denied":
		if !unprivilegedRole(deniedUID) || deniedPeer() != nil {
			os.Exit(2)
		}
	case "--escaped-child":
		if !unprivilegedRole(workerUID) {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(2)
	}
}

func unprivilegedRole(uid int) bool {
	groups, err := os.Getgroups()
	if err != nil || len(groups) != 0 || os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != ipcGID || os.Getegid() != ipcGID {
		return false
	}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return false
	}
	var capabilities [2]unix.CapUserData
	if unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &capabilities[0]) != nil {
		return false
	}
	return capabilities[0].Effective == 0 && capabilities[1].Effective == 0 && capabilities[0].Permitted == 0 && capabilities[1].Permitted == 0
}

func guestInit() {
	// Refuse host execution before any privileged mount/account operation.
	if os.Getpid() != 1 || os.Getuid() != 0 {
		os.Exit(2)
	}
	var manifest fixtureManifest
	raw, err := os.ReadFile("/fixture.json")
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Version != 1 || len(manifest.Nonce) != 32 || len(manifest.Commit) != 40 {
		os.Exit(2)
	}
	if unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil {
		os.Exit(2)
	}
	command, _ := os.ReadFile("/proc/cmdline")
	if !strings.Contains(" "+string(command)+" ", " sfl-fixture="+manifest.Nonce+" ") {
		os.Exit(2)
	}
	result := guestResult{Version: 1, Nonce: manifest.Nonce, FakeClients: true}
	result.Access, result.Roles, err = runGuest(manifest)
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Passed = true
	}
	for {
		pid, _ := unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if pid <= 0 {
			break
		}
		result.ReapedOrphans++
	}
	if result.ReapedOrphans < 1 && result.Passed {
		result.Passed = false
		result.Error = "escaped fixture child was not reaped"
	}
	encoded, _ := json.Marshal(result)
	fmt.Printf("\nSFL_FIXTURE_RESULT_%s %s\n", manifest.Nonce, encoded)
	_ = os.Stdout.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	for {
		time.Sleep(time.Hour)
	}
}

func runGuest(manifest fixtureManifest) (*r.SourceAccess, []roleReceipt, error) {
	fail := func() (*r.SourceAccess, []roleReceipt, error) {
		return nil, nil, errors.New("guest fixture setup or execution failed")
	}
	if unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=96m") != nil {
		return fail()
	}
	if unix.Mount("sysfs", "/sys", "sysfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil {
		return fail()
	}
	if os.MkdirAll("/sys/fs/cgroup", 0755) != nil || unix.Mount("none", "/sys/fs/cgroup", "cgroup2", 0, "") != nil {
		return fail()
	}
	if os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory +pids +cpu"), 0600) != nil {
		return fail()
	}
	for _, dir := range []string{"/run/sfl", "/run/workers"} {
		if os.Mkdir(dir, 0700) != nil {
			return fail()
		}
	}
	if os.Chown("/run/sfl", readerUID, ipcGID) != nil {
		return fail()
	}
	if os.WriteFile("/worker-private-canary", []byte("synthetic-only-not-a-credential"), 0600) != nil || os.Chown("/worker-private-canary", workerUID, ipcGID) != nil {
		return fail()
	}
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: manifest.Commit, Base: manifest.Commit, MergeBase: manifest.Commit}
	limits := r.DefaultLimits()
	limits.Timeout = 20 * time.Second
	session, err := r.NewSession("actual-uid-fixture", snapshot, r.Digest("fake-only-config"), r.Codex, 1, "inert fake source evidence", nil, nil, limits)
	if err != nil {
		return fail()
	}
	reader, err := r.NewReader(r.GitStore{Git: "/usr/bin/git", GitDir: "/source"}, snapshot, []string{"source.go", "worker.go"}, limits)
	if err != nil {
		return fail()
	}
	launch := &guestLauncher{}
	adapter := r.Adapter{Provider: r.Codex, Driver: r.NativeDriver{Launcher: launch}, Reader: reader, AuthDirectory: "/profile", SourceBridge: &r.SourceBridge{Program: "/sourceproxy", Directory: "/run/sfl", ReaderUID: readerUID}}
	managed := r.ManagedSourceReviewer{Adapter: adapter, Readers: launch, WorkerUID: workerUID, WorkerGID: ipcGID}
	_, access, err := managed.ReviewSource(context.Background(), session)
	if err != nil {
		return nil, nil, fmt.Errorf("managed phase failed: %w", err)
	}
	if access == nil || access.ToolCalls != 2 || access.ReaderCalls != 3 || len(access.Reads) != 3 || len(access.Coverage) != 1 {
		fmt.Fprintln(os.Stderr, "fixture source receipt differs:", access)
		return fail()
	}
	var receipts []roleReceipt
	for _, process := range launch.roles {
		receipts = append(receipts, process.receipt)
		if !process.receipt.Stopped {
			return fail()
		}
	}
	if len(receipts) != 2 || receipts[1].DescendantsKilled < 1 {
		return fail()
	}
	return access, receipts, nil
}

type guestLauncher struct {
	roles      []*roleProcess
	workerRoot string
}

// Identity is deliberately fake. This fixture never inspects an actual client,
// auth state, model availability or provider account.
func (*guestLauncher) Inspect(context.Context, r.Invocation) (r.Identity, error) {
	return r.PinnedIdentity(r.Codex)
}

func (g *guestLauncher) StartReader(ctx context.Context, config r.SourceReaderConfig) (r.SourceReaderProcess, error) {
	if config.ReaderUID != readerUID || config.WorkerUID != workerUID || config.WorkerGID != ipcGID {
		return nil, errors.New("unexpected fixture role")
	}
	p, err := newRole(ctx, "reader", readerUID, "", []string{"LANG=C.UTF-8"}, "/init", "--reader")
	if p != nil {
		g.roles = append(g.roles, p)
	}
	return p, err
}

func (g *guestLauncher) Start(ctx context.Context, inv r.Invocation) (r.Worker, error) {
	if inv.SourceEndpoint == nil || inv.SourceEndpoint.ReaderUID != readerUID || inv.Source != nil {
		return nil, errors.New("bad fixture source binding")
	}
	root := filepath.Join("/run/workers", inv.SourceEndpoint.SessionHash)
	if os.Mkdir(root, 0755) != nil {
		return nil, errors.New("fixture root exists")
	}
	g.workerRoot = root
	for _, dir := range []string{"run", "run/sfl", "profile", "dev"} {
		if os.Mkdir(filepath.Join(root, dir), 0755) != nil {
			return nil, errors.New("fixture root setup failed")
		}
	}
	for from, to := range map[string]string{"/init": "worker", "/artifacts/sourceproxy": "sourceproxy"} {
		data, err := os.ReadFile(from)
		if err != nil || os.WriteFile(filepath.Join(root, to), data, 0555) != nil {
			return nil, errors.New("fixture runtime copy failed")
		}
	}
	if os.Chmod(filepath.Join(root, "profile"), 0700) != nil || os.Chown(filepath.Join(root, "profile"), workerUID, ipcGID) != nil {
		return nil, errors.New("fixture profile setup failed")
	}
	if unix.Mknod(filepath.Join(root, "dev/null"), unix.S_IFCHR|0666, int(unix.Mkdev(1, 3))) != nil || os.Chmod(filepath.Join(root, "dev/null"), 0666) != nil {
		return nil, errors.New("fixture null device failed")
	}
	target := filepath.Join(root, filepath.Dir(inv.SourceEndpoint.Socket))
	if os.Mkdir(target, 0710) != nil || unix.Mount(filepath.Dir(inv.SourceEndpoint.Socket), target, "", unix.MS_BIND, "") != nil || unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "") != nil {
		return nil, errors.New("fixture IPC mount failed")
	}
	env := []string{"LANG=C.UTF-8", "GOMAXPROCS=1", "SFL_FIXTURE_SOCKET=" + inv.SourceEndpoint.Socket}
	// Same IPC group can reach the socket; the actual server must reject the
	// different UID before receiving MCP/source bytes.
	probe, err := newRole(ctx, "probe", deniedUID, root, env, "/worker", "--denied")
	if err != nil {
		return nil, err
	}
	_ = probe.Input().Close()
	if err = probe.Wait(); err != nil {
		_ = probe.Stop(ctx)
		return nil, errors.New("actual unauthorized peer not rejected")
	}
	if err = probe.Stop(ctx); err != nil {
		return nil, err
	}
	p, err := newRole(ctx, "worker", workerUID, root, env, "/worker", "--worker")
	if p != nil {
		g.roles = append(g.roles, p)
	}
	return p, err
}

type roleProcess struct {
	in       *os.File
	out      *os.File
	cmd      *exec.Cmd
	cgroup   string
	fd       *os.File
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
	stopErr  error
	receipt  roleReceipt
}

func newRole(ctx context.Context, name string, uid int, chroot string, env []string, program string, args ...string) (*roleProcess, error) {
	cgroup := filepath.Join("/sys/fs/cgroup", name)
	if os.Mkdir(cgroup, 0700) != nil {
		return nil, errors.New("fixture cgroup setup failed")
	}
	for key, value := range map[string]string{"memory.max": "100663296", "memory.swap.max": "0", "pids.max": "32", "cpu.max": "100000 100000"} {
		if os.WriteFile(filepath.Join(cgroup, key), []byte(value), 0600) != nil {
			return nil, errors.New("fixture resource setup failed")
		}
	}
	fd, err := os.Open(cgroup)
	if err != nil {
		return nil, err
	}
	in, write, err := os.Pipe()
	if err != nil {
		fd.Close()
		return nil, err
	}
	read, out, err := os.Pipe()
	if err != nil {
		fd.Close()
		in.Close()
		write.Close()
		return nil, err
	}
	command := exec.Command(program, args...)
	command.Env = env
	command.Stdin, command.Stdout, command.Stderr = in, out, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: chroot, Credential: &syscall.Credential{Uid: uint32(uid), Gid: ipcGID, Groups: []uint32{}}, Setpgid: true, UseCgroupFD: true, CgroupFD: int(fd.Fd()), Pdeathsig: syscall.SIGKILL}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		err = command.Start()
	}
	in.Close()
	out.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture role start failed:", name, err)
		fd.Close()
		write.Close()
		read.Close()
		return nil, err
	}
	p := &roleProcess{in: write, out: read, cmd: command, cgroup: cgroup, fd: fd, done: make(chan struct{}), receipt: roleReceipt{Role: name, UID: uid}}
	for key, destination := range map[string]*string{"memory.max": &p.receipt.MemoryMax, "pids.max": &p.receipt.PidsMax, "cpu.max": &p.receipt.CPUMax} {
		raw, _ := os.ReadFile(filepath.Join(cgroup, key))
		*destination = strings.TrimSpace(string(raw))
	}
	go func() { p.waitErr = command.Wait(); close(p.done) }()
	return p, nil
}

func (p *roleProcess) Input() io.WriteCloser { return p.in }
func (p *roleProcess) Output() io.ReadCloser { return p.out }
func (p *roleProcess) Audit() io.ReadCloser  { return p.out }
func (p *roleProcess) Wait() error           { <-p.done; return p.waitErr }
func (p *roleProcess) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		_ = p.in.Close()
		// Permit normal reader completion/audit before the forced group kill.
		if p.receipt.Role == "reader" {
			select {
			case <-p.done:
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		procs, _ := os.ReadFile(filepath.Join(p.cgroup, "cgroup.procs"))
		p.receipt.DescendantsKilled = len(strings.Fields(string(procs)))
		if os.WriteFile(filepath.Join(p.cgroup, "cgroup.kill"), []byte("1"), 0600) != nil {
			p.stopErr = errors.New("fixture containment kill failed")
			return
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			p.stopErr = ctx.Err()
			return
		}
		for {
			events, err := os.ReadFile(filepath.Join(p.cgroup, "cgroup.events"))
			if err == nil && strings.Contains(string(events), "populated 0") {
				break
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				p.stopErr = ctx.Err()
				return
			}
		}
		p.receipt.Stopped = true
		_ = p.fd.Close()
	})
	return p.stopErr
}

func deniedPeer() error {
	connection, err := net.DialTimeout("unix", os.Getenv("SFL_FIXTURE_SOCKET"), time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	data, err := io.ReadAll(io.LimitReader(connection, 1))
	if len(data) != 0 || (err != nil && errors.Is(err, os.ErrDeadlineExceeded)) {
		return errors.New("unauthorized source data or timeout")
	}
	return nil
}

func worker() error {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK", "HOME"} {
		if os.Getenv(key) != "" {
			return errors.New("ambient worker environment")
		}
	}
	for _, path := range []string{"/source", "/home/franz", "/proc", "/bin/sh", "/usr/bin/git", "/worker-private-canary"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return errors.New("forbidden worker filesystem")
		}
	}
	if os.WriteFile("/must-not-write", []byte("inert"), 0600) == nil {
		return errors.New("worker root writable")
	}
	if c, err := net.DialTimeout("tcp", "192.0.2.1:9", 100*time.Millisecond); err == nil {
		c.Close()
		return errors.New("guest network available")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	_, input, ok := bytes.Cut(raw, []byte{'\n'})
	var session r.Session
	if !ok || json.Unmarshal(input, &session) != nil || session.Validate() != nil {
		return errors.New("bad fixture session")
	}
	limits, _ := json.Marshal(session.Limits)
	proxy := exec.Command("/sourceproxy", "--socket", os.Getenv("SFL_FIXTURE_SOCKET"), "--reader-uid", strconv.Itoa(readerUID), "--limits", string(limits))
	proxy.Env = []string{"LANG=C.UTF-8"}
	to, err := proxy.StdinPipe()
	if err != nil {
		return err
	}
	from, err := proxy.StdoutPipe()
	if err != nil {
		return err
	}
	proxy.Stderr = io.Discard
	if err = proxy.Start(); err != nil {
		return err
	}
	scan := bufio.NewScanner(from)
	scan.Buffer(make([]byte, 4096), 1<<20)
	call := func(id int, method string, params any) (map[string]json.RawMessage, error) {
		if json.NewEncoder(to).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}) != nil || !scan.Scan() {
			return nil, errors.New("fixture MCP exchange failed")
		}
		var reply struct {
			ID     int                        `json:"id"`
			Result map[string]json.RawMessage `json:"result"`
			Error  any                        `json:"error"`
		}
		if json.Unmarshal(scan.Bytes(), &reply) != nil || reply.ID != id || reply.Error != nil {
			return nil, errors.New("fixture MCP reply failed")
		}
		return reply.Result, nil
	}
	if _, err = call(1, "initialize", map[string]any{"protocolVersion": r.SourceMCPVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "sfl-actual-uid-fixture", "version": "1"}}); err != nil {
		return err
	}
	if json.NewEncoder(to).Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}) != nil {
		return errors.New("fixture MCP initialized failed")
	}
	tools, err := call(2, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var inventory []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(tools["tools"], &inventory) != nil || len(inventory) != 2 || inventory[0].Name != "read" || inventory[1].Name != "search" {
		return errors.New("wrong fixture tools")
	}
	read, err := call(3, "tools/call", map[string]any{"name": "read", "arguments": map[string]string{"revision": session.Snapshot.Head, "path": "worker.go"}})
	if err != nil {
		return err
	}
	var content []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(read["content"], &content) != nil || len(content) != 1 || content[0].Text != "worker-only evidence\n" {
		return errors.New("wrong immutable source")
	}
	if _, err = call(4, "tools/call", map[string]any{"name": "search", "arguments": map[string]any{"revision": session.Snapshot.Base, "needle": "source", "max_results": 2}}); err != nil {
		return err
	}
	_ = to.Close()
	for scan.Scan() {
	}
	if scan.Err() != nil || proxy.Wait() != nil {
		return errors.New("fixture proxy failed")
	}
	// Its own process group lets this child outlive the CLI parent. The role
	// cgroup, rather than only the parent's PID/process group, must kill it.
	escaped := exec.Command("/worker", "--escaped-child")
	escaped.Env = []string{"LANG=C.UTF-8", "GOMAXPROCS=1"}
	escaped.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if escaped.Start() != nil {
		return errors.New("escaped fixture child failed")
	}
	report := r.Report{JobID: session.JobID, PolicyVersion: session.PolicyVersion, PacketHash: session.PacketHash, Provider: r.Codex, Round: session.Round, SnapshotHash: r.Digest(session.Snapshot), ConfigHash: session.ConfigHash, Findings: []r.Finding{}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
	encoded, _ := json.Marshal(report)
	fmt.Println(`{"type":"thread.started"}`)
	fmt.Println(`{"type":"turn.started"}`)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(encoded)}})
	fmt.Println(`{"type":"turn.completed"}`)
	return nil
}
