//go:build linux

package main

import (
	"errors"
	"path/filepath"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

type authRecoveryJournal struct {
	Version       int           `json:"version"`
	Provider      r.Provider    `json:"provider"`
	Binding       r.AuthBinding `json:"binding"`
	Stage         string        `json:"stage"`
	SeedHash      string        `json:"seed_hash"`
	TransferStage string        `json:"transfer_stage,omitempty"`
	TransferHash  string        `json:"transfer_hash,omitempty"`
}

func retainRecoveryJournal(c config, binding r.AuthBinding, seedHash string) (string, error) {
	raw, err := readBoundedFixture(filepath.Join(c.Root, "auth", string(c.Provider), "lease.json"), 8192)
	if err != nil {
		return "", err
	}
	record, err := r.DecodeStrict[authRecoveryJournal](raw, 8192)
	if err != nil || record.Version != 1 || record.Provider != c.Provider || record.Binding != binding || record.Stage != "active" || record.TransferStage != "exported" || record.SeedHash != seedHash || record.TransferHash != seedHash {
		return "", errors.New("original exported recovery journal required")
	}
	// This fixed schema contains only hashes, provenance and lifecycle metadata.
	// No profile or canonical credential bytes are copied into the retained record.
	if err = privateJSON(filepath.Join(c.Root, "retained-auth-recovery.json"), record); err != nil {
		return "", err
	}
	saved, err := readBoundedFixture(filepath.Join(c.Root, "retained-auth-recovery.json"), 8192)
	if err != nil {
		return "", err
	}
	return hash(saved), nil
}
