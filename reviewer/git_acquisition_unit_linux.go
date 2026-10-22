//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

var errGitUnit = errors.New("Git acquisition unit requires recovery")

// Separate nonsecret lifecycle state: this is not a provider auth journal and
// never selects a provider, imports a profile or launches a credential worker.
type gitUnitRecord struct {
	Version                                           int
	Binding                                           gitMaterializationBinding
	Unit, BootID, ManagerOwner, StubHash              string
	Started, StartSettled, Captured, Revoked, Stopped bool
	StartJob, StopJob                                 string
	Identity                                          GateIdentity
	Cgroup                                            cgroupBinding
	BootstrapGrantHash                                string                `json:",omitempty"`
	WorkerGrantHash                                   string                `json:",omitempty"`
	WorkerIdentity                                    GateIdentity          `json:",omitzero"`
	EarlyStop                                         *gitEarlyStopEvidence `json:",omitempty"`
	Birth                                             *gitUnitBirthEvidence `json:",omitempty"`
}
type gitUnitManager interface {
	containmentManager
	StartGit(context.Context, string, string, []string, string, int, int) (string, error)
}

type gitUnitOwner struct {
	operation     chan struct{}
	root          *os.Root
	lock          *os.File
	image         *retainedStubImage
	stubHash      string
	supervisor    *os.File
	manager       gitUnitManager
	kernel        containmentKernel
	rootPIDFDs    map[string]int
	grantFailed   map[string]bool
	grantEligible map[string]bool
	closed        bool
}

// openGitUnitOwner is private factory plumbing. It opens a fixed user-manager
// connection but launches nothing. The eventual factory still owns measured
// runtime/mount/helper admission, broker revocation and result publication.
func openGitUnitOwner(ctx context.Context, root, stub, stubHash string) (*gitUnitOwner, error) {
	if ctx == nil {
		return nil, errGitUnit
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	manager, err := openUserContainmentManager(bounded)
	if err != nil {
		return nil, errGitUnit
	}
	owner, err := newGitUnitOwner(ctx, root, stub, stubHash, manager, linuxContainmentKernel{})
	if err != nil {
		manager.Close()
	}
	return owner, err
}
func newGitUnitOwner(ctx context.Context, root, stub, stubHash string, manager containmentManager, kernel containmentKernel) (*gitUnitOwner, error) {
	if ctx == nil || ctx.Err() != nil || manager == nil || kernel == nil || manager.Owner() == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || privateDirectory(root) != nil {
		return nil, errGitUnit
	}
	gitManager, ok := manager.(gitUnitManager)
	if !ok {
		return nil, errGitUnit
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errGitUnit
	}
	anchored, err := os.OpenRoot(root)
	if err != nil {
		return nil, errGitUnit
	}
	owner := &gitUnitOwner{root: anchored, manager: gitManager, kernel: kernel, rootPIDFDs: map[string]int{}, grantFailed: map[string]bool{}, grantEligible: map[string]bool{}, operation: make(chan struct{}, 1), stubHash: stubHash}
	owner.operation <- struct{}{}
	good := false
	defer func() {
		if !good {
			if owner.supervisor != nil {
				owner.supervisor.Close()
			}
			if owner.image != nil {
				owner.image.Close()
			}
			if owner.lock != nil {
				owner.lock.Close()
			}
			anchored.Close()
		}
	}()
	owner.lock, err = anchored.OpenFile("git-owner.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil || privateAuthFile(owner.lock) != nil {
		return nil, errGitUnit
	}
	for {
		err = unix.Flock(int(owner.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return nil, errGitUnit
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errGitUnit
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil, errGitUnit
	}
	owner.image, err = retainStubImage(root, stub, stubHash)
	if err != nil {
		return nil, errGitUnit
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, errGitUnit
	}
	owner.supervisor = os.NewFile(uintptr(fd), "sfl-git-original-supervisor")
	if _, err := gitLivePIDFDIdentity(owner.supervisor); err != nil {
		return nil, errGitUnit
	}
	if _, err := owner.records(); err != nil {
		return nil, err
	}
	good = true
	return owner, nil
}
func gitUnitDirectory(attempt string) string { return "git-" + attempt }
func (o *gitUnitOwner) records() ([]gitUnitRecord, error) {
	directory, err := o.root.Open(".")
	if err != nil {
		return nil, errGitUnit
	}
	defer directory.Close()
	entries, err := directory.ReadDir(132)
	if err != nil || len(entries) > 130 {
		return nil, errGitUnit
	}
	records := []gitUnitRecord{}
	for _, entry := range entries {
		if entry.Name() == "git-owner.lock" || entry.Name() == "executables" {
			continue
		}
		attempt := strings.TrimPrefix(entry.Name(), "git-")
		if !strings.HasPrefix(entry.Name(), "git-") || !gitQuarantineHex(attempt, 32) || !entry.IsDir() {
			return nil, errGitUnit
		}
		record, err := o.read(attempt)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}
func (r gitUnitRecord) valid(attempt, hash string) bool {
	if r.Version != 1 || !r.Binding.valid() || r.Binding.Attempt != attempt || r.Unit != "sfl-alpha-"+attempt+".service" || !validContainmentUnit(r.Unit) || r.BootID == "" || r.ManagerOwner == "" || r.StubHash != hash || !validAuthHash(r.StubHash) {
		return false
	}
	if r.StartSettled && !r.Started || r.Captured && (!r.Started || !r.StartSettled) || r.Stopped && !r.Revoked {
		return false
	}
	if !r.Started && (r.StartJob != "" || r.StopJob != "" || r.Captured) {
		return false
	}
	for _, job := range []string{r.StartJob, r.StopJob} {
		if job != "" && (!dbus.ObjectPath(job).IsValid() || !strings.HasPrefix(job, "/org/freedesktop/systemd1/job/")) {
			return false
		}
	}
	if r.Captured {
		if r.Identity.PID <= 0 || r.Identity.StartTicks == 0 || r.Identity.BootID != r.BootID || !gitQuarantineHex(r.Identity.InvocationID, 32) || !validUnitCgroup(r.Cgroup.Path, r.Unit) || r.Cgroup.Inode == 0 || r.Cgroup.Hierarchy == (cgroupHierarchy{}) {
			return false
		}
	} else if r.Identity != (GateIdentity{}) || r.Cgroup != (cgroupBinding{}) {
		return false
	}
	if r.Birth != nil && (!r.Started || !r.Birth.valid(r) || r.Captured && (r.Birth.Unit.InvocationID != r.Identity.InvocationID || r.Birth.Unit.Cgroup != r.Cgroup)) {
		return false
	}
	if r.EarlyStop != nil && (!r.Started || !r.StartSettled || !r.Revoked || !r.Stopped || r.Captured || r.BootstrapGrantHash != "" || r.WorkerGrantHash != "" || !r.EarlyStop.valid(r.Unit) || r.Birth == nil || *r.EarlyStop != r.Birth.Unit) {
		return false
	}
	if r.BootstrapGrantHash != "" && (!r.Captured || r.BootstrapGrantHash != Digest(gitUnitGrantPacket{1, gitBootstrapGrant, r.Binding, r.Identity, r.Binding.RuntimeHash})) {
		return false
	}
	if r.WorkerGrantHash != "" {
		if r.BootstrapGrantHash == "" || r.WorkerGrantHash != Digest(gitUnitGrantPacket{1, gitWorkerGrant, r.Binding, r.WorkerIdentity, r.Binding.RuntimeHash}) || r.WorkerIdentity.PID <= 0 || r.WorkerIdentity.PID == r.Identity.PID || r.WorkerIdentity.StartTicks == 0 || r.WorkerIdentity.BootID != r.BootID || r.WorkerIdentity.InvocationID != r.Identity.InvocationID {
			return false
		}
	} else if r.WorkerIdentity != (GateIdentity{}) {
		return false
	}
	return true
}
func (o *gitUnitOwner) read(attempt string) (gitUnitRecord, error) {
	var r gitUnitRecord
	if !gitQuarantineHex(attempt, 32) || o.image == nil {
		return r, errGitUnit
	}
	directory, err := o.root.OpenRoot(gitUnitDirectory(attempt))
	if err != nil {
		return r, errGitUnit
	}
	defer directory.Close()
	info, err := directory.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return r, errGitUnit
	}
	raw, err := readAuth(directory, "git-unit.json", false)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		return gitUnitRecord{}, errGitUnit
	}
	canonical, err := json.Marshal(r)
	// Recovery authenticates a previous worker against its retained bytes;
	// the current launch image need not be the one that made this receipt.
	hash, errHash := o.recordImageHash(r.StubHash)
	if err != nil || errHash != nil || !bytes.Equal(canonical, raw) || !r.valid(attempt, hash) {
		return gitUnitRecord{}, errGitUnit
	}
	return r, nil
}
func (o *gitUnitOwner) imageHash() (string, error) {
	if o.image == nil || o.image.verify() != nil || !validAuthHash(o.stubHash) {
		return "", errGitUnit
	}
	return o.stubHash, nil
}

func (o *gitUnitOwner) recordImageHash(hash string) (string, error) {
	current, err := o.imageHash()
	if err != nil || !validAuthHash(hash) {
		return "", errGitUnit
	}
	if hash == current {
		return current, nil
	}
	// Only an existing content-addressed private copy can authenticate an old
	// record. Never import a pathname from that record or recreate a missing
	// image. Hash the actual bytes, with the existing owner/link/mode/size checks.
	directory := filepath.Join(o.root.Name(), "executables")
	if privateDirectory(directory) != nil {
		return "", errGitUnit
	}
	raw, err := readPinnedStub(filepath.Join(directory, hash), hash, true)
	if err != nil {
		return "", errGitUnit
	}
	return gitRawHash(raw), nil
}
func (o *gitUnitOwner) enter(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-o.operation:
		if ctx.Err() != nil {
			o.operation <- struct{}{}
			return false
		}
		return true
	}
}
func (o *gitUnitOwner) leave() { o.operation <- struct{}{} }
func (o *gitUnitOwner) write(r gitUnitRecord) error {
	hash, err := o.recordImageHash(r.StubHash)
	if err != nil || !r.valid(r.Binding.Attempt, hash) {
		return errGitUnit
	}
	directory, err := o.root.OpenRoot(gitUnitDirectory(r.Binding.Attempt))
	if err != nil {
		return errGitUnit
	}
	defer directory.Close()
	raw, err := json.Marshal(r)
	if err != nil || writeAuth(directory, "git-unit.json", raw) != nil {
		return errGitUnit
	}
	return nil
}
func (o *gitUnitOwner) prepare(ctx context.Context, binding gitMaterializationBinding) error {
	if !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	if o.closed || !binding.valid() {
		return errGitUnit
	}
	records, err := o.records()
	if err != nil || len(records) >= 128 {
		return errGitUnit
	}
	// A previous uncertain attempt must be recovered before another unit starts.
	for _, r := range records {
		if !r.Stopped {
			return errGitUnit
		}
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot == "" {
		return errGitUnit
	}
	hash, err := o.imageHash()
	if err != nil {
		return err
	}
	if o.root.Mkdir(gitUnitDirectory(binding.Attempt), 0700) != nil || syncAuthDirectory(o.root) != nil {
		return errGitUnit
	}
	return o.write(gitUnitRecord{Version: 1, Binding: binding, Unit: "sfl-alpha-" + binding.Attempt + ".service", BootID: boot, ManagerOwner: o.manager.Owner(), StubHash: hash})
}
func expectedGitUnit(r gitUnitRecord, v unitObservation) bool {
	return v.Unit == r.Unit && v.Path.IsValid() && gitQuarantineHex(v.InvocationID, 32) && v.Description == gitUnitDescriptionPrefix+Digest(r.Binding) && v.Transient && v.Job.ID == 0 && v.MemoryMax == 1<<30 && v.MemorySwapMax == 0 && v.TasksMax == 64 && v.CPUQuotaPerSecUSec == 1000000 && v.KillMode == "control-group" && v.Restart == "no" && expectedUnitLifecycle(v, r.Revoked) && v.NoNewPrivileges
}
func (o *gitUnitOwner) capture(ctx context.Context, r gitUnitRecord) (gitUnitRecord, error) {
	if r.ManagerOwner != o.manager.Owner() || ctx.Err() != nil {
		return r, errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return r, errGitUnit
	}
	observed, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitUnit(r, observed) || observed.MainPID == 0 {
		return r, errGitUnit
	}
	if r.Birth != nil && (r.Birth.Unit.InvocationID != observed.InvocationID || r.Birth.Unit.UnitPath != observed.Path || r.Birth.Unit.Cgroup.Path != observed.ControlGroup) {
		return r, errGitUnit
	}
	fd, err := unix.PidfdOpen(int(observed.MainPID), 0)
	if err != nil {
		return r, errGitUnit
	}
	good := false
	defer func() {
		if !good {
			unix.Close(fd)
		}
	}()
	unit, invocation, err := o.manager.UnitForPIDFD(ctx, fd)
	if err != nil || unit != r.Unit || invocation != observed.InvocationID {
		return r, errGitUnit
	}
	identity, err := processGateIdentity(int(observed.MainPID), invocation)
	if err != nil || identity.BootID != r.BootID || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return r, errGitUnit
	}
	if imageErr := o.kernel.Executable(identity.PID, r.StubHash); imageErr != nil {
		if errors.Is(imageErr, errStubImagePending) {
			return r, errors.Join(errGitUnit, errStubImagePending)
		}
		return r, errGitUnit
	}
	group, err := o.kernel.Capture(observed.ControlGroup, r.Unit)
	if err != nil || r.Birth != nil && r.Birth.Unit.Cgroup != group {
		return r, errGitUnit
	}
	if r.Captured && (r.Identity != identity || r.Cgroup != group) {
		return r, errGitUnit
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitUnit(r, after) || after.InvocationID != invocation || after.MainPID != observed.MainPID || after.ControlGroup != observed.ControlGroup {
		return r, errGitUnit
	}
	r.StartSettled = true
	r.Captured = true
	r.Identity = identity
	r.Cgroup = group
	if old, ok := o.rootPIDFDs[r.Binding.Attempt]; ok {
		unix.Close(old)
	}
	o.rootPIDFDs[r.Binding.Attempt] = fd
	good = true
	return r, nil
}
func (o *gitUnitOwner) captureStarted(ctx context.Context, r gitUnitRecord) (gitUnitRecord, error) {
	for {
		captured, err := o.capture(ctx, r)
		if !errors.Is(err, errStubImagePending) {
			return captured, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, errGitUnit
		case <-timer.C:
		}
	}
}
func (o *gitUnitOwner) start(ctx context.Context, binding gitMaterializationBinding, endpoint string) error {
	if ctx == nil {
		return errGitUnit
	}
	if !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	if o.closed || ctx.Err() != nil || !binding.valid() || !filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || len(endpoint) > 100 {
		return errGitUnit
	}
	r, err := o.read(binding.Attempt)
	if err != nil || r.Binding != binding || r.StubHash != o.stubHash || r.Started || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return errGitUnit
	}
	if _, err := gitLivePIDFDIdentity(o.supervisor); err != nil {
		return errGitUnit
	}
	supervisorIdentity, err := processGateIdentity(os.Getpid(), strings.Repeat("0", 32))
	if err != nil || gitCheckProcessPIDFD(supervisorIdentity, int(o.supervisor.Fd())) != nil {
		return errGitUnit
	}
	r.Started = true
	// Durable start intent precedes any manager call. No automatic retry follows
	// a missing reply, failed job or failed receipt write.
	if o.write(r) != nil {
		return errGitUnit
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	raw, _ := json.Marshal(binding)
	job, err := o.manager.StartGit(bounded, r.Unit, retainedStubExec, []string{retainedStubExec, endpoint, string(raw), Digest(binding), strconv.Itoa(os.Getpid())}, gitUnitDescriptionPrefix+Digest(binding), int(o.image.file.Fd()), int(o.supervisor.Fd()))
	if err != nil {
		return errGitUnit
	}
	r.StartJob = job
	if o.write(r) != nil {
		return errGitUnit
	}
	// Only this fresh accepted start may bind a pre-root identity. Recovery
	// never creates a missing birth record from whichever unit exists later.
	r, err = o.captureBirth(bounded, r)
	if err != nil || o.write(r) != nil || o.manager.WaitJob(bounded, job, r.Unit) != nil {
		return errGitUnit
	}
	r.StartSettled = true
	if o.write(r) != nil {
		return errGitUnit
	}
	captured, err := o.captureStarted(bounded, r)
	if err != nil || o.write(captured) != nil {
		return errGitUnit
	}
	// Only this completed fresh start may grant. Recovery capture never arms
	// this process-local authority, even when it obtains a valid new pidfd.
	o.grantEligible[binding.Attempt] = true
	return nil
}
func (o *gitUnitOwner) rootDead(r gitUnitRecord) error {
	if !r.Captured {
		return errGitUnit
	}
	fd, retained := o.rootPIDFDs[r.Binding.Attempt]
	if !retained {
		var err error
		fd, err = unix.PidfdOpen(r.Identity.PID, 0)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return errGitUnit
		}
		defer unix.Close(fd)
		current, err := processGateIdentity(r.Identity.PID, r.Identity.InvocationID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return errGitUnit
		}
		// PID reuse can prove the old process dead, but never selects a new root.
		if current.BootID != r.Identity.BootID || current.StartTicks != r.Identity.StartTicks {
			return nil
		}
	}
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(events, 0)
	if err != nil || n != 1 || events[0].Revents&unix.POLLIN == 0 {
		return errGitUnit
	}
	return nil
}

// stop proves only unit termination. The composing factory MUST revoke and
// join the broker before releasing a reader generation or publishing source.
func (o *gitUnitOwner) stop(ctx context.Context, binding gitMaterializationBinding) error {
	if ctx == nil {
		return errGitUnit
	}
	if !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	if o.closed || ctx.Err() != nil {
		return errGitUnit
	}
	r, err := o.read(binding.Attempt)
	if err != nil || r.Binding != binding {
		return errGitUnit
	}
	r.Revoked = true
	delete(o.grantEligible, binding.Attempt)
	if o.write(r) != nil {
		return errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil {
		return errGitUnit
	}
	if !r.Started || boot != r.BootID {
		r.Stopped = true
		return o.write(r)
	}
	if r.ManagerOwner != o.manager.Owner() {
		return errGitUnit
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	if r.Stopped {
		group := r.Cgroup
		if !r.Captured {
			if r.EarlyStop == nil || !r.EarlyStop.valid(r.Unit) {
				return errGitUnit
			}
			group = r.EarlyStop.Cgroup
		}
		if o.kernel.Empty(group) != nil || r.Captured && o.rootDead(r) != nil {
			return errGitUnit
		}
		return o.manager.RetireUnit(bounded, r.Unit)
	}
	if !r.Captured {
		terminal, handled, terminalErr := o.stopBeforeCapture(bounded, r)
		if terminalErr != nil {
			return terminalErr
		}
		if handled {
			if o.write(terminal) != nil {
				return errGitUnit
			}
			return o.manager.RetireUnit(bounded, r.Unit)
		}
		r, err = o.captureStarted(bounded, r)
		if err != nil || o.write(r) != nil {
			return errGitUnit
		}
	}
	before, err := o.manager.Observe(bounded, r.Unit)
	if err != nil || !expectedGitUnit(r, before) || before.InvocationID != r.Identity.InvocationID {
		return errGitUnit
	}
	if !r.Stopped {
		if o.kernel.Terminate(bounded, r.Cgroup) != nil {
			return errGitUnit
		}
	}
	if o.kernel.Empty(r.Cgroup) != nil || o.rootDead(r) != nil {
		return errGitUnit
	}
	after, err := waitForOriginalUnitStop(bounded, o.manager, o.kernel, before, r.Cgroup)
	if err != nil || !expectedGitUnit(r, after) || after.InvocationID != r.Identity.InvocationID || after.ControlGroup != r.Cgroup.Path && after.ControlGroup != "" || after.MainPID != 0 || (after.ActiveState != "inactive" && after.ActiveState != "failed" && after.ActiveState != "active") {
		return errGitUnit
	}
	r.Stopped = true
	if o.write(r) != nil {
		return errGitUnit
	}
	return o.manager.RetireUnit(bounded, r.Unit)
}

// Close retains the process lock and pinned image if any owned attempt lacks a
// stopped receipt. It never silently abandons an uncertain unit or stops one
// selected by an untrusted journal. Explicit recovery calls stop first.
func (o *gitUnitOwner) Close() error {
	if o == nil {
		return nil
	}
	select {
	case <-o.operation:
	default:
		return errGitUnit
	}
	defer o.leave()
	if o.closed {
		return nil
	}
	records, err := o.records()
	if err != nil {
		return err
	}
	for _, r := range records {
		if !r.Stopped {
			return errGitUnit
		}
	}
	o.closed = true
	for attempt, fd := range o.rootPIDFDs {
		unix.Close(fd)
		delete(o.rootPIDFDs, attempt)
	}
	return errors.Join(o.supervisor.Close(), o.image.Close(), o.manager.Close(), o.lock.Close(), o.root.Close())
}
