package reviewer

import (
	"bytes"
	"encoding/json"
	"io"
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
	Task         string          `json:",omitempty"`
	Control      *ControlSession `json:",omitempty"`
	Version      int
	Binding      AuthBinding
	Session      Session
	Paths        []string
	Policy       EgressPolicy
	ProfileHints NativeVMProfileHints
}

func (q nativeVMRequest) valid() bool {
	if !q.validTask() || !q.Binding.valid() || q.Session.Validate() != nil || q.Binding.SessionHash != Digest(q.Session) || len(q.Paths) > 4096 || !sort.StringsAreSorted(q.Paths) || !q.ProfileHints.valid(q.Session.Identity.Provider) {
		return false
	}
	if _, err := validateEgressPolicy(q.Session.Identity.Provider, q.Session.Limits, q.Policy); err != nil {
		return false
	}
	if !nativeSourcePolicies(q.Session.Identity.Provider, q.Policy) {
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

type nativeVMProgress struct {
	Version     int
	Kind, Stage string
}

var nativeBootStages = []string{"entry", "release", "rootfs", "proc", "mounts", "cgroup", "runtime", "modules", "ports", "intake", "binding", "ready", "runtime-missing", "runtime-short", "runtime-metadata", "runtime-invalid", "runtime-full"}

func init() {
	for _, mount := range []string{"mount-dev", "mount-sys", "mount-run"} {
		nativeBootStages = append(nativeBootStages, mount)
		for _, failure := range []string{"interrupted", "busy", "denied", "missing", "invalid", "memory", "device", "other", "missing-present", "missing-absent", "missing-notdir", "missing-staterror"} {
			nativeBootStages = append(nativeBootStages, mount+"-"+failure)
		}
	}
}

func nativeBootStageValid(stage string) bool {
	for _, known := range nativeBootStages {
		if known == stage {
			return true
		}
	}
	return false
}

func nativeVMGuestStage(stage string) bool {
	if strings.HasPrefix(stage, "client-error-") {
		class := strings.TrimPrefix(stage, "client-error-")
		return class != "" && nativeDiagnosticClass(class)
	}
	if strings.HasPrefix(stage, "diag-") {
		suffix := strings.TrimPrefix(stage, "diag-")
		return nativeBootStageValid(suffix) || suffix == "oom" || suffix == "panic" || suffix == "vm-error" || suffix == "vm-exit" || suffix == "unpack-other" || suffix == "unpack-write" || suffix == "unpack-read" || suffix == "unpack-compression" || suffix == "unpack-format" || suffix == "unpack-memory"
	}
	switch stage {
	case "runtime-prepared", "boot-control", "boot-objects", "boot-provider", "boot-request":
		return true
	case "ports", "jails", "profile", "reader", "source-ready", "source-bind", "codehost", "client-proxy", "client-ready", "auth-seed", "auth-profile", "native-execute", "native-output", "complete":
		return true
	case "client-config", "client-loopback", "client-proc", "client-listener", "client-version", "client-probe-clean":
		return true
	}
	return false
}

func readNativeVMReady(input io.Reader, q nativeVMRequest, progress func(string)) (nativeVMReady, error) {
	for range 24 {
		raw, err := readVMRecord[json.RawMessage](input, 4096)
		if err != nil {
			return nativeVMReady{}, ErrVMBoot
		}
		var stage nativeVMProgress
		if DecodeErr := json.Unmarshal(raw, &stage); DecodeErr == nil && stage.Version == 1 && stage.Kind == "guest-stage" && nativeVMGuestStage(stage.Stage) {
			canonical, _ := json.Marshal(stage)
			if !bytes.Equal(canonical, raw) {
				return nativeVMReady{}, ErrVMBoot
			}
			if progress != nil {
				progress(stage.Stage)
			}
			continue
		}
		var ready nativeVMReady
		if json.Unmarshal(raw, &ready) != nil || !ready.valid(q) {
			return nativeVMReady{}, ErrVMBoot
		}
		canonical, _ := json.Marshal(ready)
		if !bytes.Equal(canonical, raw) {
			return nativeVMReady{}, ErrVMBoot
		}
		return ready, nil
	}
	return nativeVMReady{}, ErrVMBoot
}

func (r nativeVMReady) valid(q nativeVMRequest) bool {
	return q.valid() && r.Version == 1 && r.Binding == q.Binding && r.RequestHash == Digest(q) && r.Identity == q.Session.Identity && r.RestrictedProfile == "native-vm-profile-1"
}

// The guest's privileged supervisor writes this receipt over the control port.
// It never accepts this structure from client stdout or a source tool. The host
// must separately prove whole-VM stop and validate its own provider receipt.
// A record without a report omits Report. An explicit null is a carried value,
// so only a complete record can hold a report and none may hold null.
type nativeVMFinished struct {
	Version         int
	Binding         AuthBinding
	RequestHash     string
	State           string
	Report          json.RawMessage `json:",omitempty"`
	SourceAudit     []byte
	AuthReturn      []byte
	CodeHost        *nativeVMCodeHostAccess
	GuestStage      string
	Native          *nativeResultObservation
	ExecObservation *NativeVMExecObservation `json:",omitempty"`
}
type nativeVMCodeHostAccess struct {
	Version                 int
	SessionHash             string
	UID                     int
	InputBytes, OutputBytes int64
	InputHash, OutputHash   string
	Failed, Stopped         bool
	RelayComplete           bool
	Stage                   string
}

func nativeVMCodeStage(stage string) bool {
	switch stage {
	case "", "accept-data", "accept-control", "peer", "prelude", "spawn", "probe", "ready", "exchange", "wait", "complete":
		return true
	}
	return false
}

func (a *nativeVMCodeHostAccess) valid(s Session) bool {
	return a != nil && a.Version == 2 && a.SessionHash == Digest(s) && a.UID == nativeVMCodeHostUID && a.InputBytes >= 0 && a.InputBytes <= 8<<20 && a.OutputBytes >= 0 && a.OutputBytes <= 8<<20 && validAuthHash(a.InputHash) && validAuthHash(a.OutputHash) && !a.Failed && a.Stopped && a.RelayComplete && (a.Stage == "" || a.Stage == "complete")
}
func (f nativeVMFinished) valid(q nativeVMRequest) bool {
	if !q.valid() || f.Version != 1 || f.Binding != q.Binding || f.RequestHash != Digest(q) || (f.GuestStage != "" && !nativeVMGuestStage(f.GuestStage)) || len(f.Report) > q.Session.Limits.OutputBytes || len(f.SourceAudit) > 2<<20 || len(f.AuthReturn) > authTransferLimit+authTransferHeaderLimit+len(authTransferMagic)+4 {
		return false
	}
	switch f.State {
	case "complete", "auth_failed", "provider_unavailable", "invalid_output", "cancelled", "blocked", "quota_wait", "incomplete_expired":
	default:
		return false
	}
	if f.State != "complete" && len(f.Report) != 0 || string(f.Report) == "null" {
		return false
	}
	if q.Session.Identity.Provider == Claude && f.CodeHost != nil {
		return false
	}
	if f.ExecObservation != nil {
		if q.Session.Identity.Provider != Codex || !f.ExecObservation.valid(q.Binding, Digest(q)) {
			return false
		}
		if f.State == "complete" && (f.ExecObservation.Outcome(q.Binding, Digest(q)) == "unknown" || f.ExecObservation.Outcome(q.Binding, Digest(q)) == "unsafe") {
			return false
		}
	}
	if f.CodeHost != nil && !nativeVMCodeStage(f.CodeHost.Stage) {
		return false
	}
	if f.Native != nil && !f.Native.valid(q.Session) {
		return false
	}
	if f.State == "complete" && f.Native != nil && (f.Native.Kind != "complete" || f.Native.ExitFailed) {
		return false
	}
	if f.State == "complete" && (q.Session.Identity.Provider == Codex && !f.CodeHost.valid(q.Session) || len(f.Report) == 0 || len(f.AuthReturn) == 0 || len(f.SourceAudit) == 0) {
		return false
	}
	return true
}

// failure classifies a decoded finished record on the host. A valid typed
// guest failure keeps its state; an invalid record is invalid output.
func (f nativeVMFinished) failure(q nativeVMRequest) error {
	if !f.valid(q) {
		return adapterValidation(ErrVMBoot, false)
	}
	if f.State != "complete" {
		return PhaseError{State: f.State}
	}
	return nil
}

func nativeVMSeedRecord(seed AuthTransfer) ([]byte, error) {
	var wire bytes.Buffer
	if _, err := seed.WriteTo(&wire); err != nil {
		clear(wire.Bytes())
		return nil, ErrAuthTransfer
	}
	return wire.Bytes(), nil
}

// Version 1 review bytes stay unchanged. Control is an explicit version 2 task;
// an old guest cannot silently interpret it as a production review.
func (q nativeVMRequest) validTask() bool {
	if q.Control == nil {
		return q.Version == 1 && q.Task == ""
	}
	if q.Version != 2 || q.Task != "evaluation-control-v1" {
		return false
	}
	capability, err := controlCapability(*q.Control)
	return err == nil && Digest(capability) == Digest(q.Session)
}
func (q nativeVMRequest) reportSchema() []byte {
	if q.Control != nil {
		return ControlReportSchema()
	}
	return ReportSchema()
}
func (q nativeVMRequest) validateReport(raw []byte) error {
	if q.Control != nil {
		r, err := DecodeStrict[ControlReport](raw, q.Session.Limits.OutputBytes)
		if err != nil {
			return err
		}
		return ValidateControlReport(r, *q.Control)
	}
	r, err := DecodeStrict[Report](raw, q.Session.Limits.OutputBytes)
	if err != nil {
		return err
	}
	return ValidateReport(r, q.Session)
}
