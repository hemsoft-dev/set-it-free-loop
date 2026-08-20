package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

type addOptions struct {
	repo  string
	addon string
	pr    bool
}

func runAdd(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseAddOptions(args, stderr)
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
	if !validAddon(opts.addon) {
		return fmt.Errorf("unknown addon %q\nAvailable addons: %s", opts.addon, strings.Join(knownAddonNames(), ", "))
	}

	// Read existing manifest
	manifest, err := readRemoteManifest(owner, repo)
	if err != nil {
		return fmt.Errorf("reading manifest from %s/%s: %w\nHint: run 'gh sfl init' first", owner, repo, err)
	}
	if !fullCommitSHAPattern.MatchString(manifest.SourceSHA) {
		return fmt.Errorf("manifest in %s/%s does not contain an immutable 40-character sourceSha; run 'gh sfl sync --repo %s/%s' before adding workflows", owner, repo, owner, repo)
	}

	// Check if addon is already installed
	for _, a := range manifest.Addons {
		if a == opts.addon {
			fmt.Fprintf(stdout, "  ✓ Add-on %q is already installed in %s/%s\n", opts.addon, owner, repo)
			return nil
		}
	}

	// Check if addon workflows are already present via tier.
	// Require ALL addon workflow files to be in the tier before short-circuiting,
	// since partial overlap means the addon isn't fully covered by the tier.
	tierWFs, err := workflowsForInstalledManifest(manifest)
	if err != nil {
		return fmt.Errorf("resolving installed workflows: %w", err)
	}
	addonWFs := addonWorkflows[opts.addon]
	if len(addonWFs) > 0 {
		allIncluded := true
		for _, awf := range addonWFs {
			found := false
			for _, twf := range tierWFs {
				if twf == awf {
					found = true
					break
				}
			}
			if !found {
				allIncluded = false
				break
			}
		}
		if allIncluded {
			fmt.Fprintf(stdout, "  ✓ Add-on %q is already included in the %q tier for %s/%s\n", opts.addon, manifest.Tier, owner, repo)
			fmt.Fprintf(stdout, "    Hint: run 'gh sfl sync --repo %s/%s' to ensure tier workflows are up to date\n", owner, repo)
			return nil
		}
	}

	fmt.Fprintf(stdout, "Adding %q add-on to %s/%s\n\n", opts.addon, owner, repo)

	// Fetch addon workflow files
	workflows := addonWorkflows[opts.addon]
	if workflowsIncludeReviewer(workflows) {
		if err := assertReviewerRolloutReady(owner, repo, stdout); err != nil {
			return err
		}
	}
	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return fmt.Errorf("getting default branch: %w", err)
	}
	fileMap := make(map[string]string)

	fmt.Fprintf(stdout, "  Fetching %d workflow file(s)...\n", len(workflows))
	for _, wf := range workflows {
		srcPath := sourceWorkflowPath(wf)
		content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, manifest.SourceSHA)
		if fetchErr != nil {
			return fmt.Errorf("fetching %s: %w", srcPath, fetchErr)
		}
		content, prepareErr := prepareWorkflowSource(wf, content, manifest.SourceSHA, owner+"/"+repo, defaultBranch)
		if prepareErr != nil {
			return prepareErr
		}
		rendered, renderErr := renderHemSoftWorkflow(wf, content, manifest.Version)
		if renderErr != nil {
			return fmt.Errorf("applying HemSoft engine policy to %s: %w", wf, renderErr)
		}
		fileMap[".github/workflows/"+wf] = rendered
		fmt.Fprintf(stdout, "    %s ✓\n", wf)
	}

	// Update manifest — only add the addon, don't touch SourceSHA/Version
	manifest.Addons = append(manifest.Addons, opts.addon)
	manifest.DeployedAt = time.Now().UTC()
	manifest.DeployedBy = getCurrentUser()
	manifest.EnginePolicy = mergeHemSoftEnginePolicyManifest(
		manifest.EnginePolicy,
		hemSoftEnginePolicyManifestForFileMap(fileMap),
	)
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	fileMap[".sfl/sfl.json"] = manifestJSON

	// Deploy
	commitMsg := fmt.Sprintf("chore: add SFL add-on %q\n\nDeployed by gh-sfl add", opts.addon)
	fmt.Fprintf(stdout, "\n")
	var prURL string
	if opts.pr {
		prURL, err = deployViaPullRequest(owner, repo, defaultBranch, "add", fileMap, commitMsg, false, stdout)
	} else {
		err = deployViaGit(owner, repo, defaultBranch, fileMap, commitMsg, false, stdout)
	}
	if err != nil {
		return fmt.Errorf("deploying add-on: %w", err)
	}
	if prURL != "" {
		fmt.Fprintf(stdout, "\n✅ Add-on %q pull request ready for %s/%s: %s\n", opts.addon, owner, repo, prURL)
		return nil
	}

	fmt.Fprintf(stdout, "\n✅ Add-on %q deployed to %s/%s\n", opts.addon, owner, repo)

	// Only Markdown-source workflows need gh-aw compilation. Standard Actions
	// YAML files are already executable and do not have lock-file companions.
	if addonNeedsCompilation(workflows) {
		fmt.Fprintf(stdout, "\n  ⚠ Note: This workflow is deployed as source (.md) only.\n")
		fmt.Fprintf(stdout, "  Run 'gh aw compile' in the target repo to generate the lock file.\n")
	}

	return nil
}

func addonNeedsCompilation(workflows []string) bool {
	hasMarkdownSource := false
	hasLock := false
	for _, workflow := range workflows {
		hasMarkdownSource = hasMarkdownSource || strings.HasSuffix(workflow, ".md")
		hasLock = hasLock || strings.HasSuffix(workflow, ".lock.yml")
	}
	return hasMarkdownSource && !hasLock
}

func parseAddOptions(args []string, stderr io.Writer) (addOptions, error) {
	opts := addOptions{pr: true}
	var direct bool

	flags := flag.NewFlagSet("add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeAddUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.BoolVar(&opts.pr, "pr", true, "Create a pull request (default)")
	flags.BoolVar(&direct, "direct", false, "Push directly to the default branch instead of opening a pull request")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	// The addon name is a positional argument
	if flags.NArg() == 0 {
		writeAddUsage(stderr)
		return opts, fmt.Errorf("addon name required")
	}
	if flags.NArg() > 1 {
		return opts, fmt.Errorf("only one addon can be added at a time; got: %s", strings.Join(flags.Args(), ", "))
	}

	opts.addon = flags.Arg(0)

	// Strip leading -- if user passes --policy-manager style
	opts.addon = strings.TrimPrefix(opts.addon, "--")
	if direct {
		opts.pr = false
	}

	return opts, nil
}

func writeAddUsage(w io.Writer) {
	fmt.Fprint(w, addUsage)
}

const addUsage = `Add an optional SFL add-on workflow to a repository.

Add-ons are specialized workflows that can be explicitly installed and
maintained by 'gh sfl sync'. Some add-ons are also included in higher
tiers (e.g., pr-review is part of the full tier); the command will
skip installation if the workflow is already present from the tier.

Usage:
  gh sfl add <addon-name> [flags]

Available Add-ons:
  pr-review    Subscription-backed Codex review with an immutable-head gate

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
      --pr             Create a pull request (default)
      --direct         Push directly to the default branch instead

Examples:
  gh sfl add pr-review
  gh sfl add pr-review --repo owner/repo
`
