//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
)

type VMBootFixtureConfig struct {
	Version             int
	Purpose, SourceHead string
	Boot                VMBootSpec
}
type VMBootFixtureReceipt struct {
	Version                               int
	SourceHead, BindingHash, BootSpecHash string
	VerifiedExecutable, QEMUExited        bool
	FakeOnly, ProductionBoundaryQualified bool
	Error                                 string
}

func fixtureBuildHead() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	head := ""
	dirty := true
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			head = setting.Value
		}
		if setting.Key == "vcs.modified" {
			dirty = setting.Value != "false"
		}
	}
	if dirty {
		return ""
	}
	return head
}
func readVMBootFixture(path string) (VMBootFixtureConfig, error) {
	var config VMBootFixtureConfig
	if !sourcePath(path) {
		return config, ErrVMBoot
	}
	directory := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory || privateDirectory(directory) != nil {
		return config, ErrVMBoot
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return config, ErrVMBoot
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return config, ErrVMBoot
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Size() > 65536 {
		return config, ErrVMBoot
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return config, ErrVMBoot
	}
	config, err = DecodeStrict[VMBootFixtureConfig](raw, 65536)
	if err != nil || config.Version != 1 || config.Purpose != "synthetic-role-fixture" || config.SourceHead != fixtureBuildHead() || !objectID.MatchString(config.SourceHead) || config.Boot.validate() != nil {
		return VMBootFixtureConfig{}, ErrVMBoot
	}
	return config, nil
}

// VMBootFixtureMain executes only from an explicitly created bounded transient
// fixture unit. Its receipt proves QEMU execution, not guest acceptance or whole
// unit shutdown; the external fixture driver must verify both before passing.
func VMBootFixtureMain(ctx context.Context, args []string, output io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	config, err := readVMBootFixture(args[0])
	if err != nil {
		return 2
	}
	receipt := VMBootFixtureReceipt{Version: 1, SourceHead: config.SourceHead, BindingHash: Digest(config.Boot.Binding), BootSpecHash: Digest(config.Boot), FakeOnly: true}
	run := func() error {
		if _, err := vmFixtureUnit(config.Boot.Binding.LaunchID); err != nil {
			return err
		}
		root, err := os.OpenRoot(filepath.Dir(args[0]))
		if err != nil {
			return ErrVMBoot
		}
		defer root.Close()
		serial, err := root.OpenFile("serial.log", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return ErrVMBoot
		}
		images, err := OpenVMBootImages(ctx, config.Boot)
		if err != nil {
			return errors.Join(err, serial.Close())
		}
		verified, runErr := images.RunVMBootFixture(ctx, serial)
		receipt.VerifiedExecutable = verified
		receipt.QEMUExited = runErr == nil
		// Finalize every required local resource before writing a success-shaped
		// execution receipt. The caller still owns external systemd/cgroup cleanup.
		return errors.Join(runErr, images.Close(), serial.Sync(), serial.Close())
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	err = run()
	if err != nil {
		receipt.Error = ErrVMBoot.Error()
	}
	if json.NewEncoder(output).Encode(receipt) != nil {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}
