//go:build linux

package nativefixture

import (
	"encoding/hex"
	"errors"
)

// ConfigureSyntheticRevision is fixture setup, before any backend or goroutine
// starts. It lets the six-arm fixture use a real private synthetic Git commit
// with the same fixed source text as the protocol producers. It never reads or
// changes a repository, auth profile or provider configuration.
func ConfigureSyntheticRevision(revision string) error {
	raw, err := hex.DecodeString(revision)
	if err != nil || len(raw) != 20 || hex.EncodeToString(raw) != revision {
		return errors.New("invalid synthetic source revision")
	}
	head = revision
	return nil
}
