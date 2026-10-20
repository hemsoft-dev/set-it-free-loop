//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type authExportCheck func(context.Context, AuthBinding) error

func (f authExportCheck) ConfirmAuthExport(ctx context.Context, b AuthBinding) error {
	return f(ctx, b)
}
func authExportAllowed(t *testing.T) AuthExportVerifier {
	t.Helper()
	return authExportCheck(func(_ context.Context, b AuthBinding) error {
		if b != authTestBinding() {
			t.Fatal("wrong export binding")
		}
		return nil
	})
}
func authTransferFixture(t *testing.T, provider Provider) (*AuthStore, *AuthProfile, AuthTransfer) {
	t.Helper()
	store := authTestStore(t, provider)
	profile, err := store.LeaseProfile(context.Background(), authTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { profile.Close() })
	transfer, err := profile.ExportAuth(context.Background(), authExportAllowed(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transfer.Close() })
	return store, profile, transfer
}
func transferInfo(t *testing.T, transfer AuthTransfer) AuthTransferInfo {
	t.Helper()
	info, err := transfer.Info()
	if err != nil {
		t.Fatal(err)
	}
	return info
}
func authWire(t *testing.T, transfer AuthTransfer) []byte {
	t.Helper()
	var data bytes.Buffer
	n, err := transfer.WriteTo(&data)
	if err != nil || n != int64(data.Len()) {
		t.Fatal("producer failed", n, err)
	}
	return data.Bytes()
}

func TestAuthTransferActualUnixRoundTripAndStoppedReturn(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			store, profile, seed := authTransferFixture(t, provider)
			info := transferInfo(t, seed)
			path := filepath.Join(t.TempDir(), "auth.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(time.Second))
			guest := make(chan AuthTransfer, 1)
			failure := make(chan error, 1)
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					failure <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				frame, err := ReadAuthTransfer(conn)
				if err != nil {
					failure <- err
					return
				}
				guest <- frame
			}()
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			if _, err = seed.WriteTo(conn); err != nil {
				t.Fatal(err)
			}
			conn.CloseWrite()
			var received AuthTransfer
			select {
			case received = <-guest:
			case err := <-failure:
				t.Fatal(err)
			case <-time.After(2 * time.Second):
				t.Fatal("private auth transfer did not complete")
			}
			defer received.Close()
			if transferInfo(t, received) != info {
				t.Fatal("wire changed binding")
			}
			var held []byte
			if err = received.Consume(func(raw []byte) error {
				if string(raw) != "synthetic-seed" {
					t.Error("wrong seed")
				}
				held = raw
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(held, make([]byte, len(held))) {
				t.Fatal("guest retained seed buffer")
			}
			returned, err := NewAuthReturn(info, []byte("synthetic-rotated"))
			if err != nil {
				t.Fatal(err)
			}
			returnInfo := transferInfo(t, returned)
			if err = profile.ImportAuthReturn(context.Background(), returned, authStopped(t)); err != nil {
				t.Fatal(err)
			}
			if transferInfo(t, returned) != returnInfo {
				t.Fatal("metadata changed")
			}
			if err = returned.Consume(func([]byte) error { t.Error("return replay exposed auth"); return nil }); !errors.Is(err, ErrAuthTransfer) {
				t.Fatal(err)
			}
			old, _ := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if string(old) != "synthetic-seed" {
				t.Fatal("return committed before Complete")
			}
			if err = profile.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			state, _ := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if string(state) != "synthetic-rotated" {
				t.Fatal("returned auth not committed")
			}
			if _, err = os.Stat(profile.ProfileDirectory()); !os.IsNotExist(err) {
				t.Fatal("staging profile not disposed")
			}
		})
	}
}

func TestAuthTransferRejectsCrossBindingBeforeCallingStop(t *testing.T) {
	for _, kind := range []string{"provider", "session", "launch", "containment", "seed"} {
		t.Run(kind, func(t *testing.T) {
			_, profile, seed := authTransferFixture(t, Codex)
			info := transferInfo(t, seed)
			switch kind {
			case "provider":
				info.Provider = Claude
			case "session":
				info.Binding.SessionHash = strings.Repeat("d", 64)
			case "launch":
				info.Binding.LaunchID = strings.Repeat("d", 32)
			case "containment":
				info.Binding.ContainmentID = strings.Repeat("d", 64)
			case "seed":
				info.SeedHash = strings.Repeat("d", 64)
				info.PayloadHash = info.SeedHash
			}
			returned, err := NewAuthReturn(info, []byte("foreign-return"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			err = profile.ImportAuthReturn(context.Background(), returned, authStopCheck(func(context.Context, AuthBinding) error { calls++; return nil }))
			if err == nil || calls != 0 {
				t.Fatal("cross-job return invoked stop or was accepted", calls, err)
			}
			if !returned.state.used || len(returned.state.data) != 0 {
				t.Fatal("rejected secret remained reusable")
			}
		})
	}
}

func TestAuthTransferOneUseRedactionAndFailureWiping(t *testing.T) {
	_, _, seed := authTransferFixture(t, Codex)
	copyOfSeed := seed
	for _, printed := range []string{fmt.Sprintf("%v", seed), fmt.Sprintf("%+v", seed), fmt.Sprintf("%#v", seed)} {
		if strings.Contains(printed, "synthetic-seed") || !strings.Contains(printed, "redacted") {
			t.Fatal("format exposed payload")
		}
	}
	encoded, _ := json.Marshal(map[string]any{"auth": seed})
	if strings.Contains(string(encoded), "synthetic-seed") {
		t.Fatal("JSON exposed seed")
	}
	var retained []byte
	err := seed.Consume(func(data []byte) error { retained = data; return fmt.Errorf("must not leak: %s", data) })
	if err != ErrAuthTransfer || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("raw callback error escaped", err)
	}
	if !bytes.Equal(retained, make([]byte, len(retained))) {
		t.Fatal("failed callback retained bytes")
	}
	if err = copyOfSeed.Consume(func([]byte) error { t.Error("copied transfer reused seed"); return nil }); err != ErrAuthTransfer {
		t.Fatal(err)
	}
	_, _, next := authTransferFixture(t, Claude)
	var group sync.WaitGroup
	group.Add(2)
	outputs := make([]bytes.Buffer, 2)
	results := make([]error, 2)
	for i := range results {
		go func(i int) { defer group.Done(); _, results[i] = next.WriteTo(&outputs[i]) }(i)
	}
	group.Wait()
	successes := 0
	for i, err := range results {
		if err == nil {
			successes++
		} else if outputs[i].Len() != 0 {
			t.Fatal("second consumer wrote partial frame")
		}
	}
	if successes != 1 {
		t.Fatal("one transfer had multiple consumers", successes)
	}
}

type authBrokenWriter struct{}

func (authBrokenWriter) Write(data []byte) (int, error) {
	return min(3, len(data)), errors.New("private transport failure")
}
func TestAuthTransferPartialWriteCannotRetryOrRestoreSeed(t *testing.T) {
	store, profile, seed := authTransferFixture(t, Codex)
	if _, err := seed.WriteTo(authBrokenWriter{}); err != ErrAuthTransfer {
		t.Fatal(err)
	}
	if _, err := seed.WriteTo(io.Discard); err != ErrAuthTransfer {
		t.Fatal("partial transfer retried", err)
	}
	if _, err := profile.ExportAuth(context.Background(), authExportAllowed(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("lease exported twice", err)
	}
	if err := profile.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("unknown final auth reused old seed", err)
	}
	profile.Close()
	recovered, err := store.RecoverProfile(context.Background(), authTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err = recovered.ExportAuth(context.Background(), authExportAllowed(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("restart reseeded unknown transfer", err)
	}
	if err = recovered.Retire(context.Background(), authStopped(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(store.directory, "state.json")); !os.IsNotExist(err) {
		t.Fatal("retirement preserved uncertain credentials")
	}
}

func TestAuthTransferHostileFramesFromActualProducer(t *testing.T) {
	_, _, seed := authTransferFixture(t, Codex)
	raw := authWire(t, seed)
	if _, err := ReadAuthTransfer(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"magic", "partial-prefix", "header-limit", "partial-payload", "payload-hash", "trailing", "duplicate-header", "unknown-header"} {
		t.Run(kind, func(t *testing.T) {
			bad := append([]byte{}, raw...)
			switch kind {
			case "magic":
				bad[0] ^= 1
			case "partial-prefix":
				bad = bad[:3]
			case "header-limit":
				binary.BigEndian.PutUint32(bad[len(authTransferMagic):], authTransferHeaderLimit+1)
			case "partial-payload":
				bad = bad[:len(bad)-1]
			case "payload-hash":
				bad[len(bad)-1] ^= 1
			case "trailing":
				bad = append(bad, 'x')
			case "duplicate-header", "unknown-header":
				at := len(authTransferMagic) + 4
				n := int(binary.BigEndian.Uint32(bad[len(authTransferMagic):]))
				header := append([]byte{}, bad[at:at+n-1]...)
				field := `,"version":1}`
				if kind == "unknown-header" {
					field = `,"path":"/outside"}`
				}
				header = append(header, field...)
				payload := append([]byte{}, bad[at+n:]...)
				bad = append(bad[:at], header...)
				bad = append(bad, payload...)
				binary.BigEndian.PutUint32(bad[len(authTransferMagic):], uint32(len(header)))
			}
			if transfer, err := ReadAuthTransfer(bytes.NewReader(bad)); err == nil {
				transfer.Close()
				t.Fatal("hostile private frame accepted", kind)
			}
		})
	}
	if _, err := NewAuthReturn(transferInfo(t, seed), make([]byte, authTransferLimit+1)); err == nil {
		t.Fatal("oversized credential accepted")
	}
}

func TestAuthTransferGatesBeforeExportAndReturnExposure(t *testing.T) {
	store := authTestStore(t, Codex)
	profile, err := store.LeaseProfile(context.Background(), authTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	denied := authExportCheck(func(context.Context, AuthBinding) error { return errors.New("restricted channel not ready") })
	if _, err = profile.ExportAuth(context.Background(), denied); err != ErrAuthTransfer {
		t.Fatal("export gate ignored", err)
	}
	journal, err := readAuthJournal(profile.held.root)
	if err != nil || journal.TransferStage != "" {
		t.Fatal("denied export consumed seed", journal.TransferStage, err)
	}
	seed, err := profile.ExportAuth(context.Background(), authExportAllowed(t))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	returned, err := NewAuthReturn(transferInfo(t, seed), []byte("synthetic-final"))
	if err != nil {
		t.Fatal(err)
	}
	if err = profile.ImportAuthReturn(context.Background(), returned, authStopCheck(func(context.Context, AuthBinding) error { return errors.New("still live") })); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("unconfirmed stop imported auth", err)
	}
	journal, err = readAuthJournal(profile.held.root)
	if err != nil || journal.TransferStage != "exported" {
		t.Fatal("live return changed journal", journal.TransferStage, err)
	}
	if !returned.state.used || len(returned.state.data) != 0 {
		t.Fatal("rejected live return remained buffered")
	}
	state, _ := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if string(state) != "synthetic-seed" {
		t.Fatal("live return changed canonical credentials")
	}
}

func TestAuthTransferCrashRecoveryNeverRestoresAnUnreturnedSeed(t *testing.T) {
	for _, stage := range []string{"receiving-no-pending", "receiving-pending", "receiving-staged", "returned", "committing", "committed"} {
		t.Run(stage, func(t *testing.T) {
			store, profile, _ := authTransferFixture(t, Codex)
			payload := []byte("synthetic-final-after-refresh")
			journal := profile.journal
			journal.TransferStage = "receiving"
			journal.TransferHash = authHash(payload)
			if stage != "receiving-no-pending" {
				if err := writeAuth(profile.held.root, "pending.json", payload); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "receiving-staged" || stage == "returned" || stage == "committing" || stage == "committed" {
				if err := writeAuth(profile.held.root, store.authRelative(), payload); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "returned" || stage == "committing" || stage == "committed" {
				journal.TransferStage = "returned"
			}
			if stage == "committing" || stage == "committed" {
				journal.Stage = "committing"
				journal.NextHash = journal.TransferHash
			}
			if stage == "committed" {
				journal.Stage = "committed"
				if err := writeAuth(profile.held.root, "state.json", payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeAuthJournal(profile.held.root, journal); err != nil {
				t.Fatal(err)
			}
			profile.Close()
			recovered, err := store.RecoverProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if _, err = recovered.ExportAuth(context.Background(), authExportAllowed(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("recovery exported original seed", err)
			}
			if stage == "receiving-no-pending" {
				if err = recovered.ResumeAuthReturn(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
					t.Fatal("lost return was invented", err)
				}
				if err = recovered.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
					t.Fatal("old staged seed was recommitted", err)
				}
				if err = recovered.Retire(context.Background(), authStopped(t)); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(filepath.Join(store.directory, "state.json")); !os.IsNotExist(err) {
					t.Fatal("lost return retained old credentials")
				}
				return
			}
			if strings.HasPrefix(stage, "receiving-") || stage == "returned" {
				if err = recovered.ResumeAuthReturn(context.Background(), authStopped(t)); err != nil {
					t.Fatal(err)
				}
			}
			if err = recovered.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if err != nil || !bytes.Equal(state, payload) {
				t.Fatal("recovery did not commit exact returned auth", err)
			}
			for _, name := range []string{"lease.json", "pending.json", "profile"} {
				if _, err = os.Stat(filepath.Join(store.directory, name)); !os.IsNotExist(err) {
					t.Fatal("recovery retained disposable state", name)
				}
			}
		})
	}
}

func TestAuthTransferRejectsStaleCanonicalSeedAndChangedReturnedFile(t *testing.T) {
	store, profile, seed := authTransferFixture(t, Claude)
	returned, err := NewAuthReturn(transferInfo(t, seed), []byte("synthetic-return"))
	if err != nil {
		t.Fatal(err)
	}
	if err = profile.ImportAuthReturn(context.Background(), returned, authStopped(t)); err != nil {
		t.Fatal(err)
	}
	authMustWrite(t, profile.AuthFile(), "unexpected-replacement")
	if err = profile.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("changed staged file committed", err)
	}
	state, _ := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if string(state) != "synthetic-seed" {
		t.Fatal("unexpected staged bytes became canonical")
	}
	other := authTestStore(t, Codex)
	fresh, err := other.LeaseProfile(context.Background(), authTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	authMustWrite(t, filepath.Join(other.directory, "state.json"), "different-canonical-seed")
	if _, err = fresh.ExportAuth(context.Background(), authExportAllowed(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("stale seed exported", err)
	}
}

func TestAuthTransferReceivingRetryRequiresExactReturnAndFreshStop(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			store, profile, seed := authTransferFixture(t, provider)
			info := transferInfo(t, seed)
			payload := []byte("synthetic-refreshed-return")
			frame := func(data []byte) AuthTransfer {
				t.Helper()
				returned, err := NewAuthReturn(info, data)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := ReadAuthTransfer(bytes.NewReader(authWire(t, returned)))
				if err != nil {
					t.Fatal(err)
				}
				return decoded
			}
			// A directory at the pending-file target reproduces a real filesystem
			// failure after the receiving journal has become durable.
			pending := filepath.Join(store.directory, "pending.json")
			if err := os.Mkdir(pending, 0700); err != nil {
				t.Fatal(err)
			}
			if err := profile.ImportAuthReturn(context.Background(), frame(payload), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("missing expected pending-write failure", err)
			}
			journal, err := readAuthJournal(profile.held.root)
			if err != nil || journal.TransferStage != "receiving" || journal.TransferHash != authHash(payload) {
				t.Fatal("return hash was not journaled before failed write", err)
			}
			if err = os.Remove(pending); err != nil {
				t.Fatal(err)
			}
			profile.Close()
			recovered, err := store.RecoverProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if err = recovered.ImportAuthReturn(context.Background(), frame([]byte("different-return")), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("receiving retry replaced journal-bound hash", err)
			}
			if err = recovered.ImportAuthReturn(context.Background(), frame(payload), authStopCheck(func(context.Context, AuthBinding) error { return errors.New("not stopped") })); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("retry bypassed fresh stop", err)
			}
			if _, err = os.Stat(pending); !os.IsNotExist(err) {
				t.Fatal("rejected retry persisted bytes", err)
			}
			if err = recovered.ImportAuthReturn(context.Background(), frame(payload), authStopped(t)); err != nil {
				t.Fatal("exact stopped return could not recover failed receive", err)
			}
			if err = recovered.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if err != nil || !bytes.Equal(state, payload) {
				t.Fatal("retried return was not committed exactly", err)
			}
		})
	}
}
