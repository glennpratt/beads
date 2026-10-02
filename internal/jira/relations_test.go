package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

const relURL = "https://jira.example.com"

// relStore is an in-memory graph of beads for relation diff tests.
type relStore struct {
	*configStore
	beads   map[string]*types.Issue // by bead ID
	deps    map[string][]depEdge    // issue ID -> outgoing
	updates map[string]map[string]interface{}
}

type depEdge struct {
	to  string
	typ types.DependencyType
}

func newRelStore() *relStore {
	return &relStore{configStore: &configStore{data: map[string]string{}}, beads: map[string]*types.Issue{}, deps: map[string][]depEdge{}, updates: map[string]map[string]interface{}{}}
}

func (s *relStore) bead(id, key string, it types.IssueType, meta string) *types.Issue {
	b := &types.Issue{ID: id, IssueType: it}
	if key != "" {
		ref := relURL + "/browse/" + key
		b.ExternalRef = &ref
	}
	if meta != "" {
		b.Metadata = json.RawMessage(meta)
	}
	s.beads[id] = b
	return b
}

func (s *relStore) link(from, to string, typ types.DependencyType) {
	s.deps[from] = append(s.deps[from], depEdge{to, typ})
}

func (s *relStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*types.IssueWithDependencyMetadata, error) {
	var out []*types.IssueWithDependencyMetadata
	for _, e := range s.deps[id] {
		out = append(out, &types.IssueWithDependencyMetadata{Issue: *s.beads[e.to], DependencyType: e.typ})
	}
	return out, nil
}

func (s *relStore) GetDependentsWithMetadata(_ context.Context, id string) ([]*types.IssueWithDependencyMetadata, error) {
	var out []*types.IssueWithDependencyMetadata
	for from, edges := range s.deps {
		for _, e := range edges {
			if e.to == id {
				out = append(out, &types.IssueWithDependencyMetadata{Issue: *s.beads[from], DependencyType: e.typ})
			}
		}
	}
	return out, nil
}

func (s *relStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, b := range s.beads {
		if b.ExternalRef != nil && strings.EqualFold(*b.ExternalRef, ref) {
			return b, nil
		}
	}
	return nil, nil
}

func (s *relStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	s.updates[id] = updates
	return nil
}

func relTracker(st *relStore) *Tracker {
	return &Tracker{store: st, jiraURL: relURL, hierarchyResolved: true, epicLinkField: "customfield_1", parentLinkField: "customfield_2", subtaskResolved: true, subtaskTypes: []string{"Sub-task"}}
}

func TestRelationDiffLinks(t *testing.T) {
	st := newRelStore()
	a := st.bead("a", "P-1", types.TypeStory, `{"jira_parents":[],"jira_links":["blocks|P-1|P-3","related|P-1|P-4","blocks|P-1|P-5"]}`)
	b := st.bead("b", "P-2", types.TypeStory, `{"jira_parents":[],"jira_links":[]}`)
	st.bead("c", "P-3", types.TypeStory, "")
	st.bead("d", "P-4", types.TypeStory, "")
	st.bead("e", "P-5", types.TypeStory, "")
	st.bead("mine", "", types.TypeEpic, "")
	st.link("a", "b", types.DepBlocks)    // new local: P-1 waits on P-2
	st.link("a", "mine", types.DepBlocks) // to a personal bead: never pushed
	st.link("a", "d", types.DepBlocks)    // pair P-1/P-4 still linked (as blocks): related not removed
	// P-1 -> P-3 blocks recorded and gone locally: removed.
	// P-1 -> P-5 blocks recorded, gone, but P-5 is P-1's parent-child ancestor: not a removal.
	st.link("a", "e", types.DepParentChild)

	tr := relTracker(st)
	rc := tr.relationDiff(context.Background(), a)
	var adds, removes []string
	for _, op := range rc.addLinks {
		adds = append(adds, op.sig)
	}
	for _, op := range rc.removeLinks {
		removes = append(removes, op.sig)
	}
	if want := []string{"blocks|P-1|P-2", "blocks|P-1|P-4"}; !reflect.DeepEqual(adds, want) {
		t.Errorf("adds = %v, want %v", adds, want)
	}
	if want := []string{"blocks|P-1|P-3"}; !reflect.DeepEqual(removes, want) {
		t.Errorf("removes = %v, want %v", removes, want)
	}
	// P-2 sees the same new link as incoming but does not own it.
	if rc := tr.relationDiff(context.Background(), b); !rc.empty() {
		t.Errorf("non-owning end should push nothing, got %v", rc.describe())
	}
}

func TestRelationDiffParent(t *testing.T) {
	tests := []struct {
		name      string
		meta      string
		parents   []string // bead IDs of local parents
		want      string   // describe()
		wantField string
	}{
		{"moved to another epic", `{"jira_parents":["E-1"],"jira_links":[]}`, []string{"e2"}, "epic/parent link E-2", "customfield_1"},
		{"parent removed", `{"jira_parents":["E-1"],"jira_links":[]}`, nil, "epic/parent link cleared", "customfield_1"},
		{"unchanged", `{"jira_parents":["E-1"],"jira_links":[]}`, []string{"e1"}, "", ""},
		{"personal parent ignored", `{"jira_parents":["E-1"],"jira_links":[]}`, []string{"e1", "mine"}, "", ""},
		{"Jira Parent-Child link is not a parent change", `{"jira_parents":[],"jira_links":["parent-child|S-1|E-1"]}`, []string{"e1"}, "", ""},
		{"no records: nothing", ``, []string{"e2"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newRelStore()
			s := st.bead("s", "S-1", types.TypeStory, tt.meta)
			st.bead("e1", "E-1", types.TypeEpic, "")
			st.bead("e2", "E-2", types.TypeEpic, "")
			st.bead("mine", "", types.TypeEpic, "")
			for _, p := range tt.parents {
				st.link("s", p, types.DepParentChild)
			}
			rc := relTracker(st).relationDiff(context.Background(), s)
			if got := strings.Join(rc.describe(), ", "); got != tt.want {
				t.Errorf("describe = %q, want %q", got, tt.want)
			}
			if rc.parentField != tt.wantField {
				t.Errorf("field = %q, want %q", rc.parentField, tt.wantField)
			}
		})
	}
}

func TestUpdateIssuePushesRelations(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/P-1"):
			_, _ = w.Write([]byte(`{"key":"P-1","fields":{"summary":"s","issuetype":{"name":"Story"},"status":{"name":"Open"},
				"issuelinks":[{"id":"77","type":{"name":"Blocks"},"inwardIssue":{"key":"P-3"}}]}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"key":"X","fields":{}}`))
		default:
			calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/rest/api/2")+" "+string(body))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	st := newRelStore()
	a := st.bead("a", "P-1", types.TypeStory, `{"jira_type":"Story","jira_parents":["E-1"],"jira_links":["blocks|P-1|P-3"]}`)
	a.Title, a.Status, a.Priority = "s", types.StatusOpen, 2
	st.bead("b", "P-2", types.TypeStory, "")
	st.bead("c", "P-3", types.TypeStory, "")
	st.bead("e1", "E-1", types.TypeEpic, "")
	st.bead("e2", "E-2", types.TypeEpic, "")
	st.link("a", "b", types.DepBlocks)
	st.link("a", "e2", types.DepParentChild)

	tr := relTracker(st)
	tr.client, tr.apiVersion, tr.typeMap = newTestClient(srv.URL, "2"), "2", map[string]string{"story": "Story"}
	if _, err := tr.UpdateIssue(context.Background(), "P-1", a); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PUT /issue/P-1 {"fields":{"customfield_1":"E-2"}}`,
		`DELETE /issueLink/77 `,
		`POST /issueLink {"inwardIssue":{"key":"P-2"},"outwardIssue":{"key":"P-1"},"type":{"name":"Blocks"}}`,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	raw, _ := st.updates["a"]["metadata"].(json.RawMessage)
	var meta map[string]interface{}
	_ = json.Unmarshal(raw, &meta)
	if !reflect.DeepEqual(meta["jira_parents"], []interface{}{"E-2"}) || !reflect.DeepEqual(meta["jira_links"], []interface{}{"blocks|P-1|P-2"}) {
		t.Errorf("records after push = %v", meta)
	}
}

// commentStore adds comments to relStore.
type commentStore struct {
	*relStore
	comments map[string][]*types.Comment
}

func (s *commentStore) GetIssueComments(_ context.Context, id string) ([]*types.Comment, error) {
	return s.comments[id], nil
}

func TestPushComments(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/transitions"):
			_, _ = w.Write([]byte(`{"transitions":[]}`)) // no close transition available
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"key":"P-1","fields":{"summary":"s","issuetype":{"name":"Story"},"status":{"name":"Open"}}}`))
		case strings.HasSuffix(r.URL.Path, "/comment"):
			posted = append(posted, string(body))
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	st := &commentStore{relStore: newRelStore(), comments: map[string][]*types.Comment{}}
	a := st.bead("a", "P-1", types.TypeStory, `{"jira_parents":[],"jira_links":[]}`)
	a.Title, a.Status, a.Priority = "s", types.StatusOpen, 2
	st.comments["a"] = []*types.Comment{{ID: "c1", Text: "taking this"}}
	tr := relTracker(st.relStore)
	tr.store = st
	tr.client, tr.apiVersion, tr.typeMap = newTestClient(srv.URL, "2"), "2", map[string]string{"story": "Story"}

	if got := tr.relationDiff(context.Background(), a).describe(); !reflect.DeepEqual(got, []string{"+comment: taking this"}) {
		t.Errorf("describe = %v", got)
	}
	if _, err := tr.UpdateIssue(context.Background(), "P-1", a); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(posted, []string{`{"body":"taking this"}`}) {
		t.Errorf("posted = %v", posted)
	}
	// The record now lists c1; with it, nothing is pending.
	a.Metadata = st.updates["a"]["metadata"].(json.RawMessage)
	if rc := tr.relationDiff(context.Background(), a); !rc.empty() {
		t.Errorf("after push: %v", rc.describe())
	}

	// Close with a reason: posted once even though no close transition exists.
	posted = nil
	a.Status, a.CloseReason = types.StatusClosed, "duplicate of P-9"
	for i := 0; i < 2; i++ {
		if _, err := tr.UpdateIssue(context.Background(), "P-1", a); err != nil {
			t.Fatal(err)
		}
		if u, ok := st.updates["a"]["metadata"].(json.RawMessage); ok {
			a.Metadata = u
		}
	}
	if !reflect.DeepEqual(posted, []string{`{"body":"duplicate of P-9"}`}) {
		t.Errorf("close comments posted = %v, want once", posted)
	}

	// Boilerplate reasons and disabled comment push send nothing.
	if closeCommentFor(&types.Issue{Status: types.StatusClosed, CloseReason: "done"}, []string{"status"}) != "" {
		t.Error("boilerplate close reason should not be posted")
	}
	st.configStore.data["jira.push_comments"] = "false"
	st.comments["a"] = append(st.comments["a"], &types.Comment{ID: "c2", Text: "more"})
	if rc := tr.relationDiff(context.Background(), a); len(rc.comments) != 0 {
		t.Errorf("push_comments=false still pending: %v", rc.describe())
	}
}

func TestCreateIssueCreatesLinks(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issue"):
			calls = append(calls, "create")
			_, _ = w.Write([]byte(`{"id":"1","key":"P-100","self":"x"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issue/P-100"):
			_, _ = w.Write([]byte(`{"key":"P-100","fields":{"summary":"new"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/createmeta"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			calls = append(calls, "POST "+strings.TrimPrefix(r.URL.Path, "/rest/api/2")+" "+string(body))
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	st := newRelStore()
	n := st.bead("n", "", types.TypeTask, "")
	n.Title, n.Priority, n.Labels = "new", 2, []string{"jira"}
	st.bead("cars", "CARS-1", types.TypeTask, "")
	st.bead("epic", "P-1", types.TypeEpic, "")
	st.bead("mine", "", types.TypeTask, "")
	st.link("n", "cars", types.DepBlocks)      // n waits on CARS-1
	st.link("n", "mine", types.DepBlocks)      // local-only blocker: not sent
	st.link("n", "epic", types.DepParentChild) // placed by the create itself

	tr := relTracker(st)
	tr.client, tr.apiVersion, tr.projectKeys = newTestClient(srv.URL, "2"), "2", []string{"P"}

	if got := tr.DescribeCreate(context.Background(), n); !strings.Contains(got, "+CARS-1 blocks (NEW)") {
		t.Errorf("preview = %q, want the link listed", got)
	}
	created, err := tr.CreateIssue(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Warnings) != 0 {
		t.Errorf("warnings = %v", created.Warnings)
	}
	want := []string{
		"create",
		`POST /issueLink {"inwardIssue":{"key":"CARS-1"},"outwardIssue":{"key":"P-100"},"type":{"name":"Blocks"}}`,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	raw, _ := st.updates["n"]["metadata"].(json.RawMessage)
	var meta map[string]interface{}
	_ = json.Unmarshal(raw, &meta)
	if !reflect.DeepEqual(meta["jira_links"], []interface{}{"blocks|P-100|CARS-1"}) {
		t.Errorf("recorded links = %v", meta["jira_links"])
	}
}
