//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func validStoppedFaultArm(arm r.EvaluationArm) bool {
	return arm == "" || arm == r.ArmAdversarial || arm == r.ArmCodexControl || arm == r.ArmClaudeControl
}

// This modifies only the brand-new synthetic fixture database. Earlier arms
// retain their natural order and session counts; the selected arm's first
// stopped result fails through the actual execution writer.
func installStoppedFault(ctx context.Context, db *sql.DB, arm r.EvaluationArm) error {
	if arm == "" || !validStoppedFaultArm(arm) {
		return r.ErrLiveBlocked
	}
	_, err := db.ExecContext(ctx, `CREATE TRIGGER stopped_native_fault BEFORE UPDATE ON evaluation_execution
WHEN json_extract(CAST(new.data AS TEXT),'$.Arm')='`+string(arm)+`'
AND json_extract(CAST(new.data AS TEXT),'$.Attempts[0].State')='complete'
BEGIN SELECT RAISE(ABORT,'synthetic stopped native result persistence failure'); END`)
	return err
}

func fixtureLaunchRoot(c config, arm r.EvaluationArm) (string, error) {
	name := map[r.EvaluationArm]string{r.ArmAdversarial: "a", r.ArmCodexControl: "xc", r.ArmClaudeControl: "cc"}[arm]
	if name == "" {
		return "", r.ErrLiveBlocked
	}
	return filepath.Join(c.Root, "launches", name), nil
}

// This generated, non-secret binding is only a lookup hint. The supervisor
// independently validates the complete original intent, unit and stopped
// identity. The hint cannot supply or replace that proof.
func findStoppedFixtureBinding(root, capability string) (r.AuthBinding, error) {
	directory, err := os.Open(root)
	if err != nil {
		return r.AuthBinding{}, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(33)
	if (err != nil && err != io.EOF) || len(entries) > 32 {
		return r.AuthBinding{}, r.ErrEvaluationRecovery
	}
	var found r.AuthBinding
	for _, entry := range entries {
		nonce, err := hex.DecodeString(entry.Name())
		if err != nil || len(nonce) != 16 || hex.EncodeToString(nonce) != entry.Name() || !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "auth-return-intent.json")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return r.AuthBinding{}, r.ErrEvaluationRecovery
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 65537))
		file.Close()
		var hint struct{ Binding r.AuthBinding }
		if readErr != nil || len(raw) > 65536 || json.Unmarshal(raw, &hint) != nil || hint.Binding.SessionHash != capability || hint.Binding.LaunchID != entry.Name() {
			continue
		}
		if found != (r.AuthBinding{}) {
			return r.AuthBinding{}, r.ErrEvaluationRecovery
		}
		found = hint.Binding
	}
	if found == (r.AuthBinding{}) {
		return found, r.ErrEvaluationRecovery
	}
	return found, nil
}

func (p *fixture) runStoppedJournalFault(ctx context.Context, corpus r.EvaluationCorpus, plan r.EvaluationPlan, options r.EvaluationRunnerOptions) (result error) {
	started := time.Now()
	receipt := struct {
		Version                                                                                   int
		Purpose, Stage, RunnerSourceHead, RunnerSourceTree, ImageSourceHead, CorpusHash, PlanHash string
		Arm                                                                                       r.EvaluationArm
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified                          bool
		RestartRefusedReplay, AlreadyDisposedAuth, RetiredChargedWithoutLedger, ReusedWithoutWork bool
		PhaseAttempts, ChargedRetiredSessions                                                     int
		NativeIntentHash, RetirementHash                                                          string
		ElapsedMS                                                                                 int64
		Phases                                                                                    []phase
	}{Version: 1, Purpose: "native-stopped-journal-fault", Stage: "install-fault", RunnerSourceHead: sourceHead, RunnerSourceTree: sourceTree, ImageSourceHead: p.config.ImageSourceHead, CorpusHash: corpus.Hash(), PlanHash: plan.Hash(), Arm: p.config.StoppedFaultArm, FakeOnly: true}
	defer func() {
		receipt.Phases, receipt.PhaseAttempts = p.phases, p.calls
		receipt.ElapsedMS = time.Since(started).Milliseconds()
		raw, err := json.MarshalIndent(receipt, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(p.config.Root, "result.json"), append(raw, '\n'), 0600)
		}
		result = errors.Join(result, err)
		fmt.Println(string(raw))
	}()
	path := filepath.Join(p.config.Root, "journal", "execution.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = installStoppedFault(ctx, db, receipt.Arm); err != nil {
		return err
	}
	receipt.Stage = "native-result-before-failed-save"
	_, err = r.RunEvaluation(ctx, options)
	if !errors.Is(err, r.ErrEvaluationRecovery) {
		return r.ErrEvaluationRecovery
	}
	if _, err = db.ExecContext(ctx, "DROP TRIGGER stopped_native_fault"); err != nil {
		return err
	}
	caseID := corpus.Cases[0].ID
	saved, err := p.journal.LoadExecution(ctx, caseID, receipt.Arm, 0)
	if err != nil || saved == nil || len(saved.Attempts) != 1 || saved.Attempts[0].State != "inflight" || saved.Sessions() != 1 || len(saved.Ledger) != 0 {
		return r.ErrEvaluationRecovery
	}
	expected, earlier := 1, 0
	for _, arm := range plan.Order(0, 0) {
		if arm == receipt.Arm {
			break
		}
		expected += arm.Sessions()
		earlier++
	}
	if p.calls != expected || len(p.phases) != expected {
		return r.ErrEvaluationRecovery
	}
	for _, phase := range p.phases {
		if !phase.Passed {
			return r.ErrEvaluationRecovery
		}
	}
	receipt.Stage = "reopen-refuses-replay"
	if err = p.journal.Close(); err != nil {
		return err
	}
	journal, err := r.OpenEvaluationJournal(path, r.Enrollment{RepositoryID: r.RepositoryID, InstallationID: 9001, AppID: 9002, BotID: 9003, OwnerID: r.OwnerID}, corpus, plan)
	if err != nil {
		return err
	}
	defer journal.Close()
	p.journal, options.Journal = journal, journal
	sources, workers := 0, 0
	options.Source = func(context.Context, r.EvaluationSourceRequest) (r.EvaluationSource, error) {
		sources++
		return r.EvaluationSource{}, r.ErrLiveBlocked
	}
	options.Workers = func(context.Context, r.EvaluationArm, r.Job, *r.JobEvidence) (r.EvaluationWorkers, error) {
		workers++
		return r.EvaluationWorkers{}, r.ErrLiveBlocked
	}
	_, err = r.RunEvaluation(ctx, options)
	receipt.RestartRefusedReplay = errors.Is(err, r.ErrEvaluationRecovery) && sources == 0 && workers == 0 && p.calls == expected
	if !receipt.RestartRefusedReplay {
		return r.ErrEvaluationRecovery
	}
	receipt.Stage = "recover-original-native-proof"
	attempt := saved.Attempts[0]
	capability := r.Session{}
	if attempt.Control != nil {
		capability, err = r.ControlCapabilitySession(*attempt.Control)
		if err != nil {
			return err
		}
	} else if attempt.Review != nil {
		capability = *attempt.Review
	} else {
		return r.ErrEvaluationRecovery
	}
	root, err := fixtureLaunchRoot(p.config, receipt.Arm)
	if err != nil {
		return err
	}
	binding, err := findStoppedFixtureBinding(root, attempt.CapabilityHash)
	if err != nil {
		return err
	}
	supervisor, err := r.OpenNativeVMSupervisor(ctx, root, p.config.Stub.Path, p.config.Stub.SHA256)
	if err != nil {
		return err
	}
	defer supervisor.Close()
	proof, err := supervisor.StoppedAttemptEvidence(ctx, binding, capability)
	if err != nil {
		return err
	}
	profile, profileErr := p.auth[attempt.Provider].RecoverProfile(ctx, binding)
	if profile != nil {
		profile.Close()
	}
	receipt.AlreadyDisposedAuth = profileErr != nil && profile == nil
	if !receipt.AlreadyDisposedAuth {
		return r.ErrEvaluationRecovery
	}
	receipt.NativeIntentHash = proof.IntentHash
	receipt.Stage = "retire-stopped-charged-attempt"
	recover := r.EvaluationStoppedRecovery{Binding: binding, Auth: p.auth[attempt.Provider], Stopped: supervisor, Source: proof.Source, Evidence: proof.Provider, Check: options.Check}
	if err = journal.RetireStopped(ctx, caseID, receipt.Arm, 0, recover); err != nil {
		return err
	}
	retired, err := journal.LoadExecution(ctx, caseID, receipt.Arm, 0)
	if err != nil || retired == nil || retired.Recovery == nil {
		return r.ErrEvaluationRecovery
	}
	receipt.RetiredChargedWithoutLedger = retired.State == "incomplete_expired" && retired.Sessions() == 1 && len(retired.Ledger) == 0 && !retired.Attempts[0].Unsafe && retired.Attempts[0].Report == nil && !retired.Recovery.VerifiedAt.IsZero() && retired.Recovery.NativeIntentHash == proof.IntentHash
	receipt.RetirementHash, receipt.ChargedRetiredSessions = retired.Hash(), retired.Sessions()
	if !receipt.RetiredChargedWithoutLedger || journal.RetireStopped(ctx, caseID, receipt.Arm, 0, recover) != nil {
		return r.ErrEvaluationRecovery
	}
	receipt.Stage = "reuse-without-new-native-work"
	resumed, resumeErr := r.RunEvaluation(ctx, options)
	receipt.ReusedWithoutWork = errors.Is(resumeErr, r.ErrLiveBlocked) && resumed.ReusedArms == earlier+1 && resumed.ExecutedSessions == 0 && workers == 0 && sources == 1 && p.calls == expected
	if !receipt.ReusedWithoutWork {
		return r.ErrEvaluationRecovery
	}
	receipt.Passed, receipt.Stage = true, "complete"
	return nil
}
