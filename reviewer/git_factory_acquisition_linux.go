//go:build linux

package reviewer

import (
	"context"
	"errors"
	"time"
)

// The only production implementation is gitFactoryRun. Unexported methods
// prevent an external caller from substituting an admission callback. Tests
// replace the lifecycle behind this private seam with explicitly fake units.
type gitFactoryAcquisition interface {
	retainGeneration(*githubReaderGeneration)
	acquire(context.Context, *gitGenerationReader) (*gitMaterialization, error)
	stop(context.Context) error
	close() error
}

// The outer factory retains run in its attempt registry, including on error.
// An unknown stop keeps that run and the credential generation owned. A staged
// export never escapes while either the broker or acquisition unit is live.
func runGitFactoryAcquisition(ctx context.Context, credentials *GitHubCredentialOwner, binding gitMaterializationBinding, run gitFactoryAcquisition) (_ *gitMaterialization, result error) {
	if ctx == nil || credentials == nil || !binding.valid() || run == nil {
		return nil, errGitUnit
	}
	cleanup := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	}
	generation, err := credentials.retainReaderGeneration(ctx, run.stop)
	if err != nil {
		phase, cancel := cleanup()
		defer cancel()
		if run.stop(phase) == nil {
			_ = run.close()
		}
		return nil, errGitUnit
	}
	run.retainGeneration(generation)
	materialized, acquireErr := run.acquire(generation.ctx, &gitGenerationReader{generation: generation})
	good := false
	defer func() {
		if !good && materialized != nil {
			materialized.Close()
		}
	}()
	phase, cancel := cleanup()
	defer cancel()
	if generation.stop(phase) != nil {
		return nil, errGitUnit
	}
	if run.close() != nil {
		return nil, errGitUnit
	}
	if acquireErr != nil || materialized == nil || ctx.Err() != nil {
		return nil, errors.Join(errGitUnit, ctx.Err())
	}
	materialized.mu.Lock()
	valid := !materialized.closed && materialized.manifest.Binding == binding
	materialized.mu.Unlock()
	if !valid {
		return nil, errGitUnit
	}
	good = true
	return materialized, nil
}
