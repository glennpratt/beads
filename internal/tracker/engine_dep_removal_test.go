package tracker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

func TestEnginePullRemovesStaleTrackerParent(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry run", false: "apply"}[dryRun], func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			defer store.Close()

			mk := func(id, title, ref string, it types.IssueType) {
				issue := &types.Issue{ID: id, Title: title, Status: types.StatusOpen, IssueType: it, Priority: 2}
				if ref != "" {
					issue.ExternalRef = strPtr(ref)
				}
				if err := store.CreateIssue(ctx, issue, "test-actor"); err != nil {
					t.Fatalf("CreateIssue(%s): %v", id, err)
				}
			}
			mk("bd-child", "Child", "https://t/EXT-10", types.TypeTask)
			mk("bd-old", "Old epic", "https://t/EXT-1", types.TypeEpic)
			mk("bd-new", "New epic", "https://t/EXT-2", types.TypeEpic)
			mk("bd-mine", "Personal epic", "", types.TypeEpic)
			for _, parent := range []string{"bd-old", "bd-mine"} {
				dep := &types.Dependency{IssueID: "bd-child", DependsOnID: parent, Type: types.DepParentChild}
				if err := store.AddDependency(ctx, dep, "test-actor"); err != nil {
					t.Fatalf("AddDependency: %v", err)
				}
			}

			tk := newMockTracker("test")
			tk.issues = []TrackerIssue{{ID: "10", Identifier: "EXT-10", URL: "https://t/EXT-10", Title: "Child moved", UpdatedAt: time.Now()}}
			tk.fieldMapper = &mockMapper{issueToBeads: func(ti *TrackerIssue) *IssueConversion {
				parent := func(to string) DependencyInfo {
					return DependencyInfo{FromExternalID: "EXT-10", ToExternalID: to, Type: string(types.DepParentChild), Source: DependencySourceParent}
				}
				return &IssueConversion{
					Issue:              &types.Issue{Title: ti.Title, Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2},
					Dependencies:       []DependencyInfo{parent("EXT-2")},
					RemoveDependencies: []DependencyInfo{parent("EXT-1"), parent("EXT-404")},
				}
			}}

			engine := NewEngine(tk, store, "test-actor")
			if _, err := engine.Sync(ctx, SyncOptions{Pull: true, DryRun: dryRun}); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			deps, err := store.GetDependenciesWithMetadata(ctx, "bd-child")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, d := range deps {
				got[d.ID] = true
			}
			want := map[string]bool{"bd-old": dryRun, "bd-new": !dryRun, "bd-mine": true}
			for id, present := range want {
				if got[id] != present {
					t.Errorf("parent %s present = %v, want %v (deps %v)", id, got[id], present, got)
				}
			}
		})
	}
}

func TestEnginePullKeepsStrongestDependencyPerPair(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	for _, id := range []string{"bd-a", "bd-b"} {
		ref := strPtr("https://t/" + strings.ToUpper(strings.TrimPrefix(id, "bd-")))
		if err := store.CreateIssue(ctx, &types.Issue{ID: id, Title: id, Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2, ExternalRef: ref}, "test-actor"); err != nil {
			t.Fatal(err)
		}
	}
	tk := newMockTracker("test")
	tk.issues = []TrackerIssue{{ID: "a", Identifier: "A", URL: "https://t/A", Title: "a", UpdatedAt: time.Now()}}
	tk.fieldMapper = &mockMapper{issueToBeads: func(ti *TrackerIssue) *IssueConversion {
		dep := func(typ types.DependencyType) DependencyInfo {
			return DependencyInfo{FromExternalID: "A", ToExternalID: "B", Type: string(typ), Source: DependencySourceRelation}
		}
		return &IssueConversion{
			Issue:        &types.Issue{Title: ti.Title, Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2},
			Dependencies: []DependencyInfo{dep(types.DepRelated), dep(types.DepBlocks)}, // weaker first
		}
	}}
	engine := NewEngine(tk, store, "test-actor")
	var warnings []string
	engine.OnWarning = func(msg string) { warnings = append(warnings, msg) }
	if _, err := engine.Sync(ctx, SyncOptions{Pull: true}); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	deps, err := store.GetDependenciesWithMetadata(ctx, "bd-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].ID != "bd-b" || deps[0].DependencyType != types.DepBlocks {
		t.Errorf("deps = %+v, want one blocks edge to bd-b", deps)
	}
}

func TestUniqueDependencies(t *testing.T) {
	d := func(from, to, typ string) DependencyInfo {
		return DependencyInfo{FromExternalID: from, ToExternalID: to, Type: typ}
	}
	got := uniqueDependencies([]DependencyInfo{d("A", "B", "related"), d("a", "b", "related"), d("A", "B", "blocks"), d("B", "A", "related")})
	if len(got) != 3 || got[0] != d("A", "B", "related") || got[1].Type != "blocks" || got[2].FromExternalID != "B" {
		t.Errorf("uniqueDependencies = %v", got)
	}
}
