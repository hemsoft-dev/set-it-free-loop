package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/muesli/termenv"
)

type statusOptions struct {
	repo string
}

type repositoryRuleset struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Target       string `json:"target"`
	Source       string `json:"source"`
	SourceType   string `json:"source_type"`
	Enforcement  string `json:"enforcement"`
	BypassActors []any  `json:"bypass_actors"`
	ETag         string `json:"-"`
	Conditions   struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
		RepositoryID *struct {
			RepositoryIDs []int64 `json:"repository_ids"`
		} `json:"repository_id,omitempty"`
		RepositoryName *struct {
			Include   []string `json:"include"`
			Exclude   []string `json:"exclude"`
			Protected bool     `json:"protected"`
		} `json:"repository_name,omitempty"`
	} `json:"conditions"`
	Rules []struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	} `json:"rules"`
}

type workflowRunSummary struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}

// SFL state labels in priority order
var sflStateLabels = []string{
	"sfl-processing",
	"sfl-ready-for-review",
	"sfl-needs-work",
	"sfl-done",
}

func runStatus(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseStatusOptions(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpDisplayed) {
			return nil
		}
		return err
	}

	owner, repo, err := parseRepoFlag(opts.repo)
	if err != nil {
		return err
	}

	colorEnabled := term.FromEnv().IsColorEnabled()
	styler := newTableStyler(stdout, colorEnabled)

	fmt.Fprintf(stdout, "SFL Status: %s/%s\n", owner, repo)
	fmt.Fprintln(stdout, strings.Repeat("─", 40))

	// Section 1: Manifest / Installation
	manifest, manifestErr := readRemoteManifest(owner, repo)
	if manifestErr != nil {
		fmt.Fprintf(stdout, "\n")
		fmt.Fprintf(stdout, "  %s SFL not installed\n", styler.colored("✗", termenv.ANSIRed).styled)
		fmt.Fprintf(stdout, "  Run 'gh sfl init' to deploy\n")
		return nil
	}

	fmt.Fprintf(stdout, "\n  %s Installed\n", styler.colored("✓", termenv.ANSIGreen).styled)
	fmt.Fprintf(stdout, "  Version:    %s\n", manifest.Version)
	fmt.Fprintf(stdout, "  Tier:       %s\n", manifest.Tier)
	fmt.Fprintf(stdout, "  Deployed:   %s\n", manifest.DeployedAt.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(stdout, "  By:         %s\n", manifest.DeployedBy)
	fmt.Fprintf(stdout, "  Components: %s\n", strings.Join(manifest.Components, ", "))
	if manifest.SourceSHA != "" {
		shortSHA := manifest.SourceSHA
		if len(shortSHA) > 12 {
			shortSHA = shortSHA[:12]
		}
		fmt.Fprintf(stdout, "  Source SHA: %s\n", shortSHA)
	}
	if len(manifest.Addons) > 0 {
		fmt.Fprintf(stdout, "  Add-ons:    %s\n", strings.Join(manifest.Addons, ", "))
	}

	release, releaseErr := resolveDeploymentRelease("")
	if releaseErr != nil {
		fmt.Fprintf(stdout, "  %s Could not resolve latest synchronized release\n",
			styler.colored("!", termenv.ANSIYellow).styled)
	} else if manifest.Version != release.Version || manifest.SourceSHA != release.SHA {
		updateCommand := "gh sfl sync"
		if canonicalDeploymentTier(manifest.Tier) == "reviewer" {
			updateCommand = "gh sfl init"
		}
		fmt.Fprintf(stdout, "  %s Update available — run '%s --repo %s/%s'\n",
			styler.colored("↑", termenv.ANSIYellow).styled, updateCommand, owner, repo)
	} else {
		fmt.Fprintf(stdout, "  %s Latest synchronized release (%s)\n",
			styler.colored("✓", termenv.ANSIGreen).styled, release.Ref)
	}

	if manifestIncludesReviewer(manifest) {
		printReviewerHealth(stdout, styler, owner, repo, manifest)
	}
	if canonicalDeploymentTier(manifest.Tier) == "reviewer" {
		fmt.Fprintln(stdout)
		return nil
	}

	// Section 2: Labels
	fmt.Fprintf(stdout, "\n  Labels:\n")
	repoLabels, labelErr := fetchRepoLabels(owner, repo)
	if labelErr != nil {
		fmt.Fprintf(stdout, "    ⚠ Could not fetch labels\n")
	} else {
		labelSet := make(map[string]bool)
		for _, l := range repoLabels {
			labelSet[l.Name] = true
		}

		missing := 0
		for _, name := range sflLabels {
			if !labelSet[name] {
				missing++
			}
		}

		if missing == 0 {
			fmt.Fprintf(stdout, "    %s All %d labels present\n",
				styler.colored("✓", termenv.ANSIGreen).styled, len(sflLabels))
		} else {
			fmt.Fprintf(stdout, "    %s %d of %d labels present (%d missing)\n",
				styler.colored("!", termenv.ANSIYellow).styled,
				len(sflLabels)-missing, len(sflLabels), missing)
			for _, name := range sflLabels {
				if !labelSet[name] {
					fmt.Fprintf(stdout, "      missing: %s\n", name)
				}
			}
		}
	}

	// Section 3: Active SFL PRs
	fmt.Fprintf(stdout, "\n  Active PRs:\n")
	prs, prErr := fetchPRsByLabel(owner, repo, "sfl-pr")
	if prErr != nil {
		fmt.Fprintf(stdout, "    ⚠ Could not fetch PRs\n")
	} else if len(prs) == 0 {
		fmt.Fprintf(stdout, "    No active SFL-managed PRs\n")
	} else {
		for _, pr := range prs {
			state := "unknown"
			for _, sl := range sflStateLabels {
				for _, l := range pr.Labels {
					if l.Name == sl {
						state = sl
						break
					}
				}
				if state != "unknown" {
					break
				}
			}

			stateCell := styler.plain(state)
			switch state {
			case "sfl-done":
				stateCell = styler.colored(state, termenv.ANSIGreen)
			case "sfl-processing":
				stateCell = styler.colored(state, termenv.ANSIYellow)
			case "sfl-needs-work":
				stateCell = styler.colored(state, termenv.ANSIRed)
			case "sfl-ready-for-review":
				stateCell = styler.colored(state, termenv.ANSICyan)
			}

			// Check for escalation
			escalated := false
			paused := false
			for _, l := range pr.Labels {
				if l.Name == "sfl-human-required" {
					escalated = true
				}
				if l.Name == "sfl-pause" {
					paused = true
				}
			}

			suffix := ""
			if escalated {
				suffix = styler.colored(" [ESCALATED]", termenv.ANSIRed).styled
			}
			if paused {
				suffix += styler.colored(" [PAUSED]", termenv.ANSIYellow).styled
			}

			title := trimText(pr.Title, 40)
			fmt.Fprintf(stdout, "    #%-5d %s  %s%s\n",
				pr.Number, stateCell.styled, title, suffix)
		}
	}

	fmt.Fprintln(stdout)
	return nil
}

func expectedWorkflowFiles(manifest *sflManifest) []string {
	seen := make(map[string]struct{})
	var files []string
	workflows, _ := workflowsForInstalledManifest(manifest)
	for _, workflow := range append(workflows, addonWorkflowFiles(manifest.Addons)...) {
		if _, exists := seen[workflow]; exists {
			continue
		}
		seen[workflow] = struct{}{}
		files = append(files, workflow)
	}
	sort.Strings(files)
	return files
}

func manifestIncludesReviewer(manifest *sflManifest) bool {
	workflows, err := workflowsForInstalledManifest(manifest)
	return err == nil && workflowsIncludeReviewer(workflows, addonWorkflowFiles(manifest.Addons))
}

func expectedReviewerWorkflowFiles(manifest *sflManifest) []string {
	var files []string
	for _, workflow := range expectedWorkflowFiles(manifest) {
		if strings.HasPrefix(workflow, "sfl-pr-review") {
			files = append(files, workflow)
		}
	}
	return files
}

func printReviewerHealth(stdout io.Writer, styler tableStyler, owner, repo string, manifest *sflManifest) {
	fmt.Fprintf(stdout, "\n  Reviewer package:\n")
	missing := 0
	drifted := 0
	if manifest.SourceSHA == "" {
		drifted++
		fmt.Fprintf(stdout, "    %s Manifest is missing immutable sourceSha\n",
			styler.colored("✗", termenv.ANSIRed).styled)
	}
	for _, workflow := range expectedReviewerWorkflowFiles(manifest) {
		path := ".github/workflows/" + workflow
		content, err := fetchFileRaw(owner, repo, path, "")
		switch {
		case err != nil:
			missing++
			fmt.Fprintf(stdout, "    %s %s missing\n",
				styler.colored("✗", termenv.ANSIRed).styled, path)
		default:
			source, sourceErr := fetchFileRaw(
				motherRepoOwner,
				motherRepoName,
				sourceWorkflowPath(workflow),
				manifest.SourceSHA,
			)
			expected, renderErr := renderHemSoftWorkflow(workflow, source, manifest.Version)
			if manifest.SourceSHA == "" || sourceErr != nil || renderErr != nil ||
				content != expected {
				drifted++
				fmt.Fprintf(stdout, "    %s %s differs from pinned source %s\n",
					styler.colored("!", termenv.ANSIYellow).styled, path, manifest.SourceSHA)
			} else {
				fmt.Fprintf(stdout, "    %s %s\n",
					styler.colored("✓", termenv.ANSIGreen).styled, path)
			}
		}
	}
	if missing == 0 && drifted == 0 {
		fmt.Fprintf(stdout, "    %s All reviewer package files match the pinned release source\n",
			styler.colored("✓", termenv.ANSIGreen).styled)
	}

	health, healthErr := inspectReviewerRollout(owner, repo)
	printReviewerPrerequisites(stdout, styler, health, healthErr)

	mode, ruleset, err := reviewerGatePosture(owner, repo)
	fmt.Fprintf(stdout, "\n  Merge posture:\n")
	if err != nil {
		fmt.Fprintf(stdout, "    %s Could not inspect repository rulesets: %v\n",
			styler.colored("!", termenv.ANSIYellow).styled, err)
	} else {
		switch mode {
		case "required-workflow":
			fmt.Fprintf(stdout, "    %s Gated by required reviewer workflow (%s)\n",
				styler.colored("✓", termenv.ANSIGreen).styled, ruleset)
		case "required-status-check":
			fmt.Fprintf(stdout, "    %s Gated by strict SFL reviewer gate runner check (%s)\n",
				styler.colored("✓", termenv.ANSIGreen).styled, ruleset)
		case "stale-required-workflow":
			fmt.Fprintf(
				stdout,
				"    %s Required reviewer workflow lacks strict base freshness (%s); rerun gh sfl gate\n",
				styler.colored("!", termenv.ANSIYellow).styled,
				ruleset,
			)
		case "stale-status-check":
			fmt.Fprintf(stdout, "    %s SFL Reviewer Gate Runner check is missing or lacks strict Actions ownership (%s); rerun gh sfl gate\n",
				styler.colored("!", termenv.ANSIYellow).styled, ruleset)
		default:
			fmt.Fprintf(stdout, "    %s Advisory-only; required reviewer gate is missing\n",
				styler.colored("✗", termenv.ANSIRed).styled)
		}
	}

	fmt.Fprintf(stdout, "\n  Latest reviewer run:\n")
	run, err := latestReviewerRun(owner, repo)
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "    %s Could not inspect reviewer runs: %v\n",
			styler.colored("!", termenv.ANSIYellow).styled, err)
	case run.URL == "":
		fmt.Fprintf(stdout, "    %s No reviewer run found; merge the deployment PR and run a smoke review\n",
			styler.colored("!", termenv.ANSIYellow).styled)
	case run.Status != "completed":
		fmt.Fprintf(stdout, "    %s %s (%s)\n",
			styler.colored("!", termenv.ANSIYellow).styled, run.Status, run.URL)
	case run.Conclusion != "success":
		fmt.Fprintf(stdout, "    %s %s (%s)\n",
			styler.colored("✗", termenv.ANSIRed).styled, run.Conclusion, run.URL)
	default:
		fmt.Fprintf(stdout, "    %s success (%s)\n",
			styler.colored("✓", termenv.ANSIGreen).styled, run.URL)
	}
}

func printReviewerPrerequisites(
	stdout io.Writer,
	styler tableStyler,
	health reviewerRolloutHealth,
	healthErr error,
) {
	fmt.Fprintf(stdout, "\n  Codex App:\n")
	if healthErr != nil {
		fmt.Fprintf(stdout, "    %s Could not inspect reviewer rollout prerequisites: %v\n",
			styler.colored("!", termenv.ANSIYellow).styled, healthErr)
	} else if health.AppNotice != "" {
		fmt.Fprintf(stdout, "    %s %s\n",
			styler.colored("!", termenv.ANSIYellow).styled, health.AppNotice)
	}

	fmt.Fprintf(stdout, "\n  Reviewer prerequisites:\n")
	if healthErr != nil {
		fmt.Fprintf(stdout, "    %s Could not inspect credential metadata or Actions state\n",
			styler.colored("!", termenv.ANSIYellow).styled)
	} else {
		if health.DefaultBranch == "" {
			fmt.Fprintf(stdout, "    %s Repository has no default branch\n",
				styler.colored("✗", termenv.ANSIRed).styled)
		} else {
			fmt.Fprintf(stdout, "    %s Default branch: %s\n",
				styler.colored("✓", termenv.ANSIGreen).styled, health.DefaultBranch)
		}
		if health.ActionsEnabled {
			fmt.Fprintf(stdout, "    %s GitHub Actions enabled\n",
				styler.colored("✓", termenv.ANSIGreen).styled)
		} else {
			fmt.Fprintf(stdout, "    %s GitHub Actions disabled\n",
				styler.colored("✗", termenv.ANSIRed).styled)
		}
		for _, issue := range health.ActionsIssues {
			fmt.Fprintf(stdout, "    %s %s\n",
				styler.colored("✗", termenv.ANSIRed).styled, issue)
		}
		for _, name := range reviewerRequiredVariables {
			if slices.Contains(health.MissingVariables, name) {
				fmt.Fprintf(stdout, "    %s Missing Actions variable %s\n",
					styler.colored("✗", termenv.ANSIRed).styled, name)
			} else {
				fmt.Fprintf(stdout, "    %s Actions variable %s present\n",
					styler.colored("✓", termenv.ANSIGreen).styled, name)
			}
		}
		for _, name := range reviewerRequiredSecrets {
			if slices.Contains(health.MissingSecrets, name) {
				fmt.Fprintf(stdout, "    %s Missing Actions secret %s\n",
					styler.colored("✗", termenv.ANSIRed).styled, name)
			} else {
				fmt.Fprintf(stdout, "    %s Actions secret %s present\n",
					styler.colored("✓", termenv.ANSIGreen).styled, name)
			}
		}
	}

}

func reviewerGatePosture(owner, repo string) (mode, rulesetName string, err error) {
	client, err := newRESTClient()
	if err != nil {
		return "", "", err
	}
	var repository struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", owner, repo), &repository); err != nil {
		return "", "", err
	}
	details, err := getRepositoryRulesets(client, owner, repo)
	if err != nil {
		return "", "", err
	}
	mode, name := classifyReviewerGate(details, repository.DefaultBranch, repository.ID)
	return mode, name, nil
}

func getRepositoryRulesets(client restAPI, owner, repo string) ([]repositoryRuleset, error) {
	var summaries []repositoryRuleset
	for pageNumber := 1; ; pageNumber++ {
		var pageResults []repositoryRuleset
		requestPath := fmt.Sprintf(
			"repos/%s/%s/rulesets?includes_parents=true&per_page=100&page=%d",
			owner,
			repo,
			pageNumber,
		)
		if err := client.Get(requestPath, &pageResults); err != nil {
			return nil, err
		}
		summaries = append(summaries, pageResults...)
		if len(pageResults) < 100 {
			break
		}
	}
	details := make([]repositoryRuleset, 0, len(summaries))
	for _, summary := range summaries {
		var detail repositoryRuleset
		if err := client.Get(fmt.Sprintf("repos/%s/%s/rulesets/%d", owner, repo, summary.ID), &detail); err != nil {
			return nil, err
		}
		details = append(details, detail)
	}
	return details, nil
}

func classifyReviewerGate(
	rulesets []repositoryRuleset,
	defaultBranch string,
	repositoryID int64,
) (mode, rulesetName string) {
	requiredWorkflows, reviewerStatusChecks := findAllReviewerGates(
		rulesets,
		defaultBranch,
		repositoryID,
	)
	for _, requiredWorkflow := range requiredWorkflows {
		if hasReviewerFreshnessInterlock(requiredWorkflow) {
			return "required-workflow", requiredWorkflow.Name
		}
	}
	for _, statusCheck := range reviewerStatusChecks {
		if hasReviewerFreshnessInterlock(statusCheck) {
			return "required-status-check", statusCheck.Name
		}
	}
	if len(requiredWorkflows) > 0 {
		return "stale-required-workflow", requiredWorkflows[0].Name
	}
	if len(reviewerStatusChecks) > 0 {
		return "stale-status-check", reviewerStatusChecks[0].Name
	}
	return "advisory", ""
}

func findAllReviewerGates(
	rulesets []repositoryRuleset,
	defaultBranch string,
	repositoryID int64,
) (requiredWorkflows, reviewerStatusChecks []repositoryRuleset) {
	for index := range rulesets {
		ruleset := rulesets[index]
		if !rulesetAppliesToDefaultBranch(ruleset, defaultBranch) {
			continue
		}
		hasWorkflow := false
		hasReviewerStatus := false
		for _, rule := range ruleset.Rules {
			if rule.Type == "workflows" &&
				hasRequiredReviewerWorkflow(rule.Parameters, defaultBranch, repositoryID) {
				hasWorkflow = true
			}
			if rule.Type == "required_status_checks" &&
				hasReviewerStatusCheck(rule.Parameters) {
				hasReviewerStatus = true
			}
		}
		if hasWorkflow {
			requiredWorkflows = append(requiredWorkflows, ruleset)
		} else if hasReviewerStatus {
			reviewerStatusChecks = append(reviewerStatusChecks, ruleset)
		}
	}
	return requiredWorkflows, reviewerStatusChecks
}

func hasRequiredReviewerWorkflow(parameters map[string]any, defaultBranch string, repositoryID int64) bool {
	workflows, ok := parameters["workflows"].([]any)
	if !ok {
		return false
	}
	for _, value := range workflows {
		workflow, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if workflow["path"] == ".github/workflows/sfl-pr-review-auto.yml" &&
			workflow["ref"] == "refs/heads/"+defaultBranch &&
			numericIDEquals(workflow["repository_id"], repositoryID) {
			return true
		}
	}
	return false
}

func hasReviewerStatusCheck(parameters map[string]any) bool {
	checks, ok := parameters["required_status_checks"].([]any)
	if !ok {
		return false
	}
	for _, value := range checks {
		check, ok := value.(map[string]any)
		if ok && isKnownReviewerGateCheckContext(check["context"]) {
			return true
		}
	}
	return false
}

func hasReviewerFreshnessInterlock(ruleset repositoryRuleset) bool {
	for _, rule := range ruleset.Rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		strict, _ := rule.Parameters["strict_required_status_checks_policy"].(bool)
		if !strict {
			continue
		}
		checks, ok := rule.Parameters["required_status_checks"].([]any)
		if !ok {
			continue
		}
		for _, value := range checks {
			check, ok := value.(map[string]any)
			if ok &&
				check["context"] == reviewerGateCheckContext &&
				numericIDEquals(check["integration_id"], githubActionsAppID) {
				return true
			}
		}
	}
	return false
}

func numericIDEquals(value any, expected int64) bool {
	switch id := value.(type) {
	case int:
		return int64(id) == expected
	case int64:
		return id == expected
	case float64:
		return int64(id) == expected && id == float64(int64(id))
	default:
		return false
	}
}

func rulesetAppliesToDefaultBranch(ruleset repositoryRuleset, defaultBranch string) bool {
	if ruleset.Enforcement != "active" {
		return false
	}
	for _, exclude := range ruleset.Conditions.RefName.Exclude {
		if rulesetRefPatternMatches(exclude, defaultBranch) {
			return false
		}
	}
	for _, include := range ruleset.Conditions.RefName.Include {
		if rulesetRefPatternMatches(include, defaultBranch) {
			return true
		}
	}
	return false
}

func rulesetRefPatternMatches(pattern, defaultBranch string) bool {
	if pattern == "~DEFAULT_BRANCH" || pattern == "~ALL" {
		return true
	}
	matched, err := path.Match(pattern, "refs/heads/"+defaultBranch)
	return err == nil && matched
}

func latestReviewerRun(owner, repo string) (workflowRunSummary, error) {
	client, err := newRESTClient()
	if err != nil {
		return workflowRunSummary{}, err
	}
	var response struct {
		Runs []workflowRunSummary `json:"workflow_runs"`
	}
	path := fmt.Sprintf(
		"repos/%s/%s/actions/workflows/sfl-pr-review-auto.yml/runs?per_page=1",
		owner,
		repo,
	)
	if err := client.Get(path, &response); err != nil {
		return workflowRunSummary{}, err
	}
	if len(response.Runs) == 0 {
		return workflowRunSummary{}, nil
	}
	return response.Runs[0], nil
}

func parseStatusOptions(args []string, stderr io.Writer) (statusOptions, error) {
	var opts statusOptions

	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeStatusUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	if flags.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), ", "))
	}

	return opts, nil
}

func writeStatusUsage(w io.Writer) {
	fmt.Fprint(w, statusUsage)
}

const statusUsage = `Show SFL health dashboard for a repository.

Displays installation status, version, labels, and active PR states.

Usage:
  gh sfl status [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.

Examples:
  gh sfl status                        # Status for current repo
  gh sfl status --repo owner/repo      # Status for a different repo
`
