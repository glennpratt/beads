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

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// remoteIssueJSON is a Jira Server issue as pulled: Undetermined priority and
// a Cancelled status (status category done).
const remoteIssueJSON = `{"key":"P-10","fields":{
	"summary":"Remote title",
	"description":"line one\r\nline two  ",
	"issuetype":{"name":"Story"},
	"priority":{"name":"Undetermined"},
	"status":{"name":"Cancelled","statusCategory":{"key":"done"}},
	"labels":["team-x"]
}}`

func pulledLocal(t *testing.T, tr *Tracker) (*types.Issue, *tracker.TrackerIssue) {
	t.Helper()
	var ji Issue
	if err := json.Unmarshal([]byte(remoteIssueJSON), &ji); err != nil {
		t.Fatal(err)
	}
	remote := jiraToTrackerIssue(&ji, tr.priorityMap)
	local := tr.FieldMapper().IssueToBeads(&remote).Issue
	return local, &remote
}

func TestPushFieldDiff(t *testing.T) {
	tr := &Tracker{typeMap: map[string]string{"story": "Story"}}

	tests := []struct {
		name string
		edit func(*types.Issue)
		want []string
	}{
		{"unchanged pull", func(*types.Issue) {}, nil},
		{"labels are not compared", func(i *types.Issue) { i.Labels = append(i.Labels, "jira", "mine") }, nil},
		{"description whitespace only", func(i *types.Issue) { i.Description = "line one\nline two\n" }, nil},
		{"reopened locally", func(i *types.Issue) { i.Status = types.StatusOpen }, []string{"status"}},
		{"title", func(i *types.Issue) { i.Title = "New title" }, []string{"summary"}},
		{"description", func(i *types.Issue) { i.Description = "rewritten" }, []string{"description"}},
		{"priority", func(i *types.Issue) { i.Priority = 0 }, []string{"priority"}},
		{"type", func(i *types.Issue) { i.IssueType = types.TypeBug }, []string{"issuetype"}},
		{"started locally", func(i *types.Issue) { i.Status = types.StatusInProgress }, []string{"status"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, remote := pulledLocal(t, tr)
			tt.edit(local)
			if got := tr.PushFieldDiff(local, remote); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PushFieldDiff = %v, want %v", got, tt.want)
			}
		})
	}

	// A beads-only status that maps to the same Jira status is not a change.
	open := `{"key":"P-1","fields":{"summary":"s","issuetype":{"name":"Story"},"status":{"name":"To Do"}}}`
	var ji Issue
	_ = json.Unmarshal([]byte(open), &ji)
	remote := jiraToTrackerIssue(&ji, nil)
	local := tr.FieldMapper().IssueToBeads(&remote).Issue
	local.Status = types.StatusDeferred
	if diff := tr.PushFieldDiff(local, &remote); len(diff) != 0 {
		t.Errorf("deferred vs To Do diff = %v, want none", diff)
	}
}

// recordingJira serves one issue and records writes.
type recordingJira struct {
	mu          sync.Mutex
	issue       string
	puts        []map[string]interface{}
	posts       []map[string]interface{}
	transitions int
}

func (rj *recordingJira) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rj.mu.Lock()
		defer rj.mu.Unlock()
		decode := func() map[string]interface{} {
			var body map[string]interface{}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			return body
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodPost:
			rj.transitions++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/issuetypes"):
			_, _ = w.Write([]byte(`{"values":[{"id":"7","name":"Story","subtask":false},{"id":"8","name":"Approval","subtask":true},{"id":"5","name":"Technical Sub-task","subtask":true},{"id":"9","name":"Sub-task","subtask":true}]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(rj.issue))
		case r.Method == http.MethodPut:
			rj.puts = append(rj.puts, decode())
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			rj.posts = append(rj.posts, decode())
			_, _ = w.Write([]byte(`{"id":"99","key":"P-99","self":"x"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateIssueSendsOnlyChangedFields(t *testing.T) {
	tests := []struct {
		name            string
		edit            func(*types.Issue)
		wantPutFields   []string // nil: no PUT
		wantTransitions int
	}{
		{"no change writes nothing", func(*types.Issue) {}, nil, 0},
		{"title only", func(i *types.Issue) { i.Title = "New title" }, []string{"summary"}, 0},
		{"status only transitions", func(i *types.Issue) { i.Status = types.StatusInProgress }, nil, 1},
		{"cleared description", func(i *types.Issue) { i.Description = "" }, []string{"description"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rj := &recordingJira{issue: remoteIssueJSON}
			srv := rj.server(t)
			tr := &Tracker{client: newTestClient(srv.URL, "2"), apiVersion: "2", typeMap: map[string]string{"story": "Story"}, hierarchyResolved: true}
			local, _ := pulledLocal(t, tr)
			tt.edit(local)

			if _, err := tr.UpdateIssue(context.Background(), "P-10", local); err != nil {
				t.Fatal(err)
			}
			var gotFields []string
			if len(rj.puts) > 0 {
				fields, _ := rj.puts[0]["fields"].(map[string]interface{})
				gotFields = sortedKeys(fields)
			}
			if !reflect.DeepEqual(gotFields, tt.wantPutFields) {
				t.Errorf("PUT fields = %v, want %v", gotFields, tt.wantPutFields)
			}
			if rj.transitions != tt.wantTransitions {
				t.Errorf("transitions = %d, want %d", rj.transitions, tt.wantTransitions)
			}
		})
	}
}

// depStore adds parent lookups to configStore.
type depStore struct {
	*configStore
	deps []*types.IssueWithDependencyMetadata
}

func (s *depStore) GetDependenciesWithMetadata(_ context.Context, _ string) ([]*types.IssueWithDependencyMetadata, error) {
	return s.deps, nil
}

func parentDep(ref string, it types.IssueType) *types.IssueWithDependencyMetadata {
	d := &types.IssueWithDependencyMetadata{DependencyType: types.DepParentChild}
	d.IssueType = it
	if ref != "" {
		d.ExternalRef = &ref
	}
	return d
}

func withJiraType(d *types.IssueWithDependencyMetadata, jiraType string) *types.IssueWithDependencyMetadata {
	d.Metadata = json.RawMessage(`{"jira_type":"` + jiraType + `"}`)
	return d
}

func TestCreateIssueHierarchyAndLabels(t *testing.T) {
	const jiraURL = "https://jira.example.com"
	server := HierarchyFields{EpicLink: "customfield_1", EpicName: "customfield_2", ParentLink: "customfield_3"}

	tests := []struct {
		name         string
		fields       HierarchyFields
		issueType    types.IssueType
		parents      []*types.IssueWithDependencyMetadata
		wantSet      map[string]interface{}
		wantAbsent   []string
		wantWarnings int
	}{
		{
			name: "story under Jira epic (Server)", fields: server, issueType: types.TypeStory,
			parents: []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-1", types.TypeEpic)},
			wantSet: map[string]interface{}{"customfield_1": "P-1"}, wantAbsent: []string{"parent", "customfield_2"},
		},
		{
			name: "epic under capability (Server)", fields: server, issueType: types.TypeEpic,
			parents: []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-0", types.TypeMilestone)},
			wantSet: map[string]interface{}{"customfield_3": "P-0", "customfield_2": "New work"},
		},
		{
			name: "story under Jira epic (Cloud)", issueType: types.TypeStory,
			parents: []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-1", types.TypeEpic)},
			wantSet: map[string]interface{}{"parent": map[string]interface{}{"key": "P-1"}},
		},
		{
			name: "personal (unlinked) parent ignored", fields: server, issueType: types.TypeStory,
			parents:    []*types.IssueWithDependencyMetadata{parentDep("", types.TypeEpic)},
			wantAbsent: []string{"customfield_1", "parent"},
		},
		{
			name: "task under story becomes sub-task", fields: server, issueType: types.TypeTask,
			parents: []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-10", types.TypeStory)},
			wantSet: map[string]interface{}{
				"parent":    map[string]interface{}{"key": "P-10"},
				"issuetype": map[string]interface{}{"name": "Sub-task"},
			},
			wantAbsent: []string{"customfield_1"},
		},
		{
			name: "sub-task parent cannot nest", fields: server, issueType: types.TypeTask,
			parents:    []*types.IssueWithDependencyMetadata{withJiraType(parentDep(jiraURL+"/browse/P-11", types.TypeTask), "Technical Sub-task")},
			wantAbsent: []string{"customfield_1", "parent"}, wantWarnings: 1,
		},
		{
			name: "epic under story is not placed", fields: server, issueType: types.TypeEpic,
			parents:    []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-10", types.TypeStory)},
			wantAbsent: []string{"customfield_3", "parent"}, wantWarnings: 1,
		},
		{
			name: "epic under pulled Capability (task bead, Jira type Capability)", fields: server, issueType: types.TypeEpic,
			parents: []*types.IssueWithDependencyMetadata{withJiraType(parentDep(jiraURL+"/browse/P-0", types.TypeTask), "Capability")},
			wantSet: map[string]interface{}{"customfield_3": "P-0", "customfield_2": "New work"},
		},
		{
			name: "epic parent preferred over story parent", fields: server, issueType: types.TypeStory,
			parents: []*types.IssueWithDependencyMetadata{
				parentDep(jiraURL+"/browse/P-10", types.TypeStory),
				parentDep(jiraURL+"/browse/P-1", types.TypeEpic),
			},
			wantSet: map[string]interface{}{"customfield_1": "P-1"}, wantWarnings: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rj := &recordingJira{issue: `{"key":"P-99","fields":{"summary":"New work"}}`}
			srv := rj.server(t)
			tr := &Tracker{
				client:            newTestClient(srv.URL, "2"),
				store:             &depStore{configStore: &configStore{data: map[string]string{}}, deps: tt.parents},
				jiraURL:           jiraURL,
				projectKeys:       []string{"P"},
				apiVersion:        "2",
				typeMap:           map[string]string{"story": "Story"},
				pushLabel:         "jira",
				hierarchyResolved: true,
				epicLinkField:     tt.fields.EpicLink,
				epicNameField:     tt.fields.EpicName,
				parentLinkField:   tt.fields.ParentLink,
			}
			issue := &types.Issue{ID: "gp-1", Title: "New work", IssueType: tt.issueType, Priority: 2, Labels: []string{"jira", "mine"}}

			created, err := tr.CreateIssue(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if len(rj.posts) != 1 {
				t.Fatalf("posts = %d, want 1", len(rj.posts))
			}
			fields, _ := rj.posts[0]["fields"].(map[string]interface{})
			for k, want := range tt.wantSet {
				if got := fields[k]; !reflect.DeepEqual(got, want) {
					t.Errorf("field %s = %#v, want %#v", k, got, want)
				}
			}
			for _, k := range tt.wantAbsent {
				if _, ok := fields[k]; ok {
					t.Errorf("field %s set to %#v, want absent", k, fields[k])
				}
			}
			if labels := fields["labels"]; !reflect.DeepEqual(labels, []interface{}{"mine"}) {
				t.Errorf("labels = %#v, want [mine] (push label stripped)", labels)
			}
			if len(created.Warnings) != tt.wantWarnings {
				t.Errorf("warnings = %v, want %d", created.Warnings, tt.wantWarnings)
			}
		})
	}
}

func TestHasPushLabel(t *testing.T) {
	tr := &Tracker{pushLabel: "jira"}
	if !tr.HasPushLabel(&types.Issue{Labels: []string{"x", "JIRA"}}) {
		t.Error("expected case-insensitive label match")
	}
	if tr.HasPushLabel(&types.Issue{Labels: []string{"jira-ish"}}) {
		t.Error("unexpected partial match")
	}
	if (&Tracker{}).HasPushLabel(&types.Issue{Labels: []string{"jira"}}) {
		t.Error("no push label configured should never match")
	}
}

func TestDescribeCreate(t *testing.T) {
	const jiraURL = "https://jira.example.com"
	tr := &Tracker{
		store:             &depStore{configStore: &configStore{data: map[string]string{}}, deps: []*types.IssueWithDependencyMetadata{parentDep(jiraURL+"/browse/P-1", types.TypeEpic)}},
		jiraURL:           jiraURL,
		projectKeys:       []string{"P"},
		typeMap:           map[string]string{"story": "Story"},
		pushLabel:         "jira",
		hierarchyResolved: true,
		epicLinkField:     "customfield_1",
	}
	got := tr.DescribeCreate(context.Background(), &types.Issue{ID: "gp-1", Title: "t", IssueType: types.TypeStory, Labels: []string{"jira", "mine"}})
	if want := "Story; Epic Link P-1; labels mine"; got != want {
		t.Errorf("DescribeCreate = %q, want %q", got, want)
	}
}

func TestTransitionFieldsResolution(t *testing.T) {
	var tr Transition
	raw := `{"id":"291","name":"Close","to":{"name":"Closed"},"fields":{"resolution":{"required":true,"allowedValues":[{"id":"1","name":"Won't Complete"},{"id":"2","name":"Complete"},{"id":"3","name":"Duplicate"}]}}}`
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		t.Fatal(err)
	}
	res := func(cfg map[string]string) interface{} {
		trk := &Tracker{store: &configStore{data: cfg}}
		return trk.transitionFields(context.Background(), tr)["resolution"]
	}
	if got := res(nil); !reflect.DeepEqual(got, map[string]string{"name": "Complete"}) {
		t.Errorf("default resolution = %v, want preferred Complete", got)
	}
	if got := res(map[string]string{"jira.resolution": "duplicate"}); !reflect.DeepEqual(got, map[string]string{"name": "Duplicate"}) {
		t.Errorf("configured resolution = %v, want Duplicate", got)
	}
	if got := res(map[string]string{"jira.resolution": "Nope"}); !reflect.DeepEqual(got, map[string]string{"name": "Complete"}) {
		t.Errorf("disallowed configured resolution = %v, want fallback Complete", got)
	}

	var optional Transition
	_ = json.Unmarshal([]byte(`{"id":"411","name":"Cancel","to":{"name":"Cancelled"}}`), &optional)
	if f := (&Tracker{}).transitionFields(context.Background(), optional); f != nil {
		t.Errorf("no resolution field should send no fields, got %v", f)
	}
}

func TestSubtaskTypeSelection(t *testing.T) {
	for _, tt := range []struct {
		types []string
		want  string
	}{
		{[]string{"Approval", "Sub-Risk", "Sub-task"}, "Sub-task"},
		{[]string{"Approval", "Subtask"}, "Subtask"},
		{[]string{"Technical Sub-task"}, "Technical Sub-task"},
	} {
		tr := &Tracker{subtaskTypes: tt.types, subtaskResolved: true}
		if got := tr.subtaskType(context.Background()); got != tt.want {
			t.Errorf("subtaskType(%v) = %q, want %q", tt.types, got, tt.want)
		}
	}
}

func TestAssigneeMapping(t *testing.T) {
	var server, cloud Issue
	_ = json.Unmarshal([]byte(`{"key":"P-1","fields":{"summary":"s","assignee":{"name":"gpratt","displayName":"Glenn Pratt"}}}`), &server)
	_ = json.Unmarshal([]byte(`{"key":"P-2","fields":{"summary":"s","assignee":{"accountId":"5b10ac8d","displayName":"Glenn Pratt"}}}`), &cloud)

	m := &jiraFieldMapper{apiVersion: "2"}
	got := m.IssueToBeads(&tracker.TrackerIssue{Raw: &server}).Issue
	if got.Assignee != "gpratt" || got.Owner != "Glenn Pratt" {
		t.Errorf("server pull: assignee %q owner %q", got.Assignee, got.Owner)
	}
	if got := m.IssueToBeads(&tracker.TrackerIssue{Raw: &cloud}).Issue; got.Assignee != "5b10ac8d" {
		t.Errorf("cloud pull: assignee %q, want account ID", got.Assignee)
	}

	if a := (&jiraFieldMapper{apiVersion: "2"}).IssueToTracker(&types.Issue{Title: "t", Assignee: "gpratt"})["assignee"]; !reflect.DeepEqual(a, map[string]interface{}{"name": "gpratt"}) {
		t.Errorf("v2 create assignee = %#v", a)
	}
	if a := (&jiraFieldMapper{apiVersion: "3"}).IssueToTracker(&types.Issue{Title: "t", Assignee: "5b10ac8d"})["assignee"]; !reflect.DeepEqual(a, map[string]interface{}{"accountId": "5b10ac8d"}) {
		t.Errorf("v3 create assignee = %#v", a)
	}
	if _, ok := m.IssueToTracker(&types.Issue{Title: "t"})["assignee"]; ok {
		t.Error("unassigned bead should not send assignee on create")
	}
}

func TestPushFieldDiffAssignee(t *testing.T) {
	tr := &Tracker{apiVersion: "2", typeMap: map[string]string{"story": "Story"}}
	var ji Issue
	_ = json.Unmarshal([]byte(`{"key":"P-10","fields":{"summary":"s","issuetype":{"name":"Story"},"status":{"name":"Open"},"assignee":{"name":"evaldez"}}}`), &ji)
	remote := jiraToTrackerIssue(&ji, nil)
	local := tr.FieldMapper().IssueToBeads(&remote).Issue

	local.Assignee = "EValdez"
	if d := tr.PushFieldDiff(local, &remote); len(d) != 0 {
		t.Errorf("case-only difference should not diff, got %v", d)
	}
	local.Assignee = "gpratt"
	if d := tr.PushFieldDiff(local, &remote); !reflect.DeepEqual(d, []string{"assignee"}) {
		t.Errorf("reassign diff = %v", d)
	}
}

func TestUpdateIssueUnassignSendsNull(t *testing.T) {
	rj := &recordingJira{issue: `{"key":"P-10","fields":{"summary":"s","issuetype":{"name":"Story"},"status":{"name":"Open"},"assignee":{"name":"evaldez"}}}`}
	srv := rj.server(t)
	tr := &Tracker{client: newTestClient(srv.URL, "2"), apiVersion: "2", typeMap: map[string]string{"story": "Story"}, hierarchyResolved: true}
	var ji Issue
	_ = json.Unmarshal([]byte(rj.issue), &ji)
	remote := jiraToTrackerIssue(&ji, nil)
	local := tr.FieldMapper().IssueToBeads(&remote).Issue
	local.Assignee = ""

	if _, err := tr.UpdateIssue(context.Background(), "P-10", local); err != nil {
		t.Fatal(err)
	}
	if len(rj.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(rj.puts))
	}
	fields, _ := rj.puts[0]["fields"].(map[string]interface{})
	if v, ok := fields["assignee"]; !ok || v != nil || len(fields) != 1 {
		t.Errorf("PUT fields = %#v, want only assignee: null", fields)
	}
}

func TestCreateTargetProjectAndCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/createmeta/OTHER/issuetypes"):
			_, _ = w.Write([]byte(`{"values":[{"id":"3","name":"Task","subtask":false}]}`))
		case strings.HasSuffix(r.URL.Path, "/createmeta/OTHER/issuetypes/3"):
			_, _ = w.Write([]byte(`{"values":[
				{"fieldId":"summary","name":"Summary","required":true},
				{"fieldId":"customfield_9","name":"Component Owner","required":true},
				{"fieldId":"priority","name":"Priority","required":true,"hasDefaultValue":true}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tr := &Tracker{client: newTestClient(srv.URL, "2"), store: &depStore{configStore: &configStore{data: map[string]string{}}}, projectKeys: []string{"P"}, hierarchyResolved: true, typeMap: map[string]string{"story": "Story"}}

	task := &types.Issue{ID: "gp-1", Title: "t", IssueType: types.TypeTask, Metadata: json.RawMessage(`{"jira_project":"other"}`)}
	if got := tr.targetProject(context.Background(), task); got != "OTHER" {
		t.Errorf("target project = %q, want OTHER", got)
	}
	got := tr.DescribeCreate(context.Background(), task)
	if !strings.Contains(got, "in OTHER") || !strings.Contains(got, "requires: Component Owner (customfield_9)") || strings.Contains(got, "Priority") {
		t.Errorf("describe = %q", got)
	}

	task.Metadata = json.RawMessage(`{"jira_project":"OTHER","jira_fields":{"customfield_9":{"name":"me"}}}`)
	if got := tr.DescribeCreate(context.Background(), task); strings.Contains(got, "requires") {
		t.Errorf("jira_fields should satisfy the requirement, got %q", got)
	}

	story := &types.Issue{ID: "gp-2", Title: "s", IssueType: types.TypeStory, Metadata: json.RawMessage(`{"jira_project":"OTHER"}`)}
	if got := tr.DescribeCreate(context.Background(), story); !strings.Contains(got, `OTHER has no issue type "Story" (has: Task)`) {
		t.Errorf("missing type not reported: %q", got)
	}
}
