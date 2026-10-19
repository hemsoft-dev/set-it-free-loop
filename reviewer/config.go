package reviewer

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	Adversarial     Mode = "adversarial"
	IndependentPair Mode = "independent-pair"
	CodexOnly       Mode = "codex-only"
	ClaudeOnly      Mode = "claude-only"
)

type Config struct {
	Version       int           `json:"version"`
	Policy        string        `json:"policy"`
	Mode          Mode          `json:"mode"`
	PeerRounds    int           `json:"peer_rounds"`
	Codex         Identity      `json:"codex"`
	Claude        Identity      `json:"claude"`
	Limits        Limits        `json:"limits"`
	DailyReviews  int           `json:"daily_reviews"`
	QuotaWait     time.Duration `json:"quota_wait_ns"`
	RetentionDays int           `json:"retention_days"`
}

func DefaultConfig() Config {
	c, _ := PinnedIdentity(Codex)
	h, _ := PinnedIdentity(Claude)
	return Config{1, PolicyVersion, Adversarial, 1, c, h, DefaultLimits(), 4, 24 * time.Hour, 7}
}

func (c Config) Validate() error {
	if c.Version != 1 || c.Policy != PolicyVersion {
		return errors.New("unsupported config or policy version")
	}
	switch c.Mode {
	case Adversarial, IndependentPair, CodexOnly, ClaudeOnly:
	default:
		return errors.New("unlisted review mode")
	}
	if c.PeerRounds < 1 || c.PeerRounds > 2 || (c.Mode != Adversarial && c.PeerRounds != 1) {
		return errors.New("invalid peer-round setting")
	}
	if err := c.Codex.Validate(); err != nil {
		return err
	}
	if c.Codex.Provider != Codex {
		return errors.New("invalid Codex mapping")
	}
	if err := c.Claude.Validate(); err != nil {
		return err
	}
	if c.Claude.Provider != Claude {
		return errors.New("invalid Claude mapping")
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if c.DailyReviews < 1 || c.DailyReviews > 20 || c.QuotaWait <= 0 || c.QuotaWait > 24*time.Hour || c.RetentionDays < 1 || c.RetentionDays > 30 {
		return errors.New("invalid job/day/retention budget")
	}
	return nil
}

func (c Config) Hash() string { return Digest(c) }
func (c Config) Providers() []Provider {
	switch c.Mode {
	case CodexOnly:
		return []Provider{Codex}
	case ClaudeOnly:
		return []Provider{Claude}
	default:
		return []Provider{Codex, Claude}
	}
}
func (c Config) Rounds() int {
	if c.Mode == Adversarial {
		return 1 + c.PeerRounds
	}
	return 1
}
func (c Config) MaxSessions() int { return len(c.Providers()) * c.Rounds() }

type CommandEvent struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Command struct {
	EventID  int64             `json:"event_id"`
	AuthorID int64             `json:"author_id"`
	BodyHash string            `json:"body_hash"`
	Action   string            `json:"action"`
	Values   map[string]string `json:"values"`
}

var ErrIgnored = errors.New("not an authorized owner command")

// ParseCommand consumes trusted GitHub author/event metadata, never metadata
// copied from the PR body or a model report. It performs no inference or write.
func ParseCommand(event CommandEvent) (Command, error) {
	if event.AuthorID != OwnerID || !strings.HasPrefix(event.Body, "/sfl ") {
		return Command{}, ErrIgnored
	}
	if event.ID <= 0 || event.CreatedAt.IsZero() || !event.CreatedAt.Equal(event.UpdatedAt) || len(event.Body) > 2048 {
		return Command{}, errors.New("edited, oversized or incomplete command")
	}
	parts := strings.Fields(event.Body)
	if len(parts) < 2 || len(parts) > 7 {
		return Command{}, errors.New("invalid command")
	}
	c := Command{EventID: event.ID, AuthorID: event.AuthorID, BodyHash: Digest(event.Body), Values: map[string]string{}}
	if len(parts) == 2 && parts[1] == "cancel" {
		c.Action = "cancel"
		return c, nil
	}
	if parts[1] != "config" || len(parts) < 3 {
		return Command{}, errors.New("use config, config show, config reset or cancel")
	}
	if len(parts) == 3 && (parts[2] == "show" || parts[2] == "reset") {
		c.Action = parts[2]
		return c, nil
	}
	c.Action = "configure"
	for _, part := range parts[2:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" {
			return Command{}, errors.New("invalid configuration assignment")
		}
		switch key {
		case "mode", "rounds", "model.codex", "model.claude":
		default:
			return Command{}, errors.New("unlisted configuration key")
		}
		if _, ok := c.Values[key]; ok {
			return Command{}, errors.New("duplicate configuration key")
		}
		c.Values[key] = value
	}
	return c, nil
}

// ApplyConfig returns a proposed next-request value. The durable controller
// owns acknowledgment ordering, accepted-command immutability and cancellation.
func ApplyConfig(current, defaults Config, command Command) (Config, error) {
	if err := defaults.Validate(); err != nil {
		return Config{}, err
	}
	if command.AuthorID != OwnerID || command.EventID <= 0 || len(command.BodyHash) != 64 {
		return Config{}, ErrIgnored
	}
	if command.Action == "show" || command.Action == "cancel" {
		return current, current.Validate()
	}
	if command.Action == "reset" {
		return defaults, nil
	}
	if command.Action != "configure" || len(command.Values) == 0 {
		return Config{}, errors.New("invalid command action")
	}
	next := current
	for key, value := range command.Values {
		switch key {
		case "mode":
			next.Mode = Mode(value)
		case "rounds":
			n, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, errors.New("invalid round count")
			}
			next.PeerRounds = n
		case "model.codex":
			if value == "default" {
				next.Codex = defaults.Codex
			} else {
				next.Codex.Model = value
			}
		case "model.claude":
			if value == "default" {
				next.Claude = defaults.Claude
			} else {
				next.Claude.Model = value
			}
		default:
			return Config{}, errors.New("unlisted configuration key")
		}
	}
	if next.Mode != Adversarial {
		if _, explicit := command.Values["rounds"]; !explicit {
			next.PeerRounds = 1
		}
	}
	return next, next.Validate()
}
