package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func TestIssueFieldsUnmarshalCustomAndParent(t *testing.T) {
	raw := `{
		"summary": "s",
		"parent": {"id": "1", "key": "PROJ-1"},
		"customfield_10903": "PROJ-2",
		"customfield_16601": {"data": {"key": "PROJ-3"}},
		"customfield_10000": {"key": "PROJ-4"},
		"customfield_20000": null,
		"customfield_30000": 42
	}`
	var f IssueFields
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if f.Summary != "s" || f.Parent == nil || f.Parent.Key != "PROJ-1" {
		t.Fatalf("known fields not decoded: %+v", f)
	}
	if _, ok := f.Custom["customfield_20000"]; ok {
		t.Error("null custom field should be omitted")
	}

	tests := map[string]string{
		"customfield_10903": "PROJ-2", // Server/DC bare key string
		"customfield_16601": "PROJ-3", // nested data.key
		"customfield_10000": "PROJ-4", // object with key
		"customfield_20000": "",       // null
		"customfield_30000": "",       // not a key
		"customfield_99999": "",       // absent
		"":                  "",       // unset field ID
	}
	for id, want := range tests {
		if got := f.CustomFieldKey(id); got != want {
			t.Errorf("CustomFieldKey(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestParentDependencies(t *testing.T) {
	m := &jiraFieldMapper{epicLinkField: "customfield_1", parentLinkField: "customfield_2"}
	dep := func(from, to string) tracker.DependencyInfo {
		return tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: string(types.DepParentChild), Source: tracker.DependencySourceParent}
	}

	tests := []struct {
		name string
		json string
		want []tracker.DependencyInfo
	}{
		{"story with epic link", `{"key":"P-10","fields":{"customfield_1":"P-1"}}`, []tracker.DependencyInfo{dep("P-10", "P-1")}},
		{"epic with parent link", `{"key":"P-1","fields":{"customfield_2":"P-0"}}`, []tracker.DependencyInfo{dep("P-1", "P-0")}},
		{"sub-task parent", `{"key":"P-11","fields":{"parent":{"key":"P-10"}}}`, []tracker.DependencyInfo{dep("P-11", "P-10")}},
		{"cloud parent and epic link agree", `{"key":"P-10","fields":{"parent":{"key":"P-1"},"customfield_1":"p-1"}}`, []tracker.DependencyInfo{dep("P-10", "P-1")}},
		{"self reference ignored", `{"key":"P-1","fields":{"customfield_2":"P-1"}}`, nil},
		{"no hierarchy", `{"key":"P-1","fields":{"summary":"x"}}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ji Issue
			if err := json.Unmarshal([]byte(tt.json), &ji); err != nil {
				t.Fatal(err)
			}
			got := m.parentDependencies(&ji)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dep[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}

	// Without configured custom fields only the standard parent is used.
	var ji Issue
	_ = json.Unmarshal([]byte(`{"key":"P-10","fields":{"customfield_1":"P-1"}}`), &ji)
	if deps := (&jiraFieldMapper{}).parentDependencies(&ji); len(deps) != 0 {
		t.Errorf("unconfigured mapper returned %+v", deps)
	}
}

// hierarchyServer serves /field and records the fields requested by search.
func hierarchyServer(t *testing.T, fieldCalls *int32, requested *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			atomic.AddInt32(fieldCalls, 1)
			_, _ = w.Write([]byte(`[
				{"id":"summary","name":"Summary","schema":{"type":"string"}},
				{"id":"customfield_10903","name":"Epic Link","custom":true,"schema":{"type":"any","custom":"com.pyxis.greenhopper.jira:gh-epic-link"}},
				{"id":"customfield_16601","name":"Parent Link","custom":true,"schema":{"type":"any","custom":"com.atlassian.jpo:jpo-custom-field-parent"}}
			]`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			requested.Store(r.URL.Query().Get("fields"))
			_, _ = w.Write([]byte(`{"startAt":0,"maxResults":100,"total":2,"issues":[
				{"key":"P-1","fields":{"summary":"epic","issuetype":{"name":"Epic"}}},
				{"key":"P-10","fields":{"summary":"story","issuetype":{"name":"Story"},"customfield_10903":"P-1"}}
			]}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchIssuesDiscoversHierarchyFields(t *testing.T) {
	var fieldCalls int32
	var requested atomic.Value
	srv := hierarchyServer(t, &fieldCalls, &requested)

	tr := &Tracker{
		client:      newTestClient(srv.URL, "2"),
		store:       &configStore{data: map[string]string{}},
		projectKeys: []string{"P"},
		apiVersion:  "2",
	}
	for i := 0; i < 2; i++ {
		if _, err := tr.FetchIssues(context.Background(), tracker.FetchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if fieldCalls != 1 {
		t.Errorf("/field called %d times, want 1 (cached)", fieldCalls)
	}
	fields, _ := requested.Load().(string)
	for _, want := range []string{"parent", "customfield_10903", "customfield_16601"} {
		if !strings.Contains(fields, want) {
			t.Errorf("requested fields %q missing %q", fields, want)
		}
	}

	issues, err := tr.FetchIssues(context.Background(), tracker.FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var deps []tracker.DependencyInfo
	for i := range issues {
		deps = append(deps, tr.FieldMapper().IssueToBeads(&issues[i]).Dependencies...)
	}
	if len(deps) != 1 || deps[0].FromExternalID != "P-10" || deps[0].ToExternalID != "P-1" {
		t.Errorf("dependencies = %+v, want P-10 child of P-1", deps)
	}
}

func TestResolveHierarchyFieldsConfig(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]string
		wantEpic       string
		wantParent     string
		wantFieldCalls int32
	}{
		{"explicit fields skip discovery", map[string]string{"jira.epic_link_field": "customfield_1", "jira.parent_link_field": "customfield_2"}, "customfield_1", "customfield_2", 0},
		{"none disables", map[string]string{"jira.epic_link_field": "none", "jira.parent_link_field": "NONE"}, "", "", 0},
		{"one explicit skips discovery", map[string]string{"jira.epic_link_field": "customfield_1"}, "customfield_1", "", 0},
		{"unset discovers", map[string]string{}, "customfield_10903", "customfield_16601", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fieldCalls int32
			var requested atomic.Value
			srv := hierarchyServer(t, &fieldCalls, &requested)
			tr := &Tracker{client: newTestClient(srv.URL, "2"), store: &configStore{data: tt.config}}
			tr.resolveHierarchyFields(context.Background())
			if tr.epicLinkField != tt.wantEpic || tr.parentLinkField != tt.wantParent {
				t.Errorf("fields = (%q, %q), want (%q, %q)", tr.epicLinkField, tr.parentLinkField, tt.wantEpic, tt.wantParent)
			}
			if fieldCalls != tt.wantFieldCalls {
				t.Errorf("/field calls = %d, want %d", fieldCalls, tt.wantFieldCalls)
			}
		})
	}
}
