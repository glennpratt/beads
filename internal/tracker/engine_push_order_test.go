package tracker

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

func TestEnginePushCreatesParentsFirst(t *testing.T) {
	ctx := context.Background()
	issue := func(id string) *types.Issue {
		return &types.Issue{ID: id, Title: id, Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	}
	parentOf := func(p *types.Issue) []*types.IssueWithDependencyMetadata {
		return []*types.IssueWithDependencyMetadata{{Issue: *p, DependencyType: types.DepParentChild}}
	}
	gc, ch, other, par, personal := issue("bd-gc"), issue("bd-ch"), issue("bd-other"), issue("bd-par"), issue("bd-personal")

	// Store order is child-first; bd-ch also has a parent the hook excludes.
	store := newPureTestStore(gc, ch, other, par, personal)
	store.deps = map[string][]*types.IssueWithDependencyMetadata{
		"bd-gc": parentOf(ch),
		"bd-ch": append(parentOf(personal), parentOf(par)...),
	}

	tk := newMockTracker("test")
	engine := NewEngine(tk, store, "test-actor")
	engine.PushHooks = &PushHooks{ShouldPush: func(i *types.Issue) bool { return i.ID != "bd-personal" }}
	if _, err := engine.Sync(ctx, SyncOptions{Push: true}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	pos := make(map[string]int)
	for i, c := range tk.created {
		pos[c.ID] = i
	}
	if _, ok := pos["bd-personal"]; ok || len(pos) != 4 {
		t.Fatalf("created %v, want gc, ch, other, par (not personal)", pos)
	}
	if !(pos["bd-par"] < pos["bd-ch"] && pos["bd-ch"] < pos["bd-gc"]) {
		t.Errorf("create order par=%d ch=%d gc=%d, want parent before child before grandchild", pos["bd-par"], pos["bd-ch"], pos["bd-gc"])
	}
}

func TestEnginePullFullIgnoresLastSync(t *testing.T) {
	for _, full := range []bool{false, true} {
		store := newPureTestStore()
		store.localMetadata["test.last_sync"] = "2026-01-01T00:00:00Z"
		tk := newMockTracker("test")
		var gotSince *bool
		tk.fetchIssues = func(_ context.Context, opts FetchOptions) ([]TrackerIssue, error) {
			v := opts.Since != nil
			gotSince = &v
			return nil, nil
		}
		engine := NewEngine(tk, store, "test-actor")
		if _, err := engine.Sync(context.Background(), SyncOptions{Pull: true, DryRun: true, Full: full}); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if gotSince == nil || *gotSince == full {
			t.Errorf("Full=%v: incremental Since set = %v, want %v", full, gotSince != nil && *gotSince, !full)
		}
	}
}
