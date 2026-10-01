//go:build cgo

package embeddeddolt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestHoldConnectionCoversAllPaths guards against a self-deadlock: while a
// connection is held, every store path (transactions and version control
// such as Commit) must reuse it rather than open a second engine, which
// would wait forever for the lock the held engine owns.
func TestHoldConnectionCoversAllPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := Open(ctx, filepath.Join(t.TempDir(), ".beads"), "holdconn", "main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.SetConfig(ctx, "issue_prefix", "hc"); err != nil {
		t.Fatal(err)
	}

	release, err := store.HoldConnection(ctx)
	if err != nil {
		t.Fatalf("HoldConnection: %v", err)
	}
	release2, err := store.HoldConnection(ctx) // nested hold shares the engine
	if err != nil {
		t.Fatalf("nested HoldConnection: %v", err)
	}
	if err := store.CreateIssue(ctx, &types.Issue{ID: "hc-1", Title: "one", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, "tester"); err != nil {
		t.Fatalf("CreateIssue while held: %v", err)
	}
	if err := store.Commit(ctx, "while held"); err != nil {
		t.Fatalf("Commit while held: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatal(err)
	}
	if store.held == nil {
		t.Fatal("engine closed while an outer hold remains")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if store.held != nil {
		t.Fatal("engine still held after the last release")
	}
	if _, err := store.GetIssue(ctx, "hc-1"); err != nil {
		t.Fatalf("GetIssue after release: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("timed out: a path opened a second engine while held")
	}
}
