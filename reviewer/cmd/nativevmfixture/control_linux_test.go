//go:build linux

package main

import (
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"testing"
)

func TestNativeControlCaseRejectsMixedModes(t *testing.T) {
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		c := config{Purpose: "native-control-fake-only", Provider: provider}
		if !c.validCase() {
			t.Fatal("control refused")
		}
		for _, change := range []func(*config){func(c *config) { c.Provider = "" }, func(c *config) { c.ProtocolRounds = 1 }, func(c *config) { c.Negative = true }, func(c *config) { c.Refresh = true }, func(c *config) { c.UncertainRefresh = true }, func(c *config) { c.Cancellation = "provider" }} {
			bad := c
			change(&bad)
			if bad.validCase() {
				t.Fatal("hybrid control accepted", bad)
			}
		}
	}
}
