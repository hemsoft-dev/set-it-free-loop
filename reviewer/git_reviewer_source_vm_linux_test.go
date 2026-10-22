//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitReviewerSourceVMRelease(t *testing.T, stub string) (GitReviewerSourceVMRelease, gitVMAssetSpec) {
	t.Helper()
	spec, _ := gitVMAssetsFixture(t)
	raw, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	vm := GitReviewerSourceVMRelease{Stub: VMBootAsset{stub, gitRawHash(raw), int64(len(raw))}, QEMU: spec.QEMU, Kernel: spec.Kernel, Initrd: spec.Initrd, BIOS: spec.BIOS, QEMURuntime: spec.QEMURuntime, NamespaceHelper: spec.NamespaceHelper, BootstrapLibraries: spec.BootstrapLibraries, Worker: spec.Manifest.Worker, Runtime: spec.Manifest.Runtime}
	derived, err := vm.assetSpec()
	if err != nil || Digest(derived.Manifest) != Digest(spec.Manifest) {
		t.Fatal("source VM release does not derive the pinned manifest", err)
	}
	return vm, spec
}

// Version 1 bytes are unchanged and cannot carry a source VM; version 2 must
// carry one and no host acquisition pins. The digest pins the mechanism.
func TestGitReviewerReleaseVersionPinsAcquisitionMechanism(t *testing.T) {
	o, f, _, _ := gitReviewerStartupFixture(t)
	v1 := o.Releases[o.Current]
	raw, hash, err := EncodeGitReviewerRelease(v1)
	if err != nil || strings.Contains(string(raw), "SourceVM") {
		t.Fatal("version 1 wire changed", err)
	}
	decoded, decodedHash, err := DecodeGitReviewerRelease(raw)
	if err != nil || decodedHash != hash || decoded.SourceVM != nil {
		t.Fatal("version 1 round trip", err)
	}
	vm, _ := gitReviewerSourceVMRelease(t, f.stub)
	withVM := v1
	withVM.SourceVM = &vm
	if _, _, err := EncodeGitReviewerRelease(withVM); err == nil {
		t.Fatal("version 1 accepted a source VM")
	}
	smuggled := strings.TrimSuffix(string(raw), "}") + `,"SourceVM":null}`
	if _, _, err := DecodeGitReviewerRelease([]byte(smuggled)); err == nil {
		t.Fatal("version 1 decode accepted an extra mechanism field")
	}
	v2 := GitReviewerRelease{Version: 2, Revision: v1.Revision, Providers: v1.Providers, SourceVM: &vm}
	raw2, hash2, err := EncodeGitReviewerRelease(v2)
	if err != nil || hash2 == hash {
		t.Fatal("version 2 encode", err)
	}
	back, backHash, err := DecodeGitReviewerRelease(raw2)
	if err != nil || backHash != hash2 || back.SourceVM == nil || Digest(*back.SourceVM) != Digest(vm) {
		t.Fatal("version 2 round trip", err)
	}
	for name, mutate := range map[string]func(*GitReviewerRelease){
		"missing source VM": func(r *GitReviewerRelease) { r.SourceVM = nil },
		"host worker":       func(r *GitReviewerRelease) { r.Worker = v1.Worker },
		"host runtime":      func(r *GitReviewerRelease) { r.GitRuntime = v1.GitRuntime },
		"unpinned stub":     func(r *GitReviewerRelease) { bad := vm; bad.Stub.SHA256 = ""; r.SourceVM = &bad },
		"foreign worker":    func(r *GitReviewerRelease) { bad := vm; bad.Worker.Bytes = 0; r.SourceVM = &bad },
		"version 3":         func(r *GitReviewerRelease) { r.Version = 3 },
	} {
		bad := v2
		mutate(&bad)
		if _, _, err := EncodeGitReviewerRelease(bad); err == nil {
			t.Fatal("invalid version 2 release accepted:", name)
		}
	}
	var generic map[string]any
	if json.Unmarshal(raw2, &generic) != nil {
		t.Fatal("generic")
	}
	generic["SourceVM"] = nil
	nulled, _ := json.Marshal(generic)
	if _, _, err := DecodeGitReviewerRelease(nulled); err == nil {
		t.Fatal("version 2 accepted a null source VM")
	}
}

func gitReviewerSourceVMStartupFixture(t *testing.T) (GitReviewerServiceOptions, gitVMOwnerFixture, func(context.Context, string, string, string) (*gitVMUnitOwner, error)) {
	t.Helper()
	o, _, _, _ := gitReviewerStartupFixture(t)
	vf := newGitVMOwnerFixture(t)
	if vf.owner.Close() != nil {
		t.Fatal("fixture owner transfer")
	}
	vm, _ := gitReviewerSourceVMRelease(t, vf.stub)
	v1 := o.Releases[o.Current]
	v2 := GitReviewerRelease{Version: 2, Revision: v1.Revision, Providers: v1.Providers, SourceVM: &vm}
	o.Releases = map[string]GitReviewerRelease{v2.Revision: v2}
	archive := filepath.Join(t.TempDir(), "source-archive")
	if os.Mkdir(archive, 0700) != nil {
		t.Fatal("archive directory")
	}
	o.UnitDirectory, o.ExportDirectory, o.RuntimeDirectory = vf.root, archive, ""
	opener := func(ctx context.Context, root, stub, hash string) (*gitVMUnitOwner, error) {
		return newGitVMUnitOwner(ctx, root, stub, hash, vf.manager, vf.kernel)
	}
	return o, vf, opener
}

func TestGitReviewerStartupComposesSourceVMFromVersion2(t *testing.T) {
	o, vf, opener := gitReviewerSourceVMStartupFixture(t)
	_, expected, err := EncodeGitReviewerRelease(o.Releases[o.Current])
	if err != nil {
		t.Fatal(err)
	}
	hostOpened := false
	hostOpener := func(context.Context, string, string, string) (*gitUnitOwner, error) {
		hostOpened = true
		return nil, ErrLiveBlocked
	}
	s, err := openGitReviewerServiceUnits(context.Background(), o, hostOpener, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	factory, ok := s.inner.source.(*gitVMSourceFactory)
	if !ok || hostOpened || !factory.ready || factory.releaseHash != expected || s.runtime != nil || s.unit != nil {
		t.Fatal("version 2 did not compose exactly the source VM mechanism")
	}
	if vf.manager.starts != 0 {
		t.Fatal("startup launched a source unit")
	}
	if s.Refresh(context.Background()) != nil {
		t.Fatal("explicit refresh through the source VM fence")
	}
	if s.Close(context.Background()) != nil || !factory.closed || !factory.owner.closed {
		t.Fatal("source VM startup did not close its owners")
	}
}

func TestGitReviewerStartupSourceVMRefusesHostSelectors(t *testing.T) {
	o, _, opener := gitReviewerSourceVMStartupFixture(t)
	o.RuntimeDirectory = nativeRuntimePrivateParent(t)
	if s, err := openGitReviewerServiceUnits(context.Background(), o, nil, opener); err == nil || s != nil {
		t.Fatal("version 2 accepted a host runtime directory")
	}
	o.RuntimeDirectory = ""
	o.WorkDirectory = o.UnitDirectory
	if s, err := openGitReviewerServiceUnits(context.Background(), o, openGitUnitOwner, opener); !errors.Is(err, ErrLiveBlocked) || s != nil {
		t.Fatal("version 2 accepted overlapping directories", err)
	}
	// A version 1 current release never reaches the source VM opener.
	h, _, hostOpener, _ := gitReviewerStartupFixture(t)
	vmOpened := false
	s, err := openGitReviewerServiceUnits(context.Background(), h, hostOpener, func(context.Context, string, string, string) (*gitVMUnitOwner, error) {
		vmOpened = true
		return nil, ErrLiveBlocked
	})
	if err != nil || vmOpened {
		t.Fatal("version 1 startup touched the source VM mechanism", err)
	}
	if _, ok := s.inner.source.(*gitSourceFactory); !ok || s.Close(context.Background()) != nil {
		t.Fatal("version 1 did not keep host acquisition")
	}
}
