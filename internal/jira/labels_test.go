package jira

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func TestMergePulledLabels(t *testing.T) {
	tr := &Tracker{pushLabel: "jira", localLabels: parseLabelPatterns("q4-*, ME:*")}

	withMeta := func(labels []string, meta string) *types.Issue {
		return &types.Issue{ID: "gp-1", Labels: labels, Metadata: json.RawMessage(meta)}
	}

	tests := []struct {
		name     string
		jiraNow  []string
		existing *types.Issue
		want     []string
		wantMeta map[string]interface{}
	}{
		{
			name: "new issue takes Jira labels", jiraNow: []string{"a", "b"},
			want: []string{"a", "b"}, wantMeta: map[string]interface{}{"jira_labels": []interface{}{"a", "b"}},
		},
		{
			name: "local label kept, Jira removal applied", jiraNow: []string{"a"},
			existing: withMeta([]string{"a", "b", "mine"}, `{"jira_labels":["a","b"]}`),
			want:     []string{"a", "mine"},
		},
		{
			name: "pattern label kept even if Jira had it", jiraNow: nil,
			existing: withMeta([]string{"q4-focus", "me:later", "old"}, `{"jira_labels":["q4-focus","old"]}`),
			want:     []string{"q4-focus", "me:later"},
		},
		{
			name: "no history keeps everything", jiraNow: []string{"a"},
			existing: &types.Issue{ID: "gp-1", Labels: []string{"stale", "mine"}},
			want:     []string{"a", "stale", "mine"},
		},
		{
			name: "case-insensitive dedupe", jiraNow: []string{"Team"},
			existing: withMeta([]string{"team", "jira"}, `{"jira_labels":["team"]}`),
			want:     []string{"Team", "jira"},
		},
		{
			name: "other metadata preserved", jiraNow: []string{"a"},
			existing: withMeta(nil, `{"owner_note":"x","jira_labels":[]}`),
			want:     []string{"a"},
			wantMeta: map[string]interface{}{"owner_note": "x", "jira_labels": []interface{}{"a"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv := &tracker.IssueConversion{Issue: &types.Issue{Labels: tt.jiraNow}}
			ext := &tracker.TrackerIssue{}
			tr.MergePulledLabels(ext, conv, tt.existing)
			if !reflect.DeepEqual(conv.Issue.Labels, tt.want) && !(len(conv.Issue.Labels) == 0 && len(tt.want) == 0) {
				t.Errorf("labels = %v, want %v", conv.Issue.Labels, tt.want)
			}
			if tt.wantMeta != nil {
				raw, _ := json.Marshal(ext.Metadata)
				var got map[string]interface{}
				_ = json.Unmarshal(raw, &got)
				if !reflect.DeepEqual(got, tt.wantMeta) {
					t.Errorf("metadata = %v, want %v", got, tt.wantMeta)
				}
			}
		})
	}
}

func TestMergePulledLabelsNonObjectMetadata(t *testing.T) {
	tr := &Tracker{}
	conv := &tracker.IssueConversion{Issue: &types.Issue{Labels: []string{"a"}}}
	ext := &tracker.TrackerIssue{}
	tr.MergePulledLabels(ext, conv, &types.Issue{Labels: []string{"mine"}, Metadata: json.RawMessage(`["not","an","object"]`)})
	if ext.Metadata != nil {
		t.Errorf("non-object metadata must not be replaced, got %v", ext.Metadata)
	}
	if !reflect.DeepEqual(conv.Issue.Labels, []string{"a", "mine"}) {
		t.Errorf("labels = %v", conv.Issue.Labels)
	}
}

func TestJiraLabelsFiltersLocal(t *testing.T) {
	m := &jiraFieldMapper{pushLabel: "jira", localLabels: parseLabelPatterns("q4-*,me:*")}
	got := m.jiraLabels([]string{"jira", "Q4-focus", "me:x", "team", "q4"})
	if want := []string{"team", "q4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("jiraLabels = %v, want %v", got, want)
	}
}

func TestMergePulledFields(t *testing.T) {
	tr := &Tracker{}
	jiraIssue := func(title, desc string) *types.Issue {
		return &types.Issue{Title: title, Description: desc, IssueType: types.TypeStory, Priority: 2, Status: types.StatusOpen}
	}
	// pull simulates a pull of remote into existing and returns the result.
	pull := func(existing, remote *types.Issue) (*types.Issue, *types.Issue, []string) {
		cp := *remote
		conv := &tracker.IssueConversion{Issue: &cp}
		ext := &tracker.TrackerIssue{}
		warnings := tr.MergePulled(ext, conv, existing)
		stored := *conv.Issue
		raw, _ := json.Marshal(ext.Metadata)
		stored.Metadata = raw
		stored.ID = "gp-1"
		return conv.Issue, &stored, warnings
	}

	// First pull records the Jira values.
	_, bead, _ := pull(nil, jiraIssue("T", "D"))

	t.Run("pending local edit survives re-pull", func(t *testing.T) {
		local := *bead
		local.Title = "local T"
		got, _, warnings := pull(&local, jiraIssue("T", "D"))
		if got.Title != "local T" || len(warnings) != 0 {
			t.Errorf("title = %q, warnings = %v; want local edit kept", got.Title, warnings)
		}
	})

	t.Run("jira change taken when no local edit", func(t *testing.T) {
		local := *bead
		got, _, _ := pull(&local, jiraIssue("T2", "D"))
		if got.Title != "T2" {
			t.Errorf("title = %q, want T2", got.Title)
		}
	})

	t.Run("both changed: jira wins with warning", func(t *testing.T) {
		local := *bead
		local.Title = "local T"
		got, _, warnings := pull(&local, jiraIssue("T2", "D"))
		if got.Title != "T2" || len(warnings) != 1 {
			t.Errorf("title = %q, warnings = %v; want Jira title and one warning", got.Title, warnings)
		}
	})

	t.Run("after push both agree: no warning, record advances", func(t *testing.T) {
		local := *bead
		local.Title = "local T"
		got, stored, warnings := pull(&local, jiraIssue("local T", "D"))
		if got.Title != "local T" || len(warnings) != 0 {
			t.Errorf("title = %q, warnings = %v", got.Title, warnings)
		}
		// A later Jira-only change is now taken.
		got, _, _ = pull(stored, jiraIssue("T3", "D"))
		if got.Title != "T3" {
			t.Errorf("title = %q, want T3", got.Title)
		}
	})

	t.Run("no record: jira wins", func(t *testing.T) {
		legacy := &types.Issue{ID: "gp-1", Title: "local T", Description: "D", IssueType: types.TypeStory, Priority: 2, Status: types.StatusOpen}
		got, _, _ := pull(legacy, jiraIssue("T", "D"))
		if got.Title != "T" {
			t.Errorf("title = %q, want T", got.Title)
		}
	})

	t.Run("local status and description edits kept", func(t *testing.T) {
		local := *bead
		local.Status = types.StatusInProgress
		local.Description = "notes added locally"
		got, _, _ := pull(&local, jiraIssue("T", "D"))
		if got.Status != types.StatusInProgress || got.Description != "notes added locally" {
			t.Errorf("status = %q, description = %q", got.Status, got.Description)
		}
	})
}
