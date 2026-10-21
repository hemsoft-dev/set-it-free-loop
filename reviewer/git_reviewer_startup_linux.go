//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Host capabilities are supplied separately from immutable release packages.
// The caller retains auth stores and transports. No default account is chosen.
type GitReviewerProviderHost struct {
	Root         string
	Auth         *AuthStore
	Transport    http.RoundTripper
	ProfileHints NativeVMProfileHints
	Check        func(context.Context, Session) error
}

// GitReviewerServiceOptions belongs only to the trusted operator layer. All
// directories must already exist, be private, canonical and disjoint. Check
// must qualify the exact revision AND package digest with local, read-only
// evidence; it supplies no authority when the owner has not granted activation.
type GitReviewerServiceOptions struct {
	Credentials                       GitHubCredentialOwnerOptions
	Current                           string
	Releases                          map[string]GitReviewerRelease
	Providers                         map[string]map[Provider]GitReviewerProviderHost
	UnitDirectory, ExportDirectory    string
	RuntimeDirectory, WorkDirectory   string
	Triggers                          TriggerPolicy
	Check                             func(context.Context, Enrollment, string, string) error
	Interval, MaxBackoff, TickTimeout time.Duration
}

func (GitReviewerProviderHost) String() string   { return "GitReviewerProviderHost{redacted}" }
func (GitReviewerProviderHost) GoString() string { return "GitReviewerProviderHost{redacted}" }
func (GitReviewerProviderHost) MarshalJSON() ([]byte, error) {
	return []byte(`"GitReviewerProviderHost{redacted}"`), nil
}
func (GitReviewerServiceOptions) String() string   { return "GitReviewerServiceOptions{redacted}" }
func (GitReviewerServiceOptions) GoString() string { return "GitReviewerServiceOptions{redacted}" }
func (GitReviewerServiceOptions) MarshalJSON() ([]byte, error) {
	return []byte(`"GitReviewerServiceOptions{redacted}"`), nil
}

// GitReviewerService owns one explicit startup. Construction never refreshes or
// runs. A nonnil handle returned with an error must be retained and closed.
type GitReviewerService struct {
	inner     *gitReviewerService
	unit      *gitUnitOwner
	archive   *gitExportArchive
	runtime   *gitRuntimeRoot
	helper    *os.File
	lock      *authLock
	parent    *os.Root
	container string
	identity  os.FileInfo
	operation chan struct{}
	closed    bool
}

func (*GitReviewerService) String() string   { return "GitReviewerService{redacted}" }
func (*GitReviewerService) GoString() string { return "GitReviewerService{redacted}" }
func (*GitReviewerService) MarshalJSON() ([]byte, error) {
	return []byte(`"GitReviewerService{redacted}"`), nil
}

func OpenGitReviewerService(ctx context.Context, o GitReviewerServiceOptions) (*GitReviewerService, error) {
	return openGitReviewerService(ctx, o, openGitUnitOwner)
}

// The unit opener is private and used only for fake manager/kernel tests.
func openGitReviewerService(ctx context.Context, o GitReviewerServiceOptions, openUnit func(context.Context, string, string, string) (*gitUnitOwner, error)) (_ *GitReviewerService, result error) {
	if ctx == nil || ctx.Err() != nil || !o.Credentials.Journal.valid() || o.Credentials.JWT == nil || o.Credentials.Check == nil || nilGitHubCredentialTransport(o.Credentials.ReaderTransport) || nilGitHubCredentialTransport(o.Credentials.PublisherTransport) || o.Check == nil || openUnit == nil || o.Triggers.validate() != nil || !objectID.MatchString(o.Current) || len(o.Releases) < 1 || len(o.Releases) > 8 || len(o.Providers) != len(o.Releases) || o.Interval < time.Second || o.Interval > 5*time.Minute || o.MaxBackoff < o.Interval || o.MaxBackoff > time.Hour || o.TickTimeout < time.Second || o.TickTimeout > 5*time.Minute {
		return nil, ErrLiveBlocked
	}
	phase, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	releases := map[string]GitReviewerRelease{}
	hashes := map[string]string{}
	native := map[string]map[Provider]NativeVMOptions{}
	for revision, release := range o.Releases {
		raw, hash, err := EncodeGitReviewerRelease(release)
		if err != nil || revision != release.Revision || len(o.Providers[revision]) != len(release.Providers) {
			return nil, ErrLiveBlocked
		}
		frozen, _, err := DecodeGitReviewerRelease(raw)
		if err != nil {
			return nil, err
		}
		releases[revision], hashes[revision] = frozen, hash
		native[revision] = map[Provider]NativeVMOptions{}
		for provider, p := range frozen.Providers {
			host, ok := o.Providers[revision][provider]
			if !ok || !sourcePath(host.Root) || host.Auth == nil || host.Auth.provider != provider || nilGitHubCredentialTransport(host.Transport) || host.Check == nil || !host.ProfileHints.valid(provider) {
				return nil, ErrLiveBlocked
			}
			check, identity, stub := host.Check, p.Identity, p.Stub
			native[revision][provider] = NativeVMOptions{Provider: provider, Root: host.Root, Stub: p.Stub.Path, StubHash: p.Stub.SHA256, QEMU: p.QEMU, Kernel: p.Kernel, Initrd: p.Initrd, Firmware: p.Firmware, Runtime: p.Runtime, Auth: host.Auth, Policy: p.Policy, Transport: host.Transport, ProfileHints: host.ProfileHints, Check: func(ctx context.Context, session Session) error {
				if ctx == nil || ctx.Err() != nil || session.Identity != identity || check(ctx, session) != nil || ctx.Err() != nil {
					return ErrLiveBlocked
				}
				// The native supervisor verifies the stub's content hash; the
				// package additionally fixes its exact byte count before auth.
				image, err := sealVMAsset(ctx, stub, false)
				if err != nil {
					return ErrLiveBlocked
				}
				return image.Close()
			}}
		}
	}
	current, ok := releases[o.Current]
	// Every configured alpha mode must be available for newly admitted PRs.
	// Older retained releases may still serve explicit single-provider jobs.
	if !ok || len(current.Providers) != 2 {
		return nil, ErrLiveBlocked
	}
	e := o.Credentials.Journal.enrollment
	gate := func(ctx context.Context, enrollment Enrollment, revision string) error {
		if ctx == nil || ctx.Err() != nil || enrollment != e || hashes[revision] == "" || o.Check(ctx, e, revision, hashes[revision]) != nil || ctx.Err() != nil {
			return ErrLiveBlocked
		}
		return nil
	}
	// A denied current release performs no filesystem, manager or HTTP work.
	if o.Credentials.Check(phase, e) != nil || gate(phase, e, o.Current) != nil || phase.Err() != nil {
		return nil, ErrLiveBlocked
	}
	if !gitReviewerDirectories(o.UnitDirectory, o.ExportDirectory, o.RuntimeDirectory, o.WorkDirectory) || !gitFactoryRunPaths(filepath.Join(o.WorkDirectory, "run-"+strings.Repeat("a", 32))) {
		return nil, ErrLiveBlocked
	}
	s := &GitReviewerService{operation: make(chan struct{}, 1)}
	s.operation <- struct{}{}
	// Every partial resource stays owned if bounded disposal cannot finish.
	defer func() {
		if result != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(phase), 5*time.Second)
			defer stop()
			if err := s.Close(cleanup); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	var err error
	s.lock, err = (&AuthStore{directory: o.Credentials.Journal.directory}).lock(phase)
	if err != nil {
		return s, ErrLiveBlocked
	}
	policy := o.Triggers
	policy.EnabledAt = policy.EnabledAt.UTC()
	if err := gitReviewerPersistPolicy(phase, o.Credentials.Journal.directory, e, policy); err != nil {
		return s, err
	}
	artifact, err := sealVMAsset(phase, current.GitArtifact, false)
	if err != nil {
		return s, err
	}
	defer artifact.Close()
	s.helper, err = retainNativeBootstrap(phase, current.NamespaceHelper)
	if err != nil {
		return s, err
	}
	s.parent, err = os.OpenRoot(o.RuntimeDirectory)
	if err != nil {
		return s, ErrLiveBlocked
	}
	if err = s.prepareRuntime(phase, o.RuntimeDirectory, artifact, current.GitRuntime); err != nil {
		return s, err
	}
	s.archive, err = openGitExportArchive(phase, o.ExportDirectory)
	if err != nil {
		return s, err
	}
	s.unit, err = openUnit(phase, o.UnitDirectory, current.Worker.Path, current.Worker.SHA256)
	if err != nil {
		return s, err
	}
	info, err := s.unit.image.file.Stat()
	if err != nil || info.Size() != current.Worker.Bytes {
		return s, ErrLiveBlocked
	}
	filter, err := gitFixedFilterPin()
	if err != nil {
		return s, err
	}
	manifest := gitGrantManifest{2, "isolated-git-acquisition", current.GitRuntime, VMAssetPin{current.NamespaceHelper.SHA256, current.NamespaceHelper.Bytes}, VMAssetPin{current.Worker.SHA256, current.Worker.Bytes}, filter}
	s.inner, err = newGitReviewerService(phase, gitReviewerServiceOptions{Credentials: o.Credentials, Unit: s.unit, Archive: s.archive, RuntimeRoot: s.runtime, Helper: s.helper, Manifest: manifest, Triggers: policy, Release: o.Current, ReleaseHash: hashes[o.Current], Directory: o.WorkDirectory, NativeReleases: native, Check: gate, Interval: o.Interval, MaxBackoff: o.MaxBackoff, TickTimeout: o.TickTimeout})
	if err != nil {
		return s, err
	}
	return s, nil
}

func gitReviewerDirectories(paths ...string) bool {
	identities := []os.FileInfo{}
	for i, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if !sourcePath(path) || err != nil || resolved != path || privateDirectory(path) != nil {
			return false
		}
		info, err := os.Lstat(path)
		if err != nil {
			return false
		}
		for j, previous := range identities {
			if os.SameFile(info, previous) || strings.HasPrefix(path+"/", paths[j]+"/") || strings.HasPrefix(paths[j]+"/", paths[i]+"/") {
				return false
			}
		}
		identities = append(identities, info)
	}
	return true
}

func (s *GitReviewerService) prepareRuntime(ctx context.Context, parent string, artifact *os.File, pins GitRuntimePins) error {
	directory, err := s.parent.Open(".")
	if err != nil {
		return ErrLiveBlocked
	}
	entries, err := directory.ReadDir(129)
	directory.Close()
	if err != nil && err != io.EOF || len(entries) >= 128 {
		return ErrLiveBlocked
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrLiveBlocked
	}
	name := "runtime-" + hex.EncodeToString(nonce[:])
	if s.parent.Mkdir(name, 0700) != nil {
		return ErrLiveBlocked
	}
	s.container = name
	s.identity, err = s.parent.Lstat(name)
	if err != nil {
		return ErrLiveBlocked
	}
	_, err = prepareGitRuntime(ctx, filepath.Join(parent, name), io.NewSectionReader(artifact, 0, pins.Artifact.Bytes), pins, func(prepared *gitRuntimeRoot) error {
		// Retain the original owner even if partial preparation cannot dispose
		// itself. An error must not replace that handle with a nil return value.
		s.runtime = prepared
		return nil
	})
	if err == nil {
		opened, statErr := s.runtime.parent.Stat(".")
		if statErr != nil || !os.SameFile(opened, s.identity) {
			return ErrLiveBlocked
		}
	}
	return err
}

func gitReviewerPersistPolicy(ctx context.Context, directory string, e Enrollment, p TriggerPolicy) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return ErrLiveBlocked
	}
	defer root.Close()
	value := struct {
		Version    int
		Enrollment Enrollment
		Triggers   TriggerPolicy
	}{1, e, p}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 4096 {
		return ErrLiveBlocked
	}
	const name = "reviewer-trigger-policy.json"
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == nil {
		defer f.Close()
		if privateAuthFile(f) != nil {
			return ErrLiveBlocked
		}
		got, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || !bytes.Equal(got, raw) || ctx.Err() != nil {
			return ErrLiveBlocked
		}
		return nil
	}
	if !os.IsNotExist(err) || ctx.Err() != nil {
		return ErrLiveBlocked
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrLiveBlocked
	}
	// The caller holds the startup flock. Use its existing exact, validated
	// replacement namespace so the next lock acquisition removes a crash fragment.
	tmp := ".replace-" + hex.EncodeToString(nonce[:])
	f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrLiveBlocked
	}
	defer root.Remove(tmp)
	_, writeErr := f.Write(raw)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || ctx.Err() != nil || root.Rename(tmp, name) != nil {
		return ErrLiveBlocked
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrLiveBlocked
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return ErrLiveBlocked
	}
	return nil
}

func (s *GitReviewerService) Refresh(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return ErrLiveBlocked
	}
	return s.inner.Refresh(ctx)
}
func (s *GitReviewerService) Run(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return ErrLiveBlocked
	}
	return s.inner.Run(ctx)
}
func (s *GitReviewerService) Recover(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return ErrLiveBlocked
	}
	return s.inner.Recover(ctx)
}

func (s *GitReviewerService) Close(ctx context.Context) error {
	if s == nil || ctx == nil || s.operation == nil {
		return ErrWorkerStopPending
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.inner != nil {
		if err := s.inner.Close(ctx); err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return errors.Join(ErrWorkerStopPending, ctx.Err())
	case <-s.operation:
	}
	defer func() { s.operation <- struct{}{} }()
	if s.closed {
		return nil
	}
	if s.unit != nil && !s.unit.closed {
		if !s.unit.enter(ctx) {
			return ErrWorkerStopPending
		}
		records, err := s.unit.records()
		s.unit.leave()
		if err != nil {
			return err
		}
		for _, r := range records {
			if err := s.unit.confirmFactoryStop(ctx, r.Binding); err != nil {
				return err
			}
		}
		if err := s.unit.Close(); err != nil {
			return err
		}
	}
	if s.archive != nil {
		if err := s.archive.Close(ctx); err != nil {
			return err
		}
	}
	if s.runtime != nil {
		if err := s.runtime.Close(); err != nil {
			return err
		}
	}
	if s.helper != nil {
		err := s.helper.Close()
		s.helper = nil
		if err != nil {
			return err
		}
	}
	if s.parent != nil {
		if s.container != "" {
			info, err := s.parent.Lstat(s.container)
			if err != nil || s.identity == nil || !os.SameFile(info, s.identity) || s.parent.Remove(s.container) != nil {
				return ErrWorkerStopPending
			}
			s.container = ""
		}
		if err := s.parent.Close(); err != nil {
			return err
		}
		s.parent = nil
	}
	if s.lock != nil {
		if err := s.lock.close(); err != nil {
			return err
		}
		s.lock = nil
	}
	s.closed = true
	return nil
}

// Git runtime owners retain their original descriptors after a failed disposal.
// The caller must establish physical stop before invoking Close, as with the
// existing runtime contract. A retry checks the SAME directory identity; it
// neither reopens a pathname-selected replacement nor drops the retained owner.
func (r *gitRuntimeRoot) Close() error {
	if r == nil || r.nativeRuntimeRoot == nil || r.closed {
		return nil
	}
	current, err := r.parent.Lstat(r.directoryName())
	if err != nil || !os.SameFile(current, r.identity) {
		return ErrNativeRuntime
	}
	directories := make([]string, 0, len(r.directories))
	for directory := range r.directories {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		if err := r.root.Chmod(directory, 0700); err != nil && !os.IsNotExist(err) {
			return ErrNativeRuntime
		}
	}
	if r.parent.RemoveAll(r.directoryName()) != nil {
		return ErrNativeRuntime
	}
	r.closed = true
	return errors.Join(r.root.Close(), r.parent.Close())
}
