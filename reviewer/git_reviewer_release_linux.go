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
type GitReviewerRelease struct {
	Version         int
	Revision        string
	Worker          VMBootAsset
	NamespaceHelper VMBootAsset
	GitArtifact     VMBootAsset
	GitRuntime      GitRuntimePins
	Providers       map[Provider]GitReviewerProviderRelease
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
	if r.Version != 1 || !objectID.MatchString(r.Revision) || !gitReviewerAsset(r.Worker, 64<<20) || !gitReviewerAsset(r.NamespaceHelper, 64<<20) || !gitReviewerAsset(r.GitArtifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) || r.GitRuntime.Artifact != (VMAssetPin{r.GitArtifact.SHA256, r.GitArtifact.Bytes}) || !gitRuntimePin(r.GitRuntime.Git, 64<<20) || !gitRuntimePin(r.GitRuntime.HTTP, 64<<20) || len(r.Providers) < 1 || len(r.Providers) > 2 {
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
		if provider == Codex && p.Policy.ResponsePolicy != CodexSourceResponses || provider == Claude && p.Policy.ResponsePolicy != ClaudeSourceMessages {
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
	r, err := DecodeStrict[GitReviewerRelease](raw, gitReviewerReleaseBytes)
	if err != nil || r.Validate() != nil {
		return GitReviewerRelease{}, "", ErrLiveBlocked
	}
	canonical, hash, err := EncodeGitReviewerRelease(r)
	if err != nil || !bytes.Equal(raw, canonical) {
		return GitReviewerRelease{}, "", ErrLiveBlocked
	}
	return r, hash, nil
}
