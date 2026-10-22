//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitVMOwnerJournalFixture(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "owner")
	if os.Mkdir(directory, 0700) != nil {
		t.Fatal("owner directory")
	}
	return directory
}
func gitVMUnitRecordFixture(t *testing.T) gitVMUnitRecord {
	q := gitVMGuestTestRequest(t)
	return gitVMUnitRecord{Request: q, RequestHash: Digest(q), Unit: "sfl-alpha-" + q.Source.Binding.Attempt + ".service", BootID: "synthetic-boot", ManagerOwner: ":1.10", StubHash: Digest("synthetic source host stub")}
}
func TestGitVMOwnerJournalRetainsPreparedAttemptAcrossReopen(t *testing.T) {
	directory := gitVMOwnerJournalFixture(t)
	j, err := openGitVMOwnerJournal(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	value := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{gitVMUnitRecordFixture(t)}}
	if j.write(value) != nil {
		t.Fatal("prepared journal persistence")
	}
	if j.Close(context.Background()) != nil {
		t.Fatal("journal close")
	}
	j, err = openGitVMOwnerJournal(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close(context.Background())
	got, err := readGitVMUnitJournal(j.held.root, false)
	if err != nil || Digest(got) != Digest(value) {
		t.Fatal("prepared attempt not recovered", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if other, err := openGitVMOwnerJournal(ctx, directory); err == nil {
		other.Close(context.Background())
		t.Fatal("second journal writer accepted")
	}
}
func TestGitVMOwnerJournalRejectsForeignStateWithoutCreatingLock(t *testing.T) {
	directory := gitVMOwnerJournalFixture(t)
	foreign := []byte(`{"Version":1,"Purpose":"native-review","Records":[]}`)
	if os.WriteFile(filepath.Join(directory, "source-vm-units.json"), foreign, 0600) != nil {
		t.Fatal("foreign journal")
	}
	if j, err := openGitVMOwnerJournal(context.Background(), directory); err == nil {
		j.Close(context.Background())
		t.Fatal("foreign purpose accepted")
	}
	if _, err := os.Lstat(filepath.Join(directory, "source-vm-owner.lock")); !os.IsNotExist(err) {
		t.Fatal("refused foreign journal was mutated")
	}
}
func TestGitVMOwnerJournalRejectsUnboundLifecycleStates(t *testing.T) {
	valid := gitVMUnitRecordFixture(t)
	if !valid.valid() {
		t.Fatal("prepared fixture invalid")
	}
	for _, mutate := range []func(*gitVMUnitRecord){
		func(r *gitVMUnitRecord) { r.RequestHash = Digest("other request") },
		func(r *gitVMUnitRecord) { r.Request.Purpose = "native-review"; r.RequestHash = Digest(r.Request) },
		func(r *gitVMUnitRecord) { r.StartSettled = true },
		func(r *gitVMUnitRecord) { r.Captured = true },
		func(r *gitVMUnitRecord) { r.Stopped = true },
		func(r *gitVMUnitRecord) { r.StartIntent = true; r.Revoked = true; r.Stopped = true },
		func(r *gitVMUnitRecord) { r.GrantHash = Digest("ungranted") },
		func(r *gitVMUnitRecord) { r.StartJob = "/org/freedesktop/systemd1/job/1" },
	} {
		bad := valid
		mutate(&bad)
		if bad.valid() {
			t.Fatal("unbound lifecycle state accepted")
		}
	}
	journal := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{valid, valid}}
	if journal.valid() {
		t.Fatal("duplicate attempt accepted")
	}
	journal.Records = nil
	if journal.valid() {
		t.Fatal("missing record array accepted")
	}
}
func TestGitVMOwnerJournalCanonicalFramingAndRootReplacement(t *testing.T) {
	directory := gitVMOwnerJournalFixture(t)
	j, err := openGitVMOwnerJournal(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	old := directory + ".original"
	if os.Rename(directory, old) != nil || os.Mkdir(directory, 0700) != nil {
		t.Fatal("replacement setup")
	}
	value := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{gitVMUnitRecordFixture(t)}}
	if j.write(value) == nil {
		t.Fatal("replaced root accepted")
	}
	if _, err := os.Stat(filepath.Join(directory, "source-vm-units.json")); !os.IsNotExist(err) {
		t.Fatal("replacement owner was mutated")
	}
	if os.Remove(directory) != nil || os.Rename(old, directory) != nil {
		t.Fatal("restore original")
	}
	if j.Close(context.Background()) != nil {
		t.Fatal("close restored journal")
	}
	raw, _ := json.Marshal(value)
	raw = []byte(strings.Replace(string(raw), `"Version":1`, `"Version":1,"Unknown":true`, 1))
	if os.WriteFile(filepath.Join(directory, "source-vm-units.json"), raw, 0600) != nil {
		t.Fatal("extended journal")
	}
	if j, err := openGitVMOwnerJournal(context.Background(), directory); err == nil {
		j.Close(context.Background())
		t.Fatal("extended journal accepted")
	}
}
func gitVMStoppedRecordFixture(t *testing.T) gitVMUnitRecord {
	r := gitVMUnitRecordFixture(t)
	job := "/org/freedesktop/systemd1/job/41"
	group := cgroupBinding{"/user.slice/user-1000.slice/user@1000.service/" + r.Unit, 7, cgroupHierarchy{1, 2, 3, 4}}
	invocation := strings.Repeat("d", 32)
	r.StartIntent, r.StartJob, r.StartSettled, r.Captured, r.Revoked, r.Stopped = true, job, true, true, true, true
	r.Birth = gitVMBirth{job, invocation, "/org/freedesktop/systemd1/unit/sfl_2dalpha", group}
	r.Identity = GateIdentity{InvocationID: invocation, BootID: r.BootID, PID: 4242, StartTicks: 99}
	r.Cgroup, r.GrantHash = group, Digest("one-use source grant")
	return r
}

// A born record must round-trip the strict decoder; an absent birth is the
// zero value, never JSON null.
func TestGitVMOwnerJournalRetainsBornAttemptAcrossReopen(t *testing.T) {
	directory := gitVMOwnerJournalFixture(t)
	stopped := gitVMStoppedRecordFixture(t)
	if !stopped.valid() {
		t.Fatal("stopped fixture invalid")
	}
	prepared := gitVMUnitRecordFixture(t)
	prepared.Request.Source.Binding.Attempt = strings.Repeat("e", 32)
	prepared.Request.Read.Source.Attempt = prepared.Request.Source.Binding.Attempt
	prepared.RequestHash, prepared.Unit = Digest(prepared.Request), "sfl-alpha-"+prepared.Request.Source.Binding.Attempt+".service"
	value := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{stopped, prepared}}
	if !value.valid() {
		t.Fatal("mixed journal invalid")
	}
	j, err := openGitVMOwnerJournal(context.Background(), directory)
	if err != nil || j.write(value) != nil || j.Close(context.Background()) != nil {
		t.Fatal("born journal persistence", err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "source-vm-units.json"))
	if err != nil || strings.Contains(string(raw), "null") {
		t.Fatal("journal framing contains null", err)
	}
	j, err = openGitVMOwnerJournal(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readGitVMUnitJournal(j.held.root, false)
	if err != nil || Digest(got) != Digest(value) {
		t.Fatal("born attempt not recovered", err)
	}
	if j.Close(context.Background()) != nil {
		t.Fatal("close")
	}
	nulled := strings.Replace(string(raw), `"GrantHash":""`, `"GrantHash":null`, 1)
	if nulled == string(raw) || os.WriteFile(filepath.Join(directory, "source-vm-units.json"), []byte(nulled), 0600) != nil {
		t.Fatal("null mutation setup")
	}
	if j, err := openGitVMOwnerJournal(context.Background(), directory); err == nil {
		j.Close(context.Background())
		t.Fatal("null field accepted")
	}
}

// The full record bound must survive reopen; a journal the owner can write
// but not read back would wedge every later source attempt.
func TestGitVMOwnerJournalFullBoundRoundTrips(t *testing.T) {
	directory := gitVMOwnerJournalFixture(t)
	value := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{}}
	for i := 0; i < gitVMOwnerRecords; i++ {
		r := gitVMStoppedRecordFixture(t)
		r.Request.Source.Binding.Attempt = fmt.Sprintf("%032x", i+1)
		r.Request.Read.Source.Attempt = r.Request.Source.Binding.Attempt
		r.RequestHash, r.Unit = Digest(r.Request), "sfl-alpha-"+r.Request.Source.Binding.Attempt+".service"
		r.Birth.Cgroup.Path = "/user.slice/user-1000.slice/user@1000.service/" + r.Unit
		r.Cgroup = r.Birth.Cgroup
		value.Records = append(value.Records, r)
	}
	if !value.valid() {
		t.Fatal("full journal invalid")
	}
	j, err := openGitVMOwnerJournal(context.Background(), directory)
	if err != nil || j.write(value) != nil || j.Close(context.Background()) != nil {
		t.Fatal("full journal persistence", err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "source-vm-units.json"))
	if err != nil || len(raw) <= 65536 || len(raw) > gitVMOwnerJournalBytes {
		t.Fatal("full journal size outside the tested range", len(raw), err)
	}
	j, err = openGitVMOwnerJournal(context.Background(), directory)
	if err != nil {
		t.Fatal("full journal reopen", err)
	}
	defer j.Close(context.Background())
	got, err := readGitVMUnitJournal(j.held.root, false)
	if err != nil || Digest(got) != Digest(value) {
		t.Fatal("full journal not recovered", err)
	}
	value.Records = append(value.Records, value.Records[0])
	if j.write(value) == nil {
		t.Fatal("record bound exceeded")
	}
}
