package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

type gatePutPlan struct {
	apply      bool
	err        error
	mutate     func(*repositoryRuleset)
	concurrent *repositoryRuleset
}

type gateREST struct {
	rulesets   map[int64]repositoryRuleset
	etags      map[int64]string
	nextID     int64
	postErr    error
	postApply  bool
	postMutate func(*repositoryRuleset)
	putPlans   []gatePutPlan
	putIndex   int
	posts      []string
	puts       []string
	putETags   []string
	deletes    []string
	deleteTags []string
}

func newGateREST(rulesets ...repositoryRuleset) *gateREST {
	client := &gateREST{
		rulesets:  make(map[int64]repositoryRuleset),
		etags:     make(map[int64]string),
		nextID:    100,
		postApply: true,
	}
	for _, ruleset := range rulesets {
		client.rulesets[ruleset.ID] = ruleset
		client.etags[ruleset.ID] = `"v1"`
		if ruleset.ID >= client.nextID {
			client.nextID = ruleset.ID + 1
		}
	}
	return client
}

func (c *gateREST) Get(path string, response interface{}) error {
	if strings.Contains(path, "/rulesets?") {
		var rulesets []repositoryRuleset
		for _, ruleset := range c.rulesets {
			rulesets = append(rulesets, ruleset)
		}
		slices.SortFunc(rulesets, func(first, second repositoryRuleset) int {
			return int(first.ID - second.ID)
		})
		return decodeTestResponse(response, rulesets)
	}
	id, ok := rulesetIDFromPath(path)
	if !ok {
		return fmt.Errorf("unexpected GET %s", path)
	}
	ruleset, found := c.rulesets[id]
	if !found {
		return &api.HTTPError{StatusCode: http.StatusNotFound}
	}
	return decodeTestResponse(response, ruleset)
}

func (c *gateREST) GetWithETag(path string, response interface{}) (string, error) {
	if err := c.Get(path, response); err != nil {
		return "", err
	}
	id, _ := rulesetIDFromPath(path)
	return c.etags[id], nil
}

func (c *gateREST) Post(path string, body io.Reader, response interface{}) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	c.posts = append(c.posts, path+"\n"+string(data))
	var ruleset repositoryRuleset
	if err := json.Unmarshal(data, &ruleset); err != nil {
		return err
	}
	ruleset.ID = c.nextID
	ruleset.SourceType = "Repository"
	c.nextID++
	if c.postMutate != nil {
		c.postMutate(&ruleset)
	}
	if c.postApply {
		c.rulesets[ruleset.ID] = ruleset
		c.etags[ruleset.ID] = `"v1"`
	}
	if c.postErr != nil {
		return c.postErr
	}
	return decodeTestResponse(response, ruleset)
}

func (c *gateREST) Put(path string, body io.Reader, response interface{}) error {
	return c.put(path, body, "", response)
}

func (c *gateREST) PutIfMatch(path string, body io.Reader, etag string, response interface{}) error {
	c.putETags = append(c.putETags, etag)
	return c.put(path, body, etag, response)
}

func (c *gateREST) put(path string, body io.Reader, etag string, response interface{}) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	c.puts = append(c.puts, path+"\n"+string(data))
	id, ok := rulesetIDFromPath(path)
	if !ok {
		return fmt.Errorf("unexpected PUT %s", path)
	}
	plan := gatePutPlan{apply: true}
	if c.putIndex < len(c.putPlans) {
		plan = c.putPlans[c.putIndex]
	}
	c.putIndex++
	if plan.concurrent != nil {
		concurrent := *plan.concurrent
		concurrent.ID = id
		c.rulesets[id] = concurrent
		c.etags[id] = fmt.Sprintf(`"v%d"`, c.putIndex+1)
		return &api.HTTPError{StatusCode: http.StatusPreconditionFailed}
	}
	if etag != "" && etag != c.etags[id] {
		return &api.HTTPError{StatusCode: http.StatusPreconditionFailed}
	}
	var ruleset repositoryRuleset
	if err := json.Unmarshal(data, &ruleset); err != nil {
		return err
	}
	ruleset.ID = id
	ruleset.SourceType = "Repository"
	if plan.mutate != nil {
		plan.mutate(&ruleset)
	}
	if plan.apply {
		c.rulesets[id] = ruleset
		c.etags[id] = fmt.Sprintf(`"v%d"`, c.putIndex+1)
	}
	if plan.err != nil {
		return plan.err
	}
	return decodeTestResponse(response, ruleset)
}

func (c *gateREST) Patch(string, io.Reader, interface{}) error {
	return errors.New("unexpected PATCH")
}

func (c *gateREST) Delete(path string, response interface{}) error {
	return c.DeleteIfMatch(path, "", response)
}

func (c *gateREST) DeleteIfMatch(path, etag string, _ interface{}) error {
	c.deletes = append(c.deletes, path)
	c.deleteTags = append(c.deleteTags, etag)
	id, ok := rulesetIDFromPath(path)
	if !ok {
		return fmt.Errorf("unexpected DELETE %s", path)
	}
	if etag != "" && etag != c.etags[id] {
		return &api.HTTPError{StatusCode: http.StatusPreconditionFailed}
	}
	delete(c.rulesets, id)
	delete(c.etags, id)
	return nil
}

func rulesetIDFromPath(path string) (int64, bool) {
	var id int64
	_, err := fmt.Sscanf(path[strings.LastIndex(path, "/")+1:], "%d", &id)
	return id, err == nil
}

func TestEnsureRepositoryReviewerGateCreatesRepositoryRuleset(t *testing.T) {
	client := newGateREST()
	var output bytes.Buffer
	err := ensureRepositoryReviewerGate(client, "owner", "repo", "main", nil, nil, &output)
	if err != nil {
		t.Fatalf("ensureRepositoryReviewerGate() unexpected error: %v", err)
	}
	if len(client.posts) != 1 || !strings.HasPrefix(client.posts[0], "repos/owner/repo/rulesets\n") {
		t.Fatalf("create requests = %v", client.posts)
	}
	if strings.Contains(client.posts[0], "orgs/") || strings.Contains(client.posts[0], `"type":"workflows"`) {
		t.Fatalf("create used organization-only gate fields: %s", client.posts[0])
	}
	if len(client.rulesets) != 1 {
		t.Fatalf("created rulesets = %+v", client.rulesets)
	}
	for _, ruleset := range client.rulesets {
		if !isDesiredRepositoryReviewerGate(ruleset, "main") {
			t.Fatalf("created gate is not strict: %+v", ruleset)
		}
	}
}

func TestEnsureRepositoryReviewerGateRerunIsWriteFree(t *testing.T) {
	ruleset := strictReviewerStatusRuleset(21, "Required reviewer")
	client := newGateREST(ruleset)
	err := ensureRepositoryReviewerGate(
		client,
		"owner",
		"repo",
		"main",
		[]repositoryRuleset{ruleset},
		[]repositoryRuleset{ruleset},
		io.Discard,
	)
	if err != nil {
		t.Fatalf("ensureRepositoryReviewerGate() unexpected error: %v", err)
	}
	if len(client.posts)+len(client.puts)+len(client.deletes) != 0 {
		t.Fatalf("idempotent rerun wrote state: %+v", client)
	}
}

func TestEnsureRepositoryReviewerGateRefusesStaleSharedRule(t *testing.T) {
	ruleset := legacyReviewerRuleset(21, "Shared reviewer")
	ruleset.Rules = append(ruleset.Rules, struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{Type: "required_signatures", Parameters: map[string]any{}})
	client := newGateREST(ruleset)
	err := ensureRepositoryReviewerGate(
		client,
		"owner",
		"repo",
		"main",
		[]repositoryRuleset{ruleset},
		[]repositoryRuleset{ruleset},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "contains unrelated requirements") {
		t.Fatalf("ensureRepositoryReviewerGate() error = %v", err)
	}
	if len(client.posts)+len(client.puts)+len(client.deletes) != 0 {
		t.Fatalf("shared rule was mutated: %+v", client)
	}
}

func TestEnsureRepositoryReviewerGateRefusesInheritedRuleWithoutWriting(t *testing.T) {
	ruleset := legacyReviewerRuleset(21, "Inherited reviewer")
	ruleset.SourceType = "Organization"
	client := newGateREST(ruleset)
	err := ensureRepositoryReviewerGate(
		client,
		"owner",
		"repo",
		"main",
		[]repositoryRuleset{ruleset},
		[]repositoryRuleset{ruleset},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "inherited from Organization") {
		t.Fatalf("ensureRepositoryReviewerGate() error = %v", err)
	}
	if len(client.posts)+len(client.puts)+len(client.deletes) != 0 {
		t.Fatalf("inherited rule was mutated: %+v", client)
	}
}

func TestUpdateRepositoryReviewerGateRechecksFreshStateBeforeWriting(t *testing.T) {
	summary := legacyReviewerRuleset(21, "Required reviewer")
	current := strictReviewerStatusRuleset(21, summary.Name)
	client := newGateREST(current)
	name, err := updateRepositoryReviewerGate(client, "owner", "repo", "main", summary)
	if err != nil {
		t.Fatalf("updateRepositoryReviewerGate() unexpected error: %v", err)
	}
	if name != current.Name {
		t.Fatalf("updateRepositoryReviewerGate() name = %q", name)
	}
	if len(client.puts) != 0 {
		t.Fatalf("authoritatively fresh gate was rewritten: %v", client.puts)
	}
}

func TestUpdateRepositoryReviewerGatePreservesConcurrentChange(t *testing.T) {
	snapshot := legacyReviewerRuleset(21, "Required reviewer")
	concurrent := snapshot
	concurrent.Name = "Administrator changed this rule"
	concurrent.BypassActors = []any{map[string]any{"actor_id": 99}}
	client := newGateREST(snapshot)
	client.putPlans = []gatePutPlan{{concurrent: &concurrent}}
	_, err := updateRepositoryReviewerGate(client, "owner", "repo", "main", snapshot)
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("updateRepositoryReviewerGate() error = %v", err)
	}
	if len(client.puts) != 1 || !repositoryRulesetStateEqual(client.rulesets[21], concurrent) {
		t.Fatalf("concurrent state was overwritten: %+v", client.rulesets[21])
	}
}

func TestUpdateRepositoryReviewerGateRestoresAfterAmbiguousFailure(t *testing.T) {
	snapshot := legacyReviewerRuleset(21, "Required reviewer")
	client := newGateREST(snapshot)
	client.putPlans = []gatePutPlan{
		{
			apply: true,
			err:   errors.New("connection reset after write"),
			mutate: func(ruleset *repositoryRuleset) {
				ruleset.Conditions.RefName.Include = []string{"refs/heads/release"}
			},
		},
		{apply: true},
	}
	_, err := updateRepositoryReviewerGate(client, "owner", "repo", "main", snapshot)
	if err == nil || !strings.Contains(err.Error(), "restored its prior state") {
		t.Fatalf("updateRepositoryReviewerGate() error = %v", err)
	}
	if len(client.puts) != 2 || !repositoryRulesetStateEqual(client.rulesets[21], snapshot) {
		t.Fatalf("prior state was not restored: %+v", client.rulesets[21])
	}
}

func TestUpdateRepositoryReviewerGateReportsRollbackFailure(t *testing.T) {
	snapshot := legacyReviewerRuleset(21, "Required reviewer")
	client := newGateREST(snapshot)
	client.putPlans = []gatePutPlan{
		{
			apply: true,
			err:   errors.New("connection reset after write"),
			mutate: func(ruleset *repositoryRuleset) {
				ruleset.Conditions.RefName.Include = []string{"refs/heads/release"}
			},
		},
		{apply: false, err: errors.New("rollback denied")},
	}
	_, err := updateRepositoryReviewerGate(client, "owner", "repo", "main", snapshot)
	if err == nil || !strings.Contains(err.Error(), "restoring the prior reviewer gate") ||
		!strings.Contains(err.Error(), "rollback denied") {
		t.Fatalf("updateRepositoryReviewerGate() error = %v", err)
	}
	if len(client.puts) != 2 || repositoryRulesetStateEqual(client.rulesets[21], snapshot) {
		t.Fatalf("rollback failure was not retained for diagnosis: %+v", client.rulesets[21])
	}
}

func TestCreateRepositoryReviewerGateAcceptsAppliedResponseError(t *testing.T) {
	client := newGateREST()
	client.postErr = errors.New("connection reset after create")
	name, err := createRepositoryReviewerGate(client, "owner", "repo", "main", nil)
	if err != nil {
		t.Fatalf("createRepositoryReviewerGate() unexpected error: %v", err)
	}
	if name != reviewerGateName("repo") || len(client.rulesets) != 1 {
		t.Fatalf("ambiguous create result = %q, %+v", name, client.rulesets)
	}
}

func TestCreateRepositoryReviewerGateRollsBackMalformedAmbiguousCreate(t *testing.T) {
	client := newGateREST()
	client.postErr = errors.New("connection reset after create")
	client.postMutate = func(ruleset *repositoryRuleset) {
		ruleset.Conditions.RefName.Include = []string{"refs/heads/release"}
	}
	_, err := createRepositoryReviewerGate(client, "owner", "repo", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "connection reset after create") {
		t.Fatalf("createRepositoryReviewerGate() error = %v", err)
	}
	if len(client.rulesets) != 0 || len(client.deletes) != 1 || len(client.deleteTags) != 1 {
		t.Fatalf("malformed create was not rolled back: %+v", client)
	}
}
