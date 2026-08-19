package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

var reviewerRequiredVariables = []string{}
var reviewerRequiredSecrets = []string{}

type reviewerRolloutHealth struct {
	DefaultBranch    string
	ActionsEnabled   bool
	ActionsIssues    []string
	AppNotice        string
	MissingVariables []string
	MissingSecrets   []string
}

func (health reviewerRolloutHealth) issues() []string {
	var issues []string
	if strings.TrimSpace(health.DefaultBranch) == "" {
		issues = append(issues, "repository has no default branch")
	}
	if !health.ActionsEnabled {
		issues = append(issues, "GitHub Actions is disabled")
	}
	issues = append(issues, health.ActionsIssues...)
	for _, name := range health.MissingVariables {
		issues = append(issues, "missing Actions variable "+name)
	}
	for _, name := range health.MissingSecrets {
		issues = append(issues, "missing Actions secret "+name)
	}
	sort.Strings(issues)
	return issues
}

func inspectReviewerRollout(owner, repo string) (reviewerRolloutHealth, error) {
	client, err := newRESTClient()
	if err != nil {
		return reviewerRolloutHealth{}, fmt.Errorf("creating GitHub REST client: %w", err)
	}
	return inspectReviewerRolloutWithClient(client, owner, repo)
}

func inspectReviewerRolloutWithClient(
	client restAPI,
	owner, repo string,
) (reviewerRolloutHealth, error) {
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", owner, repo), &repository); err != nil {
		return reviewerRolloutHealth{}, fmt.Errorf("reading repository metadata: %w", err)
	}

	var actionsPermissions struct {
		Enabled        bool   `json:"enabled"`
		AllowedActions string `json:"allowed_actions"`
	}
	if err := client.Get(
		fmt.Sprintf("repos/%s/%s/actions/permissions", owner, repo),
		&actionsPermissions,
	); err != nil {
		return reviewerRolloutHealth{}, fmt.Errorf("reading Actions permissions: %w", err)
	}
	actionsIssues, err := reviewerActionsPolicyIssues(client, owner, repo, actionsPermissions.AllowedActions)
	if err != nil {
		return reviewerRolloutHealth{}, err
	}

	return reviewerRolloutHealth{
		DefaultBranch:  repository.DefaultBranch,
		ActionsEnabled: actionsPermissions.Enabled,
		ActionsIssues:  actionsIssues,
		AppNotice: "The Codex GitHub App installation is verified by an authenticated review smoke test; " +
			"GitHub CLI cannot inspect the owner's ChatGPT subscription connection",
	}, nil
}

func reviewerActionsPolicyIssues(
	client restAPI,
	owner, repo, allowedActions string,
) ([]string, error) {
	switch allowedActions {
	case "all":
		return nil, nil
	case "local_only":
		return []string{"Actions policy allows only local actions; reviewer requires the pinned GitHub-owned actions/github-script action"}, nil
	case "selected":
		var selected struct {
			GitHubOwnedAllowed bool `json:"github_owned_allowed"`
		}
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/actions/permissions/selected-actions", owner, repo),
			&selected,
		); err != nil {
			return nil, fmt.Errorf("reading selected Actions policy: %w", err)
		}
		if selected.GitHubOwnedAllowed {
			return nil, nil
		}
		return []string{"selected Actions policy must allow GitHub-owned actions for the reviewer"}, nil
	case "":
		return []string{"Actions policy did not report allowed_actions"}, nil
	default:
		return []string{"unsupported Actions allowed_actions policy " + allowedActions}, nil
	}
}

func repositoryActionNames(
	client restAPI,
	owner, repo, kind string,
) (map[string]struct{}, error) {
	if kind != "variables" && kind != "secrets" {
		return nil, fmt.Errorf("unsupported Actions metadata kind %q", kind)
	}
	names := make(map[string]struct{})
	for pageNumber := 1; ; pageNumber++ {
		var response struct {
			Variables []struct {
				Name string `json:"name"`
			} `json:"variables"`
			Secrets []struct {
				Name string `json:"name"`
			} `json:"secrets"`
		}
		requestPath := fmt.Sprintf(
			"repos/%s/%s/actions/%s?per_page=100&page=%d",
			owner,
			repo,
			kind,
			pageNumber,
		)
		if err := client.Get(requestPath, &response); err != nil {
			return nil, err
		}
		pageCount := 0
		if kind == "variables" {
			pageCount = len(response.Variables)
			for _, variable := range response.Variables {
				names[variable.Name] = struct{}{}
			}
		} else {
			pageCount = len(response.Secrets)
			for _, secret := range response.Secrets {
				names[secret.Name] = struct{}{}
			}
		}
		if pageCount < 100 {
			return names, nil
		}
	}
}

func missingNames(required []string, existing map[string]struct{}) []string {
	var missing []string
	for _, name := range required {
		if _, ok := existing[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func assertReviewerRolloutReady(owner, repo string, stdout io.Writer) error {
	health, err := inspectReviewerRollout(owner, repo)
	if err != nil {
		return fmt.Errorf("reviewer rollout preflight failed: %w", err)
	}
	issues := health.issues()
	if len(issues) > 0 {
		return fmt.Errorf("reviewer rollout preflight failed for %s/%s:\n - %s", owner, repo, strings.Join(issues, "\n - "))
	}
	fmt.Fprintf(stdout, "  Reviewer rollout preflight: default branch %s and Actions policy ready ✓\n", health.DefaultBranch)
	fmt.Fprintln(stdout, "  Codex App connection: verify with a current-head review after deployment")
	fmt.Fprintln(stdout)
	return nil
}

func workflowsIncludeReviewer(workflowSets ...[]string) bool {
	for _, workflows := range workflowSets {
		for _, workflow := range workflows {
			if strings.HasPrefix(workflow, "sfl-pr-review") {
				return true
			}
		}
	}
	return false
}
