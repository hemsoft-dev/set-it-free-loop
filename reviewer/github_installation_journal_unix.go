//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

var ErrGitHubInstallationRecovery = errors.New("durable installation acquisition requires explicit recovery")

const installationAttemptCap = 1024

// GitHubInstallationRecovery is nonsecret evidence supplied by the trusted
// operator layer. Its callback must establish stopped prior work; an assertion
// from a model or the mere absence of a local transport is not that proof.
type GitHubInstallationRecovery struct {
	AttemptID    string
	OwnerID      int64
	At           time.Time
	EvidenceHash string
}

// GitHubInstallationAttempt records no JWT, token, key or remote error text.
// Receipt is qualification metadata only; it cannot recreate a transport.
type GitHubInstallationAttempt struct {
	Version              int
	ID                   string
	Enrollment           Enrollment
	Role                 GitHubCredentialRole
	State                string
	StartedAt, UpdatedAt time.Time
	MintAttempted        bool
	Receipt              *GitHubInstallationReceipt
	Recovery             *GitHubInstallationRecovery
}

// Durable metadata has optional receipt/recovery pointers, unlike model-output
// schemas that reject every null. Keep bounded, duplicate/unknown/trailing-key
// rejection and explicitly require every nonoptional field before semantics.
func installationJournalObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, ErrGitHubInstallationRecovery
	}
	allowed := map[string]bool{}
	for _, name := range required {
		allowed[name] = true
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrGitHubInstallationRecovery
		}
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return nil, ErrGitHubInstallationRecovery
		}
	}
	return fields, nil
}

func decodeGitHubInstallationAttempt(raw []byte) (GitHubInstallationAttempt, error) {
	var out GitHubInstallationAttempt
	if len(raw) == 0 || len(raw) > 4096 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return out, ErrGitHubInstallationRecovery
	}
	fields, err := installationJournalObject(raw, []string{"Version", "ID", "Enrollment", "Role", "State", "StartedAt", "UpdatedAt", "MintAttempted"}, []string{"Receipt", "Recovery"})
	if err != nil {
		return out, err
	}
	enrollmentFields := []string{"RepositoryID", "InstallationID", "AppID", "BotID", "OwnerID"}
	if _, err := installationJournalObject(fields["Enrollment"], enrollmentFields, nil); err != nil {
		return out, err
	}
	for _, name := range []string{"Receipt", "Recovery"} {
		value := fields[name]
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		if name == "Receipt" {
			receipt, err := installationJournalObject(value, []string{"Enrollment", "Role", "CheckedAt", "ExpiresAt"}, nil)
			if err != nil {
				return out, err
			}
			if _, err := installationJournalObject(receipt["Enrollment"], enrollmentFields, nil); err != nil {
				return out, err
			}
		} else if _, err := installationJournalObject(value, []string{"AttemptID", "OwnerID", "At", "EvidenceHash"}, nil); err != nil {
			return out, err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	return out, nil
}

func (a GitHubInstallationAttempt) valid(e Enrollment, role GitHubCredentialRole) bool {
	nonce, err := hex.DecodeString(a.ID)
	if err != nil || len(nonce) != 16 || a.Version != 1 || a.Enrollment != e || a.Role != role || githubPermissions(role) == nil || a.StartedAt.IsZero() || a.UpdatedAt.Before(a.StartedAt) {
		return false
	}
	switch a.State {
	case "pre_mint", "pre_mint_failed":
		if a.MintAttempted {
			return false
		}
	case "mint_pending", "unknown", "qualified":
		if !a.MintAttempted {
			return false
		}
	case "retry_authorized":
		if a.Recovery == nil || a.Recovery.AttemptID != a.ID || a.Recovery.OwnerID != OwnerID || a.Recovery.At.Before(a.StartedAt) || a.Recovery.At.After(a.UpdatedAt) || !validAuthHash(a.Recovery.EvidenceHash) {
			return false
		}
	default:
		return false
	}
	if a.State != "retry_authorized" && a.Recovery != nil {
		return false
	}
	if !a.MintAttempted && a.Receipt != nil {
		return false
	}
	if a.State == "qualified" && a.Receipt == nil {
		return false
	}
	if a.Receipt != nil && (a.Receipt.Enrollment != e || a.Receipt.Role != role || a.Receipt.CheckedAt.Before(a.StartedAt) || a.Receipt.CheckedAt.After(a.UpdatedAt) || !a.Receipt.ExpiresAt.After(a.Receipt.CheckedAt)) {
		return false
	}
	return true
}

// GitHubInstallationJournal serializes each role's complete handshake across
// processes and keeps a SQLite write-ahead latch. The caller owns Store. No
// acquisition or background renewal occurs during construction or reopening.
type GitHubInstallationJournal struct {
	store      *Store
	enrollment Enrollment
	directory  string
	now        func() time.Time
}

func (GitHubInstallationJournal) String() string   { return "GitHubInstallationJournal{redacted}" }
func (GitHubInstallationJournal) GoString() string { return "GitHubInstallationJournal{redacted}" }
func (GitHubInstallationJournal) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubInstallationJournal{redacted}"`), nil
}

func NewGitHubInstallationJournal(ctx context.Context, store *Store, now func() time.Time) (*GitHubInstallationJournal, error) {
	if ctx == nil || store == nil || store.db == nil || now == nil {
		return nil, ErrGitHubInstallationRecovery
	}
	state, err := store.Snapshot(ctx)
	if err != nil || state.Enrollment.Validate() != nil {
		return nil, ErrGitHubInstallationRecovery
	}
	directory := filepath.Join(filepath.Dir(store.path), "github-installation-attempts")
	for _, dir := range []string{directory, filepath.Join(directory, string(GitHubReader)), filepath.Join(directory, string(GitHubPublisher))} {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, ErrGitHubInstallationRecovery
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || resolved != dir || privateDirectory(dir) != nil {
			return nil, ErrGitHubInstallationRecovery
		}
	}
	if _, err = store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS github_installation_attempts (sequence INTEGER PRIMARY KEY AUTOINCREMENT, role TEXT NOT NULL CHECK(role IN ('reader','publisher')), attempt TEXT NOT NULL UNIQUE, record BLOB NOT NULL CHECK(length(record)<=4096))`); err != nil {
		return nil, ErrGitHubInstallationRecovery
	}
	return &GitHubInstallationJournal{store, state.Enrollment, directory, now}, nil
}

func (j *GitHubInstallationJournal) valid() bool {
	return j != nil && j.store != nil && j.store.db != nil && j.enrollment.Validate() == nil && j.now != nil
}
func (j *GitHubInstallationJournal) lock(ctx context.Context, role GitHubCredentialRole) (*authLock, error) {
	if !j.valid() || ctx == nil || githubPermissions(role) == nil {
		return nil, ErrGitHubInstallationRecovery
	}
	// The existing private flock helper is used only in this dedicated metadata
	// directory. No provider AuthStore/state/profile or credentials are opened.
	held, err := (&AuthStore{directory: filepath.Join(j.directory, string(role))}).lock(ctx)
	if err != nil {
		return nil, ErrGitHubInstallationRecovery
	}
	return held, nil
}

type installationRowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (j *GitHubInstallationJournal) latest(ctx context.Context, db installationRowReader, role GitHubCredentialRole) (GitHubInstallationAttempt, error) {
	var raw []byte
	var id string
	err := db.QueryRowContext(ctx, "SELECT attempt,CASE WHEN length(record)<=4096 THEN record ELSE NULL END FROM github_installation_attempts WHERE role=? ORDER BY sequence DESC LIMIT 1", string(role)).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubInstallationAttempt{}, nil
	}
	if err != nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	record, err := decodeGitHubInstallationAttempt(raw)
	if err != nil || record.ID != id || !record.valid(j.enrollment, role) {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	return record, nil
}

// Latest reports durable metadata only, never a readiness or credential grant.
func (j *GitHubInstallationJournal) Latest(ctx context.Context, role GitHubCredentialRole) (GitHubInstallationAttempt, error) {
	if !j.valid() || ctx == nil || githubPermissions(role) == nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	return j.latest(ctx, j.store.db, role)
}

func (j *GitHubInstallationJournal) write(ctx context.Context, tx *sql.Tx, record GitHubInstallationAttempt, insert bool) error {
	if !record.valid(j.enrollment, record.Role) {
		return ErrGitHubInstallationRecovery
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 4096 {
		return ErrGitHubInstallationRecovery
	}
	if insert {
		_, err = tx.ExecContext(ctx, "INSERT INTO github_installation_attempts(role,attempt,record) VALUES(?,?,?)", string(record.Role), record.ID, raw)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE github_installation_attempts SET record=? WHERE role=? AND attempt=?", raw, string(record.Role), record.ID)
		if err == nil {
			n, e := result.RowsAffected()
			if e != nil || n != 1 {
				return ErrGitHubInstallationRecovery
			}
		}
	}
	if err != nil {
		return ErrGitHubInstallationRecovery
	}
	return nil
}

// Retain at least seven days of terminal history, every unresolved attempt and
// the latest record for each role. Excess attempts fail before any HTTP work.
func (j *GitHubInstallationJournal) prune(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, "SELECT CASE WHEN length(record)<=4096 THEN record ELSE NULL END FROM github_installation_attempts ORDER BY sequence DESC LIMIT 1025")
	if err != nil {
		return ErrGitHubInstallationRecovery
	}
	seen := map[GitHubCredentialRole]bool{}
	remove := []string{}
	count := 0
	for rows.Next() {
		count++
		var raw []byte
		if rows.Scan(&raw) != nil {
			rows.Close()
			return ErrGitHubInstallationRecovery
		}
		record, err := decodeGitHubInstallationAttempt(raw)
		if err != nil || !record.valid(j.enrollment, record.Role) {
			rows.Close()
			return ErrGitHubInstallationRecovery
		}
		latest := !seen[record.Role]
		seen[record.Role] = true
		terminal := record.State == "qualified" || record.State == "pre_mint_failed" || record.State == "retry_authorized"
		if !latest && terminal && record.UpdatedAt.Before(now.Add(-7*24*time.Hour)) {
			remove = append(remove, record.ID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count > installationAttemptCap {
		return ErrGitHubInstallationRecovery
	}
	for _, id := range remove {
		if _, err := tx.ExecContext(ctx, "DELETE FROM github_installation_attempts WHERE attempt=?", id); err != nil {
			return ErrGitHubInstallationRecovery
		}
	}
	if count-len(remove) >= installationAttemptCap {
		return ErrGitHubInstallationRecovery
	}
	return nil
}

func (j *GitHubInstallationJournal) begin(ctx context.Context, role GitHubCredentialRole) (GitHubInstallationAttempt, error) {
	tx, err := j.store.db.BeginTx(ctx, nil)
	if err != nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	defer tx.Rollback()
	previous, err := j.latest(ctx, tx, role)
	if err != nil {
		return GitHubInstallationAttempt{}, err
	}
	if previous.ID != "" && previous.State != "qualified" && previous.State != "pre_mint_failed" && previous.State != "retry_authorized" {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	now := j.now().UTC()
	if now.IsZero() || now.Before(previous.UpdatedAt) || j.prune(ctx, tx, now) != nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	record := GitHubInstallationAttempt{Version: 1, ID: hex.EncodeToString(nonce[:]), Enrollment: j.enrollment, Role: role, State: "pre_mint", StartedAt: now, UpdatedAt: now}
	if j.write(ctx, tx, record, true) != nil || tx.Commit() != nil {
		return GitHubInstallationAttempt{}, ErrGitHubInstallationRecovery
	}
	return record, nil
}

func (j *GitHubInstallationJournal) transition(ctx context.Context, attempt GitHubInstallationAttempt, change func(*GitHubInstallationAttempt) error) error {
	tx, err := j.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrGitHubInstallationRecovery
	}
	defer tx.Rollback()
	record, err := j.latest(ctx, tx, attempt.Role)
	if err != nil || record.ID != attempt.ID {
		return ErrGitHubInstallationRecovery
	}
	now := j.now().UTC()
	if now.IsZero() || now.Before(record.UpdatedAt) {
		return ErrGitHubInstallationRecovery
	}
	if change(&record) != nil {
		return ErrGitHubInstallationRecovery
	}
	record.UpdatedAt = now
	if j.write(ctx, tx, record, false) != nil || tx.Commit() != nil {
		return ErrGitHubInstallationRecovery
	}
	return nil
}

// Acquire is explicit and never replays a token POST. Uncertain or interrupted
// work is latched across restart before a fresh attempt can reach HTTP.
func (j *GitHubInstallationJournal) Acquire(ctx context.Context, o GitHubInstallationOptions) (*GitHubInstallationTransport, error) {
	if !j.valid() || ctx == nil || o.Enrollment != j.enrollment || githubPermissions(o.Role) == nil {
		return nil, ErrGitHubInstallationRecovery
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	held, err := j.lock(ctx, o.Role)
	if err != nil {
		return nil, err
	}
	defer held.close()
	attempt, err := j.begin(ctx, o.Role)
	if err != nil {
		return nil, err
	}
	o.Now = j.now
	transport, err := acquireGitHubInstallation(ctx, o, func(ctx context.Context) error {
		return j.transition(ctx, attempt, func(a *GitHubInstallationAttempt) error {
			if a.State != "pre_mint" {
				return ErrGitHubInstallationRecovery
			}
			a.State = "mint_pending"
			a.MintAttempted = true
			return nil
		})
	})
	if err == nil {
		receipt := transport.Receipt()
		err = j.transition(ctx, attempt, func(a *GitHubInstallationAttempt) error {
			if a.State != "mint_pending" {
				return ErrGitHubInstallationRecovery
			}
			a.State = "qualified"
			a.Receipt = &receipt
			return nil
		})
		if err == nil {
			return transport, nil
		}
		transport.Close()
	}
	// The caller's cancelled context cannot erase uncertainty. Failed recovery
	// persistence leaves the earlier pre_mint/mint_pending latch intact.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_ = j.transition(cleanup, attempt, func(a *GitHubInstallationAttempt) error {
		if a.MintAttempted {
			a.State = "unknown"
		} else {
			a.State = "pre_mint_failed"
		}
		return nil
	})
	return nil, ErrGitHubInstallationRecovery
}

// AuthorizeRetry is a separate trusted recovery operation. It holds the same
// process lock as acquisition, verifies stopped work outside the transaction,
// then fences the exact attempt before recording nonsecret recovery evidence.
// It does not mint/revoke a token or retry any transport operation.
func (j *GitHubInstallationJournal) AuthorizeRetry(ctx context.Context, role GitHubCredentialRole, evidence GitHubInstallationRecovery, verifyStopped func(context.Context, GitHubInstallationAttempt, GitHubInstallationRecovery) error) error {
	if !j.valid() || ctx == nil || verifyStopped == nil || evidence.OwnerID != OwnerID || !validAuthHash(evidence.EvidenceHash) {
		return ErrGitHubInstallationRecovery
	}
	held, err := j.lock(ctx, role)
	if err != nil {
		return err
	}
	defer held.close()
	attempt, err := j.Latest(ctx, role)
	if err != nil || attempt.ID == "" || attempt.ID != evidence.AttemptID || evidence.At.Before(attempt.UpdatedAt) || evidence.At.After(j.now()) {
		return ErrGitHubInstallationRecovery
	}
	if attempt.State != "unknown" && attempt.State != "pre_mint" && attempt.State != "mint_pending" {
		return ErrGitHubInstallationRecovery
	}
	if verifyStopped(ctx, attempt, evidence) != nil || ctx.Err() != nil {
		return ErrGitHubInstallationRecovery
	}
	return j.transition(ctx, attempt, func(a *GitHubInstallationAttempt) error {
		if a.State != attempt.State {
			return ErrGitHubInstallationRecovery
		}
		a.State = "retry_authorized"
		a.Recovery = &evidence
		return nil
	})
}
