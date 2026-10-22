//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestStoppedFaultTargetsOnlySelectedCompletedNativeAttempt(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE evaluation_execution(slot TEXT PRIMARY KEY,data BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err = installStoppedFault(ctx, db, r.ArmCodexControl); err != nil {
		t.Fatal(err)
	}
	data := func(arm r.EvaluationArm, state string) []byte {
		raw, _ := json.Marshal(struct {
			Arm      r.EvaluationArm
			Attempts []struct{ State string }
		}{arm, []struct{ State string }{{state}}})
		return raw
	}
	inflight := data(r.ArmCodexControl, "inflight")
	if _, err = db.Exec(`INSERT INTO evaluation_execution VALUES(?,?)`, "target", inflight); err != nil {
		t.Fatal(err)
	}
	// A charged reservation and a different arm's stopped result must commit.
	for _, raw := range [][]byte{inflight, data(r.ArmAdversarial, "complete"), inflight} {
		if _, err = db.Exec(`UPDATE evaluation_execution SET data=? WHERE slot='target'`, raw); err != nil {
			t.Fatal("fault fired before the selected result", err)
		}
	}
	if _, err = db.Exec(`UPDATE evaluation_execution SET data=? WHERE slot='target'`, data(r.ArmCodexControl, "complete")); err == nil {
		t.Fatal("stopped result unexpectedly persisted")
	}
	var saved []byte
	if err = db.QueryRow(`SELECT data FROM evaluation_execution WHERE slot='target'`).Scan(&saved); err != nil || string(saved) != string(inflight) {
		t.Fatal("failed result destroyed its original reservation", err)
	}
	if _, err = db.Exec(`DROP TRIGGER stopped_native_fault`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE evaluation_execution SET data=? WHERE slot='target'`, data(r.ArmCodexControl, "complete")); err != nil {
		t.Fatal("removed fault still blocked writes", err)
	}
	for _, arm := range []r.EvaluationArm{"", r.ArmCodex, "unknown'; DROP TABLE evaluation_execution; --"} {
		if installStoppedFault(ctx, db, arm) == nil {
			t.Fatal("unsupported fault mode accepted", arm)
		}
	}
}
