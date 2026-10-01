package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/jira"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

var jiraCmd = &cobra.Command{
	Use:     "jira",
	GroupID: "advanced",
	Short:   "Jira integration commands",
	Long: `Synchronize issues between beads and Jira.

Configuration:
  bd config set jira.url "https://company.atlassian.net"
  bd config set jira.project "PROJ"
  bd config set jira.projects "PROJ1,PROJ2"   # Multiple projects
  bd config set jira.api_token "YOUR_TOKEN"
  bd config set jira.username "your_email@company.com"  # For Jira Cloud
  bd config set jira.push_prefix "hippo"       # Only push hippo-* issues to Jira
  bd config set jira.push_prefix "proj1,proj2" # Multiple prefixes (comma-separated)
  bd config set jira.push_label "jira"         # Create only beads labeled "jira"
                                               # (linked beads still update; the
                                               # label is not sent to Jira)
  bd config set jira.local_labels "q4-*,me:*"  # Personal labels: never sent to
                                               # Jira, kept across pulls

Labels: a pull keeps labels added locally and records Jira's labels in the
bead's metadata (jira_labels), so labels removed in Jira are removed locally.

Self-hosted Jira (Server/Data Center):
  bd config set jira.api_version "2"            # REST API v2
  bd config set jira.api_token "YOUR_PAT"       # No username: sent as Bearer PAT
  export JIRA_CLIENT_CERT=~/certs/me.pem       # Mutual TLS (with JIRA_CLIENT_KEY)
  export JIRA_CLIENT_KEY=~/certs/me.key
  export JIRA_CA_CERT=~/certs/corp-root.crt    # Extra trusted CA
  # (jira.client_cert/client_key/ca_cert also work, but bd will not write
  # cert/key keys to a git-tracked config.yaml)

Hierarchy (pulled as parent-child dependencies; parents must be pulled too):
  The standard parent field (sub-tasks; all levels on Jira Cloud) is always
  used. On Server/DC the Epic Link and Parent Link (Advanced Roadmaps) custom
  fields are discovered automatically; override or disable ("none") with:
  bd config set jira.epic_link_field "customfield_10008"
  bd config set jira.parent_link_field "none"

Environment variables (alternative to config):
  JIRA_API_TOKEN   - Jira API token
  JIRA_USERNAME    - Jira username/email
  JIRA_PROJECTS    - Comma-separated project keys
  JIRA_CLIENT_CERT - Client certificate (PEM) for mutual TLS
  JIRA_CLIENT_KEY  - Client private key (PEM) for mutual TLS
  JIRA_CA_CERT     - Additional CA bundle (PEM)

Examples:
  bd jira sync --pull         # Import issues from Jira
  bd jira sync --push         # Export issues to Jira
  bd jira sync                # Bidirectional sync (pull then push)
  bd jira sync --dry-run      # Preview sync without changes
  bd jira status              # Show sync status`,
}

var jiraSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Synchronize issues with Jira",
	Long: `Synchronize issues between beads and Jira.

Modes:
  --pull         Import issues from Jira into beads
  --push         Export issues from beads to Jira
  (no flags)     Bidirectional sync: pull then push, with conflict resolution

Conflict Resolution:
  By default, newer timestamp wins. Override with:
  --prefer-local   Always prefer local beads version
  --prefer-jira    Always prefer Jira version

Examples:
  bd jira sync --pull                # Import from Jira
  bd jira sync --push --create-only  # Push new issues only
  bd jira sync --dry-run             # Preview without changes
  bd jira sync --prefer-local        # Bidirectional, local wins`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraSync,
}

var jiraStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Jira sync status",
	Long: `Show the current Jira sync status, including:
  - Last sync timestamp
  - Configuration status
  - Number of issues with Jira links
  - Issues pending push (no external_ref)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraStatus,
}

func init() {
	jiraSyncCmd.Flags().Bool("pull", false, "Pull issues from Jira")
	jiraSyncCmd.Flags().Bool("push", false, "Push issues to Jira")
	jiraSyncCmd.Flags().Bool("dry-run", false, "Preview sync without making changes")
	jiraSyncCmd.Flags().Bool("full", false, "Pull everything in scope, ignoring the last sync time (e.g. after changing scopes)")
	jiraSyncCmd.Flags().Bool("prefer-local", false, "Prefer local version on conflicts")
	jiraSyncCmd.Flags().Bool("prefer-jira", false, "Prefer Jira version on conflicts")
	jiraSyncCmd.Flags().Bool("create-only", false, "Only create new issues, don't update existing")
	jiraSyncCmd.Flags().String("state", "all", "Issue state to sync: open, closed, all")
	jiraSyncCmd.Flags().StringSlice("project", nil, "Project key(s) to sync (overrides configured project/projects)")
	registerSelectiveSyncFlags(jiraSyncCmd)

	jiraCmd.AddCommand(jiraSyncCmd)
	jiraCmd.AddCommand(jiraStatusCmd)
	rootCmd.AddCommand(jiraCmd)
}

func runJiraSync(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("jira-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	pull, _ := cmd.Flags().GetBool("pull")
	push, _ := cmd.Flags().GetBool("push")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	preferLocal, _ := cmd.Flags().GetBool("prefer-local")
	preferJira, _ := cmd.Flags().GetBool("prefer-jira")
	createOnly, _ := cmd.Flags().GetBool("create-only")
	state, _ := cmd.Flags().GetString("state")

	if !dryRun {
		CheckReadonly("jira sync")
	}

	if preferLocal && preferJira {
		return HandleErrorRespectJSON("cannot use both --prefer-local and --prefer-jira")
	}

	trackerStore, err := trackerStoreForCommand(rootCtx)
	if err != nil {
		return HandleErrorRespectJSON("database not available: %v", err)
	}

	if err := validateJiraConfigForStore(trackerStore); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	ctx := rootCtx

	jt := &jira.Tracker{}
	cliProjects, _ := cmd.Flags().GetStringSlice("project")
	if len(cliProjects) > 0 {
		jt.SetProjectKeys(tracker.DeduplicateStrings(cliProjects))
	}
	if err := jt.Init(ctx, trackerStore); err != nil {
		return HandleErrorRespectJSON("initializing Jira tracker: %v", err)
	}

	engine := tracker.NewEngine(jt, trackerStore, actor)
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) }

	engine.PushHooks = buildJiraPushHooksForStore(ctx, trackerStore, jt, nil)
	engine.PullHooks = buildJiraPullHooks(jt, engine.OnWarning)

	opts := tracker.SyncOptions{
		Pull:       pull,
		Push:       push,
		DryRun:     dryRun,
		CreateOnly: createOnly,
		State:      state,
	}

	if err := applySelectiveSyncFlags(cmd, &opts, push); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if preferLocal {
		opts.ConflictResolution = tracker.ConflictLocal
	} else if preferJira {
		opts.ConflictResolution = tracker.ConflictExternal
	} else {
		opts.ConflictResolution = tracker.ConflictTimestamp
	}

	if full, _ := cmd.Flags().GetBool("full"); full && pull {
		restore := forceFullJiraPull(ctx, trackerStore, dryRun)
		defer restore()
	}

	result, err := engine.Sync(ctx, opts)
	if err != nil {
		if jsonOutput {
			if jerr := outputJSON(result); jerr != nil {
				return jerr
			}
			return SilentExit()
		}
		return HandleError("%v", err)
	}

	if jsonOutput {
		return outputJSON(result)
	}
	if dryRun {
		fmt.Println("\n✓ Dry run complete (no changes made)")
		return nil
	}
	if result.Stats.Pulled > 0 {
		fmt.Printf("✓ Pulled %d issues (%d created, %d updated)\n",
			result.Stats.Pulled, result.Stats.Created, result.Stats.Updated)
	}
	if result.Stats.Pushed > 0 {
		fmt.Printf("✓ Pushed %d issues\n", result.Stats.Pushed)
	}
	if result.Stats.Conflicts > 0 {
		fmt.Printf("→ Resolved %d conflicts\n", result.Stats.Conflicts)
	}
	fmt.Println("\n✓ Jira sync complete")
	if len(result.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range result.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}
	return nil
}

// buildJiraPushHooksForStore filters pushes and skips no-op updates.
//
// ShouldPush applies jira.push_prefix, and when jira.push_label is set, only
// creates unlinked beads that carry the label or were named explicitly
// (explicitIDs, e.g. `bd jira push <id>`). Beads already linked to Jira stay
// eligible for updates.
//
// ContentEqual compares the fields a push would send, so issues pulled from
// Jira and not edited locally are not rewritten.
func buildJiraPushHooksForStore(ctx context.Context, st tracker.Store, jt *jira.Tracker, explicitIDs []string) *tracker.PushHooks {
	explicit := make(map[string]bool, len(explicitIDs))
	for _, id := range explicitIDs {
		explicit[strings.TrimSpace(id)] = true
	}
	return &tracker.PushHooks{
		ShouldPush: func(issue *types.Issue) bool {
			if !matchesJiraPushPrefix(ctx, st, issue) {
				return false
			}
			if jt == nil || jt.PushLabel() == "" || explicit[issue.ID] {
				return true
			}
			if ref := strings.TrimSpace(derefJiraRef(issue.ExternalRef)); ref != "" && jt.IsExternalRef(ref) {
				return true
			}
			return jt.HasPushLabel(issue)
		},
		ContentEqual: func(local *types.Issue, remote *tracker.TrackerIssue) bool {
			if jt == nil {
				return false
			}
			return len(jt.PushFieldDiff(local, remote)) == 0
		},
		DescribeCreate: func(ctx context.Context, issue *types.Issue) string {
			if jt == nil {
				return ""
			}
			return jt.DescribeCreate(ctx, issue)
		},
		DescribeChanges: func(local *types.Issue, remote *tracker.TrackerIssue) string {
			if jt == nil {
				return ""
			}
			return strings.Join(jt.PushFieldDiff(local, remote), ", ")
		},
	}
}

// forceFullJiraPull clears jira.last_sync so the next pull is not
// incremental. For a dry run, the returned func restores the previous value
// so previewing never changes sync state.
func forceFullJiraPull(ctx context.Context, st tracker.Store, dryRun bool) func() {
	prev, _ := st.GetLocalMetadata(ctx, "jira.last_sync")
	if err := st.SetLocalMetadata(ctx, "jira.last_sync", ""); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not reset jira.last_sync: %v\n", err)
		return func() {}
	}
	return func() {
		if dryRun && prev != "" {
			_ = st.SetLocalMetadata(ctx, "jira.last_sync", prev)
		}
	}
}

// buildJiraPullHooks keeps local labels and pending local field edits
// across pulls (see jira.Tracker.MergePulled).
func buildJiraPullHooks(jt *jira.Tracker, warn func(string)) *tracker.PullHooks {
	return &tracker.PullHooks{
		AfterConvert: func(ctx context.Context, extIssue *tracker.TrackerIssue, conv *tracker.IssueConversion, _ string, existing *types.Issue, _ tracker.SyncOptions) error {
			for _, w := range jt.MergePulled(ctx, extIssue, conv, existing) {
				if warn != nil {
					warn(w)
				}
			}
			return nil
		},
	}
}

func matchesJiraPushPrefix(ctx context.Context, st tracker.Store, issue *types.Issue) bool {
	pushPrefix, _ := st.GetConfig(ctx, "jira.push_prefix")
	if pushPrefix == "" {
		return true
	}
	for _, prefix := range strings.Split(pushPrefix, ",") {
		prefix = strings.TrimSpace(prefix)
		prefix = strings.TrimSuffix(prefix, "-")
		if prefix != "" && strings.HasPrefix(issue.ID, prefix+"-") {
			return true
		}
	}
	return false
}

func derefJiraRef(ref *string) string {
	if ref == nil {
		return ""
	}
	return *ref
}

func runJiraStatus(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("jira-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	trackerStore, err := trackerStoreForCommand(rootCtx)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	jiraURL, _ := trackerStore.GetConfig(ctx, "jira.url")
	lastSync, _ := trackerStore.GetConfig(ctx, "jira.last_sync")

	pluralProjects, _ := trackerStore.GetConfig(ctx, "jira.projects")
	singularProject, _ := trackerStore.GetConfig(ctx, "jira.project")
	projectKeys := tracker.ResolveProjectIDs(nil, pluralProjects, singularProject)

	configured := jiraURL != "" && len(projectKeys) > 0

	// jira sync is a round-trip path — opt out of BEADS_MAX_ROWS
	// (designer §4.1) so a misconfigured env doesn't abort partway.
	allIssues, err := trackerStore.SearchIssues(ctx, "", types.IssueFilter{
		MaxRows:       0,
		MaxRowsSource: "",
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	withJiraRef := 0
	pendingPush := 0
	for _, issue := range allIssues {
		if issue.ExternalRef != nil && jira.IsJiraExternalRef(*issue.ExternalRef, jiraURL) {
			withJiraRef++
		} else if issue.ExternalRef == nil {
			pendingPush++
		}
	}

	if jsonOutput {
		primaryProject := ""
		if len(projectKeys) > 0 {
			primaryProject = projectKeys[0]
		}
		return outputJSON(map[string]interface{}{
			"configured":    configured,
			"jira_url":      jiraURL,
			"jira_project":  primaryProject,
			"jira_projects": projectKeys,
			"last_sync":     lastSync,
			"total_issues":  len(allIssues),
			"with_jira_ref": withJiraRef,
			"pending_push":  pendingPush,
		})
	}

	fmt.Println("Jira Sync Status")
	fmt.Println("================")
	fmt.Println()

	if !configured {
		fmt.Println("Status: Not configured")
		fmt.Println()
		fmt.Println("To configure Jira integration:")
		fmt.Println("  bd config set jira.url \"https://company.atlassian.net\"")
		fmt.Println("  bd config set jira.project \"PROJ\"")
		fmt.Println("  bd config set jira.projects \"PROJ1,PROJ2\"  # multiple projects")
		fmt.Println("  bd config set jira.api_token \"YOUR_TOKEN\"")
		fmt.Println("  bd config set jira.username \"your@email.com\"")
		return nil
	}

	fmt.Printf("Jira URL:     %s\n", jiraURL)
	if len(projectKeys) == 1 {
		fmt.Printf("Project:      %s\n", projectKeys[0])
	} else {
		fmt.Printf("Projects:     %s (%d projects)\n", strings.Join(projectKeys, ", "), len(projectKeys))
	}
	if lastSync != "" {
		fmt.Printf("Last Sync:    %s\n", lastSync)
	} else {
		fmt.Println("Last Sync:    Never")
	}
	fmt.Println()
	fmt.Printf("Total Issues: %d\n", len(allIssues))
	fmt.Printf("With Jira:    %d\n", withJiraRef)
	fmt.Printf("Local Only:   %d\n", pendingPush)

	if pendingPush > 0 {
		fmt.Println()
		fmt.Printf("Run 'bd jira sync --push' to push %d local issue(s) to Jira\n", pendingPush)
	}
	return nil
}

// validateJiraConfig checks that required Jira configuration is present.
func validateJiraConfig() error {
	return validateJiraConfigForStore(tracker.NewStore(store))
}

func validateJiraConfigForStore(st tracker.Store) error {
	ctx := rootCtx
	if st == nil {
		return fmt.Errorf("database not available")
	}
	jiraURL, _ := st.GetConfig(ctx, "jira.url")

	if jiraURL == "" {
		return fmt.Errorf("jira.url not configured\nRun: bd config set jira.url \"https://company.atlassian.net\"")
	}

	// Check for project configuration (singular or plural).
	pluralProjects, _ := st.GetConfig(ctx, "jira.projects")
	singularProject, _ := st.GetConfig(ctx, "jira.project")
	projectKeys := tracker.ResolveProjectIDs(nil, pluralProjects, singularProject)
	if len(projectKeys) == 0 {
		return fmt.Errorf("no Jira project configured\nRun: bd config set jira.project \"PROJ\"\nOr:  bd config set jira.projects \"PROJ1,PROJ2\"")
	}

	apiToken, _ := st.GetConfig(ctx, "jira.api_token")
	if apiToken == "" && os.Getenv("JIRA_API_TOKEN") == "" {
		return fmt.Errorf("Jira API token not configured\nRun: bd config set jira.api_token \"YOUR_TOKEN\"\nOr: export JIRA_API_TOKEN=YOUR_TOKEN")
	}

	return nil
}
