//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

type VMGrantFixtureReceipt struct {
	Version                                                                 int
	SourceHead, SourceTree, Root, Unit, StubHash, BindingHash, ManifestHash string
	Started, BeforeGrantInert, Granted, Stopped, Recovered, Passed          bool
	FakeOnly, ProductionBoundaryQualified                                   bool
	Execution                                                               VMGrantExecutionReceipt
	Journal                                                                 json.RawMessage
	Error                                                                   string
}

// successfulVMStubExit checks actual Service properties after MainPID reaches
// zero. A saved success-shaped file cannot hide failure while syncing it.
func (m *dbusContainmentManager) successfulVMStubExit(ctx context.Context, unit string, identity GateIdentity) (bool, error) {
	o, err := m.Observe(ctx, unit)
	if err != nil {
		return false, err
	}
	if o.InvocationID != identity.InvocationID {
		return false, ErrVMBoot
	}
	if o.MainPID != 0 {
		return false, nil
	}
	properties, err := m.properties(ctx, o.Path, "org.freedesktop.systemd1.Service")
	if err != nil {
		return false, err
	}
	var code, status int32
	var result string
	for _, read := range []struct {
		name   string
		target any
	}{{"ExecMainCode", &code}, {"ExecMainStatus", &status}, {"Result", &result}} {
		v, ok := properties[read.name]
		if !ok || dbus.Store([]any{v.Value()}, read.target) != nil {
			return false, ErrVMBoot
		}
	}
	if code != 1 || status != 0 || result != "success" || o.ActiveState != "active" {
		return false, ErrVMBoot
	}
	return true, nil
}

func validVMGrantExecution(r VMGrantExecutionReceipt, binding AuthBinding, manifest string) bool {
	return r.Version == 1 && r.BindingHash == Digest(binding) && r.ManifestHash == manifest && r.VerifiedExecutable && r.QEMUExited && r.FakeOnly && !r.ProductionBoundaryQualified && r.Error == ""
}

func exerciseVMGrant(ctx context.Context, config VMBootFixtureConfig, stub, stubHash string) (receipt VMGrantFixtureReceipt) {
	receipt = VMGrantFixtureReceipt{Version: 1, SourceHead: config.SourceHead, SourceTree: config.SourceTree, StubHash: stubHash, BindingHash: Digest(config.Boot.Binding), FakeOnly: true}
	fail := func(err error) {
		receipt.Passed = false
		if receipt.Error == "" {
			receipt.Error = err.Error()
		}
	}
	root, err := os.MkdirTemp("", "sfl-vm-unit-")
	if err != nil {
		fail(ErrVMBoot)
		return
	}
	receipt.Root = root
	gates := filepath.Join(root, "g")
	if os.Mkdir(gates, 0700) != nil {
		fail(ErrVMBoot)
		return
	}
	// Recovery retains the pinned cache even after original pathname replacement.
	file, err := os.OpenFile(stub, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		fail(ErrVMBoot)
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		file.Close()
		fail(ErrVMBoot)
		return
	}
	rawStub, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || len(rawStub) > 64<<20 {
		fail(ErrVMBoot)
		return
	}
	mutable := filepath.Join(root, "vmstub")
	if os.WriteFile(mutable, rawStub, 0500) != nil {
		fail(ErrVMBoot)
		return
	}
	images, err := OpenVMBootImages(ctx, config.Boot)
	if err != nil {
		fail(err)
		return
	}
	defer func() {
		if err := images.Close(); err != nil {
			fail(err)
		}
	}()
	manifest, err := images.FixtureManifest()
	if err != nil {
		fail(err)
		return
	}
	receipt.ManifestHash = Digest(manifest)
	binding := config.Boot.Binding
	receipt.Unit = "sfl-alpha-" + binding.LaunchID + ".service"
	supervisor, err := OpenVMFixtureSupervisor(ctx, gates, mutable, stubHash)
	if err != nil {
		fail(err)
		return
	}
	prepared := false
	defer func() {
		if prepared {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := supervisor.AbortAndWait(cleanup, binding); err != nil {
				fail(err)
			} else {
				receipt.Stopped = true
			}
			cancel()
		}
		if err := supervisor.Close(); err != nil {
			fail(err)
		}
		receipt.Passed = receipt.Error == "" && receipt.Started && receipt.BeforeGrantInert && receipt.Granted && receipt.Stopped && receipt.Recovered && validVMGrantExecution(receipt.Execution, binding, receipt.ManifestHash)
	}()
	if err := supervisor.Prepare(binding, manifest); err != nil {
		fail(err)
		return
	}
	prepared = true
	if err := supervisor.Start(ctx, binding); err != nil {
		fail(err)
		return
	}
	receipt.Started = true
	if err := os.Rename(mutable, mutable+".original"); err != nil {
		fail(err)
		return
	}
	if err := os.WriteFile(mutable, []byte("replacement must not execute"), 0500); err != nil {
		fail(err)
		return
	}
	serialPath := filepath.Join(gates, binding.LaunchID, "vm-serial.log")
	if _, err := os.Lstat(serialPath); !os.IsNotExist(err) {
		fail(ErrVMBoot)
		return
	}
	receipt.BeforeGrantInert = true
	journal, err := supervisor.supervisor.snapshotUnit(ctx, binding)
	if err != nil {
		fail(err)
		return
	}
	if err := supervisor.Grant(ctx, binding, images); err != nil {
		fail(err)
		return
	}
	receipt.Granted = true
	manager, ok := supervisor.supervisor.manager.(*dbusContainmentManager)
	if !ok {
		fail(ErrVMBoot)
		return
	}
	for {
		done, err := manager.successfulVMStubExit(ctx, receipt.Unit, journal.Identity)
		if err != nil {
			fail(err)
			return
		}
		if done {
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			fail(ctx.Err())
			return
		case <-timer.C:
		}
	}
	gateRoot, err := os.OpenRoot(filepath.Join(gates, binding.LaunchID))
	if err != nil {
		fail(err)
		return
	}
	raw, err := readAuth(gateRoot, "vm-execution.json", false)
	gateRoot.Close()
	if err != nil {
		fail(err)
		return
	}
	receipt.Execution, err = DecodeStrict[VMGrantExecutionReceipt](raw, 4096)
	if err != nil || !validVMGrantExecution(receipt.Execution, binding, receipt.ManifestHash) {
		fail(ErrVMBoot)
		return
	}
	if err := supervisor.AbortAndWait(ctx, binding); err != nil {
		fail(err)
		return
	}
	receipt.Stopped = true
	// A fresh supervisor must recover the durable stopped binding without a new
	// start or grant, even though the original stub pathname has been replaced.
	recovered, err := OpenVMFixtureSupervisor(ctx, gates, mutable, stubHash)
	if err != nil {
		fail(err)
		return
	}
	stopErr := recovered.AbortAndWait(ctx, binding)
	journal, err = recovered.supervisor.snapshotUnit(ctx, binding)
	closeErr = recovered.Close()
	if err := errors.Join(stopErr, err, closeErr); err != nil {
		fail(err)
		return
	}
	if !journal.Captured || !journal.StartSettled || !journal.Aborted || !journal.Stopped || journal.PayloadKind != "vm-fixture" {
		fail(ErrVMBoot)
		return
	}
	receipt.Journal, err = json.Marshal(journal)
	if err != nil {
		fail(err)
		return
	}
	receipt.Recovered = true
	return
}

// VMGrantFixtureMain is explicit fake-only host qualification. Ordinary tests
// never invoke it. The outer driver must independently validate guest output.
func VMGrantFixtureMain(ctx context.Context, args []string, output io.Writer) int {
	if len(args) != 3 || output == nil || !sourcePath(args[1]) || !validAuthHash(args[2]) {
		return 2
	}
	config, err := readVMBootFixture(args[0])
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	receipt := exerciseVMGrant(ctx, config, args[1], args[2])
	if json.NewEncoder(output).Encode(receipt) != nil {
		return 1
	}
	if !receipt.Passed {
		return 1
	}
	return 0
}
