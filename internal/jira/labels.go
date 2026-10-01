package jira

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// jiraLabelsMetadataKey records, in a bead's metadata, the Jira labels seen
// on its last pull. Pulls use it to tell labels that came from Jira (and may
// be removed when Jira removes them) from labels added locally (kept).
const jiraLabelsMetadataKey = "jira_labels"

// parseLabelPatterns splits jira.local_labels into lowercase glob patterns
// (path.Match syntax, e.g. "q4-*", "me:*", "focus").
func parseLabelPatterns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isLocalLabel reports whether a label never syncs with Jira: the push
// marker or a jira.local_labels pattern match.
func isLocalLabel(label, pushLabel string, patterns []string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		return true
	}
	if pushLabel != "" && l == strings.ToLower(pushLabel) {
		return true
	}
	for _, p := range patterns {
		if ok, err := path.Match(p, l); err == nil && ok {
			return true
		}
	}
	return false
}

// MergePulledLabels is a pull AfterConvert step. Jira is authoritative for
// labels it owns, but labels added locally survive re-pulls:
//
//	result = Jira's labels
//	       + existing labels that were not Jira labels on the last pull
//	       + existing labels matching jira.local_labels / the push label
//
// so a label removed in Jira is removed locally, while personal labels stay.
// Jira's labels are recorded in the bead's metadata (merged into, not
// replacing, any existing metadata). Beads pulled before this was recorded
// keep all their existing labels.
func (t *Tracker) MergePulledLabels(extIssue *tracker.TrackerIssue, conv *tracker.IssueConversion, existing *types.Issue) {
	if conv == nil || conv.Issue == nil {
		return
	}
	jiraNow := conv.Issue.Labels

	meta := map[string]interface{}{}
	var lastJira map[string]bool // nil: no record of the last pull
	if existing != nil && len(existing.Metadata) > 0 {
		if err := json.Unmarshal(existing.Metadata, &meta); err != nil {
			debug.Logf("jira: %s metadata is not an object, leaving it: %v\n", existing.ID, err)
			meta = nil
		}
		if raw, ok := meta[jiraLabelsMetadataKey].([]interface{}); ok {
			lastJira = make(map[string]bool, len(raw))
			for _, v := range raw {
				if s, ok := v.(string); ok {
					lastJira[strings.ToLower(s)] = true
				}
			}
		}
	}

	merged := append([]string(nil), jiraNow...)
	seen := make(map[string]bool, len(jiraNow))
	for _, l := range jiraNow {
		seen[strings.ToLower(l)] = true
	}
	if existing != nil {
		for _, l := range existing.Labels {
			key := strings.ToLower(l)
			if seen[key] {
				continue
			}
			if lastJira == nil || !lastJira[key] || isLocalLabel(l, t.pushLabel, t.localLabels) {
				merged = append(merged, l)
				seen[key] = true
			}
		}
	}
	conv.Issue.Labels = merged

	if meta == nil || extIssue == nil {
		return
	}
	recorded := jiraNow
	if recorded == nil {
		recorded = []string{}
	}
	meta[jiraLabelsMetadataKey] = recorded
	extIssue.Metadata = meta
}

// jiraPulledMetadataKey records, per pushable field, a short hash of the
// value Jira had on the last pull. It lets a pull tell a pending local edit
// (local differs from the record, Jira does not) from a Jira change.
const jiraPulledMetadataKey = "jira_pulled"

// pulledFields are the bead fields a pull overwrites and a push can send.
var pulledFields = []struct {
	name string
	get  func(*types.Issue) string
	set  func(dst, src *types.Issue)
}{
	{"title", func(i *types.Issue) string { return strings.TrimSpace(i.Title) }, func(d, s *types.Issue) { d.Title = s.Title }},
	{"description", func(i *types.Issue) string { return normalizeText(i.Description) }, func(d, s *types.Issue) { d.Description = s.Description }},
	{"issue_type", func(i *types.Issue) string { return string(i.IssueType) }, func(d, s *types.Issue) { d.IssueType = s.IssueType }},
	{"priority", func(i *types.Issue) string { return strconv.Itoa(i.Priority) }, func(d, s *types.Issue) { d.Priority = s.Priority }},
	{"status", func(i *types.Issue) string { return string(i.Status) }, func(d, s *types.Issue) { d.Status = s.Status }},
}

func fieldHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:8])
}

// MergePulled applies MergePulledLabels and keeps pending local edits to
// pushable fields. For each field, using the hash recorded at the last pull:
// a local-only change is kept (so a later push sends it), a Jira-only change
// is taken, and when both changed differently Jira wins with a warning (the
// local value remains in the bead's history). Without a record, Jira wins.
func (t *Tracker) MergePulled(extIssue *tracker.TrackerIssue, conv *tracker.IssueConversion, existing *types.Issue) []string {
	t.MergePulledLabels(extIssue, conv, existing)
	if conv == nil || conv.Issue == nil || extIssue == nil {
		return nil
	}
	meta := extIssue.Metadata
	if meta == nil {
		return nil // non-object local metadata: leave fields to Jira
	}

	var last map[string]interface{}
	if existing != nil && len(existing.Metadata) > 0 {
		var m map[string]interface{}
		if json.Unmarshal(existing.Metadata, &m) == nil {
			last, _ = m[jiraPulledMetadataKey].(map[string]interface{})
		}
	}

	var warnings []string
	record := make(map[string]interface{}, len(pulledFields))
	for _, f := range pulledFields {
		remote := f.get(conv.Issue)
		record[f.name] = fieldHash(remote)
		if existing == nil || last == nil {
			continue
		}
		stored, ok := last[f.name].(string)
		if !ok {
			continue
		}
		local := f.get(existing)
		if local == remote || fieldHash(local) == stored {
			continue // in sync, or no local edit: take Jira's value
		}
		if fieldHash(remote) == stored {
			f.set(conv.Issue, existing) // pending local edit, Jira unchanged
			continue
		}
		warnings = append(warnings, fmt.Sprintf("%s: %s changed both locally and in Jira; kept Jira's (local value is in bd history)", existing.ID, f.name))
	}
	meta[jiraPulledMetadataKey] = record
	return warnings
}
