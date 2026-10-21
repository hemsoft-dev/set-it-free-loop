//go:build linux || darwin

package reviewer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installationJournalFixture(t *testing.T) (*GitHubInstallationJournal, *Store, string) {
	t.Helper()
	store, path := openFixtureStore(t)
	journal, err := NewGitHubInstallationJournal(context.Background(), store, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	return journal, store, path
}
func installationJournalReopen(t *testing.T, path string) *GitHubInstallationJournal {
	t.Helper()
	store, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	journal, err := NewGitHubInstallationJournal(context.Background(), store, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	return journal
}
func assertJournalContainsNoCredentials(t *testing.T, j *GitHubInstallationJournal) {
	t.Helper()
	rows, err := j.store.db.Query("SELECT record FROM github_installation_attempts")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) != nil {
			t.Fatal("journal read failed")
		}
		if strings.Contains(string(raw), "synthetic-app-jwt") || strings.Contains(string(raw), "synthetic-installation-") {
			t.Fatal("credential persisted in journal")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{j.store.path, j.store.path + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "synthetic-app-jwt") || strings.Contains(string(raw), "synthetic-installation-") {
			t.Fatal("credential persisted in SQLite bytes")
		}
	}
	for _, value := range []any{j, *j} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(raw)} {
			if !strings.Contains(text, "redacted") || strings.Contains(text, j.directory) {
				t.Fatal("private journal handle exposed")
			}
		}
	}
}

func TestGitHubInstallationJournalTLSWriteAheadBothRolesAndRestart(t *testing.T) {
	j, _, path := installationJournalFixture(t)
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		f, o := installationHTTPFixture(t, role)
		observed := false
		f.mutate = func(p string, _ map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				record, err := j.Latest(context.Background(), role)
				if err != nil || record.State != "mint_pending" || !record.MintAttempted || record.Receipt != nil {
					t.Error("POST arrived without committed mint intent")
				}
				observed = true
			}
		}
		transport, err := j.Acquire(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		transport.Close()
		record, err := j.Latest(context.Background(), role)
		if err != nil || record.State != "qualified" || record.Receipt == nil || record.Receipt.Role != role || record.Enrollment != alphaEnrollment() || f.mints != 1 || f.calls != 7 || !observed {
			t.Fatal("qualified role metadata missing", err)
		}
		reopened := installationJournalReopen(t, path)
		got, err := reopened.Latest(context.Background(), role)
		if err != nil || Digest(got) != Digest(record) || f.mints != 1 {
			t.Fatal("restart changed metadata or minted credentials")
		}
	}
	assertJournalContainsNoCredentials(t, j)
}

func TestGitHubInstallationJournalUnknownAndPostMintRefusalNeverReplay(t *testing.T) {
	for _, mode := range []string{"lost mint", "bad post-mint identity", "qualification write failure", "unknown write failure"} {
		t.Run(mode, func(t *testing.T) {
			j, store, path := installationJournalFixture(t)
			f, o := installationHTTPFixture(t, GitHubReader)
			switch mode {
			case "lost mint":
				f.loseMint = true
			case "bad post-mint identity":
				f.mutate = func(p string, d map[string]any) {
					if strings.HasPrefix(p, "/users/") {
						d["id"] = int64(999)
					}
				}
			case "qualification write failure", "unknown write failure":
				states := "json_extract(NEW.record,'$.State')='qualified'"
				if mode == "unknown write failure" {
					states += " OR json_extract(NEW.record,'$.State')='unknown'"
				}
				_, err := store.db.Exec("CREATE TRIGGER reject_completion BEFORE UPDATE ON github_installation_attempts WHEN " + states + " BEGIN SELECT RAISE(ABORT,'synthetic-private-db-error'); END")
				if err != nil {
					t.Fatal(err)
				}
			}
			transport, err := j.Acquire(context.Background(), o)
			if transport != nil || !errors.Is(err, ErrGitHubInstallationRecovery) || strings.Contains(err.Error(), "synthetic-private") || f.mints != 1 {
				t.Fatal("failed mint/qualification returned transport or replayed", err)
			}
			record, err := j.Latest(context.Background(), GitHubReader)
			if err != nil || (record.State != "unknown" && record.State != "mint_pending") || !record.MintAttempted {
				t.Fatal("uncertainty not retained", record.State, err)
			}
			calls := f.calls
			reopened := installationJournalReopen(t, path)
			transport, err = reopened.Acquire(context.Background(), o)
			if transport != nil || !errors.Is(err, ErrGitHubInstallationRecovery) || f.calls != calls || f.mints != 1 {
				t.Fatal("restart replayed uncertain token POST")
			}
			assertJournalContainsNoCredentials(t, j)
		})
	}
}

func TestGitHubInstallationJournalPersistenceRefusalPreventsMint(t *testing.T) {
	for _, stage := range []string{"begin", "mint intent"} {
		t.Run(stage, func(t *testing.T) {
			j, store, _ := installationJournalFixture(t)
			f, o := installationHTTPFixture(t, GitHubReader)
			statement := "CREATE TRIGGER refuse BEFORE INSERT ON github_installation_attempts BEGIN SELECT RAISE(ABORT,'private-write-error'); END"
			if stage == "mint intent" {
				statement = "CREATE TRIGGER refuse BEFORE UPDATE ON github_installation_attempts WHEN json_extract(NEW.record,'$.State')='mint_pending' BEGIN SELECT RAISE(ABORT,'private-write-error'); END"
			}
			if _, err := store.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if transport, err := j.Acquire(context.Background(), o); transport != nil || !errors.Is(err, ErrGitHubInstallationRecovery) || strings.Contains(err.Error(), "private-write-error") {
				t.Fatal("persistence failure leaked or admitted HTTP")
			}
			expectedCalls := 0
			if stage == "mint intent" {
				expectedCalls = 2
			}
			if f.mints != 0 || f.calls != expectedCalls {
				t.Fatal("token dispatched before durable intent", f.calls, f.mints)
			}
		})
	}
}

func TestGitHubInstallationJournalExplicitRecoveryRequiresMatchingProof(t *testing.T) {
	j, _, _ := installationJournalFixture(t)
	f, o := installationHTTPFixture(t, GitHubReader)
	f.loseMint = true
	if transport, err := j.Acquire(context.Background(), o); transport != nil || err == nil {
		t.Fatal("lost response unexpectedly qualified")
	}
	attempt, err := j.Latest(context.Background(), GitHubReader)
	if err != nil {
		t.Fatal(err)
	}
	proof := GitHubInstallationRecovery{AttemptID: attempt.ID, OwnerID: OwnerID, At: controlTime, EvidenceHash: Digest("synthetic stopped acquisition")}
	verified := 0
	check := func(ctx context.Context, a GitHubInstallationAttempt, e GitHubInstallationRecovery) error {
		verified++
		if a.ID != attempt.ID || Digest(e) != Digest(proof) {
			return errors.New("foreign proof")
		}
		return nil
	}
	for _, mutate := range []func(*GitHubInstallationRecovery){
		func(p *GitHubInstallationRecovery) { p.OwnerID++ }, func(p *GitHubInstallationRecovery) { p.AttemptID = strings.Repeat("f", 32) }, func(p *GitHubInstallationRecovery) { p.At = controlTime.Add(-time.Second) }, func(p *GitHubInstallationRecovery) { p.At = controlTime.Add(time.Second) }, func(p *GitHubInstallationRecovery) { p.EvidenceHash = "" },
	} {
		bad := proof
		mutate(&bad)
		if j.AuthorizeRetry(context.Background(), GitHubReader, bad, check) == nil {
			t.Fatal("foreign or missing recovery proof accepted")
		}
	}
	if verified != 0 || f.mints != 1 {
		t.Fatal("invalid proof reached callback/network")
	}
	if j.AuthorizeRetry(context.Background(), GitHubReader, proof, nil) == nil {
		t.Fatal("missing stopped-work check accepted")
	}
	if j.AuthorizeRetry(context.Background(), GitHubReader, proof, func(context.Context, GitHubInstallationAttempt, GitHubInstallationRecovery) error {
		return errors.New("not stopped")
	}) == nil {
		t.Fatal("failed stop check accepted")
	}
	if err := j.AuthorizeRetry(context.Background(), GitHubReader, proof, check); err != nil {
		t.Fatal(err)
	}
	if verified != 1 || f.mints != 1 {
		t.Fatal("recovery itself performed network work")
	}
	authorized, _ := j.Latest(context.Background(), GitHubReader)
	if authorized.State != "retry_authorized" || Digest(authorized.Recovery) != Digest(&proof) {
		t.Fatal("proof not persisted")
	}
	f.loseMint = false
	transport, err := j.Acquire(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	transport.Close()
	current, _ := j.Latest(context.Background(), GitHubReader)
	if current.ID == attempt.ID || current.State != "qualified" || f.mints != 2 {
		t.Fatal("explicit fresh qualification not recorded")
	}
	if j.AuthorizeRetry(context.Background(), GitHubReader, proof, check) == nil {
		t.Fatal("old recovery proof reused")
	}
}

func TestGitHubInstallationJournalChild(t *testing.T) {
	mode := os.Getenv("SFL_JOURNAL_TEST_MODE")
	if mode == "" {
		t.Skip("explicit child fixture only")
	}
	store, err := OpenStore(os.Getenv("SFL_JOURNAL_TEST_DB"), alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	j, err := NewGitHubInstallationJournal(context.Background(), store, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var transport http.RoundTripper = egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected child HTTP")
	})
	if mode == "crash" {
		base, err := url.Parse(os.Getenv("SFL_JOURNAL_TEST_SERVER"))
		if err != nil || base.Scheme != "https" || base.Hostname() != "127.0.0.1" {
			t.Fatal("only explicit fake loopback TLS allowed")
		}
		pin := os.Getenv("SFL_JOURNAL_TEST_CERT")
		if !validAuthHash(pin) {
			t.Fatal("missing fake certificate pin")
		}
		inner := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("fake certificate count")
			}
			sum := sha256.Sum256(state.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != pin {
				return errors.New("fake certificate mismatch")
			}
			return nil
		}}}
		defer inner.CloseIdleConnections()
		transport = installationRoute{base, inner, t}
	}
	timeout := 10 * time.Second
	if mode == "locked" {
		timeout = 150 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	got, err := j.Acquire(ctx, GitHubInstallationOptions{alphaEnrollment(), GitHubReader, []byte("synthetic-app-jwt"), transport, func() time.Time { return controlTime }})
	if got != nil {
		got.Close()
	}
	if mode == "crash" {
		t.Fatal("crash child returned before parent killed it")
	}
	if !errors.Is(err, ErrGitHubInstallationRecovery) || calls.Load() != 0 {
		t.Fatal("process lock permitted HTTP")
	}
}
func journalChild(t *testing.T, mode, path string, extra ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestGitHubInstallationJournalChild$", "-test.v")
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "SFL_JOURNAL_TEST_MODE=" + mode, "SFL_JOURNAL_TEST_DB=" + path}, extra...)
	return cmd
}

func TestGitHubInstallationJournalSameRoleLockAcrossProcesses(t *testing.T) {
	j, _, path := installationJournalFixture(t)
	held, err := j.lock(context.Background(), GitHubReader)
	if err != nil {
		t.Fatal(err)
	}
	defer held.close()
	cmd := journalChild(t, "locked", path)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("child bypassed or failed private lock", err, string(raw))
	}
	attempt, err := j.Latest(context.Background(), GitHubReader)
	if err != nil || attempt.ID != "" {
		t.Fatal("blocked child created attempt")
	}
}

func TestGitHubInstallationJournalKilledOwnerRetainsUnknownAcrossRestart(t *testing.T) {
	for _, point := range []string{"pre_mint", "mint_pending"} {
		t.Run(point, func(t *testing.T) {
			j, _, path := installationJournalFixture(t)
			f, o := installationHTTPFixture(t, GitHubReader)
			route := o.Transport.(installationRoute)
			conn, err := tls.Dial("tcp", route.base.Host, route.inner.(*http.Transport).TLSClientConfig)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
			conn.Close()
			accepted, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.mu.Lock()
			f.mutate = func(path string, _ map[string]any) {
				if point == "pre_mint" && path == "/app" || point == "mint_pending" && strings.HasSuffix(path, "/access_tokens") {
					close(accepted)
					<-release
				}
			}
			f.mu.Unlock()
			cmd := journalChild(t, "crash", path, "SFL_JOURNAL_TEST_SERVER="+route.base.String(), "SFL_JOURNAL_TEST_CERT="+hex.EncodeToString(sum[:]))
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			t.Cleanup(func() { cmd.Process.Kill() })
			select {
			case <-accepted:
			case err := <-waited:
				unblock()
				t.Fatal("child exited before target", err)
			case <-time.After(5 * time.Second):
				unblock()
				t.Fatal("child never reached fake target")
			}
			record, err := j.Latest(context.Background(), GitHubReader)
			if err != nil || record.State != point || record.MintAttempted != (point == "mint_pending") {
				t.Fatal("child HTTP preceded durable stage", record.State, err)
			}
			proof := GitHubInstallationRecovery{AttemptID: record.ID, OwnerID: OwnerID, At: controlTime, EvidenceHash: Digest("stopped synthetic child")}
			checked := false
			short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err = j.AuthorizeRetry(short, GitHubReader, proof, func(context.Context, GitHubInstallationAttempt, GitHubInstallationRecovery) error {
				checked = true
				return nil
			})
			cancel()
			if err == nil || checked {
				t.Fatal("recovery raced a live owner")
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				if err == nil {
					t.Fatal("child was not killed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("killed child did not stop")
			}
			unblock()
			f.mu.Lock()
			f.mutate = nil
			calls, mints := f.calls, f.mints
			f.mu.Unlock()
			reopened := installationJournalReopen(t, path)
			if got, err := reopened.Acquire(context.Background(), o); got != nil || !errors.Is(err, ErrGitHubInstallationRecovery) {
				t.Fatal("killed acquisition replayed")
			}
			f.mu.Lock()
			same := f.calls == calls && f.mints == mints
			f.mu.Unlock()
			if !same {
				t.Fatal("restart made HTTP request")
			}
			if err := reopened.AuthorizeRetry(context.Background(), GitHubReader, proof, func(context.Context, GitHubInstallationAttempt, GitHubInstallationRecovery) error {
				if cmd.ProcessState == nil {
					return errors.New("child not waited")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Acquire(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			got.Close()
			f.mu.Lock()
			newMints := f.mints
			f.mu.Unlock()
			if newMints != mints+1 {
				t.Fatal("explicit recovery did not make one new mint")
			}
			assertJournalContainsNoCredentials(t, reopened)
		})
	}
}

func TestGitHubInstallationJournalHistoryCapAndTerminalRetention(t *testing.T) {
	j, store, _ := installationJournalFixture(t)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < installationAttemptCap; i++ {
		receipt := &GitHubInstallationReceipt{alphaEnrollment(), GitHubReader, controlTime, controlTime.Add(time.Hour)}
		a := GitHubInstallationAttempt{Version: 1, ID: fmt.Sprintf("%032x", i+1), Enrollment: alphaEnrollment(), Role: GitHubReader, State: "qualified", StartedAt: controlTime, UpdatedAt: controlTime, MintAttempted: true, Receipt: receipt}
		if i == 0 {
			a.State = "unknown"
			a.Receipt = nil
		}
		if err := j.write(context.Background(), tx, a, true); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f, o := installationHTTPFixture(t, GitHubReader)
	if got, err := j.Acquire(context.Background(), o); got != nil || !errors.Is(err, ErrGitHubInstallationRecovery) || f.calls != 0 {
		t.Fatal("cap admitted HTTP")
	}
	later := controlTime.Add(8 * 24 * time.Hour)
	j.now = func() time.Time { return later }
	f.mutate = func(path string, doc map[string]any) {
		if strings.HasSuffix(path, "/access_tokens") {
			doc["expires_at"] = later.Add(time.Hour)
		}
	}
	got, err := j.Acquire(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got.Close()
	var count int
	if store.db.QueryRow("SELECT count(*) FROM github_installation_attempts").Scan(&count) != nil || count != 3 {
		t.Fatal("terminal retention removed latest or unresolved evidence", count)
	}
	var raw []byte
	if store.db.QueryRow("SELECT record FROM github_installation_attempts WHERE attempt=?", fmt.Sprintf("%032x", 1)).Scan(&raw) != nil || !strings.Contains(string(raw), "unknown") {
		t.Fatal("unresolved old attempt pruned")
	}
}

func TestGitHubInstallationJournalRefusesForeignAndMalformedState(t *testing.T) {
	j, store, _ := installationJournalFixture(t)
	f, o := installationHTTPFixture(t, GitHubReader)
	bad := o
	bad.Enrollment.InstallationID++
	if got, err := j.Acquire(context.Background(), bad); got != nil || err == nil || f.calls != 0 {
		t.Fatal("foreign enrollment admitted")
	}
	if _, err := NewGitHubInstallationJournal(context.Background(), store, nil); err == nil {
		t.Fatal("ambient clock accepted")
	}
	if _, err := store.db.Exec("INSERT INTO github_installation_attempts(role,attempt,record) VALUES(?,?,?)", string(GitHubReader), strings.Repeat("a", 32), []byte(`{"Version":1,"State":"pre_mint_failed","State":"qualified"}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := j.Acquire(context.Background(), o); got != nil || err == nil || f.calls != 0 {
		t.Fatal("malformed journal admitted HTTP")
	}
}

func TestGitHubInstallationJournalRefusesSymlinkNamespace(t *testing.T) {
	store, _ := openFixtureStore(t)
	outside := t.TempDir()
	path := filepath.Join(filepath.Dir(store.path), "github-installation-attempts")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGitHubInstallationJournal(context.Background(), store, func() time.Time { return controlTime }); err == nil {
		t.Fatal("symlink journal namespace accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink target mutated")
	}
}
