package reviewer

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

const nativeVMReaderUID = 1201
const nativeVMCodexUID = 1202
const nativeVMCodeHostUID = 1203
const nativeVMClaudeUID = 1204
const nativeVMIPCGID = 1301
const nativeVMSourceRoot = "/run/sfl"

type NativeVMProfileHints struct {
	ClaudeAccountUUID      string
	ClaudeOrganizationUUID string
	ClaudeEmail            string
}

func (h NativeVMProfileHints) valid(provider Provider) bool {
	if provider == Codex {
		return h == (NativeVMProfileHints{})
	}
	for _, v := range []string{h.ClaudeAccountUUID, h.ClaudeOrganizationUUID, h.ClaudeEmail} {
		if len(v) > 254 || strings.ContainsAny(v, "\x00\r\n\t") {
			return false
		}
	}
	return true
}

// nativeVMRequest is private supervisor input. Candidate/model text is confined
// to Session and source objects; it cannot supply paths, argv or credentials.
// Profile hints are separately trusted qualification metadata, never App or
// provider credentials. They do not establish subscription eligibility.
type nativeVMRequest struct {
	Version      int
	Binding      AuthBinding
	Session      Session
	Paths        []string
	Policy       EgressPolicy
	ProfileHints NativeVMProfileHints
}

func (q nativeVMRequest) valid() bool {
	if q.Version != 1 || !q.Binding.valid() || q.Session.Validate() != nil || q.Binding.SessionHash != Digest(q.Session) || len(q.Paths) > 4096 || !sort.StringsAreSorted(q.Paths) || !q.ProfileHints.valid(q.Session.Identity.Provider) {
		return false
	}
	if _, err := validateEgressPolicy(q.Session.Identity.Provider, q.Session.Limits, q.Policy); err != nil {
		return false
	}
	if (q.Session.Identity.Provider == Codex && q.Policy.ResponsePolicy != CodexSourceResponses) || (q.Session.Identity.Provider == Claude && q.Policy.ResponsePolicy != ClaudeSourceMessages) {
		return false
	}
	previous := ""
	for _, path := range q.Paths {
		if !validPath(path) || path == previous {
			return false
		}
		previous = path
	}
	return true
}
func (q nativeVMRequest) readerConfig() SourceReaderConfig {
	uid := nativeVMCodexUID
	if q.Session.Identity.Provider == Claude {
		uid = nativeVMClaudeUID
	}
	return SourceReaderConfig{LaunchID: q.Binding.LaunchID, Session: q.Session, Paths: append([]string{}, q.Paths...), Root: nativeVMSourceRoot, ReaderUID: nativeVMReaderUID, WorkerUID: uid, WorkerGID: nativeVMIPCGID}
}

type nativeVMReady struct {
	Version           int
	Binding           AuthBinding
	RequestHash       string
	Identity          Identity
	RestrictedProfile string
}

func (r nativeVMReady) valid(q nativeVMRequest) bool {
	return q.valid() && r.Version == 1 && r.Binding == q.Binding && r.RequestHash == Digest(q) && r.Identity == q.Session.Identity && r.RestrictedProfile == "native-vm-profile-1"
}

// The guest's privileged supervisor writes this receipt over the control port.
// It never accepts this structure from client stdout or a source tool. The host
// must separately prove whole-VM stop and validate its own provider receipt.
type nativeVMFinished struct {
	Version     int
	Binding     AuthBinding
	RequestHash string
	State       string
	Report      json.RawMessage
	SourceAudit []byte
	AuthReturn  []byte
	CodeHost    *nativeVMCodeHostAccess
}
type nativeVMCodeHostAccess struct {
	Version                 int
	SessionHash             string
	UID                     int
	InputBytes, OutputBytes int64
	InputHash, OutputHash   string
	Failed, Stopped         bool
}

func (a *nativeVMCodeHostAccess) valid(s Session) bool {
	return a != nil && a.Version == 1 && a.SessionHash == Digest(s) && a.UID == nativeVMCodeHostUID && a.InputBytes >= 0 && a.InputBytes <= 8<<20 && a.OutputBytes >= 0 && a.OutputBytes <= 8<<20 && validAuthHash(a.InputHash) && validAuthHash(a.OutputHash) && !a.Failed && a.Stopped
}
func (f nativeVMFinished) valid(q nativeVMRequest) bool {
	if !q.valid() || f.Version != 1 || f.Binding != q.Binding || f.RequestHash != Digest(q) || len(f.Report) > q.Session.Limits.OutputBytes || len(f.SourceAudit) > 2<<20 || len(f.AuthReturn) > authTransferLimit+authTransferHeaderLimit+len(authTransferMagic)+4 {
		return false
	}
	switch f.State {
	case "complete", "auth_failed", "provider_unavailable", "invalid_output", "cancelled", "blocked", "quota_wait", "incomplete_expired":
	default:
		return false
	}
	if f.State != "complete" && len(f.Report) != 0 {
		return false
	}
	if q.Session.Identity.Provider == Claude && f.CodeHost != nil {
		return false
	}
	if f.State == "complete" && (q.Session.Identity.Provider == Codex && !f.CodeHost.valid(q.Session) || len(f.Report) == 0 || len(f.AuthReturn) == 0 || len(f.SourceAudit) == 0) {
		return false
	}
	return true
}

func nativeVMSeedRecord(seed AuthTransfer) ([]byte, error) {
	var wire bytes.Buffer
	if _, err := seed.WriteTo(&wire); err != nil {
		clear(wire.Bytes())
		return nil, ErrAuthTransfer
	}
	return wire.Bytes(), nil
}
