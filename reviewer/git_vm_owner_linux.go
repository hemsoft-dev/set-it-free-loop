//go:build linux

package reviewer

import (
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

type gitVMUnitManager interface {
	containmentManager
	StartGitVM(context.Context, string, string, []string, string, int, int) (string, error)
}

// The source VM unit has its own fixed cgroup limits, separate from host Git
// acquisition and from provider-bearing native VMs.
type gitVMContainmentKernel struct{ linuxContainmentKernel }

func (gitVMContainmentKernel) Capture(group, unit string) (cgroupBinding, error) {
	return captureContainmentGroup(group, unit, strconv.FormatUint(gitVMUnitMemory, 10), strconv.FormatUint(gitVMUnitTasks, 10))
}

// gitVMUnitOwner owns original source VM unit instances. It records lifecycle
// metadata before each manager call, captures the original kernel instance and
// stops only that captured instance. It holds no provider, auth or GitHub
// credential; the composing run owns grants, channels and staging.
type gitVMUnitOwner struct {
	operation     chan struct{}
	root          *os.Root
	journal       *gitVMOwnerJournal
	image         *retainedStubImage
	stubHash      string
	supervisor    *os.File
	manager       gitVMUnitManager
	kernel        containmentKernel
	rootPIDFDs    map[string]int
	grantEligible map[string]bool
	closed        bool
	// stopFailure is the fixed stage of the latest refused stop, if any.
	stopFailure string
}

func openGitVMUnitOwner(ctx context.Context, root, stub, stubHash string) (*gitVMUnitOwner, error) {
	if ctx == nil {
		return nil, errGitVMOwner
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	manager, err := openUserContainmentManager(bounded)
	if err != nil {
		return nil, errGitVMOwner
	}
	owner, err := newGitVMUnitOwner(ctx, root, stub, stubHash, manager, gitVMContainmentKernel{})
	if err != nil {
		manager.Close()
	}
	return owner, err
}

// root holds exactly the strict journal directory and the content-addressed
// stub cache. Opening it launches nothing and selects no unit.
func newGitVMUnitOwner(ctx context.Context, root, stub, stubHash string, manager containmentManager, kernel containmentKernel) (_ *gitVMUnitOwner, result error) {
	if ctx == nil || ctx.Err() != nil || manager == nil || kernel == nil || manager.Owner() == "" || !sourcePath(root) || privateDirectory(root) != nil || !validAuthHash(stubHash) {
		return nil, errGitVMOwner
	}
	vmManager, ok := manager.(gitVMUnitManager)
	if !ok {
		return nil, errGitVMOwner
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errGitVMOwner
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 2 {
		return nil, errGitVMOwner
	}
	for _, entry := range entries {
		if entry.Name() != "journal" && entry.Name() != "executables" || !entry.IsDir() {
			return nil, errGitVMOwner
		}
	}
	journalDirectory := filepath.Join(root, "journal")
	if err := os.Mkdir(journalDirectory, 0700); err != nil && !os.IsExist(err) {
		return nil, errGitVMOwner
	}
	anchored, err := os.OpenRoot(root)
	if err != nil {
		return nil, errGitVMOwner
	}
	o := &gitVMUnitOwner{root: anchored, manager: vmManager, kernel: kernel, stubHash: stubHash, rootPIDFDs: map[string]int{}, grantEligible: map[string]bool{}, operation: make(chan struct{}, 1)}
	o.operation <- struct{}{}
	defer func() {
		if result != nil {
			if o.supervisor != nil {
				o.supervisor.Close()
			}
			if o.image != nil {
				o.image.Close()
			}
			if o.journal != nil {
				o.journal.Close(context.Background())
			}
			anchored.Close()
		}
	}()
	// The journal lock serializes owners before any cache or unit is touched.
	o.journal, err = openGitVMOwnerJournal(ctx, journalDirectory)
	if err != nil {
		return nil, errGitVMOwner
	}
	o.image, err = retainStubImage(root, stub, stubHash)
	if err != nil {
		return nil, errGitVMOwner
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, errGitVMOwner
	}
	o.supervisor = os.NewFile(uintptr(fd), "sfl-source-vm-original-supervisor")
	if _, err := gitLivePIDFDIdentity(o.supervisor); err != nil {
		return nil, errGitVMOwner
	}
	value, err := o.journal.read()
	if err != nil {
		return nil, errGitVMOwner
	}
	for _, r := range value.Records {
		if _, err := o.recordImageHash(r.StubHash); err != nil {
			return nil, errGitVMOwner
		}
	}
	return o, nil
}

func (o *gitVMUnitOwner) imageHash() (string, error) {
	if o.image == nil || o.image.verify() != nil || !validAuthHash(o.stubHash) {
		return "", errGitVMOwner
	}
	return o.stubHash, nil
}

// An older record authenticates against its own retained content-addressed
// copy, never a pathname from the record or a recreated image.
func (o *gitVMUnitOwner) recordImageHash(hash string) (string, error) {
	current, err := o.imageHash()
	if err != nil || !validAuthHash(hash) {
		return "", errGitVMOwner
	}
	if hash == current {
		return current, nil
	}
	directory := filepath.Join(o.root.Name(), "executables")
	if privateDirectory(directory) != nil {
		return "", errGitVMOwner
	}
	raw, err := readPinnedStub(filepath.Join(directory, hash), hash, true)
	if err != nil || gitRawHash(raw) != hash {
		return "", errGitVMOwner
	}
	return hash, nil
}

func (o *gitVMUnitOwner) enter(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-o.operation:
		if ctx.Err() != nil || o.closed {
			o.operation <- struct{}{}
			return false
		}
		return true
	}
}
func (o *gitVMUnitOwner) leave() { o.operation <- struct{}{} }

func (o *gitVMUnitOwner) read(attempt string) (gitVMUnitRecord, error) {
	value, err := o.journal.read()
	if err != nil {
		return gitVMUnitRecord{}, errGitVMOwner
	}
	r, ok := value.record(attempt)
	if !ok {
		return gitVMUnitRecord{}, errGitVMOwner
	}
	if _, err := o.recordImageHash(r.StubHash); err != nil {
		return gitVMUnitRecord{}, errGitVMOwner
	}
	return r, nil
}
func (o *gitVMUnitOwner) write(r gitVMUnitRecord) error {
	if _, err := o.recordImageHash(r.StubHash); err != nil || !r.valid() {
		return errGitVMOwner
	}
	return o.journal.replace(r)
}

// prepare records an attempt before any unit exists. A previous unstopped
// attempt must be recovered first; there is never more than one live source VM.
func (o *gitVMUnitOwner) prepare(ctx context.Context, q gitVMGuestRequest) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	if !q.valid() {
		return errGitVMOwner
	}
	value, err := o.journal.read()
	if err != nil || len(value.Records) >= gitVMOwnerRecords {
		return errGitVMOwner
	}
	attempt := q.Source.Binding.Attempt
	for _, r := range value.Records {
		if !r.Stopped || r.Request.Source.Binding.Attempt == attempt {
			return errGitVMOwner
		}
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot == "" {
		return errGitVMOwner
	}
	hash, err := o.imageHash()
	if err != nil {
		return err
	}
	r := gitVMUnitRecord{Request: q, RequestHash: Digest(q), Unit: "sfl-alpha-" + attempt + ".service", BootID: boot, ManagerOwner: o.manager.Owner(), StubHash: hash}
	if !r.valid() {
		return errGitVMOwner
	}
	value.Records = append(value.Records, r)
	return o.journal.write(value)
}

// forget drops one terminal record so the bounded journal can admit new work.
// The composing factory calls it only after its staging is retired.
func (o *gitVMUnitOwner) forget(ctx context.Context, attempt string) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	value, err := o.journal.read()
	if err != nil {
		return errGitVMOwner
	}
	kept := []gitVMUnitRecord{}
	found := false
	for _, r := range value.Records {
		if r.Request.Source.Binding.Attempt == attempt {
			if !r.Stopped || found {
				return errGitVMOwner
			}
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return errGitVMOwner
	}
	if fd, ok := o.rootPIDFDs[attempt]; ok {
		unix.Close(fd)
		delete(o.rootPIDFDs, attempt)
	}
	value.Records = kept
	return o.journal.write(value)
}

func expectedGitVMUnit(r gitVMUnitRecord, v unitObservation) bool {
	return v.Unit == r.Unit && v.Path.IsValid() && gitQuarantineHex(v.InvocationID, 32) && v.Description == gitVMUnitDescriptionPrefix+r.RequestHash && v.Transient && v.Job.ID == 0 && v.MemoryMax == gitVMUnitMemory && v.MemorySwapMax == 0 && v.TasksMax == gitVMUnitTasks && v.CPUQuotaPerSecUSec == 1000000 && v.KillMode == "control-group" && v.Restart == "no" && expectedUnitLifecycle(v, r.Revoked) && v.NoNewPrivileges
}

func gitVMBirthProperties(r gitVMUnitRecord, v unitObservation) bool {
	job := v.Job
	v.Job = unitJob{}
	// Fixed properties exist before activation assigns an invocation/cgroup.
	v.InvocationID = strings.Repeat("a", 32)
	if !expectedGitVMUnit(r, v) {
		return false
	}
	return job.ID == 0 && (job.Path == "" || job.Path == "/") || string(job.Path) == r.StartJob && strings.HasSuffix(r.StartJob, "/"+strconv.FormatUint(uint64(job.ID), 10))
}

// captureBirth binds the original invocation and cgroup while this fresh
// accepted job is still attached. Recovery never reconstructs a missing birth.
func (o *gitVMUnitOwner) captureBirth(ctx context.Context, r gitVMUnitRecord) (gitVMUnitRecord, error) {
	if ctx.Err() != nil || !r.StartIntent || r.StartJob == "" || r.born() || r.Captured || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return r, errGitVMOwner
	}
	var before unitObservation
	var group cgroupBinding
	var path dbus.ObjectPath
	var invocation, controlGroup string
	for {
		if ctx.Err() != nil {
			return r, errGitVMOwner
		}
		var err error
		before, err = o.manager.Observe(ctx, r.Unit)
		if err != nil || !gitVMBirthProperties(r, before) || !strings.HasPrefix(string(before.Path), "/org/freedesktop/systemd1/unit/") || path != "" && path != before.Path {
			return r, errGitVMOwner
		}
		path = before.Path
		pending := before.Job.ID != 0 && (before.ActiveState == "activating" || before.ActiveState == "inactive")
		missingInvocation := before.InvocationID == "" || before.InvocationID == strings.Repeat("0", 32)
		missingGroup := before.ControlGroup == ""
		if !missingInvocation && !gitQuarantineHex(before.InvocationID, 32) || !missingGroup && !validUnitCgroup(before.ControlGroup, r.Unit) || invocation != "" && invocation != before.InvocationID || controlGroup != "" && controlGroup != before.ControlGroup {
			return r, errGitVMOwner
		}
		if !missingInvocation {
			// The first invocation must be observed with our job attached.
			if invocation == "" && before.Job.ID == 0 {
				return r, errGitVMOwner
			}
			invocation = before.InvocationID
		}
		if !missingGroup {
			controlGroup = before.ControlGroup
		}
		if !missingInvocation && !missingGroup {
			group, err = o.kernel.Capture(before.ControlGroup, r.Unit)
			if err == nil {
				if group.Path != before.ControlGroup {
					return r, errGitVMOwner
				}
				break
			}
			if !os.IsNotExist(err) {
				return r, errGitVMOwner
			}
		}
		if !pending {
			return r, errGitVMOwner
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, errGitVMOwner
		case <-timer.C:
		}
	}
	birth := gitVMBirth{r.StartJob, before.InvocationID, before.Path, group}
	if !birth.valid(r.Unit, r.StartJob) {
		return r, errGitVMOwner
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !gitVMBirthProperties(r, after) || after.Path != before.Path || after.InvocationID != before.InvocationID || after.ControlGroup != before.ControlGroup {
		return r, errGitVMOwner
	}
	current, err := o.kernel.Capture(after.ControlGroup, r.Unit)
	if err != nil {
		// A fast failure may remove the original group. Only that original
		// hierarchy can prove removal; never capture a replacement.
		if !os.IsNotExist(err) || !expectedGitVMUnit(r, after) || after.MainPID != 0 || o.kernel.Empty(group) != nil {
			return r, errGitVMOwner
		}
		confirmed, observeErr := o.manager.Observe(ctx, r.Unit)
		if observeErr != nil || confirmed != after || o.kernel.Empty(group) != nil {
			return r, errGitVMOwner
		}
	} else if current != group {
		return r, errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID || ctx.Err() != nil {
		return r, errGitVMOwner
	}
	r.Birth = birth
	return r, nil
}

// capture binds the live stub process of the born original invocation and
// retains its pidfd. It never selects a replacement under the same unit name.
func (o *gitVMUnitOwner) capture(ctx context.Context, r gitVMUnitRecord) (gitVMUnitRecord, error) {
	if r.ManagerOwner != o.manager.Owner() || ctx.Err() != nil || !r.born() {
		return r, errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return r, errGitVMOwner
	}
	observed, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitVMUnit(r, observed) || observed.MainPID == 0 || r.Birth.InvocationID != observed.InvocationID || r.Birth.UnitPath != observed.Path || r.Birth.Cgroup.Path != observed.ControlGroup {
		return r, errGitVMOwner
	}
	fd, err := unix.PidfdOpen(int(observed.MainPID), 0)
	if err != nil {
		return r, errGitVMOwner
	}
	good := false
	defer func() {
		if !good {
			unix.Close(fd)
		}
	}()
	unit, invocation, err := o.manager.UnitForPIDFD(ctx, fd)
	if err != nil || unit != r.Unit || invocation != observed.InvocationID {
		return r, errGitVMOwner
	}
	identity, err := processGateIdentity(int(observed.MainPID), invocation)
	if err != nil || identity.BootID != r.BootID || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return r, errGitVMOwner
	}
	if imageErr := o.kernel.Executable(identity.PID, r.StubHash); imageErr != nil {
		if errors.Is(imageErr, errStubImagePending) {
			return r, errors.Join(errGitVMOwner, errStubImagePending)
		}
		return r, errGitVMOwner
	}
	group, err := o.kernel.Capture(observed.ControlGroup, r.Unit)
	if err != nil || group != r.Birth.Cgroup || r.Captured && (r.Identity != identity || r.Cgroup != group) {
		return r, errGitVMOwner
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitVMUnit(r, after) || after.InvocationID != invocation || after.MainPID != observed.MainPID || after.ControlGroup != observed.ControlGroup {
		return r, errGitVMOwner
	}
	r.StartSettled, r.Captured, r.Identity, r.Cgroup = true, true, identity, group
	if old, ok := o.rootPIDFDs[r.Request.Source.Binding.Attempt]; ok {
		unix.Close(old)
	}
	o.rootPIDFDs[r.Request.Source.Binding.Attempt] = fd
	good = true
	return r, nil
}
func (o *gitVMUnitOwner) captureStarted(ctx context.Context, r gitVMUnitRecord) (gitVMUnitRecord, error) {
	for {
		captured, err := o.capture(ctx, r)
		if !errors.Is(err, errStubImagePending) {
			return captured, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, errGitVMOwner
		case <-timer.C:
		}
	}
}

// start launches the fixed stub for one prepared attempt. Durable start intent
// precedes the manager call; a lost reply, failed job or crash is never retried.
func (o *gitVMUnitOwner) start(ctx context.Context, attempt, endpoint string) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	if !filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || len(endpoint) > 100 {
		return errGitVMOwner
	}
	r, err := o.read(attempt)
	if err != nil || r.StubHash != o.stubHash || r.StartIntent || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return errGitVMOwner
	}
	if _, err := gitLivePIDFDIdentity(o.supervisor); err != nil {
		return errGitVMOwner
	}
	supervisorIdentity, err := processGateIdentity(os.Getpid(), strings.Repeat("0", 32))
	if err != nil || gitCheckProcessPIDFD(supervisorIdentity, int(o.supervisor.Fd())) != nil {
		return errGitVMOwner
	}
	raw, err := json.Marshal(r.Request)
	if err != nil {
		return errGitVMOwner
	}
	r.StartIntent = true
	if o.write(r) != nil {
		return errGitVMOwner
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	job, err := o.manager.StartGitVM(bounded, r.Unit, retainedStubExec, []string{retainedStubExec, endpoint, string(raw), r.RequestHash, strconv.Itoa(os.Getpid())}, gitVMUnitDescriptionPrefix+r.RequestHash, int(o.image.file.Fd()), int(o.supervisor.Fd()))
	if err != nil || !gitVMJob(job) {
		return errGitVMOwner
	}
	r.StartJob = job
	if o.write(r) != nil {
		return errGitVMOwner
	}
	r, err = o.captureBirth(bounded, r)
	if err != nil || o.write(r) != nil || o.manager.WaitJob(bounded, job, r.Unit) != nil {
		return errGitVMOwner
	}
	r.StartSettled = true
	if o.write(r) != nil {
		return errGitVMOwner
	}
	captured, err := o.captureStarted(bounded, r)
	if err != nil || o.write(captured) != nil {
		return errGitVMOwner
	}
	// Only this completed fresh start may grant. Recovery never re-arms it.
	o.grantEligible[attempt] = true
	return nil
}

func (o *gitVMUnitOwner) rootDead(r gitVMUnitRecord) error {
	if !r.Captured {
		return errGitVMOwner
	}
	fd, retained := o.rootPIDFDs[r.Request.Source.Binding.Attempt]
	if !retained {
		var err error
		fd, err = unix.PidfdOpen(r.Identity.PID, 0)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return errGitVMOwner
		}
		defer unix.Close(fd)
		current, err := processGateIdentity(r.Identity.PID, r.Identity.InvocationID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return errGitVMOwner
		}
		// PID reuse proves the old process dead but never selects a new root.
		if current.BootID != r.Identity.BootID || current.StartTicks != r.Identity.StartTicks {
			return nil
		}
	}
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(events, 0)
	if err != nil || n != 1 || events[0].Revents&unix.POLLIN == 0 {
		return errGitVMOwner
	}
	return nil
}

// stopBeforeCapture proves an exited born instance through its unchanged
// birth and repeated original-cgroup emptiness. A live root returns unhandled.
func (o *gitVMUnitOwner) stopBeforeCapture(ctx context.Context, r gitVMUnitRecord) (gitVMUnitRecord, bool, error) {
	if ctx.Err() != nil || r.Captured || !r.StartIntent || !r.Revoked || r.GrantHash != "" || o.grantEligible[r.Request.Source.Binding.Attempt] || r.ManagerOwner != o.manager.Owner() || !r.born() {
		return r, true, errGitVMOwner
	}
	before, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || !expectedGitVMUnit(r, before) {
		return r, true, errGitVMOwner
	}
	if before.MainPID != 0 {
		return r, false, nil
	}
	if before.ActiveState != "inactive" && before.ActiveState != "failed" && before.ActiveState != "active" {
		return r, true, errGitVMOwner
	}
	if r.Birth.InvocationID != before.InvocationID || r.Birth.UnitPath != before.Path || r.Birth.Cgroup.Path != before.ControlGroup || o.kernel.Empty(r.Birth.Cgroup) != nil {
		return r, true, errGitVMOwner
	}
	after, err := o.manager.Observe(ctx, r.Unit)
	if err != nil || after != before || o.kernel.Empty(r.Birth.Cgroup) != nil {
		return r, true, errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID || ctx.Err() != nil {
		return r, true, errGitVMOwner
	}
	r.Stopped = true
	return r, true, nil
}

// stop revokes the attempt durably, then proves the original instance gone.
// It never stops a unit by name: that call has no invocation precondition.
// The composing run must already have joined its channels and grant.
func (o *gitVMUnitOwner) stop(ctx context.Context, attempt string) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	r, err := o.read(attempt)
	if err != nil {
		return errGitVMOwner
	}
	r.Revoked = true
	delete(o.grantEligible, attempt)
	if o.write(r) != nil {
		return errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot == "" {
		return errGitVMOwner
	}
	if !r.StartIntent {
		r.Stopped = true
		return o.write(r)
	}
	if boot != r.BootID {
		// A new boot proves every original process gone, born or not.
		if !r.Stopped {
			r.Stopped = true
			if !r.born() {
				r.StopBoot = boot
			}
		}
		return o.write(r)
	}
	if r.ManagerOwner != o.manager.Owner() {
		return errGitVMOwner
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	if r.Stopped {
		group := r.Cgroup
		if !r.Captured {
			if !r.born() {
				return errGitVMOwner
			}
			group = r.Birth.Cgroup
		}
		if o.kernel.Empty(group) != nil || r.Captured && o.rootDead(r) != nil {
			return errGitVMOwner
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
				return errGitVMOwner
			}
			return o.manager.RetireUnit(bounded, r.Unit)
		}
		r, err = o.captureStarted(bounded, r)
		if err != nil || o.write(r) != nil {
			return errGitVMOwner
		}
	}
	before, err := o.settledOriginal(bounded, r)
	if err != nil {
		return o.refuseStop("original-observe")
	}
	if o.kernel.Terminate(bounded, r.Cgroup) != nil {
		return o.refuseStop("terminate")
	}
	if o.kernel.Empty(r.Cgroup) != nil || o.rootDead(r) != nil {
		return o.refuseStop("original-empty")
	}
	after, err := waitForOriginalUnitStop(bounded, o.manager, o.kernel, before, r.Cgroup)
	if err != nil || !expectedGitVMUnit(r, after) || after.InvocationID != r.Identity.InvocationID || after.ControlGroup != r.Cgroup.Path && after.ControlGroup != "" || after.MainPID != 0 || (after.ActiveState != "inactive" && after.ActiveState != "failed" && after.ActiveState != "active") {
		return o.refuseStop("original-settle")
	}
	r.Stopped = true
	if o.write(r) != nil {
		return o.refuseStop("receipt")
	}
	return o.manager.RetireUnit(bounded, r.Unit)
}

// settledOriginal observes the captured original invocation before stop. An
// exiting unit can briefly carry its own job; wait, bounded by ctx, for that
// same invocation to settle rather than refusing. Any other difference,
// including a replacement invocation, still refuses.
func (o *gitVMUnitOwner) settledOriginal(ctx context.Context, r gitVMUnitRecord) (unitObservation, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		v, err := o.manager.Observe(ctx, r.Unit)
		if err != nil || v.InvocationID != r.Identity.InvocationID {
			return unitObservation{}, errGitVMOwner
		}
		if expectedGitVMUnit(r, v) {
			return v, nil
		}
		pending := v
		pending.Job = unitJob{}
		if v.Job.ID == 0 || !expectedGitVMUnit(r, pending) {
			return unitObservation{}, errGitVMOwner
		}
		select {
		case <-ctx.Done():
			return unitObservation{}, errGitVMOwner
		case <-ticker.C:
		}
	}
}

// refuseStop records a fixed, non-secret stage for the latest stop refusal.
// Caller holds the owner token.
func (o *gitVMUnitOwner) refuseStop(stage string) error {
	o.stopFailure = stage
	return errGitVMOwner
}

// recover stops every unstopped attempt left by an earlier process. Each
// remains owned and blocks new work until its original instance is proved gone.
func (o *gitVMUnitOwner) recover(ctx context.Context) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	value, err := o.journal.read()
	o.leave()
	if err != nil {
		return errGitVMOwner
	}
	var failed error
	for _, r := range value.Records {
		if !r.Stopped {
			if err := o.stop(ctx, r.Request.Source.Binding.Attempt); err != nil {
				failed = errGitVMOwner
			}
		}
	}
	return failed
}

// Close keeps the lock and pinned image while any attempt lacks a stop
// receipt. It never abandons an uncertain unit.
func (o *gitVMUnitOwner) Close() error {
	if o == nil {
		return nil
	}
	select {
	case <-o.operation:
	default:
		return errGitVMOwner
	}
	defer o.leave()
	if o.closed {
		return nil
	}
	value, err := o.journal.read()
	if err != nil {
		return errGitVMOwner
	}
	for _, r := range value.Records {
		if !r.Stopped {
			return errGitVMOwner
		}
	}
	o.closed = true
	for attempt, fd := range o.rootPIDFDs {
		unix.Close(fd)
		delete(o.rootPIDFDs, attempt)
	}
	return errors.Join(o.journal.Close(context.Background()), o.supervisor.Close(), o.image.Close(), o.manager.Close(), o.root.Close())
}

// has reports whether the journal holds a record for attempt.
func (o *gitVMUnitOwner) has(ctx context.Context, attempt string) (bool, error) {
	if !o.enter(ctx) {
		return false, errGitVMOwner
	}
	defer o.leave()
	value, err := o.journal.read()
	if err != nil {
		return false, errGitVMOwner
	}
	_, ok := value.record(attempt)
	return ok, nil
}

// confirmStopped revalidates one durable granted stop receipt before retained
// source is used. On the same boot the captured original cgroup must still be
// empty and its root dead; a later boot alone proves every process gone.
func (o *gitVMUnitOwner) confirmStopped(ctx context.Context, attempt string) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	r, err := o.read(attempt)
	if err != nil || !r.Stopped || !r.Revoked || !r.Captured || !validAuthHash(r.GrantHash) {
		return errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot == "" {
		return errGitVMOwner
	}
	if boot != r.BootID {
		return nil
	}
	if o.kernel.Empty(r.Cgroup) != nil || o.rootDead(r) != nil {
		return errGitVMOwner
	}
	return nil
}
