//go:build linux

package reviewer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func nativeRequestFixture(t *testing.T, p Provider) nativeVMRequest {
	t.Helper()
	s := testSession(p)
	binding := authTestBinding()
	binding.SessionHash = Digest(s)
	policy := egressTestPolicy(p)
	if p == Codex {
		policy.ResponsePolicy = CodexSourceResponses
	} else {
		policy.ResponsePolicy = ClaudeSourceMessages
	}
	q := nativeVMRequest{Version: 1, Binding: binding, Session: s, Paths: []string{"source.go"}, Policy: policy}
	if !q.valid() {
		t.Fatal("invalid private request fixture")
	}
	return q
}

func TestNativeVMPrivateControlActualUnixProducerBinding(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			q := nativeRequestFixture(t, provider)
			client, server := vmSocketPair(t)
			done := make(chan error, 1)
			go func() {
				received, err := readVMRecord[nativeVMRequest](server, 1<<20)
				if err != nil || !received.valid() || Digest(received) != Digest(q) {
					done <- ErrVMBoot
					return
				}
				ready := nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}
				done <- writeVMRecord(server, ready, 4096)
			}()
			if err := writeVMRecord(client, q, 1<<20); err != nil {
				t.Fatal(err)
			}
			ready, err := readVMRecord[nativeVMReady](client, 4096)
			if err != nil || !ready.valid(q) {
				t.Fatal("ready did not bind captured private request", err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			config := q.readerConfig()
			wantWorker := nativeVMCodexUID
			if provider == Claude {
				wantWorker = nativeVMClaudeUID
			}
			if config.validate() != nil || config.ReaderUID != nativeVMReaderUID || config.WorkerUID != wantWorker || config.WorkerGID != nativeVMIPCGID || config.Root != nativeVMSourceRoot {
				t.Fatal("private role mapping differs", config.ReaderUID, config.WorkerUID)
			}
		})
	}
}

func TestNativeVMControlRejectsScopeIdentityAndResponseFallbacks(t *testing.T) {
	for _, kind := range []string{"binding", "path", "duplicates", "policy", "model", "codex-hints"} {
		t.Run(kind, func(t *testing.T) {
			q := nativeRequestFixture(t, Codex)
			switch kind {
			case "binding":
				q.Binding.SessionHash = Digest("foreign")
			case "path":
				q.Paths = []string{"../outside"}
			case "duplicates":
				q.Paths = []string{"source.go", "source.go"}
			case "policy":
				q.Policy.ResponsePolicy = ""
			case "model":
				q.Session.Identity.Model = "other-model"
			case "codex-hints":
				q.ProfileHints.ClaudeEmail = "synthetic@example.invalid"
			}
			if q.valid() {
				t.Fatal("private scope/fallback accepted", kind)
			}
		})
	}
	q := nativeRequestFixture(t, Codex)
	ready := nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}
	for _, kind := range []string{"version", "binding", "hash", "identity", "profile"} {
		bad := ready
		switch kind {
		case "version":
			bad.Version++
		case "binding":
			bad.Binding.LaunchID = strings.Repeat("a", 32)
		case "hash":
			bad.RequestHash = Digest("foreign")
		case "identity":
			bad.Identity.AuthMode = "api_key"
		case "profile":
			bad.RestrictedProfile = "synthetic-role-fixture"
		}
		if bad.valid(q) {
			t.Fatal("unqualified ready accepted", kind)
		}
	}
}

func TestNativeVMFinishedRequiresPrivateAuditsAndAuthReturn(t *testing.T) {
	q := nativeRequestFixture(t, Codex)
	report, _ := json.Marshal(fakeReport(q.Session))
	f := nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: "complete", Report: report, SourceAudit: []byte("private-audit-placeholder"), AuthReturn: []byte("private-return-placeholder"), CodeHost: &nativeVMCodeHostAccess{Version: 1, SessionHash: Digest(q.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true}}
	// Structural validation does not parse/attest these placeholder artifacts.
	// The native host reviewer must validate their actual producer records later.
	if !f.valid(q) {
		t.Fatal("structural envelope unexpectedly rejected")
	}
	for _, kind := range []string{"source", "return", "codehost", "wrong-uid", "failed-host", "oversized", "state"} {
		bad := f
		copy := *f.CodeHost
		bad.CodeHost = &copy
		switch kind {
		case "source":
			bad.SourceAudit = nil
		case "return":
			bad.AuthReturn = nil
		case "codehost":
			bad.CodeHost = nil
		case "wrong-uid":
			bad.CodeHost.UID = nativeVMCodexUID
		case "failed-host":
			bad.CodeHost.Failed = true
		case "oversized":
			bad.Report = make([]byte, q.Session.Limits.OutputBytes+1)
		case "state":
			bad.State = "approve"
		}
		if bad.valid(q) {
			t.Fatal("incomplete/forged result envelope accepted", kind)
		}
	}
	failed := f
	failed.State = "invalid_output"
	if failed.valid(q) {
		t.Fatal("failed result retained substantive findings")
	}
}

func TestNativeVMSeedRecordUsesActualOneUseAuthProducer(t *testing.T) {
	_, _, seed := authTransferFixture(t, Codex)
	wire, err := nativeVMSeedRecord(seed)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(wire)
	decoded, err := ReadAuthTransfer(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	info, err := decoded.Info()
	if err != nil || info.Binding != authTestBinding() || info.Provider != Codex || info.Kind != "seed" {
		t.Fatal("private seed producer binding lost", info, err)
	}
	if _, err = nativeVMSeedRecord(seed); err != ErrAuthTransfer {
		t.Fatal("private seed sent twice", err)
	}
}

func TestNativeVMGuestProgressCannotReplaceRestrictedReady(t *testing.T) {
	q := nativeRequestFixture(t, Codex)
	for _, kind := range []string{"complete", "progress-only", "unknown-stage", "flood", "foreign-ready"} {
		t.Run(kind, func(t *testing.T) {
			var wire bytes.Buffer
			stage := "reader"
			count := 1
			if kind == "unknown-stage" {
				stage = "candidate-auth-path"
			}
			if kind == "flood" {
				count = 16
			}
			for range count {
				if err := writeVMRecord(&wire, nativeVMProgress{1, "guest-stage", stage}, 4096); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "progress-only" {
				ready := nativeVMReady{1, q.Binding, Digest(q), q.Session.Identity, "native-vm-profile-1"}
				if kind == "foreign-ready" {
					ready.RequestHash = Digest("foreign")
				}
				if err := writeVMRecord(&wire, ready, 4096); err != nil {
					t.Fatal(err)
				}
			}
			seen := []string{}
			ready, err := readNativeVMReady(&wire, q, func(stage string) { seen = append(seen, stage) })
			if kind == "complete" {
				if err != nil || !ready.valid(q) || len(seen) != 1 || seen[0] != "reader" {
					t.Fatal("private ready after progress rejected", err)
				}
			} else if err != ErrVMBoot || ready.valid(q) {
				t.Fatal("progress or invalid ready authorized auth", kind, err)
			}
			if kind == "unknown-stage" && len(seen) != 0 {
				t.Fatal("candidate stage was retained")
			}
		})
	}
}
