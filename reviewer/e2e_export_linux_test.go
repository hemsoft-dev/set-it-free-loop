//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// This file exposes read-only test seams to the external aggregate
// qualification in e2e_qualification_linux_test.go. That test lives in package
// reviewer_test so it can import the fake-only native backend producers, which
// themselves import this package. Nothing here starts, stops or mutates work.

// E2ESourceVMRelease derives the version 2 source VM package pins from a
// measured source image and an exact-head host stub. It returns the image's
// recorded source commit so the caller can bind the release revision to it.
// The image's guest release must name that commit as its source head, and the
// stub must carry Go VCS stamping for that commit from an unmodified tree.
func E2ESourceVMRelease(t *testing.T, image, stub string) (GitReviewerSourceVMRelease, string) {
	t.Helper()
	spec, _, guest := gitVMActualImageFixture(t, image)
	raw, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal("source stub", err)
	}
	// Parse the hashed bytes, not a second read of the path.
	info, err := buildinfo.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal("source stub build info", err)
	}
	vm := GitReviewerSourceVMRelease{Stub: VMBootAsset{stub, gitRawHash(raw), int64(len(raw))}, QEMU: spec.QEMU, Kernel: spec.Kernel, Initrd: spec.Initrd, BIOS: spec.BIOS, QEMURuntime: spec.QEMURuntime, NamespaceHelper: spec.NamespaceHelper, BootstrapLibraries: spec.BootstrapLibraries, Worker: spec.Manifest.Worker, Runtime: spec.Manifest.Runtime}
	derived, err := vm.assetSpec()
	if err != nil || Digest(derived.Manifest) != Digest(spec.Manifest) {
		t.Fatal("source VM release does not derive the measured image manifest", err)
	}
	manifest, err := os.ReadFile(filepath.Join(image, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e2eRecordedCommit(manifest, guest)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2eStubRevision(info, commit); err != nil {
		t.Fatal(err)
	}
	return vm, commit
}

// e2eRecordedCommit returns the source image's clean recorded commit. The
// image's embedded guest release must name the same source head: the derived
// manifest pins the guest assets but not their head, so a guest built from
// another commit could otherwise run under this commit's release revision.
func e2eRecordedCommit(manifest []byte, guest gitVMGuestRelease) (string, error) {
	var recorded struct {
		Commit string `json:"source_commit"`
		Dirty  bool   `json:"source_dirty"`
	}
	if json.Unmarshal(manifest, &recorded) != nil || !objectID.MatchString(recorded.Commit) || recorded.Dirty {
		return "", errors.New("source image lacks a clean recorded commit")
	}
	if guest.SourceHead != recorded.Commit {
		return "", fmt.Errorf("source image guest release head %q differs from its recorded commit %s", guest.SourceHead, recorded.Commit)
	}
	return recorded.Commit, nil
}

const e2eStubPackage = "github.com/hemsoft-dev/set-it-free-loop/reviewer/cmd/gitsourcevmstub"

// e2eTestPackage is the program path Go records for this package's test binary.
const e2eTestPackage = "github.com/hemsoft-dev/set-it-free-loop/reviewer.test"

// E2EBuildStamp is the Go VCS stamp recorded in a qualified binary.
type E2EBuildStamp struct {
	Path, GoVersion, VCS, Revision, Modified string
}

// e2eBuildStamp requires info to describe program path, stamped by Go as a
// build of commit from an unmodified Git tree. A repeated setting is refused.
// Go does not stamp builds in a linked worktree, so such a binary is refused
// rather than trusted by hash alone.
func e2eBuildStamp(label string, info *debug.BuildInfo, path, commit string) (E2EBuildStamp, error) {
	stamp := E2EBuildStamp{Path: info.Path, GoVersion: info.GoVersion}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		if _, duplicate := settings[setting.Key]; duplicate {
			return stamp, errors.New(label + " repeats build setting " + setting.Key)
		}
		settings[setting.Key] = setting.Value
	}
	stamp.VCS, stamp.Revision, stamp.Modified = settings["vcs"], settings["vcs.revision"], settings["vcs.modified"]
	if info.Path != path || stamp.VCS != "git" || stamp.Revision != commit || stamp.Modified != "false" {
		return stamp, fmt.Errorf("%s %s is not stamped as an unmodified build of %s (vcs %q revision %q modified %q)", label, info.Path, commit, stamp.VCS, stamp.Revision, stamp.Modified)
	}
	return stamp, nil
}

// e2eStubRevision requires the source stub's stamp to name commit.
func e2eStubRevision(info *debug.BuildInfo, commit string) error {
	_, err := e2eBuildStamp("source stub", info, e2eStubPackage, commit)
	return err
}

// E2ETestBinaryStamp attests the running test binary: it must be this
// package's test program, stamped as an unmodified build of the release
// commit. The returned stamp is recorded in the evidence either way.
func E2ETestBinaryStamp(commit string) (E2EBuildStamp, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return E2EBuildStamp{}, errors.New("test binary carries no build info")
	}
	return e2eTestBinaryStamp(info, commit)
}

func e2eTestBinaryStamp(info *debug.BuildInfo, commit string) (E2EBuildStamp, error) {
	return e2eBuildStamp("test binary", info, e2eTestPackage, commit)
}

// E2EValidateProviderReceipt applies the production provider receipt checks
// to a native audit's evidence for one session hash under the session limits.
// Every receipt must hold the stopped-receipt invariants recovery requires
// before recording a stopped attempt; a completed session's receipt must also
// pass the completed-receipt validation an artifact requires.
func E2EValidateProviderReceipt(evidence *ProviderEvidence, provider Provider, limits Limits, sessionHash string, completed bool) error {
	if err := evidence.validateStoppedCaptured(provider, limits, sessionHash); err != nil {
		return err
	}
	if completed {
		return evidence.validateCompletedCaptured(provider, limits, sessionHash)
	}
	return nil
}

// E2ENativeManifestHash is the ManifestHash a native launch's unit journal
// and audit must carry for binding under provider release p: the digest of
// the NativeVMManifest that VMBootImages.NativeManifest derives from the
// release's pinned QEMU, kernel, initrd, firmware, runtime artifact,
// namespace helper and bootstrap libraries for that launch and session.
func E2ENativeManifestHash(p GitReviewerProviderRelease, provider Provider, binding AuthBinding) (string, error) {
	libraries, err := nativeBootstrapLibraryPins(p.Runtime.BootstrapLibraries)
	if err != nil {
		return "", err
	}
	m := NativeVMManifest{Version: 3, Purpose: "native-review", Binding: binding, SessionHash: binding.SessionHash, Provider: provider, BootstrapLibraries: libraries}
	for _, a := range []VMBootAsset{p.QEMU, p.Kernel, p.Initrd, p.Firmware, p.Runtime.Artifact, p.Runtime.NamespaceHelper} {
		m.Assets = append(m.Assets, VMAssetPin{a.SHA256, a.Bytes})
	}
	if !m.valid() {
		return "", ErrVMBoot
	}
	return Digest(m), nil
}

// E2ENativeUnitJournal is production's native unit journal record
// (unit.json), field for field.
type E2ENativeUnitJournal = unitJournal

// E2EReadNativeUnitJournal reads directory's unit.json exactly as the native
// supervisor reads its own journal (readUnit): a private, owned, singly linked
// file of at most 64 KiB, upgraded from the legacy shape, strictly decoded,
// version 1 of the native-vm payload under stubHash, naming the unit of a
// valid binding, with a manager owner and boot ID, and with a possible
// lifecycle: a settled start was started, a capture was started, settled and
// bound to a process and the unit's own cgroup, and a stop was aborted. The
// binding checked is the one the record names; the caller binds it to the
// launch directory.
func E2EReadNativeUnitJournal(directory, stubHash string) (E2ENativeUnitJournal, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return E2ENativeUnitJournal{}, err
	}
	defer root.Close()
	raw, err := readAuth(root, "unit.json", false)
	if err != nil {
		return E2ENativeUnitJournal{}, err
	}
	var named struct{ Binding AuthBinding }
	if err := json.Unmarshal(raw, &named); err != nil {
		return E2ENativeUnitJournal{}, err
	}
	s := &SystemdSupervisor{stubHash: stubHash, payloadKind: "native-vm"}
	return s.readUnit(&authLock{root: root}, named.Binding)
}

// E2ENativeAudit is the version 2 native-audit.json receipt persistAudit
// writes (nativeVMAuditReceipt), field for field and in its order.
type E2ENativeAudit struct {
	Version                                           int
	Binding                                           AuthBinding
	AttemptID                                         string
	RequestHash, ManifestHash, ReportHash, SourceHash string
	Provider                                          *ProviderEvidence
	CodeHost                                          *nativeVMCodeHostAccess
	Complete                                          bool
	Stage                                             string
	AuthExported, AuthReturned                        bool
	GuestStage                                        string
	Native                                            *nativeResultObservation
	Failure                                           string
	BootDiagnostics                                   []string
}

// E2EReadNativeAudit reads directory's native-audit.json as production
// writes it: a private, owned, singly linked file of at most 64 KiB holding
// exactly one receipt with no unknown field, whose re-encoding is the file's
// bytes. A missing, extra, duplicated or reordered field is refused.
func E2EReadNativeAudit(directory string) (E2ENativeAudit, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return E2ENativeAudit{}, err
	}
	defer root.Close()
	raw, err := readAuth(root, "native-audit.json", false)
	if err != nil {
		return E2ENativeAudit{}, err
	}
	var a E2ENativeAudit
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil || d.Decode(new(any)) == nil {
		return E2ENativeAudit{}, errors.New("native audit is not one known receipt")
	}
	canonical, err := json.Marshal(a)
	if err != nil || !bytes.Equal(canonical, raw) {
		return E2ENativeAudit{}, errors.New("native audit is not production's encoding of its receipt")
	}
	return a, nil
}

// E2ENativeRequestHash is the RequestHash a native audit records for the
// launch of binding: the digest of the version 1 review request
// reviewTaskAudited builds for session under the job's source paths (sorted),
// the release's egress policy, with its slices copied as NewNativeVMReviewer
// copies them, and the provider's profile hints. An invalid request has none.
func E2ENativeRequestHash(binding AuthBinding, session Session, paths []string, policy EgressPolicy, hints NativeVMProfileHints) (string, error) {
	policy.Routes = append([]EgressRoute{}, policy.Routes...)
	policy.Headers = append([]string{}, policy.Headers...)
	q := nativeVMRequest{Version: 1, Binding: binding, Session: session, Paths: append([]string{}, paths...), Policy: policy, ProfileHints: hints}
	if !q.valid() {
		return "", ErrLiveBlocked
	}
	return Digest(q), nil
}

// E2ECodeHostFixture is the code host relay receipt finished.valid requires
// of a completed Codex phase of session, for harness verdict fixtures.
func E2ECodeHostFixture(session Session) *nativeVMCodeHostAccess {
	return &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(session), UID: nativeVMCodeHostUID, InputHash: Digest("code host input"), OutputHash: Digest("code host output"), Stopped: true, RelayComplete: true, Stage: "complete"}
}

// E2EValidateNativeAudit binds audit a to the launch request reviewTaskAudited
// builds for session (E2ENativeRequestHash). Its guest-reported parts must be what
// persistAudit records: a valid guest stage or none, a valid native result
// observation or none, at most eight diag- boot diagnostics, and a failure
// kind nativeVMFailureKind derives exactly when the phase did not complete. A
// completed phase passed finished.valid: Codex relayed a complete, stopped
// code host session of this session, Claude none, and a native result, if
// observed, completed without an exit failure.
func E2EValidateNativeAudit(a E2ENativeAudit, session Session, paths []string, policy EgressPolicy, hints NativeVMProfileHints) error {
	if request, err := E2ENativeRequestHash(a.Binding, session, paths, policy, hints); err != nil || a.RequestHash != request {
		return errors.New("audit request is not the launch's request")
	}
	if a.GuestStage != "" && !nativeVMGuestStage(a.GuestStage) || a.Native != nil && !a.Native.valid(session) || len(a.BootDiagnostics) > 8 {
		return errors.New("audit guest stage, native observation or diagnostics are not production's")
	}
	for _, stage := range a.BootDiagnostics {
		if !strings.HasPrefix(stage, "diag-") || !nativeVMGuestStage(stage) {
			return errors.New("audit boot diagnostic is not production's")
		}
	}
	if a.Complete {
		codeHost := a.CodeHost == nil
		if session.Identity.Provider == Codex {
			codeHost = a.CodeHost.valid(session)
		}
		if a.Failure != "" || !codeHost || a.Native != nil && (a.Native.Kind != "complete" || a.Native.ExitFailed) {
			return errors.New("completed audit is not a validated phase's")
		}
		return nil
	}
	for _, cause := range []error{ErrWorkerStopPending, ErrAuthRecoveryRequired, ErrSourceAuditPending, ErrSourceService, ErrProviderAuditPending, ErrProviderEgressPending, ErrProviderEgressFailed, context.DeadlineExceeded, context.Canceled, errors.New("other")} {
		if a.Failure == nativeVMFailureKind(cause) {
			return nil
		}
	}
	return errors.New("interrupted audit records no production failure kind")
}

// E2EProtocolLimits is the session limits production's engine derives for
// job (protocolLimits).
func E2EProtocolLimits(job Job) (Limits, error) { return protocolLimits(job) }

// E2EValidateSourceAccess applies production's source receipt validation for
// session (SourceAccess.validate) to a receipt that must exist: version,
// session, tool and reader call bounds, byte bounds and running total, every
// read's path, revision, hash and size, and the coverage bound and records.
func E2EValidateSourceAccess(access *SourceAccess, session Session) error {
	if access == nil {
		return errors.New("no source receipt")
	}
	return access.validate(session)
}

// E2EValidateArtifactProviderEvidence applies the check production runs on
// every stored artifact before recovering or publishing a completed job
// (validateArtifactProviderEvidence): the artifact's completed provider
// receipt under the job's protocol limits, recorded by a provider attempt.
func E2EValidateArtifactProviderEvidence(j *DurableJob, a Artifact) error {
	return validateArtifactProviderEvidence(j, a)
}

// E2ESourceRuntimeHash is the RuntimeHash every source VM materialization
// binding of release vm carries: the digest of the source VM manifest the
// release derives, as startup derives it.
func E2ESourceRuntimeHash(vm GitReviewerSourceVMRelease) (string, error) {
	spec, err := vm.assetSpec()
	if err != nil {
		return "", err
	}
	return Digest(spec.Manifest), nil
}

// E2ERemoteReview applies production's remote receipt projection to a
// review's body and its inline comments in comment order: the body's sole
// publication marker ("" unless exactly one) and the digest of the canonical
// document, which a confirmed intent's BodyHash must equal.
func E2ERemoteReview(body string, inline []InlineComment) (marker, documentHash string) {
	return documentMarker(body), Digest(canonicalPublicationDocument(Document{Summary: body, Inline: inline}))
}

// E2ECompletedDocument renders the canonical review document production
// publishes for job completed with exactly reports, one artifact per report
// in phase order with no engine-added coverage, and the given ledger, under
// the trusted diff lines: production's renderer, the intent's marker appended
// as newIntent appends it, in canonical comment order. The caller supplies
// the ledger it expects, so an aggregation change is not absorbed here. It
// reads no durable state.
func E2ECompletedDocument(job Job, reports []Report, ledger []Entry, diff map[string]map[int]bool, marker string) (Document, error) {
	out := Outcome{Job: job, State: "dual_complete", Ledger: ledger}
	if len(job.Config.Providers()) == 1 {
		out.State = "single_complete"
	}
	for _, report := range reports {
		out.Artifacts = append(out.Artifacts, Artifact{Report: report, ValidationReads: []ReadRecord{}, Coverage: []Coverage{}})
	}
	doc, err := renderOutcome(out, diff)
	if err != nil {
		return Document{}, err
	}
	doc.Summary += "\n\n" + marker
	return canonicalPublicationDocument(doc), nil
}

// E2EStatusDocument renders the canonical status document production
// publishes for job in non-complete state after completed validated phases:
// production's renderer, the intent's marker appended as newIntent appends
// it, with no inline comment. It reads no durable state.
func E2EStatusDocument(job Job, state string, completed int, marker string) (Document, error) {
	if !statusStates[state] || completed < 0 {
		return Document{}, ErrIgnored
	}
	doc, err := renderOutcome(Outcome{Job: job, State: state, Artifacts: make([]Artifact, completed)}, nil)
	if err != nil {
		return Document{}, err
	}
	doc.Summary += "\n\n" + marker
	return canonicalPublicationDocument(doc), nil
}

// E2ESourceRecord is a nonsecret projection of one source VM ownership record.
// RequestHash binds the whole unit request; SourceRequestHash is the digest of
// its source request, which an archive entry repeats. Attempt, ReleaseHash,
// RuntimeHash and Snapshot are the complete materialization binding.
type E2ESourceRecord struct {
	Attempt, Unit, InvocationID, RequestHash string
	SourceRequestHash                        string
	// StubHash is the source stub image the owner recorded for the unit.
	StubHash                            string
	ReleaseHash, RuntimeHash            string
	ChannelID                           string
	Generation                          uint64
	Snapshot                            Snapshot
	StartIntent, StartSettled, Captured bool
	Revoked, Stopped                    bool
	GrantHash, StopBoot                 string
	// BootID is the boot the owner recorded the unit's request in.
	BootID string
}

// E2EExportRecord projects one source VM archive record: its source request
// hash and complete materialization binding, and for a staged export the
// packet hash restore requires to be the digest of the restored job's packet.
type E2EExportRecord struct {
	Attempt, State, RequestHash string
	ReleaseHash, RuntimeHash    string
	Snapshot                    Snapshot
	PacketHash                  string
}

func (s *GitReviewerService) e2eFactory() *gitVMSourceFactory {
	if s == nil || s.inner == nil {
		return nil
	}
	factory, _ := s.inner.source.(*gitVMSourceFactory)
	return factory
}

// E2ESourceJournal reads only the owner journal, under its operation token,
// which every journal write holds only for that write. It reports an error
// once the service has closed the journal.
func (s *GitReviewerService) E2ESourceJournal() ([]E2ESourceRecord, error) {
	factory := s.e2eFactory()
	if factory == nil {
		return nil, ErrLiveBlocked
	}
	journal, err := factory.owner.journal.read()
	if err != nil {
		return nil, err
	}
	records := []E2ESourceRecord{}
	for _, r := range journal.Records {
		b := r.Request.Source.Binding
		records = append(records, E2ESourceRecord{Attempt: b.Attempt, Unit: r.Unit, InvocationID: r.Birth.InvocationID, RequestHash: r.RequestHash, SourceRequestHash: Digest(r.Request.Source), StubHash: r.StubHash, ReleaseHash: b.ReleaseHash, RuntimeHash: b.RuntimeHash, ChannelID: r.Request.Read.ChannelID, Generation: r.Request.Read.Generation, Snapshot: b.Snapshot, StartIntent: r.StartIntent, StartSettled: r.StartSettled, Captured: r.Captured, Revoked: r.Revoked, Stopped: r.Stopped, GrantHash: r.GrantHash, StopBoot: r.StopBoot, BootID: r.BootID})
	}
	return records, nil
}

// E2ESourceRecords reads the owner journal and archive under their own
// operation tokens. It reports an error once the service has closed them.
func (s *GitReviewerService) E2ESourceRecords(ctx context.Context) ([]E2ESourceRecord, []E2EExportRecord, error) {
	factory := s.e2eFactory()
	if factory == nil {
		return nil, nil, ErrLiveBlocked
	}
	records, err := s.E2ESourceJournal()
	if err != nil {
		return nil, nil, err
	}
	archived, packets, err := factory.archive.records(ctx)
	if err != nil {
		return nil, nil, err
	}
	exports := []E2EExportRecord{}
	for _, r := range archived {
		exports = append(exports, E2EExportRecord{Attempt: r.Binding.Attempt, State: r.State, RequestHash: r.RequestHash, ReleaseHash: r.Binding.ReleaseHash, RuntimeHash: r.Binding.RuntimeHash, Snapshot: r.Binding.Snapshot, PacketHash: packets[r.Binding.Attempt]})
	}
	return records, exports, nil
}

// E2EProbe is one isolation read against a restored job's retained source.
// Store is the raw materialized object store; Reader is the model-facing
// reader bound to the job's snapshot and allowed paths. StoreHash and
// ReaderHash are the SHA-256 of the bytes each returned.
type E2EProbe struct {
	Revision, Path       string
	StoreOK, ReaderOK    bool
	StoreHash, ReaderErr string
	ReaderHash           string
}

// E2ERestoreProbe restores a durable job from retained bytes only, performs
// the requested reads and releases it. Restore never fetches upstream. A
// failed release fails the probe.
func (s *GitReviewerService) E2ERestoreProbe(ctx context.Context, job Job, reads [][2]string) (_ []E2EProbe, err error) {
	factory := s.e2eFactory()
	if factory == nil {
		return nil, ErrLiveBlocked
	}
	bound, err := factory.Restore(ctx, job)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, factory.ReleaseJob(context.WithoutCancel(ctx), bound)) }()
	reader, err := bound.Reader(job.Snapshot)
	if err != nil {
		return nil, err
	}
	probes := []E2EProbe{}
	for _, read := range reads {
		p := E2EProbe{Revision: read[0], Path: read[1]}
		if object, err := bound.evidence.store.Read(ctx, read[0], read[1], snapshotObjectBytes); err == nil {
			sum := sha256.Sum256(object.Data)
			p.StoreOK, p.StoreHash = true, hex.EncodeToString(sum[:])
		}
		if text, err := reader.Read(ctx, read[0], read[1]); err == nil {
			sum := sha256.Sum256([]byte(text))
			p.ReaderOK, p.ReaderHash = true, hex.EncodeToString(sum[:])
		} else {
			p.ReaderErr = err.Error()
		}
		probes = append(probes, p)
	}
	return probes, nil
}

func e2eStamped(path string, settings ...string) *debug.BuildInfo {
	info := &debug.BuildInfo{Path: path}
	for i := 0; i+1 < len(settings); i += 2 {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return info
}

func TestE2EStubRevision(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	stamped := e2eStamped
	if err := e2eStubRevision(stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"), commit); err != nil {
		t.Fatal("exact clean stamp refused", err)
	}
	for name, info := range map[string]*debug.BuildInfo{
		"unstamped":          stamped(e2eStubPackage),
		"other commit":       stamped(e2eStubPackage, "vcs", "git", "vcs.revision", other, "vcs.modified", "false"),
		"modified":           stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "true"),
		"modified unknown":   stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit),
		"other vcs":          stamped(e2eStubPackage, "vcs", "hg", "vcs.revision", commit, "vcs.modified", "false"),
		"duplicate revision": stamped(e2eStubPackage, "vcs", "git", "vcs.revision", other, "vcs.revision", commit, "vcs.modified", "false"),
		"other program":      stamped(e2eStubPackage+"x", "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"),
	} {
		if e2eStubRevision(info, commit) == nil {
			t.Error(name, "stub accepted for the recorded commit")
		}
	}
}

func TestE2ETestBinaryStamp(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	stamp, err := e2eTestBinaryStamp(e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"), commit)
	if err != nil || stamp.Path != e2eTestPackage || stamp.Revision != commit || stamp.Modified != "false" || stamp.VCS != "git" {
		t.Fatal("exact clean test binary stamp refused", stamp, err)
	}
	for name, info := range map[string]*debug.BuildInfo{
		"unstamped":          e2eStamped(e2eTestPackage),
		"other commit":       e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", other, "vcs.modified", "false"),
		"modified":           e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "true"),
		"modified unknown":   e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit),
		"other vcs":          e2eStamped(e2eTestPackage, "vcs", "hg", "vcs.revision", commit, "vcs.modified", "false"),
		"duplicate revision": e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", other, "vcs.revision", commit, "vcs.modified", "false"),
		"source stub":        e2eStamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"),
	} {
		if _, err := e2eTestBinaryStamp(info, commit); err == nil {
			t.Error(name, "test binary accepted for the release commit")
		}
	}
}

// TestE2ERemoteReview proves the remote projection reproduces exactly the
// marker and body hash of the intent whose document was posted, and nothing
// else: another summary, inline body, anchor, comment or marker differs.
func TestE2ERemoteReview(t *testing.T) {
	snapshot := Snapshot{RepositoryID: RepositoryID, HeadRepositoryID: RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40)}
	w := newIntent("review", Scope{RepositoryID, 9001, 1}, 1, 1, snapshot, "job", Document{Summary: "summary", Inline: []InlineComment{{"source.go", 2, "second"}, {"source.go", 1, "first"}}}, time.Now())
	// Production posts the intent's inline comments in order; GitHub returns
	// them in that comment order.
	posted := append([]InlineComment(nil), w.Document.Inline...)
	if marker, hash := E2ERemoteReview(w.Document.Summary, posted); marker != w.Marker || hash != w.BodyHash {
		t.Fatal("posted intent document not reproduced", marker, hash)
	}
	slices.Reverse(posted)
	if marker, hash := E2ERemoteReview(w.Document.Summary, posted); marker != w.Marker || hash != w.BodyHash {
		t.Fatal("comment order changed the canonical document", marker, hash)
	}
	edit := func(f func(*InlineComment)) []InlineComment {
		out := append([]InlineComment(nil), w.Document.Inline...)
		f(&out[0])
		return out
	}
	for name, in := range map[string]struct {
		body   string
		inline []InlineComment
	}{
		"other summary":      {strings.Replace(w.Document.Summary, "summary", "other", 1), w.Document.Inline},
		"other inline body":  {w.Document.Summary, edit(func(c *InlineComment) { c.Body = "other" })},
		"other inline line":  {w.Document.Summary, edit(func(c *InlineComment) { c.Line = 9 })},
		"other inline path":  {w.Document.Summary, edit(func(c *InlineComment) { c.Path = "other.go" })},
		"missing comment":    {w.Document.Summary, w.Document.Inline[:1]},
		"additional comment": {w.Document.Summary, append(slices.Clone(w.Document.Inline), InlineComment{"source.go", 3, "third"})},
	} {
		if _, hash := E2ERemoteReview(in.body, in.inline); hash == w.BodyHash {
			t.Error(name, "reproduced the intent's body hash")
		}
	}
	if marker, _ := E2ERemoteReview(w.Document.Summary+"\n<!-- sfl-alpha:"+strings.Repeat("c", 32)+" -->", w.Document.Inline); marker != "" {
		t.Error("a body with two markers projected marker", marker)
	}
}

func TestE2ERecordedCommit(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	manifest := func(commit string, dirty bool) []byte {
		raw, _ := json.Marshal(map[string]any{"source_commit": commit, "source_dirty": dirty})
		return raw
	}
	if got, err := e2eRecordedCommit(manifest(commit, false), gitVMGuestRelease{SourceHead: commit}); err != nil || got != commit {
		t.Fatal("matching guest release refused", got, err)
	}
	for name, in := range map[string]struct {
		manifest []byte
		head     string
	}{
		"guest from another commit": {manifest(commit, false), other},
		"guest without a head":      {manifest(commit, false), ""},
		"dirty image":               {manifest(commit, true), commit},
		"no recorded commit":        {manifest("", false), commit},
		"malformed manifest":        {[]byte("{"), commit},
	} {
		if got, err := e2eRecordedCommit(in.manifest, gitVMGuestRelease{SourceHead: in.head}); err == nil {
			t.Error(name, "accepted as release revision", got)
		}
	}
}

// TestE2ENativeManifestHash proves the harness derives exactly the manifest
// production's VMBootImages.NativeManifest builds for a launch's session from
// the same release pins, and that every pin, the launch and the provider
// change it.
func TestE2ENativeManifestHash(t *testing.T) {
	snapshot := Snapshot{RepositoryID: RepositoryID, HeadRepositoryID: RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40)}
	s, err := NewSession("e2e-native-manifest", snapshot, Digest("config"), Codex, 1, "packet", nil, nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	launch := strings.Repeat("6", 32)
	binding := AuthBinding{SessionHash: Digest(s), LaunchID: launch, ContainmentID: Digest(struct{ Session, Launch string }{Digest(s), launch})}
	asset := func(path string) VMBootAsset { return VMBootAsset{Path: path, SHA256: Digest(path), Bytes: 4096} }
	p := GitReviewerProviderRelease{Stub: asset("/fixture/nativevmstub"), QEMU: asset("/fixture/qemu"), Kernel: asset("/fixture/vmlinuz"), Initrd: asset("/fixture/native.cpio.gz"), Firmware: asset("/fixture/bios.bin"), Runtime: NativeVMRuntimeSpec{Artifact: asset("/fixture/native-qemu-runtime.bin"), NamespaceHelper: asset("/fixture/namespace-helper"), BootstrapLibraries: []VMBootAsset{asset("/lib/x86_64-linux-gnu/libc.so.6"), asset("/lib64/ld-linux-x86-64.so.2")}}}
	pins, err := nativeBootstrapLibraryPins(p.Runtime.BootstrapLibraries)
	if err != nil {
		t.Fatal(err)
	}
	// NativeManifest reads only the pins; the descriptors need only exist.
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	images := &VMBootImages{spec: VMBootSpec{Version: 1, Binding: binding, QEMU: p.QEMU, Kernel: p.Kernel, Initrd: p.Initrd}, firmware: file, firmwareAsset: p.Firmware, runtimeArtifact: file, runtimeAsset: p.Runtime.Artifact, namespaceHelper: file, namespaceHelperAsset: p.Runtime.NamespaceHelper, bootstrapLibraries: pins}
	m, err := images.NativeManifest(s)
	if err != nil {
		t.Fatal("production manifest", err)
	}
	got, err := E2ENativeManifestHash(p, Codex, binding)
	if err != nil || got != Digest(m) {
		t.Fatal("harness manifest differs from production's", got, Digest(m), err)
	}
	edits := map[string]func(*GitReviewerProviderRelease, *AuthBinding, *Provider){
		"other QEMU":     func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) { p.QEMU.SHA256 = Digest("other") },
		"other kernel":   func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) { p.Kernel.Bytes++ },
		"other initrd":   func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) { p.Initrd.SHA256 = Digest("other") },
		"other firmware": func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) { p.Firmware.SHA256 = Digest("other") },
		"other runtime": func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) {
			p.Runtime.Artifact.SHA256 = Digest("other")
		},
		"other helper": func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) { p.Runtime.NamespaceHelper.Bytes++ },
		"other libraries": func(p *GitReviewerProviderRelease, _ *AuthBinding, _ *Provider) {
			p.Runtime.BootstrapLibraries[0].SHA256 = Digest("other")
		},
		"other launch":   func(_ *GitReviewerProviderRelease, b *AuthBinding, _ *Provider) { b.LaunchID = strings.Repeat("7", 32) },
		"other provider": func(_ *GitReviewerProviderRelease, _ *AuthBinding, provider *Provider) { *provider = Claude },
	}
	for name, edit := range edits {
		q, b, provider := p, binding, Codex
		q.Runtime.BootstrapLibraries = slices.Clone(p.Runtime.BootstrapLibraries)
		edit(&q, &b, &provider)
		if other, err := E2ENativeManifestHash(q, provider, b); err == nil && other == got {
			t.Error(name, "derived the same manifest")
		}
	}
	if _, err := E2ENativeManifestHash(GitReviewerProviderRelease{}, Codex, binding); err == nil {
		t.Error("release without pins derived a manifest")
	}
}

// TestE2ENativeLaunchReceipts proves the native launch seams against the
// records production itself writes for a stopped phase: the unit journal
// reads back through readUnit, the audit decodes strictly as version 2 naming
// the phase's durable attempt and re-encodes to its own bytes, and its
// request, report and source hashes are the digests of
// the request reviewTaskAudited builds, the zero report a failed phase
// records and the source receipt the phase returned. Any other request, a
// changed receipt shape or an impossible lifecycle is refused.
func TestE2ENativeLaunchReceipts(t *testing.T) {
	p, _ := nativeHostPhaseFixture(t)
	p.attempt = strings.Repeat("a", 32)
	q := p.request
	p.finished = &nativeVMFinished{Version: 1, Binding: q.Binding, RequestHash: Digest(q), State: "invalid_output", SourceAudit: nativeHostAuditFixture(t, q)}
	attachIdleNativeProviderBridge(t, p)
	_, access, _, err := p.finish(context.Background(), Report{}, PhaseError{State: "invalid_output"})
	if err == nil || access == nil {
		t.Fatal("fixture phase did not fail with a source receipt", err)
	}
	directory := filepath.Join(p.supervisor.supervisor.root, q.Binding.LaunchID)
	unit, err := E2EReadNativeUnitJournal(directory, p.supervisor.supervisor.stubHash)
	if err != nil || unit.Binding != q.Binding || !unit.Started || !unit.StartSettled || !unit.Captured || !unit.Aborted || !unit.Stopped || unit.ManifestHash != Digest(p.manifest) {
		t.Fatal("production unit journal not read back whole", unit, err)
	}
	if _, err := E2EReadNativeUnitJournal(directory, Digest("another stub")); err == nil {
		t.Error("unit journal of another stub accepted")
	}
	a, err := E2EReadNativeAudit(directory)
	if err != nil || a.Version != 2 || a.Binding != q.Binding || a.AttemptID != p.attempt || a.Complete || a.Failure == "" || a.Stage != "" {
		t.Fatal("production audit not read back whole", a, err)
	}
	if err := E2EValidateNativeAudit(a, q.Session, q.Paths, q.Policy, q.ProfileHints); err != nil {
		t.Fatal("production audit not bound to its own request", err)
	}
	if a.ReportHash != Digest(Report{}) || a.SourceHash != Digest(access) || a.ManifestHash != Digest(p.manifest) {
		t.Fatal("audit hashes are not the failed phase's zero report, returned source receipt and manifest", a)
	}
	other := testSession(Claude)
	otherPolicy := q.Policy
	otherPolicy.Headers = append(slices.Clone(q.Policy.Headers), "X-Other")
	for name, check := range map[string]func() error{
		"another session": func() error { return E2EValidateNativeAudit(a, other, q.Paths, q.Policy, q.ProfileHints) },
		"other paths": func() error {
			return E2EValidateNativeAudit(a, q.Session, []string{"other.go"}, q.Policy, q.ProfileHints)
		},
		"another policy": func() error { return E2EValidateNativeAudit(a, q.Session, q.Paths, otherPolicy, q.ProfileHints) },
		"unknown failure": func() error {
			b := a
			b.Failure = "unknown"
			return E2EValidateNativeAudit(b, q.Session, q.Paths, q.Policy, q.ProfileHints)
		},
		"completed with a failure": func() error {
			b := a
			b.Complete = true
			return E2EValidateNativeAudit(b, q.Session, q.Paths, q.Policy, q.ProfileHints)
		},
	} {
		if check() == nil {
			t.Error("audit accepted for", name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(directory, "native-audit.json"))
	if err != nil {
		t.Fatal(err)
	}
	unitRaw, err := os.ReadFile(filepath.Join(directory, "unit.json"))
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, edit := range map[string]func(map[string]any){
		"extra field":       func(m map[string]any) { m["Extra"] = true },
		"missing field":     func(m map[string]any) { delete(m, "SourceHash") },
		"missing attempt":   func(m map[string]any) { delete(m, "AttemptID") },
		"missing diagnosis": func(m map[string]any) { delete(m, "BootDiagnostics") },
	} {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		edited, _ := json.Marshal(m)
		rewrite("native-audit.json", edited)
		if _, err := E2EReadNativeAudit(directory); err == nil {
			t.Error("audit with", name, "accepted")
		}
	}
	rewrite("native-audit.json", append(slices.Clone(raw), ' '))
	if _, err := E2EReadNativeAudit(directory); err == nil {
		t.Error("audit not in production's encoding accepted")
	}
	for _, field := range []string{"Started", "StartSettled", "Aborted"} {
		var m map[string]any
		if err := json.Unmarshal(unitRaw, &m); err != nil {
			t.Fatal(err)
		}
		m[field] = false
		edited, _ := json.Marshal(m)
		rewrite("unit.json", edited)
		if _, err := E2EReadNativeUnitJournal(directory, p.supervisor.supervisor.stubHash); err == nil {
			t.Error("captured and stopped unit journal without", field, "accepted")
		}
	}
}
