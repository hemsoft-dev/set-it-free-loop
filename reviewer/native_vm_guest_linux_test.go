//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativePrivateDescriptorExecChild(t *testing.T) {
	if os.Getenv("SFL_NATIVE_FD_TEST") != "1" {
		return
	}
	fd, _ := strconv.Atoi(os.Getenv("SFL_NATIVE_FD"))
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	inherited := err == nil && strconv.FormatUint(st.Ino, 10) == os.Getenv("SFL_NATIVE_FD_INODE")
	if inherited != (os.Getenv("SFL_NATIVE_FD_LEAK") == "1") {
		t.Fatal("private descriptor inheritance differs", inherited)
	}
}

func TestNativePrivateDescriptorCannotReachAnExecChild(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	fd, err := unix.FcntlInt(read.Fd(), unix.F_DUPFD, 100)
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		t.Fatal("descriptor stat failed")
	}
	child := func(leak string) {
		command := exec.Command(os.Args[0], "-test.run=^TestNativePrivateDescriptorExecChild$")
		command.Env = []string{"SFL_NATIVE_FD_TEST=1", "SFL_NATIVE_FD=" + strconv.Itoa(fd), "SFL_NATIVE_FD_INODE=" + strconv.FormatUint(st.Ino, 10), "SFL_NATIVE_FD_LEAK=" + leak}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("exec descriptor probe: %s %v", output, err)
		}
	}
	child("1") // Positive control reproduces ExtraFiles' unsealed descriptor.
	file := nativeGuestPrivateFile(uintptr(fd), "private-test-channel")
	if file == nil {
		unix.Close(fd)
		t.Fatal("private descriptor adoption failed")
	}
	defer file.Close()
	child("0")
}

func TestNativeReaderDrainPrecedesCgroupKill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "reader")
	if os.Mkdir(root, 0700) != nil || os.WriteFile(filepath.Join(root, "cgroup.events"), []byte("populated 0\n"), 0600) != nil || os.WriteFile(filepath.Join(root, "cgroup.kill"), []byte("0"), 0600) != nil {
		t.Fatal("fake reader group setup failed")
	}
	group, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	p := &nativeGuestProcess{in: write, group: root, groupFD: group, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(ctx) }()
	// A bounded reader audit can need more than the former 250 ms grace.
	time.Sleep(350 * time.Millisecond)
	// Real cgroup controls already exist; inspect their write rather than
	// depending on creation of an ordinary file by the old pathname helper.
	before, readErr := os.ReadFile(filepath.Join(root, "cgroup.kill"))
	close(p.done)
	err = <-stopped
	after, finalErr := os.ReadFile(filepath.Join(root, "cgroup.kill"))
	if err != nil || readErr != nil || finalErr != nil || string(before) != "0" || string(after) != "1" {
		t.Fatal("reader killed before drain or cleanup failed", string(before), string(after), readErr, finalErr, err)
	}
}

func nativeAuthFixture(provider Provider) []byte {
	// The schema mirrors the pinned fixtures. Values are fresh synthetic text;
	// these unit tests neither start a client nor establish subscription support.
	if provider == Codex {
		jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"native-unit@example.invalid"}`)) + ".synthetic"
		raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"id_token": jwt, "access_token": "unit-access-token", "refresh_token": "unit-refresh-token", "account_id": "unit-account-id"}, "last_refresh": "2026-10-20T00:00:00Z"})
		return raw
	}
	return []byte(`{"claudeAiOauth":{"accessToken":"unit-claude-access","refreshToken":"unit-claude-refresh","expiresAt":1,"scopes":["user:profile","user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_5x"}}`)
}

func TestNativeAuthStrictSubscriptionFilesAndPrivateCanaries(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			raw := nativeAuthFixture(provider)
			values, err := nativeAuthValues(provider, raw)
			if err != nil || len(values) < 2 {
				t.Fatal("synthetic subscription schema rejected", err)
			}
			for _, value := range values {
				if !nativeCredentialCanarySeen(append([]byte("prefix:"), value...), values) {
					t.Fatal("credential canary absent")
				}
			}
			if nativeCredentialCanarySeen([]byte("ordinary immutable source finding"), values) {
				t.Fatal("unrelated report denied")
			}
			for _, value := range values {
				clear(value)
			}
			other := Claude
			if provider == Claude {
				other = Codex
			}
			if _, err = nativeAuthValues(other, raw); err != ErrAuthTransfer {
				t.Fatal("opposite provider accepted", err)
			}
		})
	}
	valid := string(nativeAuthFixture(Codex))
	bad := []string{
		`null`, `[]`, `{}`, valid + `{}`,
		strings.Replace(valid, `"auth_mode":"chatgpt"`, `"auth_mode":"chatgpt","auth_mode":"chatgpt"`, 1),
		strings.Replace(valid, `"auth_mode":"chatgpt"`, `"auth_mode":"api_key"`, 1),
		strings.Replace(valid, `"OPENAI_API_KEY":null`, `"OPENAI_API_KEY":"unit-api-key"`, 1),
		strings.Replace(valid, `"unit-refresh-token"`, `""`, 1),
		strings.Replace(valid, `"unit-account-id"`, `"account\nline"`, 1),
		strings.Replace(valid, `"last_refresh"`, `"unexpected"`, 1),
		strings.Replace(valid, base64.RawURLEncoding.EncodeToString([]byte(`{"email":"native-unit@example.invalid"}`)), base64.RawURLEncoding.EncodeToString([]byte(`null`)), 1),
		strings.Replace(valid, base64.RawURLEncoding.EncodeToString([]byte(`{"email":"native-unit@example.invalid"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a","email":"b"}`)), 1),
		strings.Repeat("x", authTransferLimit+1),
	}
	for i, raw := range bad {
		if _, err := nativeAuthValues(Codex, []byte(raw)); err != ErrAuthTransfer {
			t.Fatalf("invalid auth case %d accepted", i)
		}
	}
	claude := string(nativeAuthFixture(Claude))
	for _, raw := range []string{strings.Replace(claude, `"max"`, `"api"`, 1), strings.Replace(claude, `"user:inference"`, `"user:profile"`, 1), strings.Replace(claude, `"expiresAt":1`, `"expiresAt":0`, 1), strings.Replace(claude, `"rateLimitTier"`, `"apiKey"`, 1)} {
		if _, err := nativeAuthValues(Claude, []byte(raw)); err != ErrAuthTransfer {
			t.Fatal("invalid Claude auth accepted")
		}
	}
}

type nativeChunkReader struct{ chunks [][]byte }

func (r *nativeChunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestNativeCodeStreamRejectsSplitCanaryAndOverflowBeforeForward(t *testing.T) {
	var output bytes.Buffer
	secret := []byte("private-unit-credential")
	r := &nativeChunkReader{chunks: [][]byte{[]byte("prefix-private-unit-"), []byte("credential-suffix")}}
	result := nativeCodeCopy(&output, r, [][]byte{secret})
	if result.err == nil || bytes.Contains(output.Bytes(), secret) || output.String() != "prefix-private-unit-" {
		t.Fatal("split credential forwarded", output.String())
	}
	output.Reset()
	raw := bytes.Repeat([]byte("a"), nativeCodeLimit+1)
	result = nativeCodeCopy(&output, bytes.NewReader(raw), nil)
	if result.err == nil || output.Len() > nativeCodeLimit || result.bytes != int64(output.Len()) || result.hash != authHash(output.Bytes()) {
		t.Fatal("stream bound/hash differs")
	}
	output.Reset()
	result = nativeCodeCopy(&output, strings.NewReader("bounded code RPC"), nil)
	if result.err != nil || result.hash != authHash(output.Bytes()) || result.bytes != int64(output.Len()) {
		t.Fatal("ordinary stream failed", result)
	}

}

func nativeReleaseFixture() nativeGuestRelease {
	r := nativeGuestRelease{Version: 1, Purpose: "native-review", SourceHead: strings.Repeat("a", 40)}
	r.Files = append(r.Files, nativeGuestFile{Source: "/native-init", Target: "/native-init", SHA256: authHash([]byte("unit")), Bytes: 4, Roles: []string{"reader", "codex", "claude", "codehost"}})
	for role, targets := range map[string][]string{"codex": {"/client/codex", "/sourceproxy", "/usr/bin/env", "/client/codex-code-mode-host"}, "claude": {"/client/claude", "/sourceproxy", "/usr/bin/env"}, "codehost": {"/codehost"}} {
		for _, target := range targets {
			r.Files = append(r.Files, nativeGuestFile{Source: "/runtime" + target, Target: target, SHA256: authHash([]byte("unit")), Bytes: 4, Roles: []string{role}})
		}
	}
	return r
}

func TestNativeGuestReleaseRejectsAmbientExecutablesAndMissingRoles(t *testing.T) {
	if !nativeReleaseFixture().valid() {
		t.Fatal("fixed image roles rejected")
	}
	for _, kind := range []string{"purpose", "head", "shell", "profile", "source", "role", "missing", "duplicate", "module", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			r := nativeReleaseFixture()
			switch kind {
			case "purpose":
				r.Purpose = "synthetic"
			case "head":
				r.SourceHead = "main"
			case "shell":
				r.Files[0].Target = "/bin/sh"
			case "profile":
				r.Files[0].Target = "/profile/codex/auth.json"
			case "source":
				r.Files[0].Source = "/home/franz/.codex/auth.json"
			case "role":
				r.Files[0].Roles[0] = "publisher"
			case "missing":
				r.Files[0].Roles = []string{"reader", "codex", "codehost"}
			case "duplicate":
				r.Files = append(r.Files, r.Files[0])
			case "module":
				r.Modules = []nativeGuestModule{{Path: "/runtime/not-a-module", SHA256: r.Files[0].SHA256, Bytes: 1}}
			case "bytes":
				r.Files[0].Bytes = 513 << 20
			}
			if r.valid() {
				t.Fatal("invalid release accepted")
			}
		})
	}
}

func TestNativeGuestEntrypointRefusesHostBeforeSideEffects(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("requires ordinary test process")
	}
	for _, args := range [][]string{nil, {"--client-proxy"}, {"--native-codehost"}, {"--unknown"}, {"one", "two"}} {
		if NativeVMGuestMain(args) != 2 {
			t.Fatal("host entrypoint accepted", args)
		}
	}
	if _, err := nativeGuestSetup(); err != ErrLiveBlocked {
		t.Fatal("host guest setup allowed", err)
	}
	if _, err := newNativeGuestCodeBroker("/run/native/codehost"); err != ErrLiveBlocked {
		t.Fatal("host broker allowed", err)
	}
}

func TestNativeGuestInvocationBothProvidersRetainSourceAndFixedProxy(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		q := nativeRequestFixture(t, provider)
		inv, err := nativeGuestInvocation(q, "http://127.0.0.1:49152")
		if err != nil || !readerInvocationMatches(q.readerConfig(), inv) {
			t.Fatal("source scope lost", provider, err)
		}
		if !nativeGuestVersion(q.Session.Identity, map[Provider]string{Codex: "codex-cli " + q.Session.Identity.ClientVersion, Claude: q.Session.Identity.ClientVersion + " (Claude Code)"}[provider]) || nativeGuestVersion(q.Session.Identity, "other version") {
			t.Fatal("substituted client version accepted")
		}
		if strings.Contains(strings.Join(inv.Environment, "\n"), "API_KEY") {
			t.Fatal("API auth environment present")
		}
		for _, state := range []string{"quota_wait", "incomplete_expired", "auth_failed", "cancelled"} {
			if !(nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: state}).valid(q) {
				t.Fatal("native failure classification lost", state)
			}
		}
	}
}

// guestFinishedWire runs the guest's own finish producer over a pipe and
// returns the exact control record bytes the host reads.
func guestFinishedWire(t *testing.T, q nativeVMRequest, state string, report []byte) []byte {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	wire := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(read); wire <- raw }()
	phase := &nativeGuestPhase{request: q, control: write, state: state, stage: "native-execute", report: report}
	phase.finish(context.Background())
	write.Close()
	return <-wire
}

func TestNativeVMHostReceivesTypedGuestFailureAsEncodedByTheGuest(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		q := nativeRequestFixture(t, provider)
		report, _ := json.Marshal(fakeReport(q.Session))
		cases := map[string]string{"auth_failed": "auth_failed", "provider_unavailable": "provider_unavailable", "quota_wait": "quota_wait", "incomplete_expired": "incomplete_expired", "invalid_output": "invalid_output", "cancelled": "cancelled", "": "blocked"}
		// A complete phase without a stopped auth return is downgraded by the
		// guest itself; the host must still see that typed state.
		cases["complete"] = "invalid_output"
		for state, want := range cases {
			raw := report
			if state != "complete" {
				raw = nil
			}
			wire := guestFinishedWire(t, q, state, append([]byte{}, raw...))
			finished, err := readVMRecord[nativeVMFinished](bytes.NewReader(wire), 4<<20)
			var phase PhaseError
			failure := finished.failure(q)
			if err != nil || !errors.As(failure, &phase) || phase.State != want || errors.Is(failure, ErrVMBoot) || len(finished.Report) != 0 {
				t.Error("typed guest failure did not reach the host", provider, state, finished.State, string(finished.Report), err, failure)
			}
			if bytes.Contains(wire, []byte(`"Report"`)) {
				t.Error("guest encoded an absent report", provider, state)
			}
		}
	}
}

func TestNativeVMFinishedRefusesReportsOutsideCompleteOverTheWire(t *testing.T) {
	q := nativeRequestFixture(t, Codex)
	report, _ := json.Marshal(fakeReport(q.Session))
	code := &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(q.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true, RelayComplete: true}
	complete := nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: "complete", Report: report, SourceAudit: []byte("private-audit-placeholder"), AuthReturn: []byte("private-return-placeholder"), CodeHost: code}
	roundTrip := func(value any) (nativeVMFinished, error) {
		var wire bytes.Buffer
		if err := writeVMRecord(&wire, value, 4<<20); err != nil {
			t.Fatal(err)
		}
		return readVMRecord[nativeVMFinished](&wire, 4<<20)
	}
	refused := func(got nativeVMFinished) bool {
		var phase PhaseError
		failure := got.failure(q)
		return !got.valid(q) && errors.As(failure, &phase) && phase.State == "invalid_output" && errors.Is(failure, ErrVMBoot)
	}
	if got, err := roundTrip(complete); err != nil || got.failure(q) != nil || !bytes.Equal(got.Report, report) {
		t.Fatal("complete record with a report refused", err)
	}
	missing := complete
	missing.Report = nil
	if got, err := roundTrip(missing); err != nil || !refused(got) {
		t.Fatal("complete record without a report accepted", err)
	}
	failed := complete
	failed.State = "auth_failed"
	if got, err := roundTrip(failed); err != nil || !refused(got) {
		t.Fatal("typed failure carrying a report accepted", err)
	}
	// The guest omits an absent report. An explicit null is not its encoding
	// and stays a carried value that a typed failure cannot hold.
	raw, _ := json.Marshal(nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: "auth_failed"})
	if !bytes.Contains(raw, []byte(`"Report":null`)) {
		raw = bytes.Replace(raw, []byte(`"State":"auth_failed",`), []byte(`"State":"auth_failed","Report":null,`), 1)
	}
	if got, err := roundTrip(json.RawMessage(raw)); err != nil || !refused(got) || string(got.Report) != "null" {
		t.Fatal("typed failure with an explicit null report accepted", err)
	}
	raw, _ = json.Marshal(missing)
	if !bytes.Contains(raw, []byte(`"Report":null`)) {
		raw = bytes.Replace(raw, []byte(`"State":"complete",`), []byte(`"State":"complete","Report":null,`), 1)
	}
	if got, err := roundTrip(json.RawMessage(raw)); err != nil || !refused(got) || string(got.Report) != "null" {
		t.Fatal("complete record with an explicit null report accepted", err)
	}
}

func TestNativeGuestReturnedFileRequiresBoundedSingleOwnedRegularFile(t *testing.T) {
	for _, kind := range []string{"valid", "symlink", "parent-escape", "hardlink", "permissions", "oversize", "fifo", "owner"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "profile/codex")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "auth.json")
			raw := nativeAuthFixture(Codex)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			uid := os.Getuid()
			switch kind {
			case "symlink":
				os.Remove(path)
				if err := os.Symlink("/not-a-profile", path); err != nil {
					t.Fatal(err)
				}
			case "parent-escape":
				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "auth.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				os.Remove(path)
				os.Remove(dir)
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(dir, "second")); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), authTransferLimit+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				os.Remove(path)
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "owner":
				uid++
			}
			got, err := nativeGuestReadProfileFile(root, Codex, uid)
			defer clear(got)
			if kind == "valid" {
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatal("valid returned file rejected", err)
				}
			} else if err != ErrAuthTransfer || len(got) != 0 {
				t.Fatal("unsafe return accepted", kind, err)
			}
		})
	}
}

func TestNativeGuestLauncherDoesNotStartForeignInvocation(t *testing.T) {
	q := nativeRequestFixture(t, Claude)
	inv, err := nativeGuestInvocation(q, "http://127.0.0.1:49152")
	if err != nil {
		t.Fatal(err)
	}
	l := nativeGuestLauncher{request: q, ready: nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}, invocation: inv}
	bad := inv
	bad.Args = append([]string{"--ambient-shell"}, inv.Args...)
	if _, err = l.Inspect(context.Background(), bad); !errors.Is(err, ErrVMBoot) {
		t.Fatal("foreign invocation accepted")
	}
	if worker, err := l.Start(context.Background(), inv); worker != nil || err != ErrVMBoot {
		t.Fatal("missing prestarted worker accepted")
	}
}

func TestNativeGuestConsumesActualPinnedImageProducerRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-vm-release.json")
	if err != nil {
		t.Fatal(err)
	}
	var release nativeGuestRelease
	if json.Unmarshal(raw, &release) != nil || !release.valid() {
		t.Fatal("actual image producer release rejected")
	}
	canonical, err := json.Marshal(release)
	if err != nil || !bytes.Equal(raw, canonical) {
		t.Fatal("Python producer and Go guest canonical encoding differ")
	}
	if release.SourceHead != "21d1bc85b8b588032e6becbe5b89cc7fb333f6c4" || len(release.Files) != 16 {
		t.Fatal("producer provenance changed")
	}
}

func TestNativeGuestProxyStagesRequireBoundedMatchingReady(t *testing.T) {
	q := nativeRequestFixture(t, Claude)
	for _, kind := range []string{"complete", "stage-only", "foreign", "port", "flood"} {
		var input bytes.Buffer
		count := 1
		if kind == "flood" {
			count = 8
		}
		for range count {
			if err := writeVMRecord(&input, nativeVMProgress{1, "guest-stage", "client-version"}, 4096); err != nil {
				t.Fatal(err)
			}
		}
		ready := nativeGuestProxyReady{nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}, 49152}
		if kind == "foreign" {
			ready.Ready.RequestHash = Digest("foreign")
		}
		if kind == "port" {
			ready.Port = 0
		}
		if kind != "stage-only" {
			if err := writeVMRecord(&input, ready, 4096); err != nil {
				t.Fatal(err)
			}
		}
		seen := 0
		observed, err := readNativeGuestProxyReady(&input, q, func(stage string) error {
			if stage != "client-version" {
				t.Fatal("foreign stage retained")
			}
			seen++
			return nil
		})
		if kind == "complete" {
			if err != nil || observed.Port != 49152 || seen != 1 {
				t.Fatal("proxy ready failed", err)
			}
		} else if err != ErrVMBoot {
			t.Fatal("incomplete proxy accepted", kind, err)
		}
	}
}
