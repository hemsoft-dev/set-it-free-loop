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

func gateFixture(t *testing.T) (*ContainmentGate, string, FakeGateManifest) {
	t.Helper()
	root, err := os.MkdirTemp("", "sfl-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	m := FakeGateManifest{1, "synthetic inert marker"}
	g, err := PrepareContainmentGate(root, authTestBinding(), m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Abort(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return g, root, m
}

// A real child process uses the exact stub entry point and ordinary inherited
// pipes. No systemd unit, VM, provider client, credential or inference is used.
func TestContainmentStubChild(t *testing.T) {
	if os.Getenv("SFL_GATE_TEST_CHILD") != "1" {
		return
	}
	var binding AuthBinding
	if json.Unmarshal([]byte(os.Getenv("SFL_GATE_BINDING")), &binding) != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ContainmentStubFake(ctx, os.Getenv("SFL_GATE_ENDPOINT"), binding, strings.Repeat("d", 32), os.Getenv("SFL_GATE_MANIFEST"), os.Getppid(), os.Stdout) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestContainmentGateActualPeerGrantAndSpentNonce(t *testing.T) {
	g, root, m := gateFixture(t)
	raw, _ := json.Marshal(g.binding)
	cmd := exec.Command(os.Args[0], "-test.run=^TestContainmentStubChild$")
	cmd.Env = []string{"SFL_GATE_TEST_CHILD=1", "SFL_GATE_BINDING=" + string(raw), "SFL_GATE_ENDPOINT=" + g.Endpoint(), "SFL_GATE_MANIFEST=" + Digest(m)}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill() }()
	identity, err := processGateIdentity(cmd.Process.Pid, strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err = g.GrantFake(context.Background(), identity, m); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil || strings.TrimSpace(output.String()) != Digest(m.Marker) {
		t.Fatal("inert stub failed", err, output.String())
	}
	held, err := g.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := g.read(held)
	held.close()
	if err != nil || record.Phase != "granted" || record.Identity != identity {
		t.Fatal("grant was not durable", record, err)
	}
	if err = g.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareContainmentGate(root, g.binding, m); err == nil {
		t.Fatal("spent launch ID reused")
	}
}

func TestContainmentGateAbortClosesAcceptedAndDelayedPeers(t *testing.T) {
	g, _, m := gateFixture(t)
	identity, _ := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	done := make(chan error, 1)
	go func() { done <- g.GrantFake(context.Background(), identity, m) }()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: g.Endpoint(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Wait for acceptance without sending a hello. Abort must interrupt this read.
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := len(g.peers)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	if err = g.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("aborted peer granted")
		}
	case <-time.After(time.Second):
		t.Fatal("accepted socket survived abort")
	}
	if late, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: g.Endpoint(), Net: "unix"}); err == nil {
		late.Close()
		t.Fatal("delayed peer connected after abort")
	}
}

func TestContainmentGateRecoveryRevokesOldLiveListener(t *testing.T) {
	g, root, m := gateFixture(t)
	recovered, err := OpenAbortedContainmentGate(context.Background(), root, g.binding)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.listener != nil {
		t.Fatal("recovery recreated endpoint")
	}
	// The original process's listener may still exist: the durable flock/record
	// must forbid its grant. Revocation does not claim physical process death.
	done := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		done <- ContainmentStubFake(context.Background(), g.Endpoint(), g.binding, strings.Repeat("d", 32), Digest(m), os.Getpid(), &output)
	}()
	identity, _ := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	if err = g.GrantFake(context.Background(), identity, m); err == nil {
		t.Fatal("old supervisor regranted recovered launch")
	}
	if err = <-done; err == nil || output.Len() != 0 {
		t.Fatal("revoked stub received payload", err)
	}
}

func TestContainmentGateRejectsWrongIdentityManifestAndFrames(t *testing.T) {
	for _, kind := range []string{"pid", "ticks", "boot", "invocation", "manifest", "duplicate", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			g, _, m := gateFixture(t)
			identity, _ := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
			hello := gateHello{1, g.binding, identity.InvocationID, Digest(m)}
			switch kind {
			case "pid":
				identity.PID++
			case "ticks":
				identity.StartTicks++
			case "boot":
				identity.BootID = "wrong"
			case "invocation":
				hello.InvocationID = strings.Repeat("e", 32)
			case "manifest":
				hello.ManifestHash = Digest("wrong")
			}
			raw, _ := json.Marshal(hello)
			if kind == "duplicate" {
				raw = append([]byte(`{"version":1,`), raw[1:]...)
			}
			if kind == "oversize" {
				raw = []byte(strings.Repeat("x", 4097))
			}
			done := make(chan []byte, 1)
			go func() {
				conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: g.Endpoint(), Net: "unix"})
				if err != nil {
					done <- nil
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				conn.Write(raw)
				conn.CloseWrite()
				out, _ := io.ReadAll(conn)
				done <- out
			}()
			if err := g.GrantFake(context.Background(), identity, m); err == nil {
				t.Fatal("hostile grant accepted", kind)
			}
			if out := <-done; len(out) != 0 {
				t.Fatal("hostile peer received manifest")
			}
		})
	}
}

func TestContainmentGateRejectsJournalSymlinkAndBindingMismatch(t *testing.T) {
	g, root, _ := gateFixture(t)
	wrong := g.binding
	wrong.SessionHash = Digest("wrong")
	if _, err := OpenAbortedContainmentGate(context.Background(), root, wrong); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	path := filepath.Join(g.directory, "containment.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "canary")
	if err = os.WriteFile(other, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenAbortedContainmentGate(context.Background(), root, g.binding); err == nil {
		t.Fatal("journal symlink followed")
	}
	// Restore only for ordinary cleanup, without modifying the symlink target.
	os.Remove(path)
	os.WriteFile(path, raw, 0600)
}

func TestContainmentGateFailedDeliveryCannotReplayGrant(t *testing.T) {
	g, _, m := gateFixture(t)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: g.Endpoint(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	identity, _ := processGateIdentity(os.Getpid(), strings.Repeat("d", 32))
	raw, _ := json.Marshal(gateHello{1, g.binding, identity.InvocationID, Digest(m)})
	if _, err = conn.Write(raw); err != nil {
		t.Fatal(err)
	}
	conn.CloseWrite()
	conn.CloseRead()
	if err = g.GrantFake(context.Background(), identity, m); err == nil {
		t.Fatal("closed receiver accepted delivery")
	}
	held, err := g.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := g.read(held)
	held.close()
	if err != nil || record.Phase != "granted" {
		t.Fatal("failed delivery forgot durable grant", record, err)
	}
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ContainmentStubFake(context.Background(), g.Endpoint(), g.binding, identity.InvocationID, Digest(m), os.Getpid(), &output)
	}()
	if err = g.GrantFake(context.Background(), identity, m); err == nil {
		t.Fatal("failed grant replayed")
	}
	if err = <-done; err == nil || output.Len() != 0 {
		t.Fatal("retry received work", err)
	}
}
