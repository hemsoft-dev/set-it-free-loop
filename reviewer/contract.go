// Package reviewer contains the offline adversarial alpha contracts. It has no
// credential-bearing launcher, deployment entry point, or GitHub write client.
package reviewer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const RepositoryID int64 = 1169772257
const OwnerID int64 = 8227352

type Provider string

const (
	Codex  Provider = "codex"
	Claude Provider = "claude"
)

func (p Provider) Valid() bool { return p == Codex || p == Claude }

type Snapshot struct {
	RepositoryID     int64  `json:"repository_id"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	PR               int    `json:"pr"`
	Head             string `json:"head"`
	Base             string `json:"base"`
	MergeBase        string `json:"merge_base"`
}

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (s Snapshot) Validate() error {
	if s.RepositoryID != RepositoryID || s.HeadRepositoryID <= 0 || s.PR <= 0 {
		return errors.New("snapshot is outside the alpha repository or lacks provenance")
	}
	for _, id := range []string{s.Head, s.Base, s.MergeBase} {
		if !objectID.MatchString(id) {
			return errors.New("snapshot requires immutable object IDs")
		}
	}
	return nil
}

func Digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("internal non-serializable contract")
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type Identity struct {
	Provider      Provider `json:"provider"`
	Model         string   `json:"model"`
	ClientVersion string   `json:"client_version"`
	AuthMode      string   `json:"auth_mode"`
}

// PinnedIdentity is a tested invocation pin, not proof of model entitlement.
func PinnedIdentity(p Provider) (Identity, error) {
	switch p {
	case Codex:
		return Identity{Codex, "gpt-6-astra", "0.163.0", "chatgpt"}, nil
	case Claude:
		return Identity{Claude, "claude-opus-5-5", "2.1.303", "claude_subscription"}, nil
	default:
		return Identity{}, errors.New("unknown provider")
	}
}

func (i Identity) Validate() error {
	p, err := PinnedIdentity(i.Provider)
	if err != nil || p != i {
		return errors.New("unlisted model, client, provider or subscription auth mode")
	}
	return nil
}

type Limits struct {
	Timeout     time.Duration `json:"timeout_ns"`
	InputBytes  int           `json:"input_bytes"`
	OutputBytes int           `json:"output_bytes"`
	ReaderCalls int           `json:"reader_calls"`
	ReaderBytes int           `json:"reader_bytes"`
	MaxFindings int           `json:"max_findings"`
	MaxTurns    int           `json:"max_turns"`
}

func DefaultLimits() Limits {
	return Limits{5 * time.Minute, 256 << 10, 128 << 10, 128, 2 << 20, 40, 24}
}

func (l Limits) Validate() error {
	m := DefaultLimits()
	if l.Timeout <= 0 || l.Timeout > m.Timeout {
		return errors.New("invalid phase deadline")
	}
	for _, pair := range [][2]int{{l.InputBytes, m.InputBytes}, {l.OutputBytes, m.OutputBytes}, {l.ReaderCalls, m.ReaderCalls}, {l.ReaderBytes, m.ReaderBytes}, {l.MaxFindings, m.MaxFindings}, {l.MaxTurns, m.MaxTurns}} {
		if pair[0] <= 0 || pair[0] > pair[1] {
			return errors.New("invalid resource bound")
		}
	}
	return nil
}

type Anchor struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Quote    string `json:"quote"`
	Revision string `json:"revision"`
}

type Finding struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Severity int    `json:"severity"`
	Anchor   Anchor `json:"anchor"`
}

type Judgment struct {
	FindingID string   `json:"finding_id"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	Evidence  []Anchor `json:"evidence"`
}

type Coverage struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	Known        bool  `json:"known"`
	Turns        int   `json:"turns"`
}

type Report struct {
	Provider     Provider   `json:"provider"`
	Round        int        `json:"round"`
	SnapshotHash string     `json:"snapshot_hash"`
	ConfigHash   string     `json:"config_hash"`
	Findings     []Finding  `json:"findings"`
	Judgments    []Judgment `json:"judgments"`
	Retractions  []string   `json:"retractions"`
	Coverage     []Coverage `json:"coverage"`
	Usage        Usage      `json:"usage"`
}

type Session struct {
	JobID       string   `json:"job_id"`
	Snapshot    Snapshot `json:"snapshot"`
	ConfigHash  string   `json:"config_hash"`
	Identity    Identity `json:"identity"`
	Round       int      `json:"round"`
	Packet      string   `json:"packet"`
	PeerReports []Report `json:"peer_reports"`
	Limits      Limits   `json:"limits"`
}

func (s Session) Validate() error {
	if err := s.Snapshot.Validate(); err != nil {
		return err
	}
	if err := s.Identity.Validate(); err != nil {
		return err
	}
	if err := s.Limits.Validate(); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`).MatchString(s.JobID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ConfigHash) || s.Round < 1 || s.Round > 3 {
		return errors.New("invalid job contract")
	}
	if s.Round == 1 && len(s.PeerReports) != 0 {
		return errors.New("R1 must not receive peer reports")
	}
	return nil
}

func DecodeStrict[T any](data []byte, max int) (T, error) {
	var out T
	if max <= 0 || len(data) > max {
		return out, errors.New("JSON exceeds byte limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return out, fmt.Errorf("invalid structured output: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return out, errors.New("expected exactly one JSON value")
	}
	return out, nil
}

func ValidateReport(r Report, s Session) error {
	if r.Provider != s.Identity.Provider || r.Round != s.Round || r.SnapshotHash != Digest(s.Snapshot) || r.ConfigHash != s.ConfigHash {
		return errors.New("report provenance mismatch")
	}
	if len(r.Findings) > s.Limits.MaxFindings || len(r.Judgments) > s.Limits.MaxFindings*4 || len(r.Retractions) > s.Limits.MaxFindings*3 || len(r.Coverage) > 256 {
		return errors.New("report collection limit exceeded")
	}
	if r.Usage.Turns < 0 || r.Usage.Turns > s.Limits.MaxTurns || r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 {
		return errors.New("invalid usage or turn limit exceeded")
	}
	seen := map[string]bool{}
	for _, f := range r.Findings {
		if !regexp.MustCompile(`^`+string(r.Provider)+`:[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(f.ID) || seen[f.ID] {
			return errors.New("invalid or duplicate finding ID")
		}
		seen[f.ID] = true
		if len(f.Title) == 0 || len(f.Title) > 240 || len(f.Body) == 0 || len(f.Body) > 8192 || f.Severity < 0 || f.Severity > 3 || !validPath(f.Anchor.Path) || f.Anchor.Line < 1 || len(f.Anchor.Quote) == 0 || len(f.Anchor.Quote) > 4096 || f.Anchor.Revision != s.Snapshot.Head {
			return errors.New("invalid finding")
		}
	}
	return nil
}
