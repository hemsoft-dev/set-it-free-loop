//go:build linux

package reviewer

import "context"

// NativeVMSupervisor owns only the distinct native payload. Guest policy,
// private channel/auth readiness and stopped audit validation must be composed
// by the native reviewer before a report is accepted. It starts nothing at
// construction and never loads provider credentials or installs an App.
type NativeVMSupervisor struct{ supervisor *SystemdSupervisor }

func OpenNativeVMSupervisor(ctx context.Context, root, stub, hash string) (*NativeVMSupervisor, error) {
	s, err := OpenSystemdSupervisor(ctx, root, stub, hash)
	if err != nil {
		return nil, err
	}
	s.payloadKind = "native-vm"
	s.kernel = nativeContainmentKernel{}
	return &NativeVMSupervisor{s}, nil
}
func (v *NativeVMSupervisor) Prepare(m NativeVMManifest) error {
	if v == nil || v.supervisor == nil || !m.valid() {
		return ErrVMBoot
	}
	return v.supervisor.preparePayload(m.Binding, Digest(m), 3)
}
func (v *NativeVMSupervisor) Start(ctx context.Context, binding AuthBinding) error {
	if v == nil || v.supervisor == nil {
		return ErrVMBoot
	}
	return v.supervisor.startPayload(ctx, binding)
}
func (v *NativeVMSupervisor) grant(ctx context.Context, m NativeVMManifest, images *VMBootImages, channels *nativeVMChannels) error {
	if v == nil || v.supervisor == nil || !m.valid() {
		return ErrVMBoot
	}
	s := v.supervisor
	r, err := s.snapshotUnit(ctx, m.Binding)
	if err != nil || r.Aborted || !r.StartSettled || !r.Captured || r.Stopped || r.PayloadKind != "native-vm" || r.ManifestHash != Digest(m) {
		return ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	if s.image.verify() != nil {
		return ErrAuthRecoveryRequired
	}
	if _, err := s.captureUnit(ctx, r); err != nil {
		return err
	}
	s.mu.Lock()
	gate := s.gates[m.Binding]
	s.mu.Unlock()
	if gate == nil {
		return ErrAuthRecoveryRequired
	}
	return gate.grantNativeVM(ctx, r.Identity, m, images, channels)
}
func (v *NativeVMSupervisor) AbortAndWait(ctx context.Context, binding AuthBinding) error {
	if v == nil || v.supervisor == nil {
		return ErrAuthRecoveryRequired
	}
	return v.supervisor.AbortAndWait(ctx, binding)
}
func (v *NativeVMSupervisor) ConfirmAuthStopped(ctx context.Context, binding AuthBinding) error {
	if err := v.AbortAndWait(ctx, binding); err != nil {
		return err
	}
	r, err := v.supervisor.snapshotUnit(ctx, binding)
	if err != nil || r.PayloadKind != "native-vm" || !r.Aborted || !r.Stopped {
		return ErrAuthRecoveryRequired
	}
	return nil
}
func (v *NativeVMSupervisor) Close() error {
	if v == nil || v.supervisor == nil {
		return nil
	}
	return v.supervisor.Close()
}
