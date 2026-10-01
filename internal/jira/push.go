package jira

import (
	"context"
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
// "issuetype", "priority", "status") that pushing local would change on
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

	switch {
	case isEpic && t.parentLinkField != "":
		fields[t.parentLinkField] = p.key
	case !isEpic && p.issueType == types.TypeEpic && t.epicLinkField != "":
		fields[t.epicLinkField] = p.key
	case isEpic || p.issueType == types.TypeEpic:
		// Jira Cloud: the standard parent field covers every level.
		if t.epicLinkField == "" && t.parentLinkField == "" {
			fields["parent"] = map[string]string{"key": p.key}
		} else {
			warnings = append(warnings, fmt.Sprintf("no Jira field to link %s under %s; created without a parent", jiraType, p.key))
		}
	default:
		warnings = append(warnings, fmt.Sprintf("parent %s is a %s, not an epic; created without a parent link (Jira sub-tasks are not created automatically)", p.key, p.issueType))
	}
	return warnings
}

type jiraParent struct {
	key       string
	issueType types.IssueType
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
			parents = append(parents, jiraParent{key: key, issueType: d.IssueType})
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
	if labels, ok := fields["labels"].([]string); ok && len(labels) > 0 {
		parts = append(parts, "labels "+strings.Join(labels, ","))
	}
	for _, w := range warnings {
		parts = append(parts, "warning: "+w)
	}
	return strings.Join(parts, "; ")
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
