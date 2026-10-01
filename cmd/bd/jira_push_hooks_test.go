package main

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/jira"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// jiraHookStore serves config only; other tracker.Store methods are unused.
type jiraHookStore struct {
	tracker.Store
	cfg map[string]string
}

func (s *jiraHookStore) GetConfig(_ context.Context, key string) (string, error) {
	return s.cfg[key], nil
}

func (s *jiraHookStore) GetAllConfig(_ context.Context) (map[string]string, error) {
	return s.cfg, nil
}

func TestJiraPushHooksLabelGate(t *testing.T) {
	t.Setenv("JIRA_API_TOKEN", "tok")
	ctx := context.Background()
	linked := "https://jira.example.com/browse/P-1"
	other := "https://github.com/o/r/issues/1"

	newHooks := func(t *testing.T, cfg map[string]string, explicit []string) *tracker.PushHooks {
		t.Helper()
		base := map[string]string{"jira.url": "https://jira.example.com", "jira.project": "P"}
		for k, v := range cfg {
			base[k] = v
		}
		st := &jiraHookStore{cfg: base}
		jt := &jira.Tracker{}
		if err := jt.Init(ctx, st); err != nil {
			t.Fatal(err)
		}
		return buildJiraPushHooksForStore(ctx, st, jt, explicit)
	}

	tests := []struct {
		name     string
		cfg      map[string]string
		explicit []string
		issue    types.Issue
		want     bool
	}{
		{"no push label: everything eligible", nil, nil, types.Issue{ID: "gp-1"}, true},
		{"unlabeled local bead skipped", map[string]string{"jira.push_label": "jira"}, nil, types.Issue{ID: "gp-1"}, false},
		{"labeled local bead created", map[string]string{"jira.push_label": "jira"}, nil, types.Issue{ID: "gp-1", Labels: []string{"Jira"}}, true},
		{"linked bead updated without label", map[string]string{"jira.push_label": "jira"}, nil, types.Issue{ID: "gp-1", ExternalRef: &linked}, true},
		{"other tracker ref is not a Jira link", map[string]string{"jira.push_label": "jira"}, nil, types.Issue{ID: "gp-1", ExternalRef: &other}, false},
		{"explicit ID bypasses label", map[string]string{"jira.push_label": "jira"}, []string{"gp-1"}, types.Issue{ID: "gp-1"}, true},
		{"push_prefix still applies", map[string]string{"jira.push_label": "jira", "jira.push_prefix": "work"}, nil, types.Issue{ID: "gp-1", Labels: []string{"jira"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := newHooks(t, tt.cfg, tt.explicit)
			if got := hooks.ShouldPush(&tt.issue); got != tt.want {
				t.Errorf("ShouldPush = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithoutLabel(t *testing.T) {
	got := withoutLabel([]string{"jira", "team", "JIRA", "me:x"}, " jira ")
	if len(got) != 2 || got[0] != "team" || got[1] != "me:x" {
		t.Errorf("withoutLabel = %v, want [team me:x]", got)
	}
	if got := withoutLabel([]string{"a"}, ""); len(got) != 1 {
		t.Errorf("empty label should be a no-op, got %v", got)
	}
}
