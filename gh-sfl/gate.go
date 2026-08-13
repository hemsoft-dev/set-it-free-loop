package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type gateOptions struct {
	repo string
}

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
	requiredWorkflows, legacyStatusChecks := findAllReviewerGates(
		rulesets,
		repository.DefaultBranch,
		repository.ID,
	)
	legacyStatusDetails := make([]repositoryRuleset, 0, len(legacyStatusChecks))
	for _, legacyStatusCheck := range legacyStatusChecks {
		detail, detailErr := getRulesetForMutation(
			client,
			owner,
			repo,
			legacyStatusCheck,
		)
		if detailErr != nil {
			return detailErr
		}
		if err := validateDedicatedLegacyReviewerGate(detail); err != nil {
			return err
		}
		legacyStatusDetails = append(legacyStatusDetails, detail)
	}
	legacyStatusChecks = legacyStatusDetails
	upgradedWorkflowDetails, err := upgradeStaleReviewerRulesets(
		client,
		owner,
		repo,
		requiredWorkflows,
		repository.ID,
		repository.DefaultBranch,
	)
	if err != nil {
		return err
	}
	for _, detail := range upgradedWorkflowDetails {
		for index := range requiredWorkflows {
			if requiredWorkflows[index].ID == detail.ID {
				requiredWorkflows[index].Name = detail.Name
			}
		}
	}
	freshnessUpgraded := len(upgradedWorkflowDetails) > 0
	if len(legacyStatusChecks) > 0 {
		var requiredWorkflow *repositoryRuleset
		if len(requiredWorkflows) > 0 {
			requiredWorkflow = &requiredWorkflows[0]
		} else {
			createdName, createErr := createReviewerRuleset(
				client,
				owner,
				repo,
				repository.ID,
				repository.DefaultBranch,
			)
			if createErr != nil {
				return createErr
			}
			requiredWorkflow = &repositoryRuleset{Name: createdName}
		}
		for _, legacyStatusCheck := range legacyStatusChecks {
			if err := removeDedicatedLegacyReviewerGate(
				client,
				owner,
				repo,
				legacyStatusCheck,
			); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "✓ Replaced %d legacy status-check ruleset(s) with required workflow %q\n",
			len(legacyStatusChecks), requiredWorkflow.Name)
		return nil
	}
	if len(requiredWorkflows) > 0 {
		if freshnessUpgraded {
			fmt.Fprintf(
				stdout,
				"✓ Added strict base freshness to %d required SFL reviewer workflow ruleset(s)\n",
				len(upgradedWorkflowDetails),
			)
			return nil
		}
		fmt.Fprintf(stdout, "✓ SFL reviewer workflow is already required by %q\n", requiredWorkflows[0].Name)
		return nil
	}

	created, err := createReviewerRuleset(
		client,
		owner,
		repo,
		repository.ID,
		repository.DefaultBranch,
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ Required SFL reviewer workflow enabled for %s/%s (%s)\n", owner, repo, created)
	return nil
}

func upgradeStaleReviewerRulesets(
	client restAPI,
	owner, repo string,
	requiredWorkflows []repositoryRuleset,
	repositoryID int64,
	defaultBranch string,
) ([]repositoryRuleset, error) {
	staleWorkflowDetails, err := collectStaleReviewerRulesets(
		client,
		owner,
		repo,
		requiredWorkflows,
		repositoryID,
		defaultBranch,
	)
	if err != nil {
		return nil, err
	}
	// Re-fetch and validate the complete candidate set before the first write.
	// This is intentionally separate from discovery: it prevents partial
	// mutation when any ruleset became shared, retargeted, or already current.
	// Each subsequent PUT is also protected by the freshly read ETag.
	staleWorkflowDetails, err = collectStaleReviewerRulesets(
		client,
		owner,
		repo,
		staleWorkflowDetails,
		repositoryID,
		defaultBranch,
	)
	if err != nil {
		return nil, err
	}
	for index := range staleWorkflowDetails {
		updatedName, updateErr := updateReviewerRuleset(
			client,
			owner,
			staleWorkflowDetails[index],
		)
		if updateErr != nil {
			return nil, updateErr
		}
		staleWorkflowDetails[index].Name = updatedName
	}
	return staleWorkflowDetails, nil
}

func collectStaleReviewerRulesets(
	client restAPI,
	owner, repo string,
	requiredWorkflows []repositoryRuleset,
	repositoryID int64,
	defaultBranch string,
) ([]repositoryRuleset, error) {
	staleWorkflowDetails := make([]repositoryRuleset, 0, len(requiredWorkflows))
	for _, requiredWorkflow := range requiredWorkflows {
		detail, detailErr := getRulesetForMutation(
			client,
			owner,
			repo,
			requiredWorkflow,
		)
		if detailErr != nil {
			return nil, detailErr
		}
		if hasReviewerFreshnessInterlock(detail) {
			continue
		}
		if validateErr := validateDedicatedReviewerWorkflowGate(
			detail,
			owner,
			repo,
			repositoryID,
			defaultBranch,
		); validateErr != nil {
			return nil, fmt.Errorf(
				"cannot add strict base freshness to reviewer gate: %w",
				validateErr,
			)
		}
		staleWorkflowDetails = append(staleWorkflowDetails, detail)
	}
	return staleWorkflowDetails, nil
}

func createReviewerRuleset(
	client restAPI,
	owner, repo string,
	repositoryID int64,
	defaultBranch string,
) (string, error) {
	body, err := jsonBody(reviewerRulesetPayload(repo, repositoryID, defaultBranch))
	if err != nil {
		return "", fmt.Errorf("encoding reviewer ruleset: %w", err)
	}
	var created repositoryRuleset
	if err := client.Post(fmt.Sprintf("orgs/%s/rulesets", owner), body, &created); err != nil {
		return "", fmt.Errorf(
			"creating organization reviewer ruleset (requires organization-owner access and an admin:org token): %w",
			err,
		)
	}
	return created.Name, nil
}

func updateReviewerRuleset(
	client restAPI,
	owner string,
	ruleset repositoryRuleset,
) (string, error) {
	if ruleset.ID == 0 {
		return "", fmt.Errorf("required reviewer workflow ruleset is missing its ID")
	}
	body, err := jsonBody(reviewerRulesetUpdatePayload(ruleset))
	if err != nil {
		return "", fmt.Errorf("encoding reviewer ruleset update: %w", err)
	}
	var updated repositoryRuleset
	if ruleset.ETag == "" {
		return "", fmt.Errorf(
			"required reviewer workflow ruleset %q has no entity tag; refusing an unsafe update",
			ruleset.Name,
		)
	}
	if err := client.PutIfMatch(
		fmt.Sprintf("orgs/%s/rulesets/%d", owner, ruleset.ID),
		body,
		ruleset.ETag,
		&updated,
	); err != nil {
		return "", fmt.Errorf(
			"updating organization reviewer ruleset (requires organization-owner access and an admin:org token): %w",
			err,
		)
	}
	if updated.Name == "" {
		return ruleset.Name, nil
	}
	return updated.Name, nil
}

func reviewerRulesetUpdatePayload(ruleset repositoryRuleset) map[string]any {
	conditions := map[string]any{
		"ref_name": map[string]any{
			"include": ruleset.Conditions.RefName.Include,
			"exclude": ruleset.Conditions.RefName.Exclude,
		},
	}
	if ruleset.Conditions.RepositoryID != nil {
		conditions["repository_id"] = map[string]any{
			"repository_ids": ruleset.Conditions.RepositoryID.RepositoryIDs,
		}
	}
	if ruleset.Conditions.RepositoryName != nil {
		conditions["repository_name"] = map[string]any{
			"include":   ruleset.Conditions.RepositoryName.Include,
			"exclude":   ruleset.Conditions.RepositoryName.Exclude,
			"protected": ruleset.Conditions.RepositoryName.Protected,
		}
	}
	rules := make([]any, 0, len(ruleset.Rules)+1)
	freshnessMerged := false
	for _, rule := range ruleset.Rules {
		if rule.Type == "required_status_checks" {
			rules = append(rules, map[string]any{
				"type":       rule.Type,
				"parameters": mergeReviewerFreshnessRule(rule.Parameters),
			})
			freshnessMerged = true
			continue
		}
		rules = append(rules, map[string]any{
			"type":       rule.Type,
			"parameters": rule.Parameters,
		})
	}
	if !freshnessMerged {
		rules = append(rules, reviewerFreshnessRule())
	}
	return map[string]any{
		"name":          ruleset.Name,
		"target":        ruleset.Target,
		"enforcement":   ruleset.Enforcement,
		"bypass_actors": ruleset.BypassActors,
		"conditions":    conditions,
		"rules":         rules,
	}
}

func reviewerFreshnessRule() map[string]any {
	return map[string]any{
		"type": "required_status_checks",
		"parameters": map[string]any{
			"do_not_enforce_on_create":             false,
			"strict_required_status_checks_policy": true,
			"required_status_checks": []any{
				map[string]any{
					"context":        "SFL Reviewer Approval",
					"integration_id": 15368,
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
	mergedChecks := append([]any(nil), checks...)
	hasReviewerCheck := false
	for _, value := range mergedChecks {
		check, ok := value.(map[string]any)
		if ok &&
			check["context"] == "SFL Reviewer Approval" &&
			numericIDEquals(check["integration_id"], 15368) {
			hasReviewerCheck = true
			break
		}
	}
	if !hasReviewerCheck {
		mergedChecks = append(mergedChecks, map[string]any{
			"context":        "SFL Reviewer Approval",
			"integration_id": 15368,
		})
	}
	merged["do_not_enforce_on_create"] = false
	merged["strict_required_status_checks_policy"] = true
	merged["required_status_checks"] = mergedChecks
	return merged
}

func removeDedicatedLegacyReviewerGate(
	client restAPI,
	owner, repo string,
	ruleset repositoryRuleset,
) error {
	if err := validateDedicatedLegacyReviewerGate(ruleset); err != nil {
		return err
	}
	if ruleset.ETag == "" {
		return fmt.Errorf(
			"legacy reviewer ruleset %q has no entity tag; refusing an unsafe deletion",
			ruleset.Name,
		)
	}
	if err := client.DeleteIfMatch(
		fmt.Sprintf("repos/%s/%s/rulesets/%d", owner, repo, ruleset.ID),
		ruleset.ETag,
		nil,
	); err != nil {
		return fmt.Errorf("removing legacy reviewer ruleset %q: %w", ruleset.Name, err)
	}
	return nil
}

func validateDedicatedLegacyReviewerGate(ruleset repositoryRuleset) error {
	if ruleset.SourceType != "" && ruleset.SourceType != "Repository" {
		return fmt.Errorf(
			"ruleset %q is inherited and cannot be migrated at repository scope",
			ruleset.Name,
		)
	}
	if !isDedicatedLegacyReviewerGate(ruleset) {
		return fmt.Errorf(
			"ruleset %q combines the legacy SFL status check with other requirements; remove only that status check before enabling the required workflow",
			ruleset.Name,
		)
	}
	return nil
}

func isDedicatedLegacyReviewerGate(ruleset repositoryRuleset) bool {
	if len(ruleset.Rules) != 1 || ruleset.Rules[0].Type != "required_status_checks" {
		return false
	}
	checks, ok := ruleset.Rules[0].Parameters["required_status_checks"].([]any)
	if !ok || len(checks) != 1 {
		return false
	}
	check, ok := checks[0].(map[string]any)
	return ok && check["context"] == "SFL Reviewer Approval"
}

func reviewerRulesetPayload(repo string, repositoryID int64, defaultBranch string) map[string]any {
	return map[string]any{
		"name":        fmt.Sprintf("Require SFL Reviewer Approval (%s)", repo),
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~DEFAULT_BRANCH"},
				"exclude": []string{},
			},
			"repository_id": map[string]any{
				"repository_ids": []int64{repositoryID},
			},
		},
		"rules": []any{
			map[string]any{
				"type": "workflows",
				"parameters": map[string]any{
					"do_not_enforce_on_create": false,
					"workflows": []any{
						map[string]any{
							"path":          ".github/workflows/sfl-pr-review-auto.yml",
							"ref":           "refs/heads/" + defaultBranch,
							"repository_id": repositoryID,
						},
					},
				},
			},
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
	fmt.Fprint(w, `Require the trusted SFL reviewer workflow before pull requests can merge.

The reviewer deployment must already be merged. New repositories remain
advisory-only unless this command is run explicitly.

Usage:
  gh sfl gate [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
`)
}
