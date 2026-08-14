package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

type uninstallOptions struct {
	repo                  string
	keepLabels            bool
	dryRun                bool
	force                 bool
	allowManifestFallback bool
}

func runUninstall(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseUninstallOptions(args, stderr)
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

	// Read the manifest to determine what's installed
	manifest, err := readRemoteManifest(owner, repo)
	forceFallback := false
	if err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("reading SFL installation manifest in %s/%s: %w", owner, repo, err)
		}
		if !opts.allowManifestFallback {
			return fmt.Errorf("no SFL installation found in %s/%s (.sfl/sfl.json missing)\nUse --allow-manifest-fallback to select the complete managed file list", owner, repo)
		}
		forceFallback = true
		fmt.Fprintf(stdout, "  ⚠ No manifest found — using the complete managed file list\n\n")
	}

	tier := "unknown"
	if manifest != nil {
		tier = manifest.Tier
	}
	fmt.Fprintf(stdout, "Uninstalling SFL from %s/%s (tier: %s)\n\n", owner, repo, tier)

	filesToRemove, err := uninstallFiles(manifest, forceFallback)
	if err != nil {
		return fmt.Errorf("resolving installed files: %w", err)
	}

	// Show what will be removed
	fmt.Fprintf(stdout, "  Files to remove:\n")
	for _, f := range filesToRemove {
		fmt.Fprintf(stdout, "    %s\n", f)
	}

	if !opts.keepLabels {
		fmt.Fprintf(stdout, "\n  Labels to remove:\n")
		for _, l := range sflLabels {
			fmt.Fprintf(stdout, "    %s\n", l)
		}
	}

	if opts.dryRun {
		fmt.Fprintf(stdout, "\n  (dry run — no changes made)\n")
		return nil
	}

	if !opts.force {
		fmt.Fprintf(stderr, "\n  ⚠ This will permanently remove SFL from %s/%s.\n", owner, repo)
		fmt.Fprintf(stderr, "  Re-run with --force to confirm.\n")
		return fmt.Errorf("uninstall requires --force to confirm")
	}

	// Get the default branch
	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return fmt.Errorf("getting default branch: %w", err)
	}
	client, err := newRESTClient()
	if err != nil {
		return fmt.Errorf("creating GitHub REST client: %w", err)
	}
	rulesets, err := getRepositoryRulesets(client, owner, repo)
	if err != nil {
		return fmt.Errorf("inspecting reviewer gate before uninstall: %w", err)
	}
	var repository struct {
		ID int64 `json:"id"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", owner, repo), &repository); err != nil {
		return fmt.Errorf("reading repository metadata before uninstall: %w", err)
	}
	requiredWorkflows, reviewerStatusChecks := findAllReviewerGates(
		rulesets,
		defaultBranch,
		repository.ID,
	)
	gates, err := prepareReviewerGatesForUninstall(
		client,
		owner,
		repo,
		defaultBranch,
		requiredWorkflows,
		reviewerStatusChecks,
	)
	if err != nil {
		return err
	}
	// Remove files via git
	fmt.Fprintf(stdout, "\n  Removing files...\n")
	removeGates := func() error {
		return removeReviewerGatesForUninstall(
			client,
			owner,
			repo,
			gates,
			stdout,
		)
	}
	restoreGates := func() error {
		return restoreReviewerGates(client, owner, repo, gates, stdout)
	}
	if err := removeViaGit(
		owner,
		repo,
		defaultBranch,
		filesToRemove,
		removeGates,
		restoreGates,
		stdout,
	); err != nil {
		return fmt.Errorf("removing files: %w", err)
	}

	// Remove labels
	if !opts.keepLabels {
		fmt.Fprintf(stdout, "\n  Removing labels...\n")
		removed, removeErr := removeLabelsWithClient(client, owner, repo, sflLabels, stdout)
		if removeErr != nil {
			return fmt.Errorf("removing labels after workflow uninstall (%d removed): %w", removed, removeErr)
		}
		fmt.Fprintf(stdout, "    %d labels removed\n", removed)
	}

	fmt.Fprintf(stdout, "\n✅ SFL uninstalled from %s/%s\n", owner, repo)
	return nil
}

func removeLabelsWithClient(client restAPI, owner, repo string, labels []string, stdout io.Writer) (int, error) {
	removed := 0
	var failures []error
	for _, label := range labels {
		path := fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, escapedLabelPath(label))
		if err := client.Delete(path, nil); err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				continue
			}
			failures = append(failures, fmt.Errorf("%s: %w", label, err))
			continue
		}
		removed++
		fmt.Fprintf(stdout, "    ✓ %s\n", label)
	}
	return removed, errors.Join(failures...)
}

func uninstallFiles(manifest *sflManifest, forceFallback bool) ([]string, error) {
	paths := make(map[string]struct{})
	add := func(path string) {
		paths[path] = struct{}{}
	}

	if forceFallback {
		for _, path := range managedDeploymentPaths() {
			add(path)
		}
		add(".sfl/sfl.json")
		add(".sfl/sfl-config.yml")
	} else {
		workflows, err := workflowsForInstalledManifest(manifest)
		if err != nil {
			return nil, err
		}
		for _, workflow := range workflows {
			add(".github/workflows/" + workflow)
		}
		for _, workflow := range addonWorkflowFiles(manifest.Addons) {
			add(".github/workflows/" + workflow)
		}
		add(".sfl/sfl.json")
		if canonicalDeploymentTier(manifest.Tier) != "reviewer" {
			add(".sfl/sfl-config.yml")
			add(".sfl/governance/policy.md")
			add(".sfl/governance/labels.json")
		}
	}

	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func removeReviewerGatesForUninstall(
	client restAPI,
	owner, repo string,
	gates []repositoryRuleset,
	stdout io.Writer,
) error {
	var removed []repositoryRuleset
	for _, gate := range gates {
		collectionPath, pathErr := rulesetCollectionPath(owner, repo, gate)
		if pathErr != nil {
			return pathErr
		}
		rulesetPath := fmt.Sprintf("%s/%d", collectionPath, gate.ID)
		if gate.ETag == "" {
			return fmt.Errorf(
				"reviewer ruleset %q has no entity tag; refusing an unsafe deletion",
				gate.Name,
			)
		}
		if err := client.DeleteIfMatch(
			rulesetPath,
			gate.ETag,
			nil,
		); err != nil {
			var current repositoryRuleset
			lookupErr := client.Get(rulesetPath, &current)
			var httpErr *api.HTTPError
			if errors.As(lookupErr, &httpErr) && httpErr.StatusCode == 404 {
				removed = append(removed, gate)
				fmt.Fprintf(stdout, "  ✓ Removed reviewer ruleset %q\n", gate.Name)
				continue
			} else if lookupErr != nil {
				restoreErr := restoreReviewerGates(client, owner, repo, removed, stdout)
				return errors.Join(
					fmt.Errorf(
						"removing reviewer ruleset %q: %w; could not determine whether GitHub applied the deletion: %v",
						gate.Name,
						err,
						lookupErr,
					),
					restoreErr,
				)
			}
			restoreErr := restoreReviewerGates(client, owner, repo, removed, stdout)
			if restoreErr != nil {
				return fmt.Errorf(
					"removing reviewer ruleset %q: %w; restoring previously removed rulesets: %v",
					gate.Name,
					err,
					restoreErr,
				)
			}
			return fmt.Errorf("removing reviewer ruleset %q: %w", gate.Name, err)
		}
		removed = append(removed, gate)
		fmt.Fprintf(stdout, "  ✓ Removed reviewer ruleset %q\n", gate.Name)
	}
	return nil
}

func prepareReviewerGatesForUninstall(
	client restAPI,
	owner, repo string,
	defaultBranch string,
	requiredWorkflows, reviewerStatusChecks []repositoryRuleset,
) ([]repositoryRuleset, error) {
	var gates []repositoryRuleset
	if len(requiredWorkflows) > 0 {
		return nil, fmt.Errorf(
			"an inherited required-workflow gate applies to %s/%s; remove it through its owning organization before uninstall",
			owner,
			repo,
		)
	}
	for _, statusCheck := range reviewerStatusChecks {
		detail, err := getRepositoryRulesetForMutation(client, owner, repo, statusCheck)
		if err != nil {
			return nil, fmt.Errorf("reading reviewer gate before uninstall: %w", err)
		}
		if err := validateDedicatedRepositoryReviewerGate(detail, defaultBranch); err != nil {
			return nil, err
		}
		gates = append(gates, detail)
	}
	return gates, nil
}

func restoreReviewerGates(
	client restAPI,
	owner, repo string,
	gates []repositoryRuleset,
	stdout io.Writer,
) error {
	var restoreErrors []error
	for _, gate := range gates {
		payload := map[string]any{
			"name":        gate.Name,
			"target":      gate.Target,
			"enforcement": gate.Enforcement,
			"conditions":  gate.Conditions,
			"rules":       gate.Rules,
		}
		if payload["target"] == "" {
			payload["target"] = "branch"
		}
		if payload["enforcement"] == "" {
			payload["enforcement"] = "active"
		}
		if len(gate.BypassActors) > 0 {
			payload["bypass_actors"] = gate.BypassActors
		}
		body, err := jsonBody(payload)
		if err == nil {
			var restored repositoryRuleset
			var collectionPath string
			collectionPath, err = rulesetCollectionPath(owner, repo, gate)
			if err == nil {
				err = client.Post(collectionPath, body, &restored)
			}
		}
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("%q: %w", gate.Name, err))
			continue
		}
		fmt.Fprintf(stdout, "  ✓ Restored reviewer ruleset %q after uninstall failure\n", gate.Name)
	}
	return errors.Join(restoreErrors...)
}

func rulesetCollectionPath(owner, repo string, ruleset repositoryRuleset) (string, error) {
	switch ruleset.SourceType {
	case "Organization":
		return "", fmt.Errorf(
			"ruleset %q is organization-managed; HemSoft uninstall only mutates repository rulesets",
			ruleset.Name,
		)
	case "", "Repository":
		return fmt.Sprintf("repos/%s/%s/rulesets", owner, repo), nil
	default:
		return "", fmt.Errorf(
			"ruleset %q is inherited from unsupported source type %q",
			ruleset.Name,
			ruleset.SourceType,
		)
	}
}

// removeViaGit prepares the removal commit before mutating reviewer gates.
// If the push fails after gate removal, rollback restores the original gates.
func removeViaGit(
	owner, repo, branch string,
	files []string,
	beforePush, rollback func() error,
	w io.Writer,
) error {
	tmpDir, err := os.MkdirTemp("", "gh-sfl-uninstall-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := fmt.Sprintf("git@github-personal1:%s/%s.git", owner, repo)
	cloneCmd := exec.Command("git", "clone", "--depth=1", cloneURL, tmpDir)
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		return fmt.Errorf("cloning: %s: %w", string(out), cloneErr)
	}

	runGit := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	removed, err := removeExistingManagedFiles(tmpDir, files, os.Remove, w)
	if err != nil {
		return err
	}

	// Also remove empty .sfl directory
	sflDir := filepath.Join(tmpDir, ".sfl")
	if isEmpty, _ := isDirEmpty(sflDir); isEmpty {
		os.RemoveAll(sflDir)
	}
	// Remove .sfl/governance if empty
	govDir := filepath.Join(tmpDir, ".sfl", "governance")
	if isEmpty, _ := isDirEmpty(govDir); isEmpty {
		os.RemoveAll(govDir)
	}

	if removed == 0 {
		fmt.Fprintf(w, "    No SFL files found — nothing to remove.\n")
		return beforePush()
	}

	if _, err := runGit("add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	checkCmd := exec.Command("git", "diff", "--cached", "--quiet")
	checkCmd.Dir = tmpDir
	if checkCmd.Run() == nil {
		fmt.Fprintf(w, "    No changes — files already absent.\n")
		return beforePush()
	}

	commitMsg := "chore: uninstall SFL\n\nRemoved by gh-sfl uninstall"
	if out, err := runGit("commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit: %s: %w", out, err)
	}
	localCommit, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading prepared uninstall commit: %w", err)
	}
	localCommit = strings.TrimSpace(localCommit)

	if err := beforePush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "  Pushing to %s/%s...\n", owner, repo)
	if out, err := runGit("push", "origin", "HEAD"); err != nil {
		reachedRemote, verifyErr := uninstallCommitReachedRemote(runGit, branch, localCommit)
		if verifyErr != nil {
			return fmt.Errorf(
				"git push: %s: %w; remote state is ambiguous, so reviewer gates were left removed to avoid requiring a deleted workflow: %v",
				out,
				err,
				verifyErr,
			)
		}
		if reachedRemote {
			fmt.Fprintf(w, "  Push reported an error, but the uninstall commit reached %s; continuing.\n", branch)
			return nil
		}
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("git push: %s: %w; restoring reviewer gates: %v", out, err, rollbackErr)
		}
		return fmt.Errorf("git push: %s: %w", out, err)
	}

	return nil
}

func removeExistingManagedFiles(
	root string,
	files []string,
	remove func(string) error,
	w io.Writer,
) (int, error) {
	removed := 0
	for _, file := range files {
		target := filepath.Join(root, filepath.FromSlash(file))
		if _, err := os.Stat(target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("inspecting %s before removal: %w", file, err)
		}
		if err := remove(target); err != nil {
			return removed, fmt.Errorf("removing %s: %w", file, err)
		}
		removed++
		fmt.Fprintf(w, "    ✓ %s\n", file)
	}
	return removed, nil
}

func uninstallCommitReachedRemote(
	runGit func(args ...string) (string, error),
	branch, localCommit string,
) (bool, error) {
	if _, err := runGit("fetch", "origin", branch); err != nil {
		return false, fmt.Errorf("fetching remote branch: %w", err)
	}
	_, err := runGit("merge-base", "--is-ancestor", localCommit, "FETCH_HEAD")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking whether uninstall commit reached remote: %w", err)
}

func isDirEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func parseUninstallOptions(args []string, stderr io.Writer) (uninstallOptions, error) {
	var opts uninstallOptions

	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeUninstallUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.BoolVar(&opts.keepLabels, "keep-labels", false, "Keep SFL labels in place")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "Show what would be removed without making changes")
	flags.BoolVar(&opts.dryRun, "n", false, "Show what would be removed without making changes")
	flags.BoolVar(&opts.force, "force", false, "Confirm destructive uninstall")
	flags.BoolVar(&opts.force, "f", false, "Confirm destructive uninstall")
	flags.BoolVar(&opts.allowManifestFallback, "allow-manifest-fallback", false, "Use the complete managed file list when the manifest is missing")

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

func writeUninstallUsage(w io.Writer) {
	fmt.Fprint(w, uninstallUsage)
}

const uninstallUsage = `Remove SFL workflows, config, and labels from a repository.

Reads the installed manifest to determine which files to remove, then
deletes them in a single commit. Requires --force to confirm.

Usage:
  gh sfl uninstall [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
  -f, --force          Confirm the uninstall (required)
  -n, --dry-run        Show what would be removed without making changes
      --keep-labels    Keep SFL labels (only remove workflow files)
      --allow-manifest-fallback
                       Use the complete managed file list when the manifest is missing

Examples:
  gh sfl uninstall --dry-run              # Preview what would be removed
  gh sfl uninstall --force                # Remove SFL from current repo
  gh sfl uninstall --force --keep-labels  # Remove workflows but keep labels
  gh sfl uninstall --dry-run --allow-manifest-fallback # Preview removal without a manifest
  gh sfl uninstall --force --repo o/r     # Remove SFL from another repo
`
