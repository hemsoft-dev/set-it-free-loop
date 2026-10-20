//go:build linux

package reviewer

import "context"

// VMFixtureSupervisor owns a separately pinned cmd/vmstub payload and version-2
// descriptor grant. It deliberately does not implement AuthStopVerifier or
// WorkerLauncher. Synthetic boot/whole-unit stop cannot qualify native auth.
type VMFixtureSupervisor struct{ supervisor *SystemdSupervisor }

func OpenVMFixtureSupervisor(ctx context.Context, root, stub, hash string) (*VMFixtureSupervisor, error) {
	s, err := OpenSystemdSupervisor(ctx, root, stub, hash)
	if err != nil {
		return nil, err
	}
	s.payloadKind = "vm-fixture"
	return &VMFixtureSupervisor{s}, nil
}
func (v *VMFixtureSupervisor) Prepare(binding AuthBinding, manifest VMFixtureManifest) error {
	if !manifest.valid() || manifest.Binding != binding {
		return ErrVMBoot
	}
	return v.supervisor.preparePayload(binding, Digest(manifest), 2)
}
func (v *VMFixtureSupervisor) Start(ctx context.Context, binding AuthBinding) error {
	return v.supervisor.startPayload(ctx, binding)
}
func (v *VMFixtureSupervisor) Grant(ctx context.Context, binding AuthBinding, images *VMBootImages) error {
	m, err := images.FixtureManifest()
	if err != nil || m.Binding != binding {
		return ErrVMBoot
	}
	s := v.supervisor
	r, err := s.snapshotUnit(ctx, binding)
	if err != nil || r.Aborted || !r.StartSettled || !r.Captured || r.Stopped || r.PayloadKind != "vm-fixture" || r.ManifestHash != Digest(m) {
		return ErrAuthRecoveryRequired
	}
	ctx, cancel := boundedManagerContext(ctx)
	defer cancel()
	if s.image.verify() != nil {
		return ErrAuthRecoveryRequired
	}
	if _, err = s.captureUnit(ctx, r); err != nil {
		return err
	}
	s.mu.Lock()
	gate := s.gates[binding]
	s.mu.Unlock()
	if gate == nil {
		return ErrAuthRecoveryRequired
	}
	return gate.grantVMFixture(ctx, r.Identity, images)
}
func (v *VMFixtureSupervisor) AbortAndWait(ctx context.Context, binding AuthBinding) error {
	return v.supervisor.AbortAndWait(ctx, binding)
}
func (v *VMFixtureSupervisor) Close() error { return v.supervisor.Close() }
