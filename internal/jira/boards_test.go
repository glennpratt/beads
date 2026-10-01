package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// fakeAgile serves one kanban board (id 7, filter 70) with a backlog and a
// search endpoint that records queries.
type fakeAgile struct {
	mu        sync.Mutex
	queries   []string
	boardOut  string // board/7/issue response
	backlog   string // board/7/backlog response ("" = 400, kanban)
	editModel string // greenhopper edit model ("" = 404)
}

func (f *fakeAgile) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/board/7/configuration"):
			_, _ = w.Write([]byte(`{"id":7,"name":"Team","type":"kanban","filter":{"id":"70"},"subQuery":{"query":"type != Program"},
				"ranking":{"rankCustomFieldId":139},
				"columnConfig":{"columns":[{"name":"Backlog","statuses":[{"id":"1"}]},{"name":"In Progress","statuses":[{"id":"3"}]},{"name":"Done","statuses":[{"id":"6"}]}]}}`))
		case strings.HasSuffix(p, "/filter/70"):
			_, _ = w.Write([]byte(`{"jql":"project = P AND resolution = Unresolved ORDER BY Rank ASC"}`))
		case strings.HasSuffix(p, "/board/7/issue"):
			_, _ = w.Write([]byte(f.boardOut))
		case strings.HasSuffix(p, "/board/7/backlog"):
			if f.backlog == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errorMessages":["The backlog is not available on Kanban boards"]}`))
				return
			}
			_, _ = w.Write([]byte(f.backlog))
		case strings.HasSuffix(p, "/rapidviewconfig/editmodel.json"):
			if f.editModel == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(f.editModel))
		case strings.HasSuffix(p, "/field"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/search"):
			jql := r.URL.Query().Get("jql")
			f.queries = append(f.queries, jql)
			if strings.HasPrefix(jql, "key in") {
				_, _ = w.Write([]byte(`{"startAt":0,"maxResults":100,"total":1,"issues":[{"key":"P-3","fields":{"summary":"drifted","status":{"id":"3","name":"In Progress"}}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"startAt":0,"maxResults":100,"total":0,"issues":[]}`))
		default:
			t.Errorf("unexpected %s", p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const twoIssueBoard = `{"startAt":0,"maxResults":500,"total":2,"issues":[
	{"key":"P-1","fields":{"status":{"id":"3"},"customfield_139":"0|a"}},
	{"key":"P-2","fields":{"status":{"id":"1"},"customfield_139":"0|b"}}]}`

func boardTracker(t *testing.T, srv *httptest.Server, cfg map[string]string, store tracker.Store) *Tracker {
	t.Helper()
	tr := &Tracker{client: newTestClient(srv.URL, "2"), store: store, projectKeys: []string{"P"}, apiVersion: "2", jiraURL: "https://jira.example.com"}
	if store == nil {
		tr.store = &configStore{data: cfg}
	}
	tr.loadScopeConfig(cfg)
	return tr
}

func TestScopeJQL(t *testing.T) {
	f := &fakeAgile{boardOut: twoIssueBoard}
	srv := f.server(t)
	tests := []struct {
		name string
		cfg  map[string]string
		want string
	}{
		{"legacy project only", map[string]string{}, `project = "P"`},
		{"legacy pull_jql", map[string]string{"jira.pull_jql": "a OR b"}, `project = "P" AND (a OR b)`},
		{"scopes only", map[string]string{"jira.scope.z": "z = 1", "jira.scope.a": "a = 1 OR a = 2"}, `((a = 1 OR a = 2) OR (z = 1))`},
		{"pull_jql + scope + board", map[string]string{"jira.pull_jql": "x", "jira.scope.s": "s = 1", "jira.board.team": "7"},
			`(project = "P" AND (x) OR (s = 1) OR ((project = P AND resolution = Unresolved) AND (type != Program)))`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := boardTracker(t, srv, tt.cfg, nil)
			got, err := tr.scopeJQL(context.Background())
			if err != nil || got != tt.want {
				t.Errorf("scopeJQL = %q, %v\n want %q", got, err, tt.want)
			}
		})
	}
}

func TestApplyBoardMembership(t *testing.T) {
	f := &fakeAgile{boardOut: twoIssueBoard, backlog: `{"startAt":0,"total":1,"issues":[{"key":"P-2","fields":{}}]}`}
	srv := f.server(t)
	tr := boardTracker(t, srv, map[string]string{"jira.board.team": "7"}, nil)

	apply := func(key string, existing *types.Issue) (*tracker.IssueConversion, map[string]interface{}) {
		conv := &tracker.IssueConversion{Issue: &types.Issue{Labels: []string{"jira-label"}}}
		ext := &tracker.TrackerIssue{Identifier: key, Metadata: map[string]interface{}{}}
		tr.applyBoardMembership(context.Background(), ext, conv, existing)
		return conv, ext.Metadata
	}

	conv, meta := apply("P-1", nil)
	if want := []string{"jira-label", "board:team"}; !reflect.DeepEqual(conv.Issue.Labels, want) {
		t.Errorf("on-board labels = %v, want %v", conv.Issue.Labels, want)
	}
	if meta["jira_column"] != "In Progress" || meta["jira_rank"] != "0|a" {
		t.Errorf("on-board metadata = %v", meta)
	}

	conv, meta = apply("P-2", nil)
	if want := []string{"jira-label", "backlog:team"}; !reflect.DeepEqual(conv.Issue.Labels, want) || meta["jira_column"] != "Backlog" {
		t.Errorf("backlog labels = %v, metadata %v", conv.Issue.Labels, meta)
	}

	conv, meta = apply("P-9", nil)
	if len(conv.Issue.Labels) != 1 || meta["jira_column"] != nil {
		t.Errorf("non-member labels = %v, metadata %v", conv.Issue.Labels, meta)
	}
	if v, ok := meta["jira_boards"]; !ok || v != nil {
		t.Errorf("non-member should clear jira_boards, got %v", meta)
	}

	if !isLocalLabel("board:team", "", nil) || !isLocalLabel("backlog:team", "", nil) {
		t.Error("board labels must be local")
	}
}

func TestApplyBoardMembershipKanbanBacklog(t *testing.T) {
	tests := []struct {
		name      string
		editModel string
		wantP2    string
	}{
		{"kanplan column from edit model", `{"isKanPlanEnabled":true,"rapidListConfig":{"mappedColumns":[{"name":"Backlog","isKanPlanColumn":true},{"name":"In Progress"}]}}`, "backlog:team"},
		{"kanplan disabled", `{"isKanPlanEnabled":false,"rapidListConfig":{"mappedColumns":[{"name":"Backlog","isKanPlanColumn":true}]}}`, "board:team"},
		{"edit model unavailable: first column named Backlog", "", "backlog:team"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeAgile{boardOut: twoIssueBoard, editModel: tt.editModel}
			srv := f.server(t)
			tr := boardTracker(t, srv, map[string]string{"jira.board.team": "7"}, nil)
			labels := func(key string) []string {
				conv := &tracker.IssueConversion{Issue: &types.Issue{}}
				tr.applyBoardMembership(context.Background(), &tracker.TrackerIssue{Identifier: key, Metadata: map[string]interface{}{}}, conv, nil)
				return conv.Issue.Labels
			}
			if got := labels("P-2"); !reflect.DeepEqual(got, []string{tt.wantP2}) {
				t.Errorf("P-2 (Backlog column) labels = %v, want [%s]", got, tt.wantP2)
			}
			if got := labels("P-1"); !reflect.DeepEqual(got, []string{"board:team"}) {
				t.Errorf("P-1 (In Progress) labels = %v", got)
			}
		})
	}
}

// driftStore lists local issues for drift detection.
type driftStore struct {
	*configStore
	issues []*types.Issue
}

func (s *driftStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return s.issues, nil
}

func TestFetchIssuesFetchesBoardDrift(t *testing.T) {
	f := &fakeAgile{boardOut: `{"startAt":0,"total":2,"issues":[
		{"key":"P-1","fields":{"status":{"id":"3"},"customfield_139":"0|a"}},
		{"key":"P-3","fields":{"status":{"id":"3"},"customfield_139":"0|c"}}]}`}
	srv := f.server(t)

	ref := func(k string) *string { s := "https://jira.example.com/browse/" + k; return &s }
	meta := func(rank string) json.RawMessage {
		raw, _ := json.Marshal(map[string]interface{}{"jira_boards": map[string]boardEntry{"team": {Column: "In Progress", Rank: rank, OnBoard: true}}})
		return raw
	}
	store := &driftStore{configStore: &configStore{data: map[string]string{}}, issues: []*types.Issue{
		{ID: "gp-1", ExternalRef: ref("P-1"), Metadata: meta("0|a")}, // unchanged
		{ID: "gp-3", ExternalRef: ref("P-3"), Metadata: meta("0|b")}, // rank changed
		{ID: "gp-4", ExternalRef: ref("P-4"), Metadata: meta("0|d")}, // left the board
		{ID: "gp-5", ExternalRef: ref("P-5")},                        // never on a board
	}}
	tr := boardTracker(t, srv, map[string]string{"jira.board.team": "7"}, store)
	tr.hierarchyResolved = true

	issues, err := tr.FetchIssues(context.Background(), tracker.FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var keyQueries []string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "key in") {
			keyQueries = append(keyQueries, q)
		}
	}
	// Drift first; a full pull then refreshes the other tracked issues.
	if len(keyQueries) == 0 || keyQueries[0] != "key in (P-3, P-4)" {
		t.Errorf("key queries = %q, want drift query key in (P-3, P-4) first", keyQueries)
	}
	if len(issues) == 0 || issues[0].Identifier != "P-3" {
		t.Errorf("issues = %v", issues)
	}
}

func TestStripOrderBy(t *testing.T) {
	for in, want := range map[string]string{
		"project = KPP ORDER BY Rank ASC":    "project = KPP",
		"a = 1 order by created desc, b asc": "a = 1",
		"no order":                           "no order",
	} {
		if got := stripOrderBy(in); got != want {
			t.Errorf("stripOrderBy(%q) = %q, want %q", in, got, want)
		}
	}
}
