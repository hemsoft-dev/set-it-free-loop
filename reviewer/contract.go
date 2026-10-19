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
	"reflect"
	"regexp"
	"strings"
	"time"
)

const RepositoryID int64 = 1169772257
const OwnerID int64 = 8227352
const PolicyVersion = "alpha-policy-1"

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
	JobID         string     `json:"job_id"`
	PolicyVersion string     `json:"policy_version"`
	PacketHash    string     `json:"packet_hash"`
	Provider      Provider   `json:"provider"`
	Round         int        `json:"round"`
	SnapshotHash  string     `json:"snapshot_hash"`
	ConfigHash    string     `json:"config_hash"`
	Findings      []Finding  `json:"findings"`
	Judgments     []Judgment `json:"judgments"`
	Retractions   []string   `json:"retractions"`
	Coverage      []Coverage `json:"coverage"`
	Usage         Usage      `json:"usage"`
}

type Session struct {
	JobID         string   `json:"job_id"`
	PolicyVersion string   `json:"policy_version"`
	Snapshot      Snapshot `json:"snapshot"`
	SnapshotHash  string   `json:"snapshot_hash"`
	PacketHash    string   `json:"packet_hash"`
	ConfigHash    string   `json:"config_hash"`
	Identity      Identity `json:"identity"`
	Round         int      `json:"round"`
	Packet        string   `json:"packet"`
	PeerReports   []Report `json:"peer_reports"`
	FocusIDs      []string `json:"focus_ids"`
	Limits        Limits   `json:"limits"`
}

func NewSession(job string, snap Snapshot, configHash string, p Provider, round int, packet string, peers []Report, focus []string, limits Limits) (Session, error) {
	i, err := PinnedIdentity(p)
	if err != nil {
		return Session{}, err
	}
	s := Session{JobID: job, PolicyVersion: PolicyVersion, Snapshot: snap, SnapshotHash: Digest(snap), PacketHash: Digest(packet), ConfigHash: configHash, Identity: i, Round: round, Packet: packet, PeerReports: peers, FocusIDs: focus, Limits: limits}
	return s, s.Validate()
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
	if strings.TrimSpace(s.Packet) == "" || len(s.Packet) > s.Limits.InputBytes || s.PacketHash != Digest(s.Packet) || s.SnapshotHash != Digest(s.Snapshot) || s.PolicyVersion != PolicyVersion {
		return errors.New("missing evidence packet or mismatched immutable headers")
	}
	if s.Round < 3 && len(s.FocusIDs) != 0 {
		return errors.New("focus IDs are only valid for R3")
	}
	if (s.Round == 2 && len(s.PeerReports) != 2) || (s.Round == 3 && len(s.PeerReports) != 4) {
		return errors.New("missing frozen peer reports")
	}
	seen := map[string]bool{}
	known := map[string]bool{}
	for _, pr := range s.PeerReports {
		if !pr.Provider.Valid() || pr.Round < 1 || pr.Round >= s.Round {
			return errors.New("unexpected peer report round/provider")
		}
		key := fmt.Sprintf("%s:%d", pr.Provider, pr.Round)
		if seen[key] {
			return errors.New("duplicate peer report")
		}
		seen[key] = true
		ps := s
		ps.Round = pr.Round
		ps.Identity, _ = PinnedIdentity(pr.Provider)
		ps.PeerReports = nil
		ps.FocusIDs = nil
		for _, earlier := range s.PeerReports {
			if earlier.Round < pr.Round {
				ps.PeerReports = append(ps.PeerReports, earlier)
			}
		}
		if err := ValidateReport(pr, ps); err != nil {
			return fmt.Errorf("invalid frozen peer report: %w", err)
		}
		for _, f := range pr.Findings {
			known[f.ID] = true
		}
	}
	if len(s.FocusIDs) > 160 {
		return errors.New("too many focus IDs")
	}
	focus := map[string]bool{}
	for _, id := range s.FocusIDs {
		if !known[id] || focus[id] {
			return errors.New("invalid or duplicate focus ID")
		}
		focus[id] = true
	}
	return nil
}

func DecodeStrict[T any](data []byte, max int) (T, error) {
	var out T
	if max <= 0 || len(data) > max {
		return out, errors.New("JSON exceeds byte limit")
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return out, err
	}
	if err := requireShape(data, reflect.TypeOf(out), 0); err != nil {
		return out, err
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

func uniqueJSONKeys(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("structured JSON nesting limit")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate or invalid JSON key")
			}
			keys[name] = true
		}
		if err := uniqueJSONKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// Require every schema field and reject nulls. encoding/json otherwise accepts
// null for structs/scalars and silently assigns zero values.
func requireShape(data []byte, t reflect.Type, depth int) error {
	if depth > 32 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null or excessive nesting in structured output")
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			v, ok := fields[name]
			if !ok {
				return fmt.Errorf("missing structured field %s", name)
			}
			if err := requireShape(v, f.Type, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, v := range values {
			if err := requireShape(v, t.Elem(), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateReport(r Report, s Session) error {
	if r.JobID != s.JobID || r.PolicyVersion != s.PolicyVersion || r.PacketHash != s.PacketHash || r.Provider != s.Identity.Provider || r.Round != s.Round || r.SnapshotHash != Digest(s.Snapshot) || r.ConfigHash != s.ConfigHash {
		return errors.New("report provenance mismatch")
	}
	if len(r.Findings) > s.Limits.MaxFindings || len(r.Judgments) > s.Limits.MaxFindings*4 || len(r.Retractions) > s.Limits.MaxFindings*3 || len(r.Coverage) > 256 {
		return errors.New("report collection limit exceeded")
	}
	if r.Usage.Turns < 0 || r.Usage.Turns > s.Limits.MaxTurns || r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 {
		return errors.New("invalid usage or turn limit exceeded")
	}
	seen := map[string]bool{}
	prior := map[string]Finding{}
	required := map[string]bool{}
	focus := map[string]bool{}
	for _, id := range s.FocusIDs {
		focus[id] = true
	}
	for _, pr := range s.PeerReports {
		for _, f := range pr.Findings {
			prior[f.ID] = f
			if pr.Provider != r.Provider && (s.Round == 2 || focus[f.ID]) {
				required[f.ID] = true
			}
		}
	}
	for _, f := range r.Findings {
		_, existing := prior[f.ID]
		if !regexp.MustCompile(`^`+string(r.Provider)+`:[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(f.ID) || seen[f.ID] || existing {
			return errors.New("invalid or duplicate finding ID")
		}
		seen[f.ID] = true
		if len(f.Title) == 0 || len(f.Title) > 240 || len(f.Body) == 0 || len(f.Body) > 8192 || f.Severity < 0 || f.Severity > 3 || !validPath(f.Anchor.Path) || f.Anchor.Line < 1 || len(f.Anchor.Quote) == 0 || len(f.Anchor.Quote) > 4096 || f.Anchor.Revision != s.Snapshot.Head {
			return errors.New("invalid finding")
		}
	}
	if s.Round == 1 && (len(r.Judgments) != 0 || len(r.Retractions) != 0) {
		return errors.New("R1 cannot judge peers or retract prior findings")
	}
	judged := map[string]bool{}
	for _, j := range r.Judgments {
		_, known := prior[j.FindingID]
		own := strings.HasPrefix(j.FindingID, string(r.Provider)+":")
		if !known || judged[j.FindingID] || len(j.Reason) == 0 || len(j.Reason) > 8192 || len(j.Evidence) > 4 {
			return errors.New("invalid judgment reference or bounds")
		}
		if s.Round == 3 && !focus[j.FindingID] {
			return errors.New("R3 judgment outside captured focus")
		}
		if own && (s.Round != 3 || j.Verdict != "respond") {
			return errors.New("invalid own-finding judgment")
		}
		cited := true
		switch j.Verdict {
		case "confirm", "reject":
			if own {
				return errors.New("cannot confirm or reject own finding")
			}
		case "cannot_verify":
			cited = false
			if own {
				return errors.New("invalid self judgment")
			}
		case "respond":
			if !own || s.Round != 3 {
				return errors.New("response must address own R3 finding")
			}
		default:
			if !strings.HasPrefix(j.Verdict, "duplicate-of:") {
				return errors.New("unknown judgment verdict")
			}
			id := strings.TrimPrefix(j.Verdict, "duplicate-of:")
			if _, ok := prior[id]; !ok || !strings.HasPrefix(id, string(r.Provider)+":") || own {
				return errors.New("invalid duplicate reference")
			}
		}
		if cited && len(j.Evidence) == 0 {
			return errors.New("judgment lacks source evidence")
		}
		for _, a := range j.Evidence {
			if !validAnchor(a, s.Snapshot) {
				return errors.New("invalid judgment anchor")
			}
		}
		judged[j.FindingID] = true
	}
	for id := range required {
		if !judged[id] {
			return errors.New("missing peer finding disposition")
		}
	}
	retracted := map[string]bool{}
	for _, id := range r.Retractions {
		if _, ok := prior[id]; !ok || !strings.HasPrefix(id, string(r.Provider)+":") || retracted[id] || (s.Round == 3 && !focus[id]) {
			return errors.New("invalid origin retraction")
		}
		retracted[id] = true
	}
	for _, c := range r.Coverage {
		if (c.Path != "" && !validPath(c.Path)) || len(c.Reason) == 0 || len(c.Reason) > 1024 {
			return errors.New("invalid coverage record")
		}
	}
	return nil
}

func validAnchor(a Anchor, s Snapshot) bool {
	return validPath(a.Path) && a.Line > 0 && len(a.Quote) > 0 && len(a.Quote) <= 4096 && a.Revision == s.Head
}
