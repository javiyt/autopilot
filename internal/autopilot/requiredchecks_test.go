package autopilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeWorkflow(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestScanWorkflowProducers(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "test.yml", `name: Test
on:
  push:
    branches: [main]
  pull_request:
jobs:
  check:
    name: Vet + test + build
    runs-on: ubuntu-latest
    steps:
      - run: go vet ./...
  cross:
    runs-on: ubuntu-latest
    steps:
      - run: go build ./...
  cross-arm64:
    name: Cross-build linux/arm64 (Raspberry Pi target)
    runs-on: ubuntu-latest
    steps:
      - run: echo arm64
`)
	writeWorkflow(t, dir, "release.yml", `name: SemVer Release
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo release
`)
	writeWorkflow(t, dir, "dispatch.yml", `on: [push, workflow_dispatch]
jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - run: echo smoke
`)
	writeWorkflow(t, dir, "notes.md", "not a workflow")

	got := scanWorkflowProducers(dir)
	if len(got) != 3 {
		t.Fatalf("want 3 producers, got %d: %+v", len(got), got)
	}
	byFile := map[string]workflowProducer{}
	for _, p := range got {
		byFile[p.File] = p
	}
	test := byFile["test.yml"]
	if test.Name != "Test" {
		t.Errorf("test.yml name = %q", test.Name)
	}
	if want := []string{"pull_request", "push"}; !reflect.DeepEqual(test.Triggers, want) {
		t.Errorf("test.yml triggers = %v, want %v", test.Triggers, want)
	}
	if want := []string{"Cross-build linux/arm64 (Raspberry Pi target)", "Vet + test + build", "cross"}; !reflect.DeepEqual(test.Jobs, want) {
		t.Errorf("test.yml jobs = %v, want %v", test.Jobs, want)
	}
	if !test.produces("Vet + test + build") || test.produces("test") {
		t.Errorf("produces() mismatch: %+v", test)
	}
	release := byFile["release.yml"]
	if release.hasTrigger("pull_request") || !release.hasTrigger("push") {
		t.Errorf("release.yml triggers = %v", release.Triggers)
	}
	disp := byFile["dispatch.yml"]
	if !disp.hasTrigger("workflow_dispatch") || !disp.hasTrigger("push") {
		t.Errorf("dispatch.yml triggers = %v", disp.Triggers)
	}
	if disp.Name != "dispatch" {
		t.Errorf("workflow without name: use file base name, got %q", disp.Name)
	}
	if !disp.produces("dispatch") || !disp.produces("smoke") {
		t.Errorf("summary-run and job producers missing: %+v", disp)
	}
}

func TestScanWorkflowProducersMissingDir(t *testing.T) {
	if got := scanWorkflowProducers(t.TempDir()); got != nil {
		t.Fatalf("want nil for missing workflows dir, got %+v", got)
	}
}

func TestParseRulesetRequiredContexts(t *testing.T) {
	// GitHub returns either {rulesets:[...]} or a bare array; both forms are
	// handled. "target" is the branch name; only active rulesets targeting
	// the base contribute; rule parameters may be null.
	doc := `{"rulesets":[
		{"id":1,"name":"main","target":"main","enforcement":"active",
		 "rules":[
		   {"type":"deletion","parameters":null},
		   {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"},{"context":"Cross-build","integration_id":15368}]}},
		   {"type":"pull_request","parameters":{"required_approving_review_count":0}}
		 ]},
		{"id":2,"name":"dev","target":"dev","enforcement":"active",
		 "rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"dev-only"}]}}]},
		{"id":3,"name":"draft","target":"main","enforcement":"evaluate",
		 "rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"draft-only"}]}}]}
	]}`
	got, err := parseRulesetRequiredContexts([]byte(doc), "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Cross-build", "test"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("object form: got %v, want %v", got, want)
	}

	arrayForm := `[{
		"id":1,"name":"main","target":"main","enforcement":"active",
		"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Go check"}]}}]
	}]`
	got, err = parseRulesetRequiredContexts([]byte(arrayForm), "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Go check"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("array form: got %v, want %v", got, want)
	}

	// No active ruleset for the base branch: empty, no error.
	got, err = parseRulesetRequiredContexts([]byte(doc), "release")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated base: got %v, want empty", got)
	}

	if _, err := parseRulesetRequiredContexts([]byte("not json"), "main"); err == nil {
		t.Fatal("want error for malformed JSON")
	}
}

func TestParseProtectionRequiredContexts(t *testing.T) {
	got, err := parseProtectionRequiredContexts([]byte(`{"required_status_checks":{"contexts":["test","lint"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"test", "lint"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got, err = parseProtectionRequiredContexts([]byte(`{"required_status_checks":null}`))
	if err != nil || got != nil {
		t.Fatalf("null protection: got %v err %v", got, err)
	}
}

func TestParseCheckRunContexts(t *testing.T) {
	got, err := parseCheckRunContexts([]byte(`{"total_count":2,"check_runs":[{"name":"Vet + test + build"},{"name":"Cross-build"},{"name":""}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got["Vet + test + build"] || !got["Cross-build"] || got[""] || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestComputeCheckGaps(t *testing.T) {
	producers := []workflowProducer{
		{File: "test.yml", Name: "Test", Jobs: []string{"Vet + test + build", "Cross-build"}, Triggers: []string{"pull_request", "push"}},
		{File: "smoke.yml", Name: "Smoke", Jobs: []string{"Smoke"}, Triggers: []string{"push", "workflow_dispatch"}},
	}
	reported := map[string]bool{"Vet + test + build": true, "Cross-build": true}
	gaps := computeCheckGaps([]string{"test", "Vet + test + build", "Smoke", "Cross-build", "lint"}, reported, producers)
	if len(gaps) != 3 {
		t.Fatalf("want 3 gaps, got %+v", gaps)
	}
	// Stable sorted order: Smoke, lint, test
	if gaps[0].Context != "Smoke" || gaps[1].Context != "lint" || gaps[2].Context != "test" {
		t.Fatalf("gap order: %+v", gaps)
	}
	if gaps[0].stale() || !gaps[0].dispatchable() {
		t.Errorf("Smoke should be dispatchable: %+v", gaps[0])
	}
	if !gaps[1].stale() || gaps[1].Producer != nil {
		t.Errorf("lint should be stale: %+v", gaps[1])
	}
	if !gaps[2].stale() {
		t.Errorf("test should be stale: %+v", gaps[2])
	}
	if got := computeCheckGaps([]string{"Cross-build"}, reported, producers); len(got) != 0 {
		t.Fatalf("fully reported: got gaps %+v", got)
	}
}

func TestNewRequiredCheckSet(t *testing.T) {
	producers := []workflowProducer{
		{File: "test.yml", Name: "Test", Jobs: []string{"Vet + test + build", "Cross-build", "Cross-build linux/arm64 (Raspberry Pi target)"}, Triggers: []string{"pull_request", "push"}},
		{File: "release.yml", Name: "Release", Jobs: []string{"Release job"}, Triggers: []string{"push"}},
		{File: "smoke.yml", Name: "Smoke", Jobs: []string{"Smoke"}, Triggers: []string{"workflow_dispatch"}},
	}
	// Stale "test" drops out (no run); reported "Cross-build" is kept via
	// intersection AND via the pull_request producer; push-only and
	// dispatch-only jobs never enter the required set.
	got := newRequiredCheckSet([]string{"test", "Cross-build", "external-ok"}, map[string]bool{"Cross-build": true, "external-ok": true, "Vet + test + build": true}, producers)
	want := []string{
		"Cross-build",
		"Cross-build linux/arm64 (Raspberry Pi target)",
		"Vet + test + build",
		"external-ok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := newRequiredCheckSet(nil, map[string]bool{}, producers); len(got) != 3 {
		t.Fatalf("no required input: got %v", got)
	}
}

func TestBuildRulesetRewritePayload(t *testing.T) {
	doc := `{
		"id": 42,
		"name": "main",
		"target": "main",
		"enforcement": "active",
		"bypass_mode": "admins_and_custom_props",
		"bypass_actors": [{"actor_id": 7, "actor_type": "User", "actor_role": "Owner"}],
		"source_type": "Repository",
		"source": "javiyt/autopilot",
		"node_id": "RRS_x",
		"rules": [
			{"type": "deletion", "parameters": null},
			{"type": "required_status_checks", "parameters": {"strict_required_status_checks_policy": true, "do_not_enforce_on_create": true, "required_status_checks": [{"context": "test", "integration_id": 15368}]}},
			{"type": "pull_request", "parameters": {"required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false}}
		]
	}`
	payload, err := buildRulesetRewritePayload([]byte(doc), []string{"Cross-build", "Vet + test + build"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name         string            `json:"name"`
		Target       string            `json:"target"`
		Enforcement  string            `json:"enforcement"`
		BypassMode   string            `json:"bypass_mode"`
		BypassActors []json.RawMessage `json:"bypass_actors"`
		ID           *int              `json:"id"`
		Source       *string           `json:"source"`
		NodeID       *string           `json:"node_id"`
		Rules        []struct {
			Type       string `json:"type"`
			Parameters struct {
				Required []struct {
					Context       string `json:"context"`
					IntegrationID int    `json:"integration_id"`
				} `json:"required_status_checks"`
				Strict            bool `json:"strict_required_status_checks_policy"`
				NoEnforceOnCreate bool `json:"do_not_enforce_on_create"`
				ReviewCount       int  `json:"required_approving_review_count"`
			} `json:"parameters"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload not valid JSON: %v\n%s", err, payload)
	}
	if got.Name != "main" || got.Target != "main" || got.Enforcement != "active" {
		t.Errorf("identity not preserved: %+v", got)
	}
	if got.ID != nil || got.Source != nil || got.NodeID != nil {
		t.Errorf("read-only fields must not be echoed back: id=%v source=%v node_id=%v", got.ID, got.Source, got.NodeID)
	}
	if len(got.BypassActors) != 1 {
		t.Errorf("bypass_actors not preserved: %v", got.BypassActors)
	}
	if len(got.Rules) != 3 {
		t.Fatalf("want 3 rules preserved, got %d", len(got.Rules))
	}
	var reqRule *struct {
		Type       string `json:"type"`
		Parameters struct {
			Required []struct {
				Context       string `json:"context"`
				IntegrationID int    `json:"integration_id"`
			} `json:"required_status_checks"`
			Strict            bool `json:"strict_required_status_checks_policy"`
			NoEnforceOnCreate bool `json:"do_not_enforce_on_create"`
			ReviewCount       int  `json:"required_approving_review_count"`
		} `json:"parameters"`
	}
	sawDeletion := false
	for i := range got.Rules {
		switch got.Rules[i].Type {
		case "required_status_checks":
			reqRule = &got.Rules[i]
		case "deletion":
			sawDeletion = true
		}
	}
	if reqRule == nil || !sawDeletion {
		t.Fatalf("rule set changed: %+v", got.Rules)
	}
	// The rule's other parameters must survive the rewrite verbatim.
	if !reqRule.Parameters.Strict || !reqRule.Parameters.NoEnforceOnCreate {
		t.Errorf("required_status_checks policy flags not preserved: %+v", reqRule.Parameters)
	}
	if want := []string{"Cross-build", "Vet + test + build"}; len(reqRule.Parameters.Required) != 2 || reqRule.Parameters.Required[0].Context != want[0] || reqRule.Parameters.Required[1].Context != want[1] {
		t.Errorf("required list wrong: %+v", reqRule.Parameters.Required)
	}
	for _, r := range reqRule.Parameters.Required {
		if r.IntegrationID != 15368 {
			t.Errorf("integration_id must be the Actions app id 15368, got %d", r.IntegrationID)
		}
	}

	// A ruleset without a required_status_checks rule gains one.
	minDoc := `{"name":"main","target":"main","enforcement":"active","rules":[{"type":"deletion","parameters":null}]}`
	payload, err = buildRulesetRewritePayload([]byte(minDoc), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	var reparse map[string]interface{}
	if err := json.Unmarshal(payload, &reparse); err != nil {
		t.Fatal(err)
	}
	rules := reparse["rules"].([]interface{})
	if len(rules) != 2 {
		t.Fatalf("want 2 rules after adding required_status_checks, got %d", len(rules))
	}

	if _, err := buildRulesetRewritePayload([]byte(`{"target":"main"}`), nil); err == nil {
		t.Fatal("want error for ruleset document without a name")
	}
}

func TestDecideGapRemediation(t *testing.T) {
	stale := checkGap{Context: "test"}
	live := checkGap{Context: "Smoke", Producer: &workflowProducer{File: "smoke.yml", Triggers: []string{"workflow_dispatch"}}}
	liveNoDispatch := checkGap{Context: "Vet + test + build", Producer: &workflowProducer{File: "test.yml", Triggers: []string{"pull_request"}}}
	head := "1111111111111111111111111111111111111111"

	cases := []struct {
		name       string
		gaps       []checkGap
		dispatched map[string]bool
		healedSHA  string
		autoFix    bool
		grace      bool
		cap        bool
		wantAction remediationAction
		wantInNote string
	}{
		{"no gaps", nil, nil, "", true, true, false, remediationNone, ""},
		{"grace window", []checkGap{stale}, nil, "", true, false, false, remediationWaiting, "grace window"},
		{"stale + autofix + not healed", []checkGap{stale}, nil, "", true, true, false, remediationHealed, "rewriting"},
		{"stale + already healed for head", []checkGap{stale}, nil, head, true, true, false, remediationBlock, "already rewritten"},
		{"stale + autofix off", []checkGap{stale}, nil, "", false, true, false, remediationBlock, "disabled"},
		{"live dispatchable, not dispatched", []checkGap{live}, nil, "", true, true, false, remediationDispatched, "smoke.yml"},
		{"live dispatchable, already dispatched", []checkGap{live}, map[string]bool{"Smoke": true}, "", true, true, false, remediationWaiting, "waiting"},
		{"live non-dispatchable", []checkGap{liveNoDispatch}, nil, "", true, true, false, remediationWaiting, "produced by the head's workflows"},
		{"live non-dispatchable + cap blocks with guidance", []checkGap{liveNoDispatch}, nil, "", true, true, true, remediationBlock, "does not declare workflow_dispatch"},
		{"live dispatchable, already dispatched + cap blocks", []checkGap{live}, map[string]bool{"Smoke": true}, "", true, true, true, remediationBlock, "Actions queue"},
		{"grace window + cap blocks", []checkGap{liveNoDispatch}, nil, "", true, false, true, remediationBlock, "CI wait window"},
		{"mixed stale and dispatchable prefers rewrite", []checkGap{stale, live}, nil, "", true, true, false, remediationHealed, "stale"},
		{"live gaps with autofix off block", []checkGap{live}, nil, "", false, true, false, remediationBlock, "disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideGapRemediation(c.gaps, c.dispatched, c.healedSHA, head, c.autoFix, c.grace, c.cap)
			if d.Action != c.wantAction {
				t.Fatalf("action = %v, want %v (note: %s)", d.Action, c.wantAction, d.Note)
			}
			if c.wantInNote != "" && !strings.Contains(d.Note, c.wantInNote) {
				t.Errorf("note %q does not contain %q", d.Note, c.wantInNote)
			}
			if c.wantAction == remediationBlock && d.blockError() == nil {
				t.Error("block decision must carry an error")
			}
			if c.wantAction != remediationBlock && d.blockError() != nil {
				t.Error("non-block decision must not carry an error")
			}
		})
	}
}
