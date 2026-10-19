package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
)

// Jira commands keep a workspace shared between machines (one Dolt remote,
// several clones) in step: they pull the Dolt remote before talking to Jira
// and push it after a Jira push.
//
// The pull matters most before a push: what has already been sent (pushed
// comment IDs, close-comment hashes, pulled field hashes) is recorded in the
// database, so a machine that has not pulled another machine's push could
// send the same comment again. The push afterwards publishes this machine's
// records right away.
//
// A push must send what its preview showed. A dry run opens the store
// read-only and cannot pull, so pull before previewing (`bd dolt pull`; plan
// push does this). A real push pulls again and refuses if that pull changed
// any issue it is about to push: preview again first.
//
// It does nothing without a configured Dolt remote, with dolt.local-only, or
// with jira.dolt_sync=false (or JIRA_DOLT_SYNC=false).

// jiraDoltSyncStore returns the store when Jira commands should sync the
// Dolt remote.
func jiraDoltSyncStore(ctx context.Context) (storage.DoltStorage, bool) {
	if isDoltLocalOnly() || config.GetBool("no-push") {
		return nil, false
	}
	st := getStore()
	if st == nil {
		return nil, false
	}
	v := os.Getenv("JIRA_DOLT_SYNC")
	if v == "" {
		v, _ = st.GetConfig(ctx, "jira.dolt_sync")
	}
	if v = strings.ToLower(strings.TrimSpace(v)); v == "false" || v == "0" || v == "off" {
		return nil, false
	}
	if configured, err := hasConfiguredRemote(ctx, st); err != nil || !configured {
		return nil, false
	}
	return st, true
}

// jiraDoltPullBefore pulls the Dolt remote before a Jira operation.
//
// Dry runs don't pull (the store is read-only); they print a hint instead.
// For a pull, a failed Dolt pull is a warning. For a push it stops the
// command, as does a pull that changed any of ids (the issues being pushed),
// since the push would no longer match its preview.
func jiraDoltPullBefore(ctx context.Context, forPush, dryRun bool, ids []string) error {
	st, ok := jiraDoltSyncStore(ctx)
	if !ok {
		return nil
	}
	if dryRun {
		fmt.Fprintln(os.Stderr, "Dolt: not pulled for a dry run (read-only); run `bd dolt pull` first for an up-to-date preview")
		return nil
	}
	var before string
	if forPush {
		before = pushSnapshot(ctx, st, ids)
	}
	fmt.Fprintln(os.Stderr, "Dolt: pulling the remote before Jira...")
	if err := st.Pull(ctx); err != nil {
		if !forPush {
			fmt.Fprintf(os.Stderr, "Warning: Dolt pull failed (continuing with the Jira pull): %v\n", err)
			return nil
		}
		return fmt.Errorf("Dolt pull before Jira push failed: %w\n"+
			"Resolve it first (bd dolt pull; --strategy ours|theirs for a conflict), "+
			"or set jira.dolt_sync=false to push without syncing", err)
	}
	if forPush && len(ids) > 0 && pushSnapshot(ctx, st, ids) != before {
		return fmt.Errorf("the Dolt pull brought in changes to %s from another machine, "+
			"so this push would differ from its preview: preview again (bd jira push --dry-run) before pushing",
			strings.Join(ids, ", "))
	}
	return nil
}

// pushSnapshot hashes what a push of ids reads: each issue's fields and
// metadata, labels, dependencies and comments.
func pushSnapshot(ctx context.Context, st storage.DoltStorage, ids []string) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, id := range ids {
		issue, err := st.GetIssue(ctx, id)
		_ = enc.Encode(issue)
		_ = enc.Encode(fmt.Sprint(err))
		labels, _ := st.GetLabels(ctx, id)
		_ = enc.Encode(labels)
		deps, _ := st.GetDependenciesWithMetadata(ctx, id)
		_ = enc.Encode(deps)
		comments, _ := st.GetIssueComments(ctx, id)
		_ = enc.Encode(comments)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// jiraDoltPushAfter pushes the Dolt remote after a Jira push. A failure is a
// warning: Jira already has the change, and the next sync retries.
func jiraDoltPushAfter(ctx context.Context) {
	st, ok := jiraDoltSyncStore(ctx)
	if !ok {
		return
	}
	fmt.Fprintln(os.Stderr, "Dolt: pushing the remote after Jira...")
	if err := st.Push(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Dolt push after Jira push failed (run bd dolt push): %v\n", err)
	}
}
