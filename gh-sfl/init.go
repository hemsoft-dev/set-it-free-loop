package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	gh "github.com/cli/go-gh/v2"
)

type initOptions struct {
	repo         string
	tier         string
	tierExplicit bool
	addons       []string
	slackWebhook string
	pr           bool
	direct       bool
	sourceRef    string
}

// stringSliceFlag implements flag.Value for repeatable string flags.
type stringSliceFlag struct {
	values *[]string
}

func (f *stringSliceFlag) String() string {
	if f.values == nil {
		return ""
	}
	return strings.Join(*f.values, ", ")
}

func (f *stringSliceFlag) Set(val string) error {
	// Strip leading -- if user passes --policy-manager style value
	val = strings.TrimPrefix(val, "--")
	*f.values = append(*f.values, val)
	return nil
}

// Tier definitions: which workflow files to deploy per tier.
var tierWorkflows = map[string][]string{
	"reviewer": {
		"sfl-pr-review.md",
		"sfl-pr-review.lock.yml",
		"sfl-pr-review-auto.yml",
		"sfl-pr-review-recovery.yml",
	},
	"minimal": {
		"sfl-dispatcher.yml",
	},
	"standard": {
		"sfl-dispatcher.yml",
		"sfl-auditor.yml",
		"daily-repo-status.md",
		"repo-audit.md",
		"issue-processor.md",
		"simplisticate.md",
	},
	"full": {
		"sfl-dispatcher.yml",
		"sfl-auditor.yml",
		"daily-repo-status.md",
		"repo-audit.md",
		"issue-processor.md",
		"simplisticate.md",
		"pr-analyzer-general.md",
		"pr-analyzer-quality.md",
		"pr-analyzer-security.md",
		"pr-analyzer-testing.md",
		"pr-fixer.md",
		"pr-promoter.md",
		"sfl-pr-review.md",
		"sfl-pr-review.lock.yml",
		"sfl-pr-review-auto.yml",
		"sfl-pr-review-recovery.yml",
	},
}
var tierComponents = map[string][]string{
	"reviewer": {"sfl-pr-review", "sfl-pr-review-auto", "sfl-pr-review-recovery"},
	"minimal":  {"labels", "governance", "sfl-dispatcher"},
	"standard": {"labels", "governance", "sfl-dispatcher", "sfl-auditor", "daily-repo-status", "repo-audit", "issue-processor", "simplisticate"},
	"full":     {"labels", "governance", "sfl-dispatcher", "sfl-auditor", "daily-repo-status", "repo-audit", "issue-processor", "simplisticate", "pr-analyzer-general", "pr-analyzer-quality", "pr-analyzer-security", "pr-analyzer-testing", "pr-fixer", "pr-promoter", "sfl-pr-review", "sfl-pr-review-auto", "sfl-pr-review-recovery"},
}

func canonicalDeploymentTier(tier string) string {
	if tier == "review" {
		return "reviewer"
	}
	return tier
}

func workflowsForInstalledManifest(manifest *sflManifest) ([]string, error) {
	tier := canonicalDeploymentTier(manifest.Tier)
	if workflows, ok := tierWorkflows[tier]; ok {
		return workflows, nil
	}
	if tier != "custom" {
		return nil, fmt.Errorf("unsupported installed SFL tier %q", manifest.Tier)
	}

	components := make(map[string]struct{}, len(manifest.Components))
	for _, component := range manifest.Components {
		components[component] = struct{}{}
	}
	var workflows []string
	for _, workflow := range tierWorkflows["full"] {
		component := strings.TrimSuffix(strings.TrimSuffix(workflow, ".yml"), ".md")
		component = strings.TrimSuffix(component, ".lock")
		if _, ok := components[component]; ok {
			workflows = append(workflows, workflow)
		}
	}
	if len(workflows) == 0 {
		return nil, fmt.Errorf("custom SFL manifest has no recognized workflow components")
	}
	return workflows, nil
}

var governanceFiles = []string{
	"deployment/governance/policy.md",
	"deployment/governance/labels.json",
}

const sflVersionPlaceholder = "__SFL_VERSION__"

var semanticVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)
var fullCommitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func renderWorkflow(content, sflVersion string) string {
	return strings.ReplaceAll(content, sflVersionPlaceholder, sflVersion)
}

type deploymentRelease struct {
	Version string
	Ref     string
	SHA     string
}

func resolveDeploymentRelease(sourceRef string) (deploymentRelease, error) {
	ref := sourceRef
	if ref == "" {
		latest, err := fetchLatestReleaseFunc(motherRepoOwner, motherRepoName)
		if err != nil {
			return deploymentRelease{}, fmt.Errorf("resolving latest synchronized SFL release: %w", err)
		}
		ref = latest
	}

	version := strings.TrimPrefix(ref, "v")
	if !strings.HasPrefix(ref, "v") || !semanticVersionPattern.MatchString(version) {
		return deploymentRelease{}, fmt.Errorf("invalid SFL release ref %q (expected a v-prefixed semantic version)", ref)
	}

	sha, err := getCommitSHA(motherRepoOwner, motherRepoName, ref)
	if err != nil {
		return deploymentRelease{}, fmt.Errorf("resolving SFL release %s: %w", ref, err)
	}
	versionContent, err := fetchFileRaw(motherRepoOwner, motherRepoName, "VERSION", sha)
	if err != nil {
		return deploymentRelease{}, fmt.Errorf("fetching VERSION from %s: %w", ref, err)
	}
	if actual := strings.TrimSpace(versionContent); actual != version {
		return deploymentRelease{}, fmt.Errorf("release %s contains VERSION %q", ref, actual)
	}

	return deploymentRelease{Version: version, Ref: ref, SHA: sha}, nil
}

func runInit(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseInitOptions(args, stderr)
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
	existingManifest, err := readRemoteManifest(owner, repo)
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("checking existing SFL deployment in %s/%s: %w", owner, repo, err)
	}
	if err := validateInitTierTransition(existingManifest, opts, owner+"/"+repo); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Deploying SFL (%s tier) to %s/%s\n\n", opts.tier, owner, repo)

	fmt.Fprintf(stdout, "  Resolving synchronized SFL release...\n")
	release, err := resolveDeploymentRelease(opts.sourceRef)
	if err != nil {
		return err
	}
	shortReleaseSHA := release.SHA
	if len(shortReleaseSHA) > 12 {
		shortReleaseSHA = shortReleaseSHA[:12]
	}
	fmt.Fprintf(stdout, "  SFL release: %s (%s)\n\n", release.Ref, shortReleaseSHA)

	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return fmt.Errorf("getting default branch: %w", err)
	}

	fileMap := make(map[string]string)

	workflows := tierWorkflows[opts.tier]
	fmt.Fprintf(stdout, "  Fetching %d workflow files...\n", len(workflows))
	for _, wf := range workflows {
		srcPath := sourceWorkflowPath(wf)
		content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, release.SHA)
		if fetchErr != nil {
			return fmt.Errorf("fetching %s: %w", srcPath, fetchErr)
		}
		rendered, renderErr := renderHemSoftWorkflow(wf, content, release.Version)
		if renderErr != nil {
			return fmt.Errorf("applying HemSoft engine policy to %s: %w", wf, renderErr)
		}
		fileMap[".github/workflows/"+wf] = rendered
		fmt.Fprintf(stdout, "    %s ✓\n", wf)
	}

	// Add-on workflows (only if --addon flags were specified)
	if len(opts.addons) > 0 {
		addonFiles := addonWorkflowFiles(opts.addons)
		fmt.Fprintf(stdout, "\n  Fetching %d add-on workflow file(s)...\n", len(addonFiles))
		for _, wf := range addonFiles {
			srcPath := sourceWorkflowPath(wf)
			content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, release.SHA)
			if fetchErr != nil {
				return fmt.Errorf("fetching add-on %s: %w", srcPath, fetchErr)
			}
			rendered, renderErr := renderHemSoftWorkflow(wf, content, release.Version)
			if renderErr != nil {
				return fmt.Errorf("applying HemSoft engine policy to add-on %s: %w", wf, renderErr)
			}
			fileMap[".github/workflows/"+wf] = rendered
			fmt.Fprintf(stdout, "    %s ✓ (add-on)\n", wf)
		}
	}

	if opts.tier != "reviewer" {
		fmt.Fprintf(stdout, "\n  Fetching governance files...\n")
		for _, gf := range governanceFiles {
			content, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, gf, release.SHA)
			if fetchErr != nil {
				return fmt.Errorf("fetching %s: %w", gf, fetchErr)
			}
			dstPath := ".sfl/" + gf[len("deployment/"):]
			fileMap[dstPath] = content
			fmt.Fprintf(stdout, "    %s ✓\n", dstPath)
		}
	}

	// Manifest
	deployedAt := time.Now().UTC()
	deployedBy := getCurrentUser()
	if existingManifest != nil &&
		existingManifest.Version == release.Version &&
		existingManifest.Tier == opts.tier &&
		existingManifest.SourceSHA == release.SHA {
		deployedAt = existingManifest.DeployedAt
		deployedBy = existingManifest.DeployedBy
	}
	manifest := &sflManifest{
		Version:      release.Version,
		Tier:         opts.tier,
		MotherRepo:   motherRepoOwner + "/" + motherRepoName,
		DeployedAt:   deployedAt,
		DeployedBy:   deployedBy,
		SourceSHA:    release.SHA,
		Components:   tierComponents[opts.tier],
		Addons:       opts.addons,
		EnginePolicy: hemSoftEnginePolicyManifestForFileMap(fileMap),
	}
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	fileMap[".sfl/sfl.json"] = manifestJSON
	fmt.Fprintf(stdout, "    .sfl/sfl.json ✓\n")

	shortSHA := release.SHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}
	commitMsg := fmt.Sprintf("chore: deploy SFL %s (%s tier)\n\nDeployed by gh-sfl from %s/%s@%s", release.Version, opts.tier, motherRepoOwner, motherRepoName, shortSHA)

	fmt.Fprintf(stdout, "\n")
	var prURL string
	if opts.pr {
		prURL, err = deployViaPullRequest(owner, repo, defaultBranch, "init", fileMap, commitMsg, true, stdout)
	} else {
		err = deployViaGit(owner, repo, defaultBranch, fileMap, commitMsg, true, stdout)
	}
	if err != nil {
		return fmt.Errorf("deploying files: %w", err)
	}

	if opts.tier != "reviewer" {
		fmt.Fprintf(stdout, "\n  Syncing labels...\n")
		labelsJSON, fetchErr := fetchFileRaw(motherRepoOwner, motherRepoName, "deployment/governance/labels.json", release.SHA)
		if fetchErr != nil {
			return fmt.Errorf("fetching labels.json: %w", fetchErr)
		}

		var labels []labelDef
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return fmt.Errorf("parsing labels.json: %w", err)
		}
		created, updated, labelErr := ensureLabels(owner, repo, labels)
		if labelErr != nil {
			return fmt.Errorf("syncing labels: %w", labelErr)
		}
		fmt.Fprintf(stdout, "    %d created, %d updated (%d total)\n", created, updated, len(labels))

		fmt.Fprintf(stdout, "\n  Setting repo variables...\n")
		repoVars := []struct{ name, value string }{
			{"SFL_ENABLED", "true"},
			{"SFL_MAX_CYCLES", "20"},
		}
		if opts.slackWebhook != "" {
			repoVars = append(repoVars, struct{ name, value string }{"SFL_SLACK_ENABLED", "true"})
		}
		for _, v := range repoVars {
			if err := ensureRepoVariable(owner, repo, v.name, v.value); err != nil {
				return fmt.Errorf("setting variable %s: %w", v.name, err)
			}
			fmt.Fprintf(stdout, "    %s=%s ✓\n", v.name, v.value)
		}
	}

	if opts.slackWebhook != "" {
		fmt.Fprintf(stdout, "\n  Setting repo secrets...\n")
		if err := setRepoSecret(owner, repo, "SFL_SLACK_WEBHOOK", opts.slackWebhook); err != nil {
			return fmt.Errorf("setting secret SFL_SLACK_WEBHOOK: %w", err)
		}
		fmt.Fprintf(stdout, "    SFL_SLACK_WEBHOOK ✓\n")
	}

	if prURL != "" {
		fmt.Fprintf(stdout, "\n✅ SFL %s deployment pull request ready for %s/%s: %s\n", release.Version, owner, repo, prURL)
	} else {
		fmt.Fprintf(stdout, "\n✅ SFL %s deployed to %s/%s (%s tier)\n", release.Version, owner, repo, opts.tier)
	}
	return nil
}

func validateInitTierTransition(existing *sflManifest, opts initOptions, target string) error {
	if existing == nil || canonicalDeploymentTier(existing.Tier) == "reviewer" || opts.tierExplicit {
		return nil
	}
	return fmt.Errorf(
		"%s has an existing manifest with tier %q; use 'gh sfl sync' to preserve it or rerun init with '--tier reviewer' to migrate it explicitly",
		target,
		existing.Tier,
	)
}

func parseInitOptions(args []string, stderr io.Writer) (initOptions, error) {
	var opts initOptions
	opts.tier = "reviewer"
	opts.pr = true

	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeInitUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.tier, "tier", "reviewer", "Deployment tier: reviewer, minimal, standard, or full")
	flags.StringVar(&opts.tier, "t", "reviewer", "Deployment tier: reviewer, minimal, standard, or full")
	flags.StringVar(&opts.slackWebhook, "slack-webhook", "", "Slack Incoming Webhook URL for notifications")
	flags.Var(&stringSliceFlag{values: &opts.addons}, "addon", "Optional add-on to include (repeatable)")
	flags.BoolVar(&opts.pr, "pr", true, "Create a pull request (default)")
	flags.BoolVar(&opts.direct, "direct", false, "Push directly to the default branch instead of opening a pull request")
	flags.StringVar(&opts.sourceRef, "source-ref", "", "Synchronized SFL release tag to deploy (defaults to latest)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}
	flags.Visit(func(setFlag *flag.Flag) {
		if setFlag.Name == "tier" || setFlag.Name == "t" {
			opts.tierExplicit = true
		}
	})

	if flags.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), ", "))
	}

	switch opts.tier {
	case "reviewer", "minimal", "standard", "full":
		// valid
	default:
		return opts, fmt.Errorf("invalid tier %q (must be reviewer, minimal, standard, or full)", opts.tier)
	}
	if opts.direct {
		opts.pr = false
	}
	if opts.tier == "reviewer" && opts.slackWebhook != "" {
		return opts, fmt.Errorf("--slack-webhook requires a legacy SFL suite tier")
	}

	// Validate and deduplicate addons
	seen := make(map[string]bool)
	var deduped []string
	for _, a := range opts.addons {
		a = strings.TrimPrefix(a, "--")
		if !validAddon(a) {
			return opts, fmt.Errorf("unknown addon %q\nAvailable addons: %s", a, strings.Join(knownAddonNames(), ", "))
		}
		if !seen[a] {
			seen[a] = true
			deduped = append(deduped, a)
		}
	}

	// Remove addons whose workflows are already included in the selected tier
	tierWFs := tierWorkflows[opts.tier]
	var filtered []string
	for _, a := range deduped {
		// Only filter out an add-on if ALL of its workflow files are already
		// included in the tier. Partial overlap means the add-on provides
		// files not covered by the tier, so it should still be installed.
		addonWFs := addonWorkflows[a]
		allIncluded := len(addonWFs) > 0
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
		if !allIncluded {
			filtered = append(filtered, a)
		}
	}
	opts.addons = filtered

	return opts, nil
}

func getLatestCommitSHA(owner, repo string) (string, error) {
	return getCommitSHA(owner, repo, "")
}

func getCommitSHA(owner, repo, ref string) (string, error) {
	apiPath := fmt.Sprintf("repos/%s/%s/commits?per_page=1", owner, repo)
	if ref != "" {
		apiPath += "&sha=" + url.QueryEscape(ref)
	}
	if client, ok, err := sourceReadClient(owner, repo); err != nil {
		return "", err
	} else if ok {
		var commits []struct {
			SHA string `json:"sha"`
		}
		if err := client.Get(apiPath, &commits); err != nil {
			return "", fmt.Errorf("fetching source commit: %w", err)
		}
		if len(commits) == 0 || commits[0].SHA == "" {
			return "", fmt.Errorf("fetching source commit: GitHub returned no commits")
		}
		return commits[0].SHA, nil
	}

	stdoutBuf, stderrBuf, err := gh.Exec(
		"api", apiPath,
		"--jq", ".[0].sha",
	)
	if err != nil {
		return "", fmt.Errorf("%s: %w", stderrBuf.String(), err)
	}
	return strings.TrimSpace(stdoutBuf.String()), nil
}

func getCurrentUser() string {
	stdoutBuf, _, err := gh.Exec("api", "user", "--jq", ".login")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(stdoutBuf.String())
}

func writeInitUsage(w io.Writer) {
	fmt.Fprint(w, initUsage)
}

const initUsage = `Deploy the latest synchronized SFL PR Reviewer release.

Usage:
  gh sfl init [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
  -t, --tier string    Advanced deployment tier: reviewer, minimal, standard, or full (default "reviewer")
      --direct         Push directly instead of opening a deployment pull request
      --source-ref     Deploy a specific synchronized release tag (defaults to latest)

Tiers:
  reviewer   SFL PR Reviewer package only (default)
  minimal    SFL Dispatcher only (label routing)
  standard   Dispatcher + Processor + Review Reactor (full quality loop)
  full       Standard + Repo Audit + Simplisticate Audit + Simplisticate PR + PR Review

Examples:
  gh sfl init --repo owner/repo            # Deploy the latest reviewer through a PR
  gh sfl init                              # Deploy the reviewer to the current repo through a PR
  gh sfl init --direct                     # Intentionally push the reviewer directly
  gh sfl init --tier standard              # Deploy standard tier to current repo
`
