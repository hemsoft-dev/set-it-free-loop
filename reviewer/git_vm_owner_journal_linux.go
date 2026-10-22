//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

var errGitVMOwner = errors.New("source VM ownership requires recovery")

const gitVMOwnerPurpose = "source-vm-unit-owner"
const gitVMOwnerRecords = 64

// A full journal of bound requests exceeds the 64 KiB auth-file reader, so
// the journal has its own reader with the same private-file checks.
const gitVMOwnerJournalBytes = 1 << 20

type gitVMBirth struct {
	StartJob, InvocationID string
	UnitPath               dbus.ObjectPath
	Cgroup                 cgroupBinding
}

func (b gitVMBirth) valid(unit, job string) bool {
	h := b.Cgroup.Hierarchy
	return b.StartJob == job && gitVMJob(job) && gitQuarantineHex(b.InvocationID, 32) && b.InvocationID != strings.Repeat("0", 32) && b.UnitPath.IsValid() && strings.HasPrefix(string(b.UnitPath), "/org/freedesktop/systemd1/unit/") && validUnitCgroup(b.Cgroup.Path, unit) && b.Cgroup.Inode != 0 && h.Device != 0 && h.Inode != 0 && h.MountNamespace != 0 && h.CgroupNamespace != 0
}
func gitVMJob(job string) bool {
	raw := strings.TrimPrefix(job, "/org/freedesktop/systemd1/job/")
	n, err := strconv.ParseUint(raw, 10, 32)
	return err == nil && n != 0 && job == "/org/freedesktop/systemd1/job/"+strconv.FormatUint(n, 10)
}

// A protected lifecycle record, never a caller-supplied stop or admission flag.
// No provider/auth binding or credential bytes can be represented here.
type gitVMUnitRecord struct {
	Request                                               gitVMGuestRequest
	RequestHash, Unit, BootID, ManagerOwner, StubHash     string
	StartIntent, StartSettled, Captured, Revoked, Stopped bool
	StartJob                                              string
	Birth                                                 gitVMBirth
	Identity                                              GateIdentity
	Cgroup                                                cgroupBinding
	GrantHash                                             string
}

// The zero birth means the start job has not reported its original unit.
func (r gitVMUnitRecord) born() bool { return r.Birth != (gitVMBirth{}) }

func (r gitVMUnitRecord) valid() bool {
	if !r.Request.valid() || r.RequestHash != Digest(r.Request) || r.Unit != "sfl-alpha-"+r.Request.Source.Binding.Attempt+".service" || !validContainmentUnit(r.Unit) || r.BootID == "" || len(r.BootID) > 64 || r.ManagerOwner == "" || len(r.ManagerOwner) > 256 || !validAuthHash(r.StubHash) {
		return false
	}
	if r.StartJob != "" && !gitVMJob(r.StartJob) || r.StartSettled && (!r.StartIntent || r.StartJob == "" || !r.born()) || r.Captured && !r.StartSettled || r.Stopped && (!r.Revoked || r.StartIntent && !r.born()) {
		return false
	}
	if !r.StartIntent && (r.StartJob != "" || r.born() || r.StartSettled || r.Captured) {
		return false
	}
	if r.born() && (!r.StartIntent || !r.Birth.valid(r.Unit, r.StartJob)) {
		return false
	}
	if r.Captured {
		if !r.born() || r.Identity.PID <= 0 || r.Identity.StartTicks == 0 || r.Identity.BootID != r.BootID || r.Identity.InvocationID != r.Birth.InvocationID || r.Cgroup != r.Birth.Cgroup {
			return false
		}
	} else if r.Identity != (GateIdentity{}) || r.Cgroup != (cgroupBinding{}) {
		return false
	}
	if r.GrantHash != "" && (!r.Captured || !validAuthHash(r.GrantHash)) {
		return false
	}
	return true
}

type gitVMUnitJournal struct {
	Version int
	Purpose string
	Records []gitVMUnitRecord
}

func (j gitVMUnitJournal) valid() bool {
	if j.Version != 1 || j.Purpose != gitVMOwnerPurpose || j.Records == nil || len(j.Records) > gitVMOwnerRecords {
		return false
	}
	seen := map[string]bool{}
	for _, r := range j.Records {
		attempt := r.Request.Source.Binding.Attempt
		if !r.valid() || seen[attempt] {
			return false
		}
		seen[attempt] = true
	}
	return true
}

// Metadata ownership only. The composing source supervisor must independently
// capture and stop original kernel instances before recording terminal state.
type gitVMOwnerJournal struct {
	directory string
	identity  os.FileInfo
	held      *authLock
	operation chan struct{}
	closed    bool
}

func readGitVMOwnerFile(root *os.Root, optional bool) ([]byte, error) {
	file, err := root.OpenFile("source-vm-units.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if optional && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errGitVMOwner
	}
	defer file.Close()
	if privateAuthFile(file) != nil {
		return nil, errGitVMOwner
	}
	raw, err := io.ReadAll(io.LimitReader(file, gitVMOwnerJournalBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > gitVMOwnerJournalBytes {
		return nil, errGitVMOwner
	}
	return raw, nil
}
func readGitVMUnitJournal(root *os.Root, optional bool) (gitVMUnitJournal, error) {
	empty := gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{}}
	raw, err := readGitVMOwnerFile(root, optional)
	if err != nil {
		return empty, errGitVMOwner
	}
	if raw == nil {
		return empty, nil
	}
	j, err := DecodeStrict[gitVMUnitJournal](raw, gitVMOwnerJournalBytes)
	canonical, marshalErr := json.Marshal(j)
	if err != nil || marshalErr != nil || !j.valid() || !bytes.Equal(raw, canonical) {
		return empty, errGitVMOwner
	}
	return j, nil
}
func preflightGitVMOwnerJournal(root *os.Root) error {
	if _, err := readGitVMUnitJournal(root, true); err != nil {
		return err
	}
	file, err := root.Open(".")
	if err != nil {
		return errGitVMOwner
	}
	defer file.Close()
	entries, err := file.ReadDir(129)
	if err != nil && err != io.EOF || len(entries) > 128 {
		return errGitVMOwner
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "source-vm-units.json" || name == "source-vm-owner.lock" {
			continue
		}
		if strings.HasPrefix(name, ".replace-") && gitQuarantineHex(strings.TrimPrefix(name, ".replace-"), 32) && entry.Type().IsRegular() {
			continue
		}
		return errGitVMOwner
	}
	return nil
}
func openGitVMOwnerJournal(ctx context.Context, directory string) (_ *gitVMOwnerJournal, result error) {
	if ctx == nil || ctx.Err() != nil || !sourcePath(directory) || privateDirectory(directory) != nil {
		return nil, errGitVMOwner
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitVMOwner
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		return nil, errGitVMOwner
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errGitVMOwner
	}
	if !gitVMArchiveRootMatches(directory, root, identity) || preflightGitVMOwnerJournal(root) != nil {
		root.Close()
		return nil, errGitVMOwner
	}
	lock, err := lockGitVMArchiveRoot(ctx, root, "source-vm-owner.lock")
	if err != nil {
		root.Close()
		return nil, errGitVMOwner
	}
	j := &gitVMOwnerJournal{directory: directory, identity: identity, held: &authLock{root, lock}, operation: make(chan struct{}, 1)}
	j.operation <- struct{}{}
	defer func() {
		if result != nil {
			j.held.close()
		}
	}()
	// Validate the original retained lock before removing any stale atomic temp.
	if j.verify() != nil || clearAuthTemps(root) != nil || j.verify() != nil {
		return nil, errGitVMOwner
	}
	value, err := readGitVMUnitJournal(root, true)
	if err != nil {
		return nil, err
	}
	if raw, err := readGitVMOwnerFile(root, true); err != nil {
		return nil, errGitVMOwner
	} else if raw == nil {
		if j.write(value) != nil {
			return nil, errGitVMOwner
		}
	}
	return j, nil
}
func (j *gitVMOwnerJournal) verify() error {
	if j == nil || j.closed || j.held == nil || !gitVMArchiveRootMatches(j.directory, j.held.root, j.identity) || privateAuthFile(j.held.file) != nil {
		return errGitVMOwner
	}
	directory, err := j.held.root.Open(".")
	if err != nil {
		return errGitVMOwner
	}
	defer directory.Close()
	var opened, named unix.Stat_t
	if unix.Fstat(int(j.held.file.Fd()), &opened) != nil || unix.Fstatat(int(directory.Fd()), "source-vm-owner.lock", &named, unix.AT_SYMLINK_NOFOLLOW) != nil || opened.Dev != named.Dev || opened.Ino != named.Ino || named.Nlink != 1 {
		return errGitVMOwner
	}
	return nil
}
func (j *gitVMOwnerJournal) write(value gitVMUnitJournal) error {
	if j.verify() != nil || !value.valid() {
		return errGitVMOwner
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > gitVMOwnerJournalBytes || writeAuth(j.held.root, "source-vm-units.json", raw) != nil || j.verify() != nil {
		return errGitVMOwner
	}
	return nil
}
func (j *gitVMOwnerJournal) enter(ctx context.Context) bool {
	if j == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-j.operation:
		return true
	}
}
func (j *gitVMOwnerJournal) leave() { j.operation <- struct{}{} }
func (j *gitVMOwnerJournal) Close(ctx context.Context) error {
	if !j.enter(ctx) {
		return errGitVMOwner
	}
	defer j.leave()
	if j.closed {
		return nil
	}
	if j.verify() != nil {
		return errGitVMOwner
	}
	j.closed = true
	return j.held.close()
}
