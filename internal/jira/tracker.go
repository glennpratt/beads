package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func init() {
	tracker.Register("jira", func() tracker.IssueTracker {
		return &Tracker{}
	})
}

// Tracker implements tracker.IssueTracker for Jira.
type Tracker struct {
	client           *Client
	store            tracker.Store
	jiraURL          string
	projectKeys      []string                          // one or more project keys (first is primary)
	apiVersion       string                            // "2" or "3" (default: "3")
	statusMap        map[string]string                 // beads status → Jira status name (from jira.status_map.* config)
	typeMap          map[string]string                 // beads type → Jira type (from jira.type_map.* config)
	priorityMap      map[string]string                 // beads priority → Jira priority name (from jira.priority_map.* config)
	customFields     map[string]interface{}            // Jira field name/id → value (from jira.custom_fields.* config)
	typeCustomFields map[string]map[string]interface{} // Jira issue type → Jira field name/id → value

	// Hierarchy link custom fields (Jira Server/DC). Resolved lazily on the
	// first fetch from config or by discovery; empty means not available.
	epicLinkField     string
	epicNameField     string
	parentLinkField   string
	hierarchyResolved bool

	// pushLabel (jira.push_label) marks unlinked beads that may be created in
	// Jira. It is stripped from the labels sent to Jira.
	pushLabel string

	// localLabels (jira.local_labels) are glob patterns for labels that never
	// sync: not sent on create, and kept locally across pulls.
	localLabels []string

	// Sub-task issue type names for the primary project, resolved lazily
	// (jira.subtask_type, else discovery, else "Sub-task"); the first is
	// used when creating sub-tasks.
	subtaskTypes    []string
	subtaskResolved bool

	// Create-screen metadata cache for dry-run checks.
	createTypes  map[string][]ProjectIssueType
	createFields map[string][]CreateField

	// Pull scopes and boards (see boards.go).
	scopes         []namedScope
	boards         []*boardSpec
	boardsResolved bool
	boardsErr      error
	boardMembers   map[string]map[string]boardEntry // issue key -> board name -> entry
	membersLoaded  bool
	membersErr     error

	// linkMap overrides link type kinds (jira.link_map.<type>; see links.go).
	linkMap map[string]string

	// refreshMeta bypasses the metadata cache (full pulls; see metacache.go).
	refreshMeta bool
}

// SetProjectKeys sets project keys before Init(). When set, Init() uses these
// instead of reading from config. This supports the --project CLI flag.
func (t *Tracker) SetProjectKeys(keys []string) {
	t.projectKeys = keys
}

// ProjectKeys returns the list of configured project keys.
func (t *Tracker) ProjectKeys() []string {
	return t.projectKeys
}

// PrimaryProjectKey returns the first configured project key.
func (t *Tracker) PrimaryProjectKey() string {
	if len(t.projectKeys) == 0 {
		return ""
	}
	return t.projectKeys[0]
}

func (t *Tracker) Name() string         { return "jira" }
func (t *Tracker) DisplayName() string  { return "Jira" }
func (t *Tracker) ConfigPrefix() string { return "jira" }

func (t *Tracker) Init(ctx context.Context, store tracker.Store) error {
	t.store = store

	jiraURL, err := t.getConfig(ctx, "jira.url", "JIRA_URL")
	if err != nil || jiraURL == "" {
		return fmt.Errorf("Jira URL not configured (set jira.url or JIRA_URL)")
	}
	t.jiraURL = jiraURL

	// Resolve project keys: use pre-set keys (from CLI), or fall back to config.
	if len(t.projectKeys) == 0 {
		pluralVal, _ := t.getConfig(ctx, "jira.projects", "JIRA_PROJECTS")
		singularVal, _ := t.getConfig(ctx, "jira.project", "JIRA_PROJECT")
		t.projectKeys = tracker.ResolveProjectIDs(nil, pluralVal, singularVal)
	}
	if len(t.projectKeys) == 0 {
		return fmt.Errorf("Jira project not configured (set jira.project, jira.projects, or JIRA_PROJECT)")
	}

	username, _ := t.getConfig(ctx, "jira.username", "JIRA_USERNAME")
	apiToken, err := t.getConfig(ctx, "jira.api_token", "JIRA_API_TOKEN")
	if err != nil || apiToken == "" {
		return fmt.Errorf("Jira API token not configured (set jira.api_token or JIRA_API_TOKEN)")
	}

	t.client = NewClient(jiraURL, username, apiToken)

	// Optional mutual TLS / custom CA for self-hosted Jira behind client-cert auth.
	clientCert, _ := t.getConfig(ctx, "jira.client_cert", "JIRA_CLIENT_CERT")
	clientKey, _ := t.getConfig(ctx, "jira.client_key", "JIRA_CLIENT_KEY")
	caCert, _ := t.getConfig(ctx, "jira.ca_cert", "JIRA_CA_CERT")
	if err := t.client.ConfigureTLS(clientCert, clientKey, caCert); err != nil {
		return err
	}

	pushLabel, _ := t.getConfig(ctx, "jira.push_label", "JIRA_PUSH_LABEL")
	t.pushLabel = strings.TrimSpace(pushLabel)
	localLabels, _ := t.getConfig(ctx, "jira.local_labels", "JIRA_LOCAL_LABELS")
	t.localLabels = parseLabelPatterns(localLabels)

	apiVersion, _ := t.getConfig(ctx, "jira.api_version", "JIRA_API_VERSION")
	if apiVersion == "" {
		apiVersion = "3"
	}
	t.apiVersion = apiVersion
	t.client.APIVersion = apiVersion

	// Load optional custom status map from all jira.status_map.* config keys.
	// Using GetAllConfig supports arbitrary (including custom) beads status names.
	if allConfig, err := t.store.GetAllConfig(ctx); err == nil {
		const statusPrefix = "jira.status_map."
		statusMap := make(map[string]string)
		for key, val := range allConfig {
			if strings.HasPrefix(key, statusPrefix) && val != "" {
				statusMap[strings.TrimPrefix(key, statusPrefix)] = val
			}
		}
		if len(statusMap) > 0 {
			t.statusMap = statusMap
		}

		const typePrefix = "jira.type_map."
		typeMap := make(map[string]string)
		for key, val := range allConfig {
			if strings.HasPrefix(key, typePrefix) && val != "" {
				typeMap[strings.TrimPrefix(key, typePrefix)] = val
			}
		}
		if len(typeMap) > 0 {
			t.typeMap = typeMap
		}

		const priorityPrefix = "jira.priority_map."
		priorityMap := make(map[string]string)
		for key, val := range allConfig {
			if strings.HasPrefix(key, priorityPrefix) && val != "" {
				priorityMap[strings.TrimPrefix(key, priorityPrefix)] = val
			}
		}
		if len(priorityMap) > 0 {
			t.priorityMap = priorityMap
		}

		t.loadScopeConfig(allConfig)
		t.linkMap = make(map[string]string)
		for key, val := range allConfig {
			if strings.HasPrefix(key, "jira.link_map.") && strings.TrimSpace(val) != "" {
				t.linkMap[strings.ToLower(strings.TrimPrefix(key, "jira.link_map."))] = strings.ToLower(strings.TrimSpace(val))
			}
		}

		const customFieldPrefix = "jira.custom_fields."
		customFields := make(map[string]interface{})
		typeCustomFields := make(map[string]map[string]interface{})
		for key, val := range allConfig {
			if !strings.HasPrefix(key, customFieldPrefix) || strings.TrimSpace(val) == "" {
				continue
			}

			suffix := strings.TrimPrefix(key, customFieldPrefix)
			if suffix == "" {
				continue
			}

			parsed, err := parseJiraCustomFieldValue(val)
			if err != nil {
				return fmt.Errorf("parse %s: %w", key, err)
			}

			parts := strings.SplitN(suffix, ".", 2)
			if len(parts) == 2 {
				if parts[0] == "" || parts[1] == "" {
					continue
				}
				if typeCustomFields[parts[0]] == nil {
					typeCustomFields[parts[0]] = make(map[string]interface{})
				}
				typeCustomFields[parts[0]][parts[1]] = parsed
				continue
			}
			customFields[suffix] = parsed
		}
		if len(customFields) > 0 {
			t.customFields = customFields
		}
		if len(typeCustomFields) > 0 {
			t.typeCustomFields = typeCustomFields
		}
	}

	return nil
}

func (t *Tracker) Validate() error {
	if t.client == nil {
		return fmt.Errorf("Jira tracker not initialized")
	}
	return nil
}

func (t *Tracker) Close() error { return nil }

// resolveHierarchyFields determines the Epic Link and Parent Link custom
// field IDs from jira.epic_link_field / jira.parent_link_field, discovering
// them from the field list when neither is configured. "none" disables a
// field. Discovery failures are logged and leave hierarchy import to the
// standard parent field only.
func (t *Tracker) resolveHierarchyFields(ctx context.Context) {
	if t.hierarchyResolved {
		return
	}
	t.hierarchyResolved = true

	epicLink, _ := t.getConfig(ctx, "jira.epic_link_field", "JIRA_EPIC_LINK_FIELD")
	epicName, _ := t.getConfig(ctx, "jira.epic_name_field", "JIRA_EPIC_NAME_FIELD")
	parentLink, _ := t.getConfig(ctx, "jira.parent_link_field", "JIRA_PARENT_LINK_FIELD")
	if epicLink == "" && epicName == "" && parentLink == "" {
		var hf HierarchyFields
		if !t.cacheGet(ctx, "fields", &hf) {
			var err error
			hf, err = t.client.DiscoverHierarchyFields(ctx)
			if err != nil {
				debug.Logf("jira: hierarchy field discovery failed, using parent field only: %v\n", err)
			} else {
				t.cachePut(ctx, "fields", hf)
			}
		}
		epicLink, epicName, parentLink = hf.EpicLink, hf.EpicName, hf.ParentLink
	}
	for _, f := range []*string{&epicLink, &epicName, &parentLink} {
		if strings.EqualFold(strings.TrimSpace(*f), "none") {
			*f = ""
		}
	}
	t.epicLinkField, t.epicNameField, t.parentLinkField = epicLink, epicName, parentLink
	debug.Logf("jira: hierarchy fields: epic_link=%q epic_name=%q parent_link=%q\n", epicLink, epicName, parentLink)

	var extra []string
	for _, f := range []string{epicLink, parentLink} {
		if f != "" {
			extra = append(extra, f)
		}
	}
	t.client.ExtraFields = extra
}

func (t *Tracker) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	t.refreshMeta = opts.Refresh
	t.resolveHierarchyFields(ctx)

	// Scope: project + jira.pull_jql, OR-ed with jira.scope.* and board filters.
	jql, err := t.scopeJQL(ctx)
	if err != nil {
		return nil, err
	}

	// State filter
	switch opts.State {
	case "open":
		jql += " AND statusCategory != Done"
	case "closed":
		jql += " AND statusCategory = Done"
	}

	// Incremental sync
	if opts.Since != nil {
		jql += " AND " + updatedSinceJQL(*opts.Since, time.Now())
	}

	jql += " ORDER BY updated DESC"

	// Board membership is independent of the search; load it concurrently.
	var membersDone chan error
	if len(t.boards) > 0 {
		membersDone = make(chan error, 1)
		go func() { membersDone <- t.loadBoardMembership(ctx) }()
	}

	debug.Logf("jira: search JQL: %s\n", jql)
	issues, err := t.client.SearchIssues(ctx, jql)
	if membersDone != nil {
		if mErr := <-membersDone; err == nil && mErr != nil {
			err = mErr
		}
	}
	if err != nil {
		return nil, err
	}
	debug.Logf("jira: search returned %d issues\n", len(issues))

	fetched := make(map[string]bool, len(issues))
	addIssues := func(extra []Issue) {
		for _, i := range extra {
			if k := strings.ToUpper(i.Key); !fetched[k] {
				fetched[k] = true
				issues = append(issues, i)
			}
		}
	}
	all := issues
	issues = nil
	addIssues(all)

	// Board membership changes (rank, column, aging out of a board filter)
	// need not bump an issue's updated time; fetch drifted issues too.
	if len(t.boards) > 0 {
		if err := t.loadBoardMembership(ctx); err != nil {
			return nil, err
		}
		if drift := t.boardDriftKeys(ctx, fetched); len(drift) > 0 {
			debug.Logf("jira: board membership changed for %d issues; fetching them\n", len(drift))
			extra, err := t.searchKeysWhere(ctx, drift, "")
			if err != nil {
				return nil, err
			}
			addIssues(extra)
		}
	}

	local, err := t.localJiraKeys(ctx)
	if err != nil {
		return nil, err
	}
	// Issues outside every scope (pulled via a link, or that left a scope)
	// only refresh here.
	if opts.Since != nil {
		extra, err := t.refreshTrackedKeys(ctx, *opts.Since, local, fetched)
		if err != nil {
			return nil, err
		}
		addIssues(extra)
	}
	// Follow links one hop to issues not yet pulled.
	if follow := t.linkedKeysToFollow(ctx, issues, fetched, local); len(follow) > 0 {
		debug.Logf("jira: following links to %d issues outside the pull scope\n", len(follow))
		extra, err := t.searchKeysWhere(ctx, follow, "")
		if err != nil {
			return nil, err
		}
		addIssues(extra)
	}

	result := make([]tracker.TrackerIssue, 0, len(issues))
	for i := range issues {
		result = append(result, jiraToTrackerIssue(&issues[i], t.priorityMap))
	}
	return result, nil
}

// updatedSinceJQL builds an incremental-sync clause as a relative period
// ("-Nm"). Absolute JQL dates are interpreted in the Jira user's profile
// timezone and accept no zone suffix (Jira Server rejects "... UTC"), while a
// relative period is evaluated against the server clock. The window is rounded
// up with a minute of slack so boundary updates are not missed.
func updatedSinceJQL(since, now time.Time) string {
	minutes := int(math.Ceil(now.Sub(since).Minutes())) + 1
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf(`updated >= "-%dm"`, minutes)
}

func (t *Tracker) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	t.resolveHierarchyFields(ctx)
	issue, err := t.client.GetIssue(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, nil
	}
	ti := jiraToTrackerIssue(issue, t.priorityMap)
	return &ti, nil
}

func (t *Tracker) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	t.resolveHierarchyFields(ctx)
	mapper := t.FieldMapper()
	fields := mapper.IssueToTracker(issue)

	fields["project"] = map[string]string{"key": t.targetProject(ctx, issue)}
	warnings := t.applyCreateHierarchy(ctx, issue, fields)
	for k, v := range extraCreateFields(issue) {
		fields[k] = v
	}
	debug.Logf("jira: create %s fields: %s\n", issue.ID, strings.Join(sortedKeys(fields), ","))

	created, err := t.client.CreateIssue(ctx, fields)
	if err != nil {
		return nil, err
	}

	ti := jiraToTrackerIssue(created, t.priorityMap)
	ti.Warnings = append(ti.Warnings, warnings...)
	return &ti, nil
}

// UpdateIssue sends only the fields whose mapped value differs from Jira, and
// transitions status only when the beads-level status differs. Comparing in
// beads space means lossy mappings (e.g. an "Undetermined" priority or a
// "Cancelled" status) are not rewritten unless the bead actually changed.
func (t *Tracker) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	mapper := t.FieldMapper()

	current, err := t.client.GetIssue(ctx, externalID)
	if err != nil {
		return nil, err
	}
	currentTI := jiraToTrackerIssue(current, t.priorityMap)
	diff := t.PushFieldDiff(issue, &currentTI)
	rc := t.relationDiff(ctx, issue)
	rc.closeComment = closeCommentFor(issue, diff)
	if len(diff) == 0 && rc.empty() {
		debug.Logf("jira: update %s (%s): no changes\n", externalID, issue.ID)
		return &currentTI, nil
	}
	debug.Logf("jira: update %s (%s): changed %s %v\n", externalID, issue.ID, strings.Join(diff, ","), rc.describe())

	all := mapper.IssueToTracker(issue)
	fields := make(map[string]interface{})
	statusChanged := false
	for _, name := range diff {
		if name == "status" {
			statusChanged = true
			continue
		}
		if v, ok := all[name]; ok {
			fields[name] = v
		} else if name == "description" {
			fields[name] = "" // cleared locally
		} else if name == "assignee" {
			fields[name] = nil // unassigned locally
		}
	}

	if rc.parentChanged {
		var v interface{}
		switch {
		case rc.newParent == "":
			v = nil
		case rc.parentField == "parent":
			v = map[string]string{"key": rc.newParent}
		default:
			v = rc.newParent
		}
		fields[rc.parentField] = v
	}
	if len(fields) > 0 {
		if err := t.client.UpdateIssue(ctx, externalID, fields); err != nil {
			return nil, err
		}
	}
	if err := t.applyRelationLinks(ctx, externalID, rc); err != nil {
		return nil, err
	}
	// Comment before a close transition, so the reason precedes the close
	// and is posted even if the workflow closes without a comment screen.
	if err := t.applyComments(ctx, externalID, rc); err != nil {
		return nil, err
	}
	t.recordPushedRelations(ctx, issue, rc)
	if statusChanged {
		if err := t.applyTransition(ctx, externalID, issue.Status); err != nil {
			return nil, err
		}
	}

	// Re-fetch to return the state after the update.
	current, err = t.client.GetIssue(ctx, externalID)
	if err != nil {
		return nil, err
	}
	ti := jiraToTrackerIssue(current, t.priorityMap)
	return &ti, nil
}

// applyTransition finds and applies the Jira workflow transition matching the given beads status.
// If no matching transition is available (e.g., the issue is already in the target state or the
// workflow doesn't permit the path), it silently succeeds.
func (t *Tracker) applyTransition(ctx context.Context, key string, status types.Status) error {
	mapper := t.FieldMapper()
	desiredName, ok := mapper.StatusToTracker(status).(string)
	if !ok || desiredName == "" {
		return nil
	}

	transitions, err := t.client.GetIssueTransitions(ctx, key)
	if err != nil {
		return err
	}

	for _, tr := range transitions {
		if strings.EqualFold(tr.To.Name, desiredName) {
			fields := t.transitionFields(ctx, tr)
			debug.Logf("jira: transition %s via %q to %q fields=%v\n", key, tr.Name, tr.To.Name, fields)
			return t.client.TransitionIssueWithFields(ctx, key, tr.ID, fields)
		}
	}

	debug.Logf("jira: no available transition to %q for %s (%d transitions checked)\n", desiredName, key, len(transitions))
	return nil
}

func (t *Tracker) FieldMapper() tracker.FieldMapper {
	return &jiraFieldMapper{
		apiVersion:       t.apiVersion,
		statusMap:        t.statusMap,
		typeMap:          t.typeMap,
		priorityMap:      t.priorityMap,
		customFields:     t.customFields,
		typeCustomFields: t.typeCustomFields,
		epicLinkField:    t.epicLinkField,
		parentLinkField:  t.parentLinkField,
		pushLabel:        t.pushLabel,
		localLabels:      t.localLabels,
		linkMap:          t.linkMap,
	}
}

func (t *Tracker) IsExternalRef(ref string) bool {
	return IsJiraExternalRef(ref, t.jiraURL)
}

func (t *Tracker) ExtractIdentifier(ref string) string {
	return ExtractJiraKey(ref)
}

func (t *Tracker) BuildExternalRef(issue *tracker.TrackerIssue) string {
	return fmt.Sprintf("%s/browse/%s", t.jiraURL, issue.Identifier)
}

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys (e.g. jira.api_token), reads from config.yaml first
// to avoid leaking secrets when pushing the Dolt database to remotes.
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) (string, error) {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); val != "" {
			return val, nil
		}
		if envVar != "" {
			if envVal := os.Getenv(envVar); envVal != "" {
				return envVal, nil
			}
		}
		return "", nil
	}

	val, err := t.store.GetConfig(ctx, key)
	if err == nil && val != "" {
		return val, nil
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			return envVal, nil
		}
	}
	return "", nil
}

func parseJiraCustomFieldValue(value string) (interface{}, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}
	return trimmed, nil
}

// jiraToTrackerIssue converts a Jira API Issue to the generic TrackerIssue format.
// priorityMap is optional (nil uses hardcoded defaults).
func jiraToTrackerIssue(ji *Issue, priorityMap map[string]string) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:         ji.ID,
		Identifier: ji.Key,
		URL:        ji.Self,
		Title:      ji.Fields.Summary,
		Labels:     ji.Fields.Labels,
		Raw:        ji,
	}

	// Description: convert ADF to plain text
	ti.Description = DescriptionToPlainText(ji.Fields.Description)

	// Priority
	if ji.Fields.Priority != nil {
		ti.Priority = jiraPriorityToNumeric(ji.Fields.Priority.Name, priorityMap)
	}

	// State
	if ji.Fields.Status != nil {
		ti.State = ji.Fields.Status.Name
	}

	// Type
	if ji.Fields.IssueType != nil {
		ti.Type = ji.Fields.IssueType.Name
	}

	// Assignee
	if ji.Fields.Assignee != nil {
		ti.Assignee = ji.Fields.Assignee.DisplayName
		ti.AssigneeEmail = ji.Fields.Assignee.EmailAddress
		ti.AssigneeID = ji.Fields.Assignee.Login()
	}

	// Timestamps
	if t, err := ParseTimestamp(ji.Fields.Created); err == nil {
		ti.CreatedAt = t
	}
	if t, err := ParseTimestamp(ji.Fields.Updated); err == nil {
		ti.UpdatedAt = t
	}

	// Store Jira-specific metadata
	ti.Metadata = map[string]interface{}{
		"source_system": fmt.Sprintf("jira:%s:%s", projectKeyFromIssue(ji), ji.Key),
	}
	if ji.Fields.IssueType != nil {
		ti.Metadata["jira_type"] = ji.Fields.IssueType.Name
	}
	// Jira's own status and priority names, which beads maps lossily (e.g.
	// "Escalated" -> in_progress, "Undetermined" -> P2).
	if ji.Fields.Status != nil {
		ti.Metadata["jira_status"] = ji.Fields.Status.Name
	}
	if ji.Fields.Priority != nil {
		ti.Metadata["jira_priority"] = ji.Fields.Priority.Name
	}

	return ti
}

// jiraPriorityToNumeric converts a Jira priority name to a numeric value (0=highest, 4=lowest).
// If priorityMap is non-nil, it checks the custom mapping first (inverted: find which beads
// priority key maps to a Jira name matching the input).
func jiraPriorityToNumeric(name string, priorityMap map[string]string) int {
	// Check custom map first (inverted lookup: find beads key whose value matches name).
	if priorityMap != nil {
		for beadsKey, jiraName := range priorityMap {
			if strings.EqualFold(name, jiraName) {
				if v, err := strconv.Atoi(beadsKey); err == nil && v >= 0 && v <= 4 {
					return v
				}
			}
		}
	}
	// Hardcoded defaults.
	switch strings.ToLower(name) {
	case "highest":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	case "lowest":
		return 4
	default:
		return 2
	}
}

// projectKeyFromIssue extracts the project key from a Jira issue.
func projectKeyFromIssue(ji *Issue) string {
	if ji.Fields.Project != nil {
		return ji.Fields.Project.Key
	}
	// Fall back to extracting from issue key (e.g., "PROJ-123" → "PROJ")
	if idx := strings.LastIndex(ji.Key, "-"); idx > 0 {
		return ji.Key[:idx]
	}
	return ""
}
