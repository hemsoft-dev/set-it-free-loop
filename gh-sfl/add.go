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
	fileMap := make(map[string]string)

	fmt.Fprintf(stdout, "  Fetching %d workflow file(s)...\n", len(workflows))
	for _, wf := range workflows {
		srcPath := sourceWorkflowPath(wf)
		content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, "")
		if fetchErr != nil {
			return fmt.Errorf("fetching %s: %w", srcPath, fetchErr)
		}
		fileMap[".github/workflows/"+wf] = content
		fmt.Fprintf(stdout, "    %s ✓\n", wf)
	}

	// Update manifest — only add the addon, don't touch SourceSHA/Version
	manifest.Addons = append(manifest.Addons, opts.addon)
	manifest.DeployedAt = time.Now().UTC()
	manifest.DeployedBy = getCurrentUser()
	manifest.EnginePolicy = hemSoftEnginePolicyManifestForFileMap(fileMap)
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	fileMap[".sfl/sfl.json"] = manifestJSON

	// Deploy
	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return fmt.Errorf("getting default branch: %w", err)
	}

	commitMsg := fmt.Sprintf("chore: add SFL add-on %q\n\nDeployed by gh-sfl add", opts.addon)
	fmt.Fprintf(stdout, "\n")
	if err := deployViaGit(owner, repo, defaultBranch, fileMap, commitMsg, false, stdout); err != nil {
		return fmt.Errorf("deploying add-on: %w", err)
	}

	fmt.Fprintf(stdout, "\n✅ Add-on %q deployed to %s/%s\n", opts.addon, owner, repo)

	// Only suggest compilation if the addon doesn't ship a precompiled lock file
	hasLock := false
	for _, wf := range workflows {
		if strings.HasSuffix(wf, ".lock.yml") {
			hasLock = true
			break
		}
	}
	if !hasLock {
		fmt.Fprintf(stdout, "\n  ⚠ Note: This workflow is deployed as source (.md) only.\n")
		fmt.Fprintf(stdout, "  Run 'gh aw compile' in the target repo to generate the lock file.\n")
	}

	return nil
}

func parseAddOptions(args []string, stderr io.Writer) (addOptions, error) {
	var opts addOptions

	flags := flag.NewFlagSet("add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeAddUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")

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
  pr-review    Evidence-based PR review with recovery and a zero-finding approval gate

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.

Examples:
  gh sfl add pr-review
  gh sfl add pr-review --repo owner/repo
`
