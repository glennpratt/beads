package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
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

func TestSearchKeysWhereChunksConcurrently(t *testing.T) {
	var mu sync.Mutex
	var queries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jql := r.URL.Query().Get("jql")
		mu.Lock()
		queries++
		mu.Unlock()
		inner := strings.TrimSuffix(strings.TrimPrefix(jql, "key in ("), ")")
		var issues []string
		for _, k := range strings.Split(inner, ", ") {
			issues = append(issues, `{"key":"`+k+`","fields":{}}`)
		}
		_, _ = w.Write([]byte(`{"startAt":0,"maxResults":1000,"total":` + strconv.Itoa(len(issues)) + `,"issues":[` + strings.Join(issues, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	keys := make([]string, 450)
	for i := range keys {
		keys[i] = "P-" + strconv.Itoa(i+1)
	}
	tr := &Tracker{client: newTestClient(srv.URL, "2")}
	issues, err := tr.searchKeysWhere(context.Background(), keys, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 450 || issues[0].Key != "P-1" || issues[449].Key != "P-450" {
		t.Errorf("got %d issues (first %v, last %v), want 450 in key order", len(issues), issues[0].Key, issues[len(issues)-1].Key)
	}
	if queries != 3 {
		t.Errorf("queries = %d, want 3 chunks", queries)
	}
}

// linkStore lists local beads for localJiraKeys.
type linkStore struct {
	*configStore
	issues []*types.Issue
}

func (s *linkStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return s.issues, nil
}

func TestFetchIssuesFollowsLinksOneHopOnly(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/field") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		jql := r.URL.Query().Get("jql")
		mu.Lock()
		queries = append(queries, jql)
		mu.Unlock()
		issue := func(key, linkTo string) string {
			return `{"key":"` + key + `","fields":{"issuelinks":[{"type":{"name":"Relates"},"outwardIssue":{"key":"` + linkTo + `"}}]}}`
		}
		var out []string
		switch {
		case strings.HasPrefix(jql, "key in"):
			if strings.Contains(jql, "B-1") {
				out = append(out, issue("B-1", "D-1")) // tracked, reached via a link
			}
			if strings.Contains(jql, "C-1") {
				out = append(out, issue("C-1", "E-1")) // newly followed from scope
			}
		default:
			out = append(out, issue("A-1", "C-1")) // the scope search
		}
		_, _ = w.Write([]byte(`{"startAt":0,"maxResults":100,"total":` + strconv.Itoa(len(out)) + `,"issues":[` + strings.Join(out, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)

	ref := func(k string) *string { s := "https://jira.example.com/browse/" + k; return &s }
	store := &linkStore{configStore: &configStore{data: map[string]string{}}, issues: []*types.Issue{{ID: "b", ExternalRef: ref("B-1")}}}
	tr := &Tracker{client: newTestClient(srv.URL, "2"), store: store, projectKeys: []string{"A"}, apiVersion: "2", jiraURL: "https://jira.example.com", hierarchyResolved: true}

	issues, err := tr.FetchIssues(context.Background(), tracker.FetchOptions{}) // full pull
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range issues {
		got = append(got, i.Identifier)
	}
	if want := []string{"A-1", "C-1", "B-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fetched %v, want %v (no D-1 or E-1: links of linked issues are not followed)", got, want)
	}
	for _, q := range queries {
		if strings.Contains(q, "D-1") || strings.Contains(q, "E-1") {
			t.Errorf("followed a second hop: %q", q)
		}
	}
}
