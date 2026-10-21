//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

var ErrEvaluationRecovery = errors.New("evaluation attempt requires explicit stopped recovery; replay refused")
var ErrEvaluationFailed = errors.New("evaluation includes failed or unscored arms")

const evaluationRecordLimit = 8 << 20

// EvaluationAttempt is private execution evidence, never ground truth. An
// inflight reservation is charged until a trusted before-work result is saved.
type EvaluationAttempt struct {
	Index                     int
	Provider                  Provider
	InputHash, CapabilityHash string
	Review                    *Session
	Control                   *ControlSession
	StartedAt, FinishedAt     time.Time
	State                     string
	Charged, Unsafe           bool
	Report                    json.RawMessage `json:",omitempty"`
	Source                    *SourceAccess
	Evidence                  *ProviderEvidence
}

// EvaluationExecution precedes independent actionability assessment. Failed
// executions retain diagnostics/receipts but no substantive partial ledger.
type EvaluationExecution struct {
	Recovery              *EvaluationRecoveryRecord `json:",omitempty"`
	Version               int
	CaseID                string
	Arm                   EvaluationArm
	Repeat                int
	CorpusHash, PlanHash  string
	Job                   Job
	StartedAt, FinishedAt time.Time
	State                 string
	Attempts              []EvaluationAttempt
	Ledger                []Entry
}

func (e EvaluationExecution) Hash() string { return Digest(e) }
func (e EvaluationExecution) Sessions() int {
	n := 0
	for _, a := range e.Attempts {
		if a.Charged {
			n++
		}
	}
	return n
}

// EvaluationJournal uses a dedicated private SQLite directory and enrollment.
// It has no GitHub writer. Never share this directory with an interactive
// profile or another service's credential/journal owner.
type EvaluationJournal struct {
	store                *Store
	corpus               EvaluationCorpus
	plan                 EvaluationPlan
	corpusHash, planHash string
	mu                   sync.Mutex
	active, closed       bool
}

func OpenEvaluationJournal(path string, enrollment Enrollment, corpus EvaluationCorpus, plan EvaluationPlan) (*EvaluationJournal, error) {
	if err := plan.Validate(corpus); err != nil {
		return nil, err
	}
	// Freeze caller-owned maps/slices before retaining provenance.
	raw, err := json.Marshal(struct {
		Corpus EvaluationCorpus
		Plan   EvaluationPlan
	}{corpus, plan})
	if err != nil || len(raw) > 32<<20 {
		return nil, ErrFenced
	}
	var captured struct {
		Corpus EvaluationCorpus
		Plan   EvaluationPlan
	}
	if json.Unmarshal(raw, &captured) != nil {
		return nil, ErrFenced
	}
	// JSON replaces invalid UTF-8. Validate the retained representation before
	// opening storage so distinct caller IDs cannot collapse into one slot.
	if captured.Plan.Validate(captured.Corpus) != nil {
		return nil, ErrFenced
	}
	store, err := OpenStore(path, enrollment, captured.Plan.Config)
	if err != nil {
		return nil, err
	}
	journal := &EvaluationJournal{store: store, corpus: captured.Corpus, plan: captured.Plan, corpusHash: captured.Corpus.Hash(), planHash: captured.Plan.Hash()}
	if _, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS evaluation_meta(id INTEGER PRIMARY KEY CHECK(id=1),corpus_hash TEXT NOT NULL,plan_hash TEXT NOT NULL)`); err == nil {
		_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS evaluation_execution(slot TEXT PRIMARY KEY,data BLOB NOT NULL)`)
	}
	if err == nil {
		tx, beginErr := store.db.BeginTx(context.Background(), nil)
		err = beginErr
		if err == nil {
			defer tx.Rollback()
			_, err = tx.Exec(`INSERT OR IGNORE INTO evaluation_meta(id,corpus_hash,plan_hash) VALUES(1,?,?)`, corpus.Hash(), plan.Hash())
			var ch, ph string
			if err == nil {
				err = tx.QueryRow(`SELECT corpus_hash,plan_hash FROM evaluation_meta WHERE id=1`).Scan(&ch, &ph)
			}
			if err == nil && (ch != corpus.Hash() || ph != plan.Hash()) {
				err = ErrFenced
			}
			if err == nil {
				err = tx.Commit()
			}
		}
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	if err == nil {
		expected := map[string]bool{}
		for _, test := range journal.corpus.Cases {
			for repeat := 0; repeat < journal.plan.Repeats; repeat++ {
				for _, arm := range EvaluationArms() {
					expected[evaluationSlot(test.ID, arm, repeat)] = true
				}
			}
		}
		rows, readErr := store.db.Query(`SELECT slot FROM evaluation_execution LIMIT ?`, len(expected)+1)
		err = readErr
		if err == nil {
			count := 0
			for rows.Next() {
				var slot string
				if err = rows.Scan(&slot); err != nil {
					break
				}
				count++
				if !expected[slot] || count > len(expected) {
					err = ErrFenced
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return journal, nil
}
func (j *EvaluationJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active {
		return ErrFenced
	}
	if j.closed {
		return nil
	}
	if err := j.store.Close(); err != nil {
		return err
	}
	j.closed = true
	return nil
}
func (j *EvaluationJournal) acquire(ctx context.Context) (func(), error) {
	if j == nil || ctx == nil {
		return nil, ErrFenced
	}
	j.mu.Lock()
	if j.closed || j.active {
		j.mu.Unlock()
		return nil, ErrFenced
	}
	j.active = true
	j.mu.Unlock()
	held, err := (&AuthStore{directory: filepath.Dir(j.store.path)}).lock(ctx)
	if err != nil {
		j.mu.Lock()
		j.active = false
		j.mu.Unlock()
		return nil, err
	}
	return func() { held.close(); j.mu.Lock(); j.active = false; j.mu.Unlock() }, nil
}
func evaluationSlot(caseID string, arm EvaluationArm, repeat int) string {
	return Digest(struct {
		Case   string
		Arm    EvaluationArm
		Repeat int
	}{caseID, arm, repeat})
}
func (j *EvaluationJournal) testCase(caseID string) (EvaluationCase, error) {
	for _, c := range j.corpus.Cases {
		if c.ID == caseID {
			return c, nil
		}
	}
	return EvaluationCase{}, ErrFenced
}
func (j *EvaluationJournal) read(ctx context.Context, caseID string, arm EvaluationArm, repeat int) (*EvaluationExecution, error) {
	var raw []byte
	err := j.store.db.QueryRowContext(ctx, `SELECT data FROM evaluation_execution WHERE slot=?`, evaluationSlot(caseID, arm, repeat)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > evaluationRecordLimit {
		return nil, ErrFenced
	}
	var record EvaluationExecution
	if json.Unmarshal(raw, &record) != nil {
		return nil, ErrFenced
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(raw, canonical) || record.CaseID != caseID || record.Arm != arm || record.Repeat != repeat {
		return nil, ErrFenced
	}
	if err = j.validateExecution(record); err != nil {
		return nil, err
	}
	return &record, nil
}

// LoadExecution returns a decoded copy. It neither replays nor launches work.
func (j *EvaluationJournal) LoadExecution(ctx context.Context, caseID string, arm EvaluationArm, repeat int) (*EvaluationExecution, error) {
	if j == nil || ctx == nil {
		return nil, ErrFenced
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrFenced
	}
	return j.read(ctx, caseID, arm, repeat)
}
func (j *EvaluationJournal) write(ctx context.Context, record EvaluationExecution, previous *EvaluationExecution) error {
	if err := j.validateExecution(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > evaluationRecordLimit {
		return ErrFenced
	}
	tx, err := j.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	slot := evaluationSlot(record.CaseID, record.Arm, record.Repeat)
	var current []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM evaluation_execution WHERE slot=?`, slot).Scan(&current)
	if previous == nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO evaluation_execution(slot,data) VALUES(?,?)`, slot, raw)
	} else {
		expected, _ := json.Marshal(previous)
		if err != nil || !bytes.Equal(current, expected) {
			return ErrFenced
		}
		_, err = tx.ExecContext(ctx, `UPDATE evaluation_execution SET data=? WHERE slot=?`, raw, slot)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func cloneEvaluationExecution(e EvaluationExecution) EvaluationExecution {
	raw, _ := json.Marshal(e)
	var out EvaluationExecution
	json.Unmarshal(raw, &out)
	return out
}
