package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// PushLabel returns the configured jira.push_label marker ("" if unset).
func (t *Tracker) PushLabel() string { return t.pushLabel }

// HasPushLabel reports whether the issue carries the jira.push_label marker.
func (t *Tracker) HasPushLabel(issue *types.Issue) bool {
	if t.pushLabel == "" {
		return false
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(l), t.pushLabel) {
			return true
		}
	}
	return false
}

// PushFieldDiff returns the Jira fields ("summary", "description",
// "issuetype", "priority", "status", "assignee") that pushing local would change on
// remote. Type, priority and status are compared by the Jira value each side
// maps to, so lossy mappings (an "Undetermined" priority, a "Cancelled"
// status imported as closed, a beads-only "deferred" status) do not count as
// changes. Labels are not compared: they are only sent on create.
func (t *Tracker) PushFieldDiff(local *types.Issue, remote *tracker.TrackerIssue) []string {
	mapper := t.FieldMapper()
	conv := mapper.IssueToBeads(remote)
	if conv == nil || conv.Issue == nil {
		return []string{"summary", "description", "issuetype", "priority", "status"}
	}
	r := conv.Issue

	var diff []string
	if strings.TrimSpace(local.Title) != strings.TrimSpace(r.Title) {
		diff = append(diff, "summary")
	}
	if normalizeText(local.Description) != normalizeText(r.Description) {
		diff = append(diff, "description")
	}
	if mapper.TypeToTracker(local.IssueType) != mapper.TypeToTracker(r.IssueType) {
		diff = append(diff, "issuetype")
	}
	if mapper.PriorityToTracker(local.Priority) != mapper.PriorityToTracker(r.Priority) {
		diff = append(diff, "priority")
	}
	if mapper.StatusToTracker(local.Status) != mapper.StatusToTracker(r.Status) {
		diff = append(diff, "status")
	}
	if !strings.EqualFold(strings.TrimSpace(local.Assignee), strings.TrimSpace(r.Assignee)) {
		diff = append(diff, "assignee")
	}
	return diff
}

// normalizeText makes descriptions comparable across Jira's CRLF line endings
// and trailing whitespace.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// applyCreateHierarchy places a new Jira issue under its Jira-linked beads
// parent: an Epic via Epic Link (Server/DC) or parent (Cloud), and an Epic
// under a higher level via Parent Link (Server/DC) or parent (Cloud). It also
// fills Epic Name, which Server/DC requires when creating an Epic. Parents
// without a Jira link (e.g. a personal epic) are ignored. It returns
// warnings for placements it could not make.
func (t *Tracker) applyCreateHierarchy(ctx context.Context, issue *types.Issue, fields map[string]interface{}) []string {
	jiraType := ""
	if it, ok := fields["issuetype"].(map[string]string); ok {
		jiraType = it["name"]
	}
	isEpic := strings.EqualFold(jiraType, "Epic")
	if isEpic && t.epicNameField != "" {
		fields[t.epicNameField] = issue.Title
	}

	parents := t.jiraParents(ctx, issue.ID)
	if len(parents) == 0 {
		return nil
	}
	var warnings []string
	if len(parents) > 1 {
		warnings = append(warnings, fmt.Sprintf("multiple Jira-linked parents; using %s", parents[0].key))
	}
	p := parents[0]

	// Jira Cloud has no Epic Link / Parent Link fields: parent covers every level.
	cloud := t.epicLinkField == "" && t.parentLinkField == ""
	parentIsEpic := p.issueType == types.TypeEpic || strings.EqualFold(p.jiraType, "Epic")

	switch {
	case isEpic && !t.epicParentAllowed(ctx, p):
		warnings = append(warnings, fmt.Sprintf("an Epic cannot be placed under %s (a %s); created without a parent", p.key, firstNonEmpty(p.jiraType, string(p.issueType))))
	case isEpic && t.parentLinkField != "":
		fields[t.parentLinkField] = p.key
	case parentIsEpic && t.epicLinkField != "":
		fields[t.epicLinkField] = p.key
	case (isEpic || parentIsEpic) && cloud:
		fields["parent"] = map[string]string{"key": p.key}
	case isEpic || parentIsEpic:
		warnings = append(warnings, fmt.Sprintf("no Jira field to link %s under %s; created without a parent", jiraType, p.key))
	case t.isSubtaskType(ctx, p.jiraType):
		warnings = append(warnings, fmt.Sprintf("parent %s is a Jira %s, which cannot have sub-tasks; created without a parent", p.key, p.jiraType))
	default:
		// Under a story/task: create as the project's sub-task type.
		fields["issuetype"] = map[string]string{"name": t.subtaskType(ctx)}
		fields["parent"] = map[string]string{"key": p.key}
	}
	return warnings
}

// epicParentAllowed reports whether an Epic may sit under p: only a level
// above epics (e.g. Capability/Initiative via Parent Link). Uses the parent's
// Jira type when known, else its beads type.
func (t *Tracker) epicParentAllowed(ctx context.Context, p jiraParent) bool {
	if p.jiraType != "" {
		for _, low := range []string{"Epic", "Story", "Task", "Bug"} {
			if strings.EqualFold(p.jiraType, low) {
				return false
			}
		}
		return !t.isSubtaskType(ctx, p.jiraType)
	}
	switch p.issueType {
	case types.TypeEpic, types.TypeStory, types.TypeTask, types.TypeBug, types.TypeFeature, types.TypeChore:
		return false
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveSubtaskTypes loads the sub-task issue type names once.
func (t *Tracker) resolveSubtaskTypes(ctx context.Context) {
	if t.subtaskResolved {
		return
	}
	t.subtaskResolved = true
	if t.store != nil {
		if configured, _ := t.getConfig(ctx, "jira.subtask_type", "JIRA_SUBTASK_TYPE"); strings.TrimSpace(configured) != "" {
			t.subtaskTypes = []string{strings.TrimSpace(configured)}
			return
		}
	}
	if t.client != nil && t.PrimaryProjectKey() != "" {
		its, err := t.client.GetProjectIssueTypes(ctx, t.PrimaryProjectKey())
		if err != nil {
			debug.Logf("jira: sub-task type discovery failed: %v\n", err)
		}
		for _, it := range its {
			if it.Subtask {
				t.subtaskTypes = append(t.subtaskTypes, it.Name)
			}
		}
	}
	if len(t.subtaskTypes) == 0 {
		t.subtaskTypes = []string{"Sub-task"}
	}
	debug.Logf("jira: sub-task types: %v\n", t.subtaskTypes)
}

// subtaskType returns the issue type name used to create sub-tasks: the
// standard "Sub-task"/"Subtask" when the project has several sub-task types
// (e.g. "Approval", "Sub-Risk"), else the only or first one.
func (t *Tracker) subtaskType(ctx context.Context) string {
	t.resolveSubtaskTypes(ctx)
	for _, name := range t.subtaskTypes {
		n := strings.ToLower(strings.ReplaceAll(name, "-", ""))
		if n == "subtask" {
			return name
		}
	}
	return t.subtaskTypes[0]
}

// isSubtaskType reports whether a Jira issue type name is a sub-task type.
func (t *Tracker) isSubtaskType(ctx context.Context, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	t.resolveSubtaskTypes(ctx)
	for _, s := range t.subtaskTypes {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// metadataString reads a string key from a bead's JSON metadata.
func metadataString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

type jiraParent struct {
	key       string
	issueType types.IssueType
	jiraType  string // Jira issue type name from metadata, when pulled
}

// jiraParents returns the issue's parent-child parents that are linked to
// Jira, epics first.
func (t *Tracker) jiraParents(ctx context.Context, issueID string) []jiraParent {
	if t.store == nil || issueID == "" {
		return nil
	}
	deps, err := t.store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		debug.Logf("jira: looking up parents of %s: %v\n", issueID, err)
		return nil
	}
	var parents []jiraParent
	for _, d := range deps {
		if d == nil || d.DependencyType != types.DepParentChild || d.ExternalRef == nil {
			continue
		}
		if !t.IsExternalRef(*d.ExternalRef) {
			continue
		}
		if key := ExtractJiraKey(*d.ExternalRef); key != "" {
			parents = append(parents, jiraParent{key: key, issueType: d.IssueType, jiraType: metadataString(d.Metadata, "jira_type")})
		}
	}
	sort.SliceStable(parents, func(i, j int) bool {
		return parents[i].issueType == types.TypeEpic && parents[j].issueType != types.TypeEpic
	})
	return parents
}

// DescribeCreate summarizes the fields CreateIssue would send for issue:
// Jira type, parent placement, labels, and any placement warnings.
func (t *Tracker) DescribeCreate(ctx context.Context, issue *types.Issue) string {
	t.resolveHierarchyFields(ctx)
	fields := t.FieldMapper().IssueToTracker(issue)
	warnings := t.applyCreateHierarchy(ctx, issue, fields)

	var parts []string
	if it, ok := fields["issuetype"].(map[string]string); ok {
		parts = append(parts, it["name"])
	}
	for _, f := range []struct{ id, label string }{
		{t.epicLinkField, "Epic Link"},
		{t.parentLinkField, "Parent Link"},
	} {
		if v, ok := fields[f.id]; ok && f.id != "" {
			parts = append(parts, fmt.Sprintf("%s %v", f.label, v))
		}
	}
	if p, ok := fields["parent"].(map[string]string); ok {
		parts = append(parts, "parent "+p["key"])
	}
	if a, ok := fields["assignee"].(map[string]interface{}); ok {
		for _, v := range a {
			parts = append(parts, fmt.Sprintf("assignee %v", v))
		}
	}
	if labels, ok := fields["labels"].([]string); ok && len(labels) > 0 {
		parts = append(parts, "labels "+strings.Join(labels, ","))
	}
	for _, w := range warnings {
		parts = append(parts, "warning: "+w)
	}
	return strings.Join(parts, "; ")
}

// preferredResolutions are tried, in order, when a transition requires a
// resolution and jira.resolution is unset or not allowed.
var preferredResolutions = []string{"Done", "Fixed", "Complete", "Resolved"}

// transitionFields returns screen fields a transition requires. Currently
// this is the resolution (jira.resolution if allowed, else a preferred or the
// first allowed value), which some workflows require to close an issue.
func (t *Tracker) transitionFields(ctx context.Context, tr Transition) map[string]interface{} {
	res, ok := tr.Fields["resolution"]
	if !ok || !res.Required || len(res.AllowedValues) == 0 {
		return nil
	}
	allowed := func(name string) (string, bool) {
		for _, v := range res.AllowedValues {
			if strings.EqualFold(v.Name, strings.TrimSpace(name)) {
				return v.Name, true
			}
		}
		return "", false
	}
	choice := res.AllowedValues[0].Name
	configured := ""
	if t.store != nil {
		configured, _ = t.getConfig(ctx, "jira.resolution", "JIRA_RESOLUTION")
	}
	if name, ok := allowed(configured); ok && configured != "" {
		choice = name
	} else {
		for _, p := range preferredResolutions {
			if name, ok := allowed(p); ok {
				choice = name
				break
			}
		}
	}
	return map[string]interface{}{"resolution": map[string]string{"name": choice}}
}

// assigneeValue builds the assignee field: {"name": u} on Server/DC (v2),
// {"accountId": u} on Cloud (v3).
func (m *jiraFieldMapper) assigneeValue(user string) map[string]interface{} {
	if m.apiVersion == "2" {
		return map[string]interface{}{"name": user}
	}
	return map[string]interface{}{"accountId": user}
}

// jiraLabels returns labels to send to Jira, without the push marker or
// jira.local_labels matches.
func (m *jiraFieldMapper) jiraLabels(labels []string) []string {
	if m.pushLabel == "" && len(m.localLabels) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if !isLocalLabel(l, m.pushLabel, m.localLabels) {
			out = append(out, l)
		}
	}
	return out
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
