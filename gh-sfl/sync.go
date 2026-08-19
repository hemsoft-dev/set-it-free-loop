package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

type syncOptions struct {
	repo      string
	dryRun    bool
	pr        bool
	sourceRef string
}

func shouldPreserveSyncAudit(manifest *sflManifest, release deploymentRelease, installedTier string) bool {
	return manifest.Version == release.Version &&
		manifest.SourceSHA == release.SHA && manifest.Tier == installedTier
}

func runSync(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseSyncOptions(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpDisplayed) {
			return nil
		}
		return err
	}

	owner, repo, err := parseMutationTarget(opts.repo)
	if err != nil {
		return err
	}

	// Read remote manifest to know what's deployed
	manifest, err := readRemoteManifest(owner, repo)
	if err != nil {
		return fmt.Errorf("reading manifest from %s/%s: %w\nHint: run 'gh sfl init' first to deploy SFL", owner, repo, err)
	}

	fmt.Fprintf(stdout, "Syncing SFL in %s/%s (current: %s, tier: %s)\n\n", owner, repo, manifest.Version, manifest.Tier)

	release, err := resolveDeploymentRelease(opts.sourceRef)
	if err != nil {
		return fmt.Errorf("resolving SFL source revision: %w", err)
	}
	sourceRef := release.SHA
	latestVersion := release.Version
	latestSHA := release.SHA
	shortLatestSHA := release.SHA
	if len(shortLatestSHA) > 12 {
		shortLatestSHA = shortLatestSHA[:12]
	}

	workflows, err := workflowsForInstalledManifest(manifest)
	if err != nil {
		return fmt.Errorf("resolving installed workflows: %w", err)
	}
	if shouldPreflightReviewerSync(opts.dryRun, workflows, addonWorkflowFiles(manifest.Addons)) {
		if err := assertReviewerRolloutReady(owner, repo, stdout); err != nil {
			return err
		}
	}
	installedTier := canonicalDeploymentTier(manifest.Tier)
	if manifest.SourceSHA == latestSHA {
		if opts.dryRun {
			fmt.Fprintf(stdout, "  ✓ Source revision is current (version %s, SHA %s)\n", latestVersion, shortLatestSHA)
		} else {
			fmt.Fprintf(stdout, "  ✓ Source revision is current (version %s, SHA %s); verifying managed files for drift\n\n", latestVersion, shortLatestSHA)
		}
	} else {
		fmt.Fprintf(stdout, "  Updating: %s → %s\n", manifest.Version, release.Version)
	}
	if manifest.SourceSHA != latestSHA && manifest.SourceSHA != "" && latestSHA != "" {
		currentSHA := manifest.SourceSHA
		if len(currentSHA) > 12 {
			currentSHA = currentSHA[:12]
		}
		fmt.Fprintf(stdout, "  SHA: %s → %s\n\n", currentSHA, shortLatestSHA)
	}

	if opts.dryRun {
		fmt.Fprintf(stdout, "  (dry run — no changes made)\n")
		return nil
	}

	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return fmt.Errorf("getting default branch: %w", err)
	}

	// Collect all files to deploy
	fileMap := make(map[string]string)

	// Include add-on workflows from manifest
	addonFiles := addonWorkflowFiles(manifest.Addons)
	totalWorkflows := len(workflows) + len(addonFiles)

	fmt.Fprintf(stdout, "  Fetching %d workflow files...\n", totalWorkflows)
	for _, wf := range workflows {
		srcPath := sourceWorkflowPath(wf)
		content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, sourceRef)
		if fetchErr != nil {
			return fmt.Errorf("fetching %s: %w", srcPath, fetchErr)
		}
		if err := validateReviewerSource(wf, content, sourceRef, owner+"/"+repo); err != nil {
			return err
		}
		rendered, renderErr := renderHemSoftWorkflow(wf, content, latestVersion)
		if renderErr != nil {
			return fmt.Errorf("applying HemSoft engine policy to %s: %w", wf, renderErr)
		}
		fileMap[".github/workflows/"+wf] = rendered
		fmt.Fprintf(stdout, "    %s ✓\n", wf)
	}
	for _, wf := range addonFiles {
		srcPath := sourceWorkflowPath(wf)
		content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, sourceRef)
		if fetchErr != nil {
			return fmt.Errorf("fetching add-on %s: %w", srcPath, fetchErr)
		}
		if err := validateReviewerSource(wf, content, sourceRef, owner+"/"+repo); err != nil {
			return err
		}
		rendered, renderErr := renderHemSoftWorkflow(wf, content, latestVersion)
		if renderErr != nil {
			return fmt.Errorf("applying HemSoft engine policy to add-on %s: %w", wf, renderErr)
		}
		fileMap[".github/workflows/"+wf] = rendered
		fmt.Fprintf(stdout, "    %s ✓ (add-on)\n", wf)
	}

	if installedTier != "reviewer" {
		fmt.Fprintf(stdout, "\n  Fetching governance files...\n")
		for _, gf := range governanceFiles {
			content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, gf, sourceRef)
			if fetchErr != nil {
				return fmt.Errorf("fetching %s: %w", gf, fetchErr)
			}
			dstPath := ".sfl/" + gf[len("deployment/"):]
			fileMap[dstPath] = content
			fmt.Fprintf(stdout, "    %s ✓\n", dstPath)
		}
	}

	// Preserve audit fields for a true no-op sync. Direct deployments compare
	// raw bytes, while PR deployments intentionally ignore these audit fields.
	deploymentMetadataCurrent := shouldPreserveSyncAudit(manifest, release, installedTier)
	deployedAt := time.Now().UTC()
	deployedBy := getCurrentUser()
	if deploymentMetadataCurrent {
		deployedAt = manifest.DeployedAt
		deployedBy = manifest.DeployedBy
	}

	// Update manifest
	manifest.Version = release.Version
	manifest.SourceSHA = latestSHA
	normalizeManifestForSync(manifest, installedTier)
	manifest.DeployedAt = deployedAt
	manifest.DeployedBy = deployedBy
	manifest.EnginePolicy = hemSoftEnginePolicyManifestForFileMap(fileMap)
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	fileMap[".sfl/sfl.json"] = manifestJSON
	fmt.Fprintf(stdout, "    .sfl/sfl.json ✓\n")

	// Deploy all files in a single atomic commit.
	shortSHA := latestSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}
	commitMsg := fmt.Sprintf("chore: sync SFL %s (%s tier)\n\nSynced by gh-sfl from %s/%s@%s", latestVersion, manifest.Tier, motherRepoOwner, motherRepoName, shortSHA)

	fmt.Fprintf(stdout, "\n")
	var prURL string
	if opts.pr {
		prURL, err = deployViaPullRequest(owner, repo, defaultBranch, "sync", fileMap, commitMsg, true, stdout)
	} else {
		err = deployViaGit(owner, repo, defaultBranch, fileMap, commitMsg, true, stdout)
	}
	if err != nil {
		return fmt.Errorf("deploying files: %w", err)
	}

	// Sync labels
	if installedTier != "reviewer" {
		fmt.Fprintf(stdout, "\n  Syncing labels...\n")
		labelsJSON, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, "deployment/governance/labels.json", sourceRef)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "  Warning: label sync skipped; could not fetch labels: %v\n", fetchErr)
		} else {
			var labels []labelDef
			if jsonErr := json.Unmarshal([]byte(labelsJSON), &labels); jsonErr != nil {
				fmt.Fprintf(stderr, "  Warning: label sync skipped; labels are invalid JSON: %v\n", jsonErr)
			} else {
				created, updated, labelErr := ensureLabels(owner, repo, labels)
				if labelErr != nil {
					fmt.Fprintf(stderr, "  Warning: label sync error: %v\n", labelErr)
				} else {
					fmt.Fprintf(stdout, "    %d created, %d updated\n", created, updated)
				}
			}
		}
	}

	if prURL != "" {
		fmt.Fprintf(stdout, "\n✅ SFL %s sync pull request ready for %s/%s: %s\n", latestVersion, owner, repo, prURL)
		return nil
	}

	fmt.Fprintf(stdout, "\n✅ SFL synced to %s in %s/%s\n", latestVersion, owner, repo)

	// Merge main into open sfl-pr branches so they pick up the new reactor
	fmt.Fprintf(stdout, "\n  Updating open PR branches...\n")
	updateAgentPRBranches(owner, repo, stdout)

	return nil
}

func normalizeManifestForSync(manifest *sflManifest, installedTier string) {
	components := manifest.Components[:0]
	for _, component := range manifest.Components {
		if component != "sfl-pr-review-recovery" {
			components = append(components, component)
		}
	}
	manifest.Components = components

	if installedTier != "reviewer" {
		return
	}

	manifest.Tier = "reviewer"
	manifest.Components = append([]string(nil), tierComponents["reviewer"]...)
}

func shouldPreflightReviewerSync(dryRun bool, workflowSets ...[]string) bool {
	return !dryRun && workflowsIncludeReviewer(workflowSets...)
}

func parseSyncOptions(args []string, stderr io.Writer) (syncOptions, error) {
	opts := syncOptions{pr: true}
	var direct bool

	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeSyncUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "Show what would change without making changes")
	flags.BoolVar(&opts.dryRun, "n", false, "Show what would change without making changes")
	flags.BoolVar(&opts.pr, "pr", true, "Create a pull request (default)")
	flags.BoolVar(&direct, "direct", false, "Push directly to the default branch instead of opening a pull request")
	flags.StringVar(&opts.sourceRef, "source-ref", "", "Synchronized SFL release tag to deploy (defaults to latest)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	if flags.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), ", "))
	}
	if direct {
		opts.pr = false
	}

	return opts, nil
}

func writeSyncUsage(w io.Writer) {
	fmt.Fprint(w, syncUsage)
}

const syncUsage = `Update an existing SFL deployment from a synchronized release.

Compares the installed SFL version against the latest synchronized release and
updates the files tracked by its manifest while preserving the installed tier.

Usage:
  gh sfl sync [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
  -n, --dry-run        Show what would change without making changes
      --pr             Create a signed commit in a pull request (default)
      --direct         Push directly to the default branch instead of opening a pull request
      --source-ref     Immutable SFL source ref to deploy (for example, v6.5.0)

Examples:
  gh sfl sync                          # Open a sync PR for the current repo
  gh sfl sync --repo owner/repo        # Open a sync PR for a different repo
  gh sfl sync --repo owner/repo --direct # Push directly (explicit opt-in)
  gh sfl sync --dry-run                # Preview changes without applying
`
