//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// NativeVMGuestMain is the fixed sealed-image entrypoint. Normal host execution
// refuses before any mount, cgroup, profile or device operation. Internal modes
// additionally require their fixed guest PID/UID and descriptor contracts.
func NativeVMGuestMain(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "--native-reader":
			return nativeGuestReaderMain()
		case "--client-proxy":
			return nativeGuestClientProxyMain()
		case "--native-codehost":
			return nativeGuestCodeHostMain()
		case "--native-codehost-child":
			return nativeGuestCodeHostChild()
		case "--native-codehost-relay":
			return nativeGuestCodeRelay()
		}
		return 2
	}
	if len(args) != 0 || os.Getpid() != 1 || os.Getuid() != 0 {
		return 2
	}
	nativeGuestBootStage("entry")
	// Even setup failure must power off before a host can classify the launch.
	defer unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	release, err := nativeGuestSetup()
	if err != nil {
		return 2
	}
	nativeGuestBootStage("ports")
	control, err := nativeGuestPort("control")
	if err != nil {
		return 2
	}
	defer control.Close()
	bootProgress := func(stage string) bool {
		return writeVMRecord(control, nativeVMProgress{1, "guest-stage", stage}, 4096) == nil
	}
	if !bootProgress("boot-control") {
		return 2
	}
	objects, err := nativeGuestPort("objects")
	if err != nil {
		return 2
	}
	defer objects.Close()
	if !bootProgress("boot-objects") {
		return 2
	}
	provider, err := nativeGuestPort("provider")
	if err != nil {
		return 2
	}
	defer provider.Close()
	if !bootProgress("boot-provider") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { control.Close(); objects.Close(); provider.Close() })
	defer stop()
	if !bootProgress("boot-request") {
		return 2
	}
	nativeGuestBootStage("intake")
	q, err := readVMRecord[nativeVMRequest](control, 4<<20)
	if err != nil || !q.valid() {
		return 2
	}
	nativeGuestBootStage("binding")
	line, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return 2
	}
	bound := false
	for _, field := range strings.Fields(string(line)) {
		if field == "sfl-native="+q.Binding.LaunchID {
			bound = true
		}
	}
	if !bound {
		return 2
	}
	ctx, deadline := context.WithTimeout(ctx, q.Session.Limits.Timeout)
	defer deadline()
	phase := &nativeGuestPhase{request: q, control: control}
	// All roles are stopped and audit/auth return constructed on every path,
	// including a malformed native result after a successful refresh.
	defer phase.finish(context.WithoutCancel(ctx))
	if phase.progress("jails") != nil {
		return 2
	}
	readerRoot, err := release.jail("reader")
	if err != nil {
		return 2
	}
	clientRoot, err := release.jail(string(q.Session.Identity.Provider))
	if err != nil {
		return 2
	}
	phase.clientRoot = clientRoot
	if phase.progress("profile") != nil {
		return 2
	}
	if nativeGuestPrepareProfile(clientRoot, q) != nil {
		return 2
	}
	if phase.progress("reader") != nil {
		return 2
	}
	readerSource := filepath.Join(readerRoot, nativeVMSourceRoot)
	if os.Mkdir(readerSource, 0700) != nil || os.Chown(readerSource, nativeVMReaderUID, nativeVMIPCGID) != nil {
		return 2
	}
	phase.reader, err = startNativeGuestRole(ctx, "reader", readerRoot, nativeVMReaderUID, []*os.File{objects}, "--native-reader")
	if err != nil {
		return 2
	}
	phase.readerAudit = make(chan readerAuditResult, 1)
	readyReader := make(chan bool, 1)
	go readManagedAudit(phase.reader.out, q.readerConfig(), readyReader, phase.readerAudit)
	if phase.progress("source-ready") != nil {
		return 2
	}
	manifest, _ := json.Marshal(q.readerConfig())
	if !nativeGuestWrite(ctx, phase.reader.in, manifest, true) {
		return 2
	}
	select {
	case ready := <-readyReader:
		if !ready {
			return 2
		}
	case <-ctx.Done():
		return 2
	}
	if phase.progress("source-bind") != nil {
		return 2
	}
	sourceMount := filepath.Join(clientRoot, nativeVMSourceRoot, Digest(q.Session))
	if os.MkdirAll(filepath.Dir(sourceMount), 0755) != nil || os.Mkdir(sourceMount, 0710) != nil || nativeGuestBindDirectory(filepath.Join(readerSource, Digest(q.Session)), sourceMount) != nil {
		return 2
	}
	if q.Session.Identity.Provider == Codex {
		if phase.progress("codehost") != nil {
			return 2
		}
		codeRoot, err := release.jail("codehost")
		if err != nil {
			return 2
		}
		if os.Chown(filepath.Join(codeRoot, "tmp"), nativeVMCodeHostUID, nativeVMIPCGID) != nil || os.Chmod(filepath.Join(codeRoot, "tmp"), 0700) != nil {
			return 2
		}
		phase.broker, err = newNativeGuestCodeBroker(codeRoot)
		if err != nil {
			return 2
		}
		if os.Mkdir(filepath.Join(clientRoot, "codehost"), 0710) != nil || nativeGuestBindDirectory("/run/native/code-rpc", filepath.Join(clientRoot, "codehost")) != nil {
			return 2
		}
	}
	if phase.progress("client-proxy") != nil {
		return 2
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return 2
	}
	defer configRead.Close()
	defer configWrite.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return 2
	}
	defer func() {
		if phase.clientStatusInput == nil {
			readyRead.Close()
		}
	}()
	defer readyWrite.Close()
	phase.client, err = startNativeGuestRole(ctx, "client", clientRoot, 0, []*os.File{configRead, provider, readyWrite}, "--client-proxy")
	configRead.Close()
	readyWrite.Close()
	if err != nil {
		return 2
	}
	phase.clientControl = configWrite
	stopConfig := context.AfterFunc(ctx, func() { configWrite.Close(); readyRead.Close() })
	defer stopConfig()
	if writeVMRecord(configWrite, q, 4<<20) != nil {
		return 2
	}
	if phase.progress("client-ready") != nil {
		return 2
	}
	observed, err := readNativeGuestProxyReady(readyRead, q, phase.progress)
	if err != nil || !observed.Ready.valid(q) || observed.Port <= 0 || observed.Port > 65535 {
		return 2
	}
	phase.clientStatusInput = readyRead
	phase.clientStatus = make(chan string, 1)
	go func() { phase.clientStatus <- readNativeGuestClientStatus(readyRead, q) }()
	inv, err := nativeGuestInvocation(q, "http://127.0.0.1:"+strconv.Itoa(observed.Port))
	if err != nil {
		return 2
	}
	nativeGuestBootStage("ready")
	if writeVMRecord(control, observed.Ready, 4096) != nil {
		return 2
	}
	phase.stage = "auth-seed"
	wire, err := readVMRecord[[]byte](control, 100<<10)
	if err != nil {
		return 2
	}
	defer clear(wire)
	seed, err := ReadAuthTransfer(bytes.NewReader(wire))
	if err != nil {
		return 2
	}
	defer seed.Close()
	info, err := seed.Info()
	if err != nil || info.Kind != "seed" || info.Binding != q.Binding || info.Provider != q.Session.Identity.Provider {
		return 2
	}
	phase.seed = &info
	phase.stage = "auth-profile"
	if seed.Consume(func(raw []byte) error {
		values, err := nativeAuthValues(info.Provider, raw)
		if err != nil {
			return err
		}
		phase.canaries = values
		if info.Provider == Claude {
			for _, hint := range []string{q.ProfileHints.ClaudeAccountUUID, q.ProfileHints.ClaudeOrganizationUUID, q.ProfileHints.ClaudeEmail} {
				if hint != "" {
					phase.canaries = append(phase.canaries, []byte(hint))
				}
			}
		}
		return nativeGuestWriteProfile(clientRoot, info.Provider, raw)
	}) != nil {
		phase.state = "auth_failed"
		return 1
	}
	if phase.broker != nil {
		phase.brokerDone = make(chan nativeVMCodeHostAccess, 1)
		brokerCtx, cancelBroker := context.WithCancel(ctx)
		phase.cancelBroker = cancelBroker
		go func() { phase.brokerDone <- phase.broker.serve(brokerCtx, q.Session, phase.canaries) }()
	}
	launcher := &nativeGuestLauncher{request: q, ready: observed.Ready, invocation: inv, process: phase.client, control: configWrite}
	phase.stage = "native-execute"
	data, _, err := (NativeDriver{Launcher: launcher, observe: phase.observeNative}).Execute(ctx, inv)
	phase.observeClientExit()
	if err != nil {
		phase.state = phaseState(err)
		return 1
	}
	phase.stage = "native-output"
	if nativeCredentialCanarySeen(data, phase.canaries) {
		phase.state = "invalid_output"
		clear(data)
		return 1
	}
	report, err := DecodeStrict[Report](data, q.Session.Limits.OutputBytes)
	if err != nil || ValidateReport(report, q.Session) != nil {
		phase.state = "invalid_output"
		clear(data)
		return 1
	}
	phase.report, phase.state = data, "complete"
	phase.stage = "complete"
	return 0
}

func nativeGuestBindDirectory(source, target string) error {
	if unix.Mount(source, target, "", unix.MS_BIND, "") != nil || unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil {
		return ErrVMBoot
	}
	return nil
}

func nativeGuestWrite(ctx context.Context, f *os.File, data []byte, closeAfter bool) bool {
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()
	n, err := f.Write(data)
	if closeAfter {
		err = errors.Join(err, f.Close())
	}
	return err == nil && n == len(data) && ctx.Err() == nil
}

type nativeGuestLauncher struct {
	mu         sync.Mutex
	request    nativeVMRequest
	ready      nativeVMReady
	invocation Invocation
	process    *nativeGuestProcess
	control    *os.File
	started    bool
}

func (l *nativeGuestLauncher) Inspect(ctx context.Context, inv Invocation) (Identity, error) {
	if ctx.Err() != nil || !l.ready.valid(l.request) || Digest(inv) != Digest(l.invocation) {
		return Identity{}, ErrVMBoot
	}
	return l.ready.Identity, nil
}
func (l *nativeGuestLauncher) Start(ctx context.Context, inv Invocation) (Worker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.Inspect(ctx, inv); err != nil || l.started || l.process == nil {
		return nil, ErrVMBoot
	}
	l.started = true
	stop := context.AfterFunc(ctx, func() { l.control.Close() })
	defer stop()
	if writeVMRecord(l.control, nativeGuestStart{1, "start"}, 256) != nil {
		return l.process, ErrVMBoot
	}
	l.control.Close()
	return l.process, nil
}

type nativeGuestPhase struct {
	request           nativeVMRequest
	control           *os.File
	clientRoot        string
	client, reader    *nativeGuestProcess
	clientControl     *os.File
	clientStatusInput *os.File
	clientStatus      chan string
	readerAudit       chan readerAuditResult
	broker            *nativeGuestCodeBroker
	brokerDone        chan nativeVMCodeHostAccess
	cancelBroker      context.CancelFunc
	seed              *AuthTransferInfo
	canaries          [][]byte
	report            []byte
	state             string
	stage             string
	nativeMu          sync.Mutex
	native            *nativeResultObservation
}

func (p *nativeGuestPhase) observeNative(value nativeResultObservation) {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if value.valid(p.request.Session) {
		p.native = &value
	}
}

func (p *nativeGuestPhase) nativeSnapshot() *nativeResultObservation {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if p.native == nil {
		return nil
	}
	copy := *p.native
	return &copy
}

func (p *nativeGuestPhase) progress(stage string) error {
	if !nativeVMGuestStage(stage) {
		return ErrVMBoot
	}
	p.stage = stage
	return writeVMRecord(p.control, nativeVMProgress{1, "guest-stage", stage}, 4096)
}

func (p *nativeGuestPhase) finish(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	defer func() {
		for _, value := range p.canaries {
			clear(value)
		}
		clear(p.report)
	}()
	finished := nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: p.state, GuestStage: p.stage}
	if finished.State == "" {
		finished.State = "blocked"
	}
	stopped := true
	if p.clientControl != nil {
		p.clientControl.Close()
	}
	if p.client != nil {
		stopped = p.client.Stop(ctx) == nil
		p.client.out.Close()
	}
	if p.clientStatusInput != nil {
		select {
		case class := <-p.clientStatus:
			if class != "" && finished.State != "complete" {
				finished.GuestStage = "client-error-" + class
			}
		case <-ctx.Done():
		}
		p.clientStatusInput.Close()
	}
	if p.reader != nil {
		stopped = p.reader.Stop(ctx) == nil && stopped
		if p.readerAudit != nil {
			select {
			case audit := <-p.readerAudit:
				if audit.err == nil {
					decoded, err := DecodeSourceReaderAudit(p.request.readerConfig(), audit.raw)
					if err == nil && !decoded.Failed {
						finished.SourceAudit = audit.raw
					}
				}
			case <-ctx.Done():
				stopped = false
			}
		}
		p.reader.out.Close()
	}
	if p.broker != nil {
		// The client is stopped. Revoke new intake without cancelling an
		// already adopted pipe exchange that still needs to drain.
		p.broker.close()
	}
	if p.brokerDone != nil {
		select {
		case code := <-p.brokerDone:
			finished.CodeHost = &code
		case <-ctx.Done():
			p.cancelBroker()
			stopped = false
		}
	}
	if p.cancelBroker != nil {
		p.cancelBroker()
	}
	if p.broker != nil {
		p.broker.close()
	}
	if finished.CodeHost != nil {
		stopped = stopped && finished.CodeHost.Stopped
	}
	if stopped && p.seed != nil {
		raw, err := nativeGuestReadProfile(p.clientRoot, p.seed.Provider)
		if err == nil {
			defer clear(raw)
			values, err := nativeAuthValues(p.seed.Provider, raw)
			if err == nil {
				p.canaries = append(p.canaries, values...)
				returned, err := NewAuthReturn(*p.seed, raw)
				if err == nil {
					finished.AuthReturn, _ = nativeVMSeedRecord(returned)
					returned.Close()
				}
			}
		}
	}
	defer clear(finished.AuthReturn)
	finished.Native = p.nativeSnapshot()
	if finished.State == "complete" {
		finished.Report = p.report
		if !stopped || nativeCredentialCanarySeen(p.report, p.canaries) || !finished.valid(p.request) {
			finished.State, finished.Report = "invalid_output", nil
		}
	}
	stop := context.AfterFunc(ctx, func() { p.control.Close() })
	defer stop()
	_ = writeVMRecord(p.control, finished, 4<<20)
}

func nativeGuestPrepareProfile(root string, q nativeVMRequest) error {
	uid := nativeVMCodexUID
	if q.Session.Identity.Provider == Claude {
		uid = nativeVMClaudeUID
	}
	for _, path := range []string{filepath.Join(root, "profile"), filepath.Join(root, "tmp")} {
		if os.Chown(path, uid, nativeVMIPCGID) != nil || os.Chmod(path, 0700) != nil {
			return ErrVMBoot
		}
	}
	dir := filepath.Join(root, "profile", string(q.Session.Identity.Provider))
	if os.Mkdir(dir, 0700) != nil || os.Chown(dir, uid, nativeVMIPCGID) != nil {
		return ErrVMBoot
	}
	if q.Session.Identity.Provider == Claude {
		account := map[string]any{"hasCompletedOnboarding": true, "lastOnboardingVersion": q.Session.Identity.ClientVersion, "oauthAccount": map[string]string{"accountUuid": q.ProfileHints.ClaudeAccountUUID, "organizationUuid": q.ProfileHints.ClaudeOrganizationUUID, "emailAddress": q.ProfileHints.ClaudeEmail}}
		raw, _ := json.Marshal(account)
		if nativeGuestCreateFile(filepath.Join(dir, ".claude.json"), raw, uid) != nil {
			return ErrVMBoot
		}
	}
	return nativeGuestCreateFile(filepath.Join(root, "work/sfl-report.schema.json"), ReportSchema(), 0)
}

func nativeGuestCreateFile(path string, raw []byte, uid int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrVMBoot
	}
	defer f.Close()
	if f.Chown(uid, nativeVMIPCGID) != nil {
		return ErrVMBoot
	}
	if uid == 0 && f.Chmod(0444) != nil {
		return ErrVMBoot
	}
	if n, err := f.Write(raw); err != nil || n != len(raw) || f.Sync() != nil {
		return ErrVMBoot
	}
	return nil
}

func nativeGuestAuthPath(root string, provider Provider) string {
	name := "auth.json"
	if provider == Claude {
		name = ".credentials.json"
	}
	return filepath.Join(root, "profile", string(provider), name)
}

func nativeGuestWriteProfile(root string, provider Provider, raw []byte) error {
	uid := nativeVMCodexUID
	if provider == Claude {
		uid = nativeVMClaudeUID
	}
	if !provider.Valid() {
		return ErrAuthTransfer
	}
	return nativeGuestCreateFile(nativeGuestAuthPath(root, provider), raw, uid)
}

func nativeGuestReadProfile(root string, provider Provider) ([]byte, error) {
	if !provider.Valid() {
		return nil, ErrAuthTransfer
	}
	uid := nativeVMCodexUID
	if provider == Claude {
		uid = nativeVMClaudeUID
	}
	return nativeGuestReadProfileFile(root, provider, uid)
}

func nativeGuestReadProfileFile(root string, provider Provider, uid int) ([]byte, error) {
	// Root-open confines even parent symlink replacement to this disposable
	// profile tree. The provider role has already been killed and reaped.
	profile, err := os.OpenRoot(filepath.Join(root, "profile"))
	if err != nil {
		return nil, ErrAuthTransfer
	}
	defer profile.Close()
	f, err := profile.OpenFile(filepath.Join(string(provider), filepath.Base(nativeGuestAuthPath(root, provider))), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrAuthTransfer
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() <= 0 || info.Size() > authTransferLimit {
		return nil, ErrAuthTransfer
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid || st.Nlink != 1 {
		return nil, ErrAuthTransfer
	}
	raw, err := io.ReadAll(io.LimitReader(f, authTransferLimit+1))
	if err != nil || len(raw) != int(info.Size()) || len(raw) > authTransferLimit {
		clear(raw)
		return nil, ErrAuthTransfer
	}
	return raw, nil
}
