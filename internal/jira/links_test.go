package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func issueWithLinks(t *testing.T, key, links string) *Issue {
	t.Helper()
	var ji Issue
	if err := json.Unmarshal([]byte(`{"key":"`+key+`","fields":{"issuelinks":`+links+`}}`), &ji); err != nil {
		t.Fatal(err)
	}
	return &ji
}

func TestLinkDependenciesDirection(t *testing.T) {
	m := &jiraFieldMapper{linkMap: map[string]string{"cloners": "ignore"}}
	dep := func(from, to string, typ types.DependencyType) tracker.DependencyInfo {
		return tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: string(typ), Source: tracker.DependencySourceRelation}
	}
	tests := []struct {
		name string
		// The same link as seen from each end.
		fromA, fromB string
		want         []tracker.DependencyInfo
	}{
		{"A blocks B: B depends on A",
			`[{"type":{"name":"Blocks"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Blocks"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("B-1", "A-1", types.DepBlocks)}},
		{"A depends on B (Dependent)",
			`[{"type":{"name":"Dependent"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Dependent"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("A-1", "B-1", types.DepBlocks)}},
		{"A duplicates B",
			`[{"type":{"name":"Duplicate"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Duplicate"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("A-1", "B-1", types.DepDuplicates)}},
		{"A is parent of B",
			`[{"type":{"name":"Parent-Child"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Parent-Child"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("B-1", "A-1", types.DepParentChild)}},
		{"relates: canonical order",
			`[{"type":{"name":"Relates"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Relates"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("A-1", "B-1", types.DepRelated)}},
		{"unknown type is related",
			`[{"type":{"name":"Root Cause"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Root Cause"},"inwardIssue":{"key":"A-1"}}]`,
			[]tracker.DependencyInfo{dep("A-1", "B-1", types.DepRelated)}},
		{"ignored via link_map",
			`[{"type":{"name":"Cloners"},"outwardIssue":{"key":"B-1"}}]`,
			`[{"type":{"name":"Cloners"},"inwardIssue":{"key":"A-1"}}]`,
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := m.linkDependencies(issueWithLinks(t, "A-1", tt.fromA))
			gotB := m.linkDependencies(issueWithLinks(t, "B-1", tt.fromB))
			if !reflect.DeepEqual(gotA, tt.want) || !reflect.DeepEqual(gotB, tt.want) {
				t.Errorf("from A: %v\nfrom B: %v\nwant:   %v", gotA, gotB, tt.want)
			}
		})
	}
}

func TestReconcileLinks(t *testing.T) {
	rel := func(from, to string) tracker.DependencyInfo {
		return tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: "blocks", Source: tracker.DependencySourceRelation}
	}
	conv := &tracker.IssueConversion{Dependencies: []tracker.DependencyInfo{rel("A-1", "C-1")}}
	meta := map[string]interface{}{}
	existing := &types.Issue{Metadata: json.RawMessage(`{"jira_links":["blocks|A-1|B-1","blocks|A-1|C-1"]}`)}
	reconcileLinks(conv, existing, meta)
	if want := []tracker.DependencyInfo{rel("A-1", "B-1")}; !reflect.DeepEqual(conv.RemoveDependencies, want) {
		t.Errorf("removals = %v, want %v", conv.RemoveDependencies, want)
	}
	if !reflect.DeepEqual(meta["jira_links"], []string{"blocks|A-1|C-1"}) {
		t.Errorf("record = %v", meta["jira_links"])
	}

	conv = &tracker.IssueConversion{}
	reconcileLinks(conv, &types.Issue{}, map[string]interface{}{})
	if len(conv.RemoveDependencies) != 0 {
		t.Errorf("no record should remove nothing, got %v", conv.RemoveDependencies)
	}
}

func TestLinkedKeysToFollow(t *testing.T) {
	fetched := []Issue{*issueWithLinks(t, "A-1", `[
		{"type":{"name":"Blocks"},"outwardIssue":{"key":"ROSE-9"}},
		{"type":{"name":"Relates"},"inwardIssue":{"key":"A-2"}},
		{"type":{"name":"Relates"},"inwardIssue":{"key":"KMI-3"}},
		{"type":{"name":"Cloners"},"outwardIssue":{"key":"X-1"}}]`)}
	tr := &Tracker{store: &configStore{data: map[string]string{}}, linkMap: map[string]string{"cloners": "ignore"}}
	got := tr.linkedKeysToFollow(context.Background(), fetched, map[string]bool{"A-1": true, "A-2": true}, map[string]bool{"KMI-3": true})
	if !reflect.DeepEqual(got, []string{"ROSE-9"}) {
		t.Errorf("follow = %v, want [ROSE-9] (A-2 fetched, KMI-3 local, X-1 ignored)", got)
	}
	tr.store = &configStore{data: map[string]string{"jira.follow_links": "false"}}
	if got := tr.linkedKeysToFollow(context.Background(), fetched, nil, nil); got != nil {
		t.Errorf("follow_links=false should follow nothing, got %v", got)
	}
}

func TestSearchKeysWhereDropsMissingKeys(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jql := r.URL.Query().Get("jql")
		queries = append(queries, jql)
		if strings.Contains(jql, "GONE-1") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorMessages":["An issue with key 'GONE-1' does not exist for field 'key'."]}`))
			return
		}
		_, _ = w.Write([]byte(`{"startAt":0,"maxResults":100,"total":1,"issues":[{"key":"P-1","fields":{}}]}`))
	}))
	t.Cleanup(srv.Close)
	tr := &Tracker{client: newTestClient(srv.URL, "2")}
	issues, err := tr.searchKeysWhere(context.Background(), []string{"P-1", "GONE-1"}, `updated >= "-5m"`)
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues = %v, err = %v", issues, err)
	}
	if want := []string{`key in (P-1, GONE-1) AND updated >= "-5m"`, `key in (P-1) AND updated >= "-5m"`}; !reflect.DeepEqual(queries, want) {
		t.Errorf("queries = %q, want %q", queries, want)
	}
}

func TestStrongestPerPair(t *testing.T) {
	d := func(from, to, typ string) tracker.DependencyInfo {
		return tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: typ}
	}
	got := strongestPerPair([]tracker.DependencyInfo{
		d("A", "B", "related"), d("B", "A", "blocks"), // same pair: blocks wins
		d("C", "E", "parent-child"), d("C", "E", "related"), // epic link + relates
		d("X", "Y", "duplicates"),
	})
	want := []tracker.DependencyInfo{d("B", "A", "blocks"), d("C", "E", "parent-child"), d("X", "Y", "duplicates")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
