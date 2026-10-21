//go:build linux

package reviewer

import "context"

// A bounded explicit catalog keeps older qualified releases available for their
// immutable durable jobs. No default release, provider, account or transport is
// selected. The native reviewer still verifies assets/containment before auth.
func newGitServiceNativeRuntime(catalog map[string]map[Provider]NativeVMOptions, gate func(context.Context, string) error) (func(context.Context, Job, *JobEvidence) (Engine, error), error) {
	if gate == nil || len(catalog) == 0 || len(catalog) > 8 {
		return nil, ErrLiveBlocked
	}
	releases := map[string]map[Provider]NativeVMOptions{}
	for release, providers := range catalog {
		if !objectID.MatchString(release) || len(providers) == 0 || len(providers) > 2 {
			return nil, ErrLiveBlocked
		}
		releases[release] = map[Provider]NativeVMOptions{}
		for provider, options := range providers {
			if !provider.Valid() || options.Provider != provider || options.Reader != nil || nilGitHubCredentialTransport(options.Transport) {
				return nil, ErrLiveBlocked
			}
			// Reuse the native module's structural checks without a filesystem,
			// account or credential read. The real reader comes only from a job.
			probe := options
			probe.Reader = &Reader{}
			if _, err := NewNativeVMReviewer(probe); err != nil {
				return nil, err
			}
			options.Policy.Routes = append([]EgressRoute(nil), options.Policy.Routes...)
			options.Policy.Headers = append([]string(nil), options.Policy.Headers...)
			options.Runtime.BootstrapLibraries = append([]VMBootAsset(nil), options.Runtime.BootstrapLibraries...)
			releases[release][provider] = options
		}
	}
	return func(ctx context.Context, job Job, bound *JobEvidence) (Engine, error) {
		if ctx == nil || ctx.Err() != nil || bound.validateJob(job) != nil {
			return Engine{}, ErrLiveBlocked
		}
		providers, ok := releases[job.Release]
		if !ok || gate(ctx, job.Release) != nil || ctx.Err() != nil {
			return Engine{}, ErrLiveBlocked
		}
		engine := Engine{Reviewers: map[Provider]Reviewer{}, Reader: bound.Reader}
		for _, provider := range job.Config.Providers() {
			options, ok := providers[provider]
			if !ok {
				return Engine{}, ErrLiveBlocked
			}
			reader, err := bound.Reader(job.Snapshot)
			if err != nil {
				return Engine{}, err
			}
			options.Reader = reader
			identity := job.Config.Codex
			if provider == Claude {
				identity = job.Config.Claude
			}
			providerCheck := options.Check
			options.Check = func(ctx context.Context, session Session) error {
				if ctx == nil || ctx.Err() != nil || session.Validate() != nil || session.JobID != job.ID || session.Snapshot != job.Snapshot || session.Packet != job.Packet || session.ConfigHash != job.Config.Hash() || session.Identity != identity || session.Round > job.Config.Rounds() {
					return ErrLiveBlocked
				}
				if gate(ctx, job.Release) != nil || ctx.Err() != nil || providerCheck(ctx, session) != nil || ctx.Err() != nil {
					return ErrLiveBlocked
				}
				return nil
			}
			worker, err := NewNativeVMReviewer(options)
			if err != nil {
				return Engine{}, err
			}
			engine.Reviewers[provider] = worker
		}
		return engine, nil
	}, nil
}
