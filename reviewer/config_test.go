package reviewer

import (
	"strings"
	"testing"
	"time"
)

func event(body string) CommandEvent {
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	return CommandEvent{10, OwnerID, body, at, at}
}

func TestOwnerConfigurationIsTypedAndHasNoExecution(t *testing.T) {
	defaults := DefaultConfig()
	current := defaults
	current.PeerRounds = 2
	c, err := ParseCommand(event("/sfl config mode=claude-only model.claude=default"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ApplyConfig(current, defaults, c)
	if err != nil || next.Mode != ClaudeOnly || next.PeerRounds != 1 {
		t.Fatal(next, err)
	}
	if current.Mode != Adversarial || current.PeerRounds != 2 {
		t.Fatal("mutated captured configuration")
	}
	defaults.DailyReviews = 2
	reset, _ := ParseCommand(event("/sfl config reset"))
	next, err = ApplyConfig(current, defaults, reset)
	if err != nil || next.DailyReviews != 2 {
		t.Fatal("reset bypassed trusted controller defaults")
	}
	if DefaultConfig().MaxSessions() != 4 {
		t.Fatal("wrong adversarial default")
	}
}

func TestRejectUnauthorizedEditedOrUnlistedConfiguration(t *testing.T) {
	for _, body := range []string{"/sfl config endpoint=https://evil.example", "/sfl config mode=codex-only mode=adversarial", "/sfl config model.codex=unlisted", "/sfl config rounds=3", "/sfl config mode=unknown", "/sfl config mode=claude-only rounds=2"} {
		c, err := ParseCommand(event(body))
		if err == nil {
			_, err = ApplyConfig(DefaultConfig(), DefaultConfig(), c)
		}
		if err == nil {
			t.Fatal("accepted hostile configuration", body)
		}
	}
	e := event("/sfl cancel")
	e.AuthorID++
	if _, err := ParseCommand(e); err != ErrIgnored {
		t.Fatal("non-owner command produced actionable response")
	}
	e = event("/sfl config show")
	e.UpdatedAt = e.UpdatedAt.Add(time.Second)
	if _, err := ParseCommand(e); err == nil {
		t.Fatal("accepted prior command edit")
	}
	e = event("/sfl config " + strings.Repeat("x", 2048))
	if _, err := ParseCommand(e); err == nil {
		t.Fatal("unbounded command")
	}
}
