//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func execObservationFixture(t *testing.T) (nativeVMRequest, NativeVMExecObservation) {
	q := nativeRequestFixture(t, Codex)
	return q, NativeVMExecObservation{Version: 1, Binding: q.Binding, RequestHash: Digest(q), ImageSourceHead: strings.Repeat("a", 40), HelperSHA256: Digest("pinned-helper"), HelperEntry: "sh-deny-v1", UID: nativeVMCodexUID, Complete: true}
}

func TestNativeExecObservationNeverEquatesMissingEvidenceWithIgnoredHooks(t *testing.T) {
	q, original := execObservationFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*NativeVMExecObservation)
		want   string
	}{
		{"zero", func(o *NativeVMExecObservation) {}, "no-observed-attempt"},
		{"denied", func(o *NativeVMExecObservation) { o.Attempts = 1; o.Denied = 1 }, "attempted-and-denied"},
		{"unsafe", func(o *NativeVMExecObservation) { o.Attempts = 1 }, "unsafe"},
		{"wrong-session", func(o *NativeVMExecObservation) { o.Binding.SessionHash = Digest("other") }, "unknown"},
		{"stale-launch", func(o *NativeVMExecObservation) { o.Binding.LaunchID = Digest("foreign-launch")[:32] }, "unknown"},
		{"wrong-request", func(o *NativeVMExecObservation) { o.RequestHash = Digest("other") }, "unknown"},
		{"uid", func(o *NativeVMExecObservation) { o.UID = nativeVMCodeHostUID }, "unknown"},
		{"incomplete", func(o *NativeVMExecObservation) { o.Complete = false }, "unknown"},
		{"forged-or-failed", func(o *NativeVMExecObservation) { o.Failed = true }, "unknown"},
		{"malformed-count", func(o *NativeVMExecObservation) { o.Denied = 1 }, "unknown"},
		{"negative", func(o *NativeVMExecObservation) { o.Attempts = -1 }, "unknown"},
		{"flood", func(o *NativeVMExecObservation) { o.Attempts = 65 }, "unknown"},
		{"missing-pin", func(o *NativeVMExecObservation) { o.HelperSHA256 = "" }, "unknown"},
		{"missing-image", func(o *NativeVMExecObservation) { o.ImageSourceHead = "" }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := original
			tc.change(&o)
			if got := o.Outcome(q.Binding, Digest(q)); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	if (*NativeVMExecObservation)(nil).Outcome(q.Binding, Digest(q)) != "unknown" {
		t.Fatal("missing observation accepted")
	}
}

func TestNativeExecObservationIsBoundInsideThePrivateEnvelope(t *testing.T) {
	q, o := execObservationFixture(t)
	f := nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: "invalid_output", ExecObservation: &o}
	if !f.valid(q) {
		t.Fatal("bound noncomplete receipt refused")
	}
	for _, raw := range []string{`{`, `{"Version":1}`, string(mustExecJSON(t, o))[:30]} {
		var decoded NativeVMExecObservation
		if json.Unmarshal([]byte(raw), &decoded) == nil && decoded.Bound(q.Binding, Digest(q)) {
			t.Fatal("truncated observation accepted")
		}
	}
	f.ExecObservation.RequestHash = Digest("stale")
	if f.valid(q) {
		t.Fatal("stale observation accepted in private envelope")
	}
	// Missing evidence remains permitted for older guest images, but is not
	// an observation. The codex-settings fixture separately refuses it.
	f.ExecObservation = nil
	if !f.valid(q) {
		t.Fatal("legacy failure receipt changed")
	}
}

func mustExecJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativeDenyExecSyntheticChild(t *testing.T) {
	if os.Getenv("SFL_DENY_TEST_SOCKET") == "" {
		return
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: os.Getenv("SFL_DENY_TEST_SOCKET"), Net: "unix"})
	if err != nil {
		os.Exit(125)
	}
	defer conn.Close()
	os.Exit(nativeDenyExecExchange(conn))
}

func TestNativeDenyExecAuthenticatesOriginalProcessAndAlwaysDenies(t *testing.T) {
	group, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "uid", "gid", "executable", "cgroup", "entry", "forged-peer"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "exec.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, program, "-test.run=^TestNativeDenyExecSyntheticChild$", "--", "-c", "touch "+filepath.Join(dir, "must-not-exist"))
			command.Args[0] = "/bin/sh"
			if kind == "entry" {
				command.Args[0] = "/bin/foreign"
			}
			command.Env = []string{"SFL_DENY_TEST_SOCKET=" + path}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			listener.SetDeadline(time.Now().Add(2 * time.Second))
			conn, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
			pin, wantGroup := program, string(group)
			switch kind {
			case "uid":
				uid++
			case "gid":
				gid++
			case "executable":
				pin = "/dev/null"
			case "cgroup":
				wantGroup = "0::/other-job\n"
			}
			ok := nativeDenyExecPeerMatches(conn, pin, uid, gid, wantGroup)
			if kind == "forged-peer" {
				ok = nativeDenyExecPeer(conn, program)
			}
			if ok != (kind == "valid") {
				t.Fatal("peer validation differs", kind, ok)
			}
			o := &nativeGuestExecObserver{}
			if observed := o.observe(conn, ok); observed != (kind == "valid") {
				t.Fatal("forged helper record accepted", kind)
			}
			conn.Close()
			err = command.Wait()
			exit, yes := err.(*exec.ExitError)
			if !yes || exit.ExitCode() != 126 {
				t.Fatal("helper did not deny", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "must-not-exist")); !os.IsNotExist(err) {
				t.Fatal("candidate command executed")
			}
			if kind == "valid" && (o.result.Attempts != 1 || o.result.Denied != 1) {
				t.Fatal("actual synthetic helper entry not recorded")
			}
		})
	}
}

func TestNativeDenyExecRejectsForgedAndTruncatedFrames(t *testing.T) {
	for _, frame := range []string{"", nativeDenyExecFrame[:5], nativeDenyExecFrame + "extra", "{\"Denied\":true}"} {
		path := filepath.Join(t.TempDir(), "sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan bool, 1)
		go func() {
			conn, err := listener.AcceptUnix()
			if err != nil {
				done <- false
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			o := &nativeGuestExecObserver{}
			done <- o.observe(conn, true)
		}()
		client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(client, bytes.NewBufferString(frame))
		client.CloseWrite()
		if <-done {
			t.Fatal("malformed frame accepted", frame)
		}
		client.Close()
		listener.Close()
	}
}

func TestNativeDenyHelperRefusesHostAndMissingObserver(t *testing.T) {
	if NativeVMDenyShell() != 126 || nativeDenyExec(filepath.Join(t.TempDir(), "missing.sock")) != 126 {
		t.Fatal("helper allowed execution")
	}
}
