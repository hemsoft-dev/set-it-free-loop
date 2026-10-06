package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type gateOptions struct {
	repo string
}

const (
	reviewerGateCheckContext             = "SFL Reviewer Gate Runner"
	legacyReviewerGateCheckContext       = "SFL Reviewer Approval"
	githubActionsAppID             int64 = 15368
)

func runGate(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseGateOptions(args, stderr)
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
	if err := requireOrganizationAdmin(owner, repo); err != nil {
		return err
	}
	client, err := newRESTClient()
	if err != nil {
		return fmt.Errorf("creating GitHub REST client: %w", err)
	}
	var repository struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", owner, repo), &repository); err != nil {
		return fmt.Errorf("reading repository metadata: %w", err)
	}
	if repository.ID == 0 || repository.DefaultBranch == "" {
		return fmt.Errorf("GitHub returned incomplete repository metadata")
	}
	if _, err := fetchFileRaw(
		owner,
		repo,
		".github/workflows/sfl-pr-review-auto.yml",
		repository.DefaultBranch,
	); err != nil {
		return fmt.Errorf("reviewer deployment must be merged before enabling its gate: %w", err)
	}

	rulesets, err := getRepositoryRulesets(client, owner, repo)
	if err != nil {
		return fmt.Errorf("inspecting reviewer gate: %w", err)
	}
	requiredWorkflows, reviewerStatusChecks := findAllReviewerGates(
		rulesets,
		repository.DefaultBranch,
		repository.ID,
	)
	for _, workflow := range requiredWorkflows {
		if isRepositoryManagedRuleset(workflow) && hasReviewerFreshnessInterlock(workflow) {
			fmt.Fprintf(stdout, "✓ SFL reviewer gate is already required by %q\n", workflow.Name)
			return nil
		}
	}
	return ensureRepositoryReviewerGate(
		client,
		owner,
		repo,
		repository.DefaultBranch,
		rulesets,
		reviewerStatusChecks,
		stdout,
	)
}

func ensureRepositoryReviewerGate(
	client restAPI,
	owner, repo, defaultBranch string,
	allRulesets, reviewerStatusChecks []repositoryRuleset,
	stdout io.Writer,
) error {
	for _, ruleset := range reviewerStatusChecks {
		if isRepositoryManagedRuleset(ruleset) && hasReviewerFreshnessInterlock(ruleset) {
			fmt.Fprintf(stdout, "✓ SFL reviewer gate is already required by %q\n", ruleset.Name)
			return nil
		}
	}
	if len(reviewerStatusChecks) > 1 {
		return fmt.Errorf(
			"found %d stale SFL reviewer status-check rulesets; consolidate them before enabling the gate",
			len(reviewerStatusChecks),
		)
	}
	if len(reviewerStatusChecks) == 1 {
		name, err := updateRepositoryReviewerGate(
			client,
			owner,
			repo,
			defaultBranch,
			reviewerStatusChecks[0],
		)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "✓ Updated strict SFL reviewer gate %q\n", name)
		return nil
	}

	name, err := createRepositoryReviewerGate(
		client,
		owner,
		repo,
		defaultBranch,
		allRulesets,
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ Required SFL reviewer gate enabled for %s/%s (%s)\n", owner, repo, name)
	return nil
}

func updateRepositoryReviewerGate(
	client restAPI,
	owner, repo, defaultBranch string,
	summary repositoryRuleset,
) (string, error) {
	if summary.SourceType != "" && summary.SourceType != "Repository" {
		return "", fmt.Errorf(
			"reviewer status-check ruleset %q is inherited from %s and cannot be changed at repository scope",
			summary.Name,
			summary.SourceType,
		)
	}
	snapshot, err := getRepositoryRulesetForMutation(client, owner, repo, summary)
	if err != nil {
		return "", err
	}
	if isDesiredRepositoryReviewerGate(snapshot, defaultBranch) {
		return snapshot.Name, nil
	}
	if err := validateDedicatedRepositoryReviewerGate(snapshot, defaultBranch); err != nil {
		return "", err
	}
	desired, err := cloneRepositoryRuleset(snapshot)
	if err != nil {
		return "", fmt.Errorf("copying reviewer gate before update: %w", err)
	}
	desired.Rules[0].Parameters = mergeReviewerFreshnessRule(desired.Rules[0].Parameters)
	path := fmt.Sprintf("repos/%s/%s/rulesets/%d", owner, repo, snapshot.ID)
	if err := ensureRepositoryRulesetUnchanged(client, path, snapshot); err != nil {
		return "", fmt.Errorf("reviewer gate %q changed before update: %w", snapshot.Name, err)
	}
	body, err := jsonBody(repositoryRulesetMutationPayload(desired))
	if err != nil {
		return "", fmt.Errorf("encoding reviewer gate update: %w", err)
	}
	var updated repositoryRuleset
	updateErr := client.Put(path, body, &updated)

	current, _, readErr := readRepositoryRulesetWithETag(client, path, snapshot.Name)
	if readErr != nil {
		failures := []error{fmt.Errorf("could not verify the gate after the update: %w", readErr)}
		if updateErr != nil {
			failures = append(failures, fmt.Errorf("updating repository reviewer gate %q: %w", snapshot.Name, updateErr))
		}
		return "", errors.Join(failures...)
	}
	if isDesiredRepositoryReviewerGate(current, defaultBranch) {
		return current.Name, nil
	}
	if repositoryRulesetStateEqual(current, snapshot) {
		if updateErr != nil {
			return "", fmt.Errorf("updating repository reviewer gate %q: %w", snapshot.Name, updateErr)
		}
		return "", fmt.Errorf("GitHub accepted the update but reviewer gate %q remained unchanged", snapshot.Name)
	}

	if updated.ID == 0 || !repositoryRulesetStateEqual(current, updated) {
		return "", errors.Join(
			fmt.Errorf("reviewer gate %q changed during update; no rollback was attempted", snapshot.Name),
			updateErr,
		)
	}

	rollbackErr := restoreRepositoryReviewerGate(client, path, current, snapshot)
	if rollbackErr != nil {
		return "", errors.Join(
			fmt.Errorf("reviewer gate %q did not reach the required state", snapshot.Name),
			updateErr,
			fmt.Errorf("restoring the prior reviewer gate: %w", rollbackErr),
		)
	}
	return "", errors.Join(
		fmt.Errorf("reviewer gate %q did not reach the required state; restored its prior state", snapshot.Name),
		updateErr,
	)
}

func createRepositoryReviewerGate(
	client restAPI,
	owner, repo, defaultBranch string,
	before []repositoryRuleset,
) (string, error) {
	payload := repositoryReviewerGatePayload(repo)
	body, err := jsonBody(payload)
	if err != nil {
		return "", fmt.Errorf("encoding reviewer gate: %w", err)
	}
	collectionPath := fmt.Sprintf("repos/%s/%s/rulesets", owner, repo)
	var created repositoryRuleset
	createErr := client.Post(collectionPath, body, &created)
	if createErr == nil && created.ID != 0 {
		path := fmt.Sprintf("%s/%d", collectionPath, created.ID)
		current, _, readErr := readRepositoryRulesetWithETag(client, path, created.Name)
		if readErr == nil && isDesiredRepositoryReviewerGate(current, defaultBranch) {
			return current.Name, nil
		}
		verificationErr := readErr
		if verificationErr == nil {
			verificationErr = fmt.Errorf("created ruleset does not enforce the strict Actions-owned approval check")
		}
		rollbackErr := removeCreatedRepositoryReviewerGate(client, path, created.Name)
		return "", errors.Join(
			fmt.Errorf("GitHub created reviewer gate %q but post-write verification failed: %w", created.Name, verificationErr),
			rollbackErr,
		)
	}
	if createErr == nil {
		createErr = fmt.Errorf("GitHub did not return the created ruleset ID")
	}

	after, inspectErr := getRepositoryRulesets(client, owner, repo)
	if inspectErr != nil {
		return "", errors.Join(
			fmt.Errorf("creating repository reviewer gate: %w", createErr),
			fmt.Errorf("could not inspect the repository after the create attempt: %w", inspectErr),
		)
	}
	newRulesets := newRepositoryRulesets(before, after)
	expectedName := reviewerGateName(repo)
	var desired []repositoryRuleset
	var rollbackCandidates []repositoryRuleset
	var unattributed []repositoryRuleset
	var verificationErrors []error
	for _, candidate := range newRulesets {
		path := fmt.Sprintf("%s/%d", collectionPath, candidate.ID)
		detail, _, detailErr := readRepositoryRulesetWithETag(client, path, candidate.Name)
		if detailErr == nil && isDesiredRepositoryReviewerGate(detail, defaultBranch) {
			desired = append(desired, detail)
			continue
		}
		if detailErr != nil {
			verificationErrors = append(
				verificationErrors,
				fmt.Errorf("verifying newly created reviewer gate %q: %w", candidate.Name, detailErr),
			)
		} else {
			verificationErrors = append(
				verificationErrors,
				fmt.Errorf("newly created reviewer gate %q does not enforce the strict Actions-owned approval check", candidate.Name),
			)
		}
		if candidate.Name == expectedName {
			rollbackCandidates = append(rollbackCandidates, candidate)
		} else {
			unattributed = append(unattributed, candidate)
		}
	}
	var rollbackErrors []error
	for _, ruleset := range rollbackCandidates {
		path := fmt.Sprintf("%s/%d", collectionPath, ruleset.ID)
		if rollbackErr := removeCreatedRepositoryReviewerGate(client, path, ruleset.Name); rollbackErr != nil {
			rollbackErrors = append(rollbackErrors, rollbackErr)
		}
	}
	for _, ruleset := range unattributed {
		verificationErrors = append(
			verificationErrors,
			fmt.Errorf(
				"new repository ruleset %q (ID %d) appeared during the ambiguous create and was not modified because it no longer has the expected name %q",
				ruleset.Name,
				ruleset.ID,
				expectedName,
			),
		)
	}
	if len(desired) == 1 && len(unattributed) == 0 && len(rollbackErrors) == 0 {
		return desired[0].Name, nil
	}
	if len(desired) > 1 {
		verificationErrors = append(
			verificationErrors,
			fmt.Errorf("ambiguous create produced %d strict repository reviewer gates", len(desired)),
		)
	}
	return "", errors.Join(
		fmt.Errorf("creating repository reviewer gate: %w", createErr),
		errors.Join(verificationErrors...),
		errors.Join(rollbackErrors...),
	)
}

func restoreRepositoryReviewerGate(
	client restAPI,
	path string,
	expectedCurrent repositoryRuleset,
	snapshot repositoryRuleset,
) error {
	if err := ensureRepositoryRulesetUnchanged(client, path, expectedCurrent); err != nil {
		return fmt.Errorf("reviewer gate changed before rollback: %w", err)
	}
	body, err := jsonBody(repositoryRulesetMutationPayload(snapshot))
	if err != nil {
		return err
	}
	var restored repositoryRuleset
	if err := client.Put(path, body, &restored); err != nil {
		return err
	}
	current, _, err := readRepositoryRulesetWithETag(client, path, snapshot.Name)
	if err != nil {
		return err
	}
	if !repositoryRulesetStateEqual(current, snapshot) {
		return fmt.Errorf("GitHub accepted the rollback but did not restore the prior ruleset")
	}
	return nil
}

func removeCreatedRepositoryReviewerGate(client restAPI, path, name string) error {
	created, _, err := readRepositoryRulesetWithETag(client, path, name)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("reading newly created reviewer gate %q before rollback: %w", name, err)
	}
	if err := ensureRepositoryRulesetUnchanged(client, path, created); err != nil {
		return fmt.Errorf("newly created reviewer gate %q changed before rollback: %w", name, err)
	}
	deleteErr := client.Delete(path, nil)
	var current repositoryRuleset
	lookupErr := client.Get(path, &current)
	if isNotFoundError(lookupErr) {
		return nil
	}
	if lookupErr != nil {
		return errors.Join(
			fmt.Errorf("removing newly created reviewer gate %q: %w", name, deleteErr),
			fmt.Errorf("could not verify rollback: %w", lookupErr),
		)
	}
	if deleteErr != nil {
		return fmt.Errorf("removing newly created reviewer gate %q: %w", name, deleteErr)
	}
	return fmt.Errorf("GitHub accepted deletion of newly created reviewer gate %q but it still exists", name)
}

func readRepositoryRulesetWithETag(
	client restAPI,
	path, name string,
) (repositoryRuleset, string, error) {
	var ruleset repositoryRuleset
	etag, err := client.GetWithETag(path, &ruleset)
	if err != nil {
		return repositoryRuleset{}, "", err
	}
	if etag == "" {
		return repositoryRuleset{}, "", fmt.Errorf("ruleset %q did not return an entity tag", name)
	}
	ruleset.ETag = etag
	return ruleset, etag, nil
}

func ensureRepositoryRulesetUnchanged(
	client restAPI,
	path string,
	expected repositoryRuleset,
) error {
	current, _, err := readRepositoryRulesetWithETag(client, path, expected.Name)
	if err != nil {
		return err
	}
	if current.ETag != expected.ETag || !repositoryRulesetStateEqual(current, expected) {
		return fmt.Errorf("repository ruleset changed concurrently")
	}
	return nil
}

func getRepositoryRulesetForMutation(
	client restAPI,
	owner, repo string,
	summary repositoryRuleset,
) (repositoryRuleset, error) {
	path := fmt.Sprintf("repos/%s/%s/rulesets/%d", owner, repo, summary.ID)
	detail, _, err := readRepositoryRulesetWithETag(client, path, summary.Name)
	if err != nil {
		return repositoryRuleset{}, fmt.Errorf("reading reviewer gate %q before mutation: %w", summary.Name, err)
	}
	return detail, nil
}

func validateDedicatedRepositoryReviewerGate(ruleset repositoryRuleset, defaultBranch string) error {
	if ruleset.SourceType != "" && ruleset.SourceType != "Repository" {
		return fmt.Errorf("reviewer status-check ruleset %q is not repository-managed", ruleset.Name)
	}
	if !rulesetAppliesToDefaultBranch(ruleset, defaultBranch) {
		return fmt.Errorf("reviewer status-check ruleset %q does not target the active default branch", ruleset.Name)
	}
	if ruleset.Target != "" && ruleset.Target != "branch" {
		return fmt.Errorf("reviewer status-check ruleset %q does not target branches", ruleset.Name)
	}
	if ruleset.Conditions.RepositoryID != nil || ruleset.Conditions.RepositoryName != nil {
		return fmt.Errorf("reviewer status-check ruleset %q has organization-only repository conditions", ruleset.Name)
	}
	if len(ruleset.Rules) != 1 || ruleset.Rules[0].Type != "required_status_checks" ||
		!isSingleReviewerStatusCheck(ruleset.Rules[0].Parameters) {
		return fmt.Errorf(
			"reviewer status-check ruleset %q contains unrelated requirements; separate the SFL check before updating it",
			ruleset.Name,
		)
	}
	return nil
}

func isSingleReviewerStatusCheck(parameters map[string]any) bool {
	checks, ok := parameters["required_status_checks"].([]any)
	if !ok || len(checks) != 1 {
		return false
	}
	check, ok := checks[0].(map[string]any)
	return ok && isKnownReviewerGateCheckContext(check["context"])
}

func isKnownReviewerGateCheckContext(value any) bool {
	context, ok := value.(string)
	return ok && (context == reviewerGateCheckContext || context == legacyReviewerGateCheckContext)
}

func isDesiredRepositoryReviewerGate(ruleset repositoryRuleset, defaultBranch string) bool {
	return validateDedicatedRepositoryReviewerGate(ruleset, defaultBranch) == nil &&
		hasReviewerFreshnessInterlock(ruleset)
}

func repositoryRulesetMutationPayload(ruleset repositoryRuleset) map[string]any {
	payload := map[string]any{
		"name":        ruleset.Name,
		"target":      ruleset.Target,
		"enforcement": ruleset.Enforcement,
		"conditions":  ruleset.Conditions,
		"rules":       ruleset.Rules,
	}
	if ruleset.BypassActors != nil {
		payload["bypass_actors"] = ruleset.BypassActors
	}
	return payload
}

func repositoryRulesetStateEqual(first, second repositoryRuleset) bool {
	firstJSON, firstErr := jsonBody(repositoryRulesetMutationPayload(first))
	secondJSON, secondErr := jsonBody(repositoryRulesetMutationPayload(second))
	if firstErr != nil || secondErr != nil {
		return false
	}
	firstBytes, firstErr := io.ReadAll(firstJSON)
	secondBytes, secondErr := io.ReadAll(secondJSON)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstBytes, secondBytes)
}

func cloneRepositoryRuleset(ruleset repositoryRuleset) (repositoryRuleset, error) {
	body, err := jsonBody(ruleset)
	if err != nil {
		return repositoryRuleset{}, err
	}
	var clone repositoryRuleset
	data, err := io.ReadAll(body)
	if err != nil {
		return repositoryRuleset{}, err
	}
	if err := json.Unmarshal(data, &clone); err != nil {
		return repositoryRuleset{}, err
	}
	clone.ETag = ruleset.ETag
	return clone, nil
}

func newRepositoryRulesets(before, after []repositoryRuleset) []repositoryRuleset {
	existingIDs := make(map[int64]struct{}, len(before))
	for _, ruleset := range before {
		existingIDs[ruleset.ID] = struct{}{}
	}
	var result []repositoryRuleset
	for _, ruleset := range after {
		if _, existed := existingIDs[ruleset.ID]; !existed {
			result = append(result, ruleset)
		}
	}
	return result
}

func isRepositoryManagedRuleset(ruleset repositoryRuleset) bool {
	return ruleset.SourceType == "" || ruleset.SourceType == "Repository"
}

func reviewerFreshnessRule() map[string]any {
	return map[string]any{
		"type": "required_status_checks",
		"parameters": map[string]any{
			"do_not_enforce_on_create":             false,
			"strict_required_status_checks_policy": true,
			"required_status_checks": []any{
				map[string]any{
					"context":        reviewerGateCheckContext,
					"integration_id": githubActionsAppID,
				},
			},
		},
	}
}

func mergeReviewerFreshnessRule(parameters map[string]any) map[string]any {
	merged := make(map[string]any, len(parameters)+2)
	for key, value := range parameters {
		merged[key] = value
	}
	checks, _ := parameters["required_status_checks"].([]any)
	mergedChecks := make([]any, 0, len(checks)+1)
	hasReviewerCheck := false
	for _, value := range checks {
		check, ok := value.(map[string]any)
		if ok && isKnownReviewerGateCheckContext(check["context"]) {
			updatedCheck := make(map[string]any, len(check)+1)
			for key, checkValue := range check {
				updatedCheck[key] = checkValue
			}
			updatedCheck["context"] = reviewerGateCheckContext
			updatedCheck["integration_id"] = githubActionsAppID
			mergedChecks = append(mergedChecks, updatedCheck)
			hasReviewerCheck = true
			continue
		}
		mergedChecks = append(mergedChecks, value)
	}
	if !hasReviewerCheck {
		mergedChecks = append(mergedChecks, map[string]any{
			"context":        reviewerGateCheckContext,
			"integration_id": githubActionsAppID,
		})
	}
	merged["do_not_enforce_on_create"] = false
	merged["strict_required_status_checks_policy"] = true
	merged["required_status_checks"] = mergedChecks
	return merged
}

func reviewerGateName(repo string) string {
	return fmt.Sprintf("Require SFL Reviewer Gate Runner (%s)", repo)
}

func repositoryReviewerGatePayload(repo string) map[string]any {
	return map[string]any{
		"name":        reviewerGateName(repo),
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~DEFAULT_BRANCH"},
				"exclude": []string{},
			},
		},
		"rules": []any{
			reviewerFreshnessRule(),
		},
	}
}

func parseGateOptions(args []string, stderr io.Writer) (gateOptions, error) {
	var opts gateOptions
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGateUsage(stderr) }
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

func writeGateUsage(w io.Writer) {
	fmt.Fprint(w, `Require the strict Actions-owned SFL approval check before pull requests can merge.

The reviewer deployment must already be merged. New repositories remain
advisory-only unless this command is run explicitly.

Usage:
  gh sfl gate [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
`)
}
