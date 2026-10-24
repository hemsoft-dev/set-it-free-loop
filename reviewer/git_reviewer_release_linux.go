//go:build linux

package reviewer

import (
	"bytes"
	"encoding/json"
)

const gitReviewerReleaseBytes = 256 << 10

// GitReviewerRelease is a trusted package descriptor, never PR configuration.
// Its digest pins acquisition and provider assets together. Creating a package
// does not promote it, attest containment or authorize subscription use.
//
// The acquisition mechanism is part of the package digest. Version 1 selects
// host Git acquisition through Worker, NamespaceHelper, GitArtifact and
// GitRuntime. Version 2 selects the measured source VM through SourceVM, and
// its host acquisition fields must be zero. Version 1 bytes are unchanged, so
// no existing release can silently change mechanism.
type GitReviewerRelease struct {
	Version         int
	Revision        string
	Worker          VMBootAsset
	NamespaceHelper VMBootAsset
	GitArtifact     VMBootAsset
	GitRuntime      GitRuntimePins
	Providers       map[Provider]GitReviewerProviderRelease
	SourceVM        *GitReviewerSourceVMRelease `json:",omitempty"`
}

// gitReviewerReleaseV1 is the exact version 1 wire shape, without SourceVM.
type gitReviewerReleaseV1 struct {
	Version         int
	Revision        string
	Worker          VMBootAsset
	NamespaceHelper VMBootAsset
	GitArtifact     VMBootAsset
	GitRuntime      GitRuntimePins
	Providers       map[Provider]GitReviewerProviderRelease
}

// GitReviewerSourceVMRelease pins the host stub, the measured guest boot
// inputs, the host QEMU runtime and namespace helper, and the guest's worker
// and Git runtime. It carries no credential, provider, URL or argument.
type GitReviewerSourceVMRelease struct {
	Stub                                                     VMBootAsset
	QEMU, Kernel, Initrd, BIOS, QEMURuntime, NamespaceHelper VMBootAsset
	BootstrapLibraries                                       []VMBootAsset
	Worker                                                   VMAssetPin
	Runtime                                                  GitRuntimePins
}

// assetSpec derives the exact source VM manifest these pins describe.
func (s GitReviewerSourceVMRelease) assetSpec() (gitVMAssetSpec, error) {
	filter, err := gitFixedFilterPin()
	if err != nil || !gitReviewerAsset(s.Stub, 64<<20) || len(s.BootstrapLibraries) > 16 {
		return gitVMAssetSpec{}, ErrLiveBlocked
	}
	libraries, err := nativeBootstrapLibraryPins(s.BootstrapLibraries)
	if err != nil {
		return gitVMAssetSpec{}, ErrLiveBlocked
	}
	spec := gitVMAssetSpec{QEMU: s.QEMU, Kernel: s.Kernel, Initrd: s.Initrd, BIOS: s.BIOS, QEMURuntime: s.QEMURuntime, NamespaceHelper: s.NamespaceHelper, BootstrapLibraries: append([]VMBootAsset(nil), s.BootstrapLibraries...)}
	spec.Manifest = gitVMManifest{Version: 1, Purpose: gitVMSourcePurpose, QEMU: gitVMAssetPin(s.QEMU), Kernel: gitVMAssetPin(s.Kernel), Initrd: gitVMAssetPin(s.Initrd), BIOS: gitVMAssetPin(s.BIOS), QEMURuntime: gitVMAssetPin(s.QEMURuntime), NamespaceHelper: gitVMAssetPin(s.NamespaceHelper), BootstrapLibraries: libraries, Runtime: s.Runtime, Worker: s.Worker, Filter: filter}
	if !spec.valid() {
		return gitVMAssetSpec{}, ErrLiveBlocked
	}
	return spec, nil
}

type GitReviewerProviderRelease struct {
	Identity                       Identity
	Stub                           VMBootAsset
	QEMU, Kernel, Initrd, Firmware VMBootAsset
	Runtime                        NativeVMRuntimeSpec
	Policy                         EgressPolicy
}

func gitReviewerAsset(a VMBootAsset, limit int64) bool {
	return sourcePath(a.Path) && validAuthHash(a.SHA256) && a.Bytes > 0 && a.Bytes <= limit
}

func (r GitReviewerRelease) Validate() error {
	if !objectID.MatchString(r.Revision) || len(r.Providers) < 1 || len(r.Providers) > 2 {
		return ErrLiveBlocked
	}
	switch r.Version {
	case 1:
		if r.SourceVM != nil || !gitReviewerAsset(r.Worker, 64<<20) || !gitReviewerAsset(r.NamespaceHelper, 64<<20) || !gitReviewerAsset(r.GitArtifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) || r.GitRuntime.Artifact != (VMAssetPin{r.GitArtifact.SHA256, r.GitArtifact.Bytes}) || !gitRuntimePin(r.GitRuntime.Git, 64<<20) || !gitRuntimePin(r.GitRuntime.HTTP, 64<<20) {
			return ErrLiveBlocked
		}
	case 2:
		if r.SourceVM == nil || r.Worker != (VMBootAsset{}) || r.NamespaceHelper != (VMBootAsset{}) || r.GitArtifact != (VMBootAsset{}) || r.GitRuntime != (GitRuntimePins{}) {
			return ErrLiveBlocked
		}
		if _, err := r.SourceVM.assetSpec(); err != nil {
			return ErrLiveBlocked
		}
	default:
		return ErrLiveBlocked
	}
	for provider, p := range r.Providers {
		if !provider.Valid() || p.Identity.Provider != provider || p.Identity.Validate() != nil || p.Policy.Provider != provider {
			return ErrLiveBlocked
		}
		for index, asset := range []VMBootAsset{p.QEMU, p.Kernel, p.Initrd, p.Firmware, p.Runtime.Artifact} {
			if !gitReviewerAsset(asset, vmAssetLimit(index)) {
				return ErrLiveBlocked
			}
		}
		if !gitReviewerAsset(p.Stub, 64<<20) || !gitReviewerAsset(p.Runtime.NamespaceHelper, 64<<20) {
			return ErrLiveBlocked
		}
		if _, err := nativeBootstrapLibraryPins(p.Runtime.BootstrapLibraries); err != nil {
			return ErrLiveBlocked
		}
		if _, err := validateEgressPolicy(provider, DefaultLimits(), p.Policy); err != nil {
			return ErrLiveBlocked
		}
		if !nativeSourcePolicies(provider, p.Policy) {
			return ErrLiveBlocked
		}
	}
	return nil
}

// EncodeGitReviewerRelease emits the canonical package bytes. Paths are trusted
// host selectors and no credentials, transports or eligibility callbacks belong
// in this artifact. Approval must identify both revision and returned digest.
func EncodeGitReviewerRelease(r GitReviewerRelease) ([]byte, string, error) {
	if r.Validate() != nil {
		return nil, "", ErrLiveBlocked
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > gitReviewerReleaseBytes {
		return nil, "", ErrLiveBlocked
	}
	return raw, gitRawHash(raw), nil
}

func DecodeGitReviewerRelease(raw []byte) (GitReviewerRelease, string, error) {
	// The version selects the exact strict wire shape; canonical re-encoding
	// below still rejects any other byte difference.
	var probe struct{ Version int }
	if len(raw) > gitReviewerReleaseBytes || json.Unmarshal(raw, &probe) != nil {
		return GitReviewerRelease{}, "", ErrLiveBlocked
	}
	var r GitReviewerRelease
	var err error
	switch probe.Version {
	case 1:
		var v1 gitReviewerReleaseV1
		v1, err = DecodeStrict[gitReviewerReleaseV1](raw, gitReviewerReleaseBytes)
		r = GitReviewerRelease{Version: v1.Version, Revision: v1.Revision, Worker: v1.Worker, NamespaceHelper: v1.NamespaceHelper, GitArtifact: v1.GitArtifact, GitRuntime: v1.GitRuntime, Providers: v1.Providers}
	case 2:
		r, err = DecodeStrict[GitReviewerRelease](raw, gitReviewerReleaseBytes)
	default:
		err = ErrLiveBlocked
	}
	if err != nil || r.Validate() != nil {
		return GitReviewerRelease{}, "", ErrLiveBlocked
	}
	canonical, hash, err := EncodeGitReviewerRelease(r)
	if err != nil || !bytes.Equal(raw, canonical) {
		return GitReviewerRelease{}, "", ErrLiveBlocked
	}
	return r, hash, nil
}
