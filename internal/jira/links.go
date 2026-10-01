package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// Issue links become beads dependencies. Each Jira link type maps to a kind
// (jira.link_map.<Type> overrides, case-insensitive):
//
//	blocks      outward issue blocks inward issue   ("A blocks B")
//	depends_on  outward issue depends on inward one ("A depends on B")
//	duplicates  outward duplicates inward
//	parent      outward is the parent of inward
//	related     non-blocking relation (default for unlisted types)
//	ignore      not imported
const (
	linkBlocks     = "blocks"
	linkDependsOn  = "depends_on"
	linkDuplicates = "duplicates"
	linkParent     = "parent"
	linkRelated    = "related"
	linkIgnore     = "ignore"
)

var defaultLinkKinds = map[string]string{
	"blocks":              linkBlocks,
	"externally blocks":   linkBlocks,
	"gantt dependency":    linkBlocks,
	"gantt end to start":  linkBlocks,
	"issue chaining":      linkBlocks,
	"dependent":           linkDependsOn,
	"dependency":          linkDependsOn,
	"external dependency": linkDependsOn,
	"duplicate":           linkDuplicates,
	"parent-child":        linkParent,
}

// IssueLink is an entry of the issuelinks field.
type IssueLink struct {
	ID   string `json:"id,omitempty"`
	Type struct {
		Name    string `json:"name"`
		Inward  string `json:"inward"`
		Outward string `json:"outward"`
	} `json:"type"`
	InwardIssue  *LinkedIssue `json:"inwardIssue,omitempty"`
	OutwardIssue *LinkedIssue `json:"outwardIssue,omitempty"`
}

// LinkedIssue is the other end of an issue link.
type LinkedIssue struct {
	Key string `json:"key"`
}

// linkKind returns the kind for a Jira link type name.
func linkKind(linkMap map[string]string, typeName string) string {
	name := strings.ToLower(strings.TrimSpace(typeName))
	if k, ok := linkMap[name]; ok {
		return k
	}
	if k, ok := defaultLinkKinds[name]; ok {
		return k
	}
	return linkRelated
}

// linkDependencies converts an issue's links to dependencies, oriented so
// that the same link read from either end yields the same edge.
func (m *jiraFieldMapper) linkDependencies(ji *Issue) []tracker.DependencyInfo {
	var deps []tracker.DependencyInfo
	add := func(from, to string, t types.DependencyType) {
		deps = append(deps, tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: string(t), Source: tracker.DependencySourceRelation})
	}
	for _, l := range ji.Fields.IssueLinks {
		// outward: "this <outward text> other"; inward: "other <outward text> this".
		this, other := ji.Key, ""
		src, dst := this, other
		switch {
		case l.OutwardIssue != nil && l.OutwardIssue.Key != "":
			other = l.OutwardIssue.Key
			src, dst = this, other
		case l.InwardIssue != nil && l.InwardIssue.Key != "":
			other = l.InwardIssue.Key
			src, dst = other, this
		default:
			continue
		}
		if strings.EqualFold(other, this) {
			continue
		}
		switch linkKind(m.linkMap, l.Type.Name) {
		case linkBlocks: // src blocks dst: dst depends on src
			add(dst, src, types.DepBlocks)
		case linkDependsOn: // src depends on dst
			add(src, dst, types.DepBlocks)
		case linkDuplicates: // src duplicates dst
			add(src, dst, types.DepDuplicates)
		case linkParent: // src is parent of dst
			add(dst, src, types.DepParentChild)
		case linkIgnore:
		default: // related: symmetric, canonical order
			a, b := strings.ToUpper(this), strings.ToUpper(other)
			if a > b {
				a, b = b, a
			}
			add(a, b, types.DepRelated)
		}
	}
	return deps
}

// jiraLinksMetadataKey records the link dependencies seen on the last pull
// as "type|from|to" so links removed in Jira can be removed locally.
const jiraLinksMetadataKey = "jira_links"

func linkSignature(d tracker.DependencyInfo) string {
	return d.Type + "|" + strings.ToUpper(d.FromExternalID) + "|" + strings.ToUpper(d.ToExternalID)
}

// reconcileLinks records this pull's link dependencies and asks the engine
// to remove those recorded on the last pull that Jira no longer has. Links
// added locally were never recorded, so they are kept.
func reconcileLinks(conv *tracker.IssueConversion, existing *types.Issue, meta map[string]interface{}) {
	current := make(map[string]bool)
	var record []string
	for _, d := range conv.Dependencies {
		if d.Source != tracker.DependencySourceRelation {
			continue
		}
		sig := linkSignature(d)
		if !current[sig] {
			current[sig] = true
			record = append(record, sig)
		}
	}
	sort.Strings(record)

	if existing != nil && len(existing.Metadata) > 0 {
		var m map[string]interface{}
		if json.Unmarshal(existing.Metadata, &m) == nil {
			if last, ok := m[jiraLinksMetadataKey].([]interface{}); ok {
				for _, v := range last {
					sig, _ := v.(string)
					parts := strings.SplitN(sig, "|", 3)
					if len(parts) != 3 || current[sig] {
						continue
					}
					conv.RemoveDependencies = append(conv.RemoveDependencies, tracker.DependencyInfo{
						FromExternalID: parts[1], ToExternalID: parts[2], Type: parts[0], Source: tracker.DependencySourceRelation,
					})
				}
			}
		}
	}
	if record == nil {
		record = []string{}
	}
	meta[jiraLinksMetadataKey] = record
}

// localJiraKeys returns the Jira keys of linked local beads.
func (t *Tracker) localJiraKeys(ctx context.Context) (map[string]bool, error) {
	keys := make(map[string]bool)
	if t.store == nil {
		return keys, nil
	}
	local, err := t.store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, err
	}
	for _, issue := range local {
		if issue == nil || issue.ExternalRef == nil || !t.IsExternalRef(*issue.ExternalRef) {
			continue
		}
		if k := strings.ToUpper(ExtractJiraKey(*issue.ExternalRef)); k != "" {
			keys[k] = true
		}
	}
	return keys, nil
}

// linkedKeysToFollow returns keys linked from fetched issues that are
// neither fetched nor already local (one hop; their own links are not
// followed further). jira.follow_links=false disables this.
func (t *Tracker) linkedKeysToFollow(ctx context.Context, fetched []Issue, fetchedKeys, local map[string]bool) []string {
	if v, _ := t.getConfig(ctx, "jira.follow_links", "JIRA_FOLLOW_LINKS"); strings.EqualFold(strings.TrimSpace(v), "false") {
		return nil
	}
	want := make(map[string]bool)
	for _, i := range fetched {
		for _, l := range i.Fields.IssueLinks {
			for _, li := range []*LinkedIssue{l.InwardIssue, l.OutwardIssue} {
				if li == nil || li.Key == "" {
					continue
				}
				k := strings.ToUpper(li.Key)
				if !fetchedKeys[k] && !local[k] && linkKind(t.linkMap, l.Type.Name) != linkIgnore {
					want[k] = true
				}
			}
		}
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// missingKeyRE matches keys in Jira's "An issue with key 'X-1' does not
// exist" errors.
var missingKeyRE = regexp.MustCompile(`key '([A-Za-z][A-Za-z0-9_]*-\d+)' does not exist`)

// searchKeysWhere fetches issues by key in chunks, adding an optional JQL
// condition. Keys Jira reports as nonexistent (deleted, or no permission)
// are dropped and the chunk retried, so one stale bead cannot break a pull.
func (t *Tracker) searchKeysWhere(ctx context.Context, keys []string, cond string) ([]Issue, error) {
	var out []Issue
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := append([]string(nil), keys[start:end]...)
		for attempt := 0; attempt < 3 && len(chunk) > 0; attempt++ {
			jql := "key in (" + strings.Join(chunk, ", ") + ")"
			if cond != "" {
				jql += " AND " + cond
			}
			issues, err := t.client.SearchIssues(ctx, jql)
			if err == nil {
				out = append(out, issues...)
				break
			}
			missing := missingKeyRE.FindAllStringSubmatch(err.Error(), -1)
			if len(missing) == 0 || attempt == 2 {
				return out, err
			}
			drop := make(map[string]bool, len(missing))
			for _, m := range missing {
				drop[strings.ToUpper(m[1])] = true
			}
			debug.Logf("jira: skipping %d keys Jira reports missing: %v\n", len(drop), drop)
			kept := chunk[:0]
			for _, k := range chunk {
				if !drop[strings.ToUpper(k)] {
					kept = append(kept, k)
				}
			}
			chunk = kept
		}
	}
	return out, nil
}

// refreshTrackedKeys returns local Jira-linked issues outside this pull's
// results that changed since the last sync: issues pulled via a link or
// that left every scope (e.g. a ticket that left a board) stay current.
func (t *Tracker) refreshTrackedKeys(ctx context.Context, since time.Time, local, fetchedKeys map[string]bool) ([]Issue, error) {
	var keys []string
	for k := range local {
		if !fetchedKeys[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	issues, err := t.searchKeysWhere(ctx, keys, updatedSinceJQL(since, time.Now()))
	if err != nil {
		return nil, fmt.Errorf("refreshing tracked issues: %w", err)
	}
	debug.Logf("jira: %d tracked issues outside the pull scope changed\n", len(issues))
	return issues, nil
}

// strongestPerPair keeps one dependency per issue pair (either direction):
// parent-child, then blocks, then duplicates, then related. A pair holds a
// single dependency in beads, and several Jira links (or a link plus an Epic
// Link) between the same two issues are common.
func strongestPerPair(deps []tracker.DependencyInfo) []tracker.DependencyInfo {
	rank := func(t string) int {
		switch types.DependencyType(t) {
		case types.DepParentChild:
			return 4
		case types.DepBlocks:
			return 3
		case types.DepDuplicates:
			return 2
		}
		return 1
	}
	pair := func(d tracker.DependencyInfo) string {
		a, b := strings.ToUpper(d.FromExternalID), strings.ToUpper(d.ToExternalID)
		if a > b {
			a, b = b, a
		}
		return a + "|" + b
	}
	best := make(map[string]int)
	for i, d := range deps {
		if j, ok := best[pair(d)]; !ok || rank(d.Type) > rank(deps[j].Type) {
			best[pair(d)] = i
		}
	}
	var out []tracker.DependencyInfo
	for i, d := range deps {
		if best[pair(d)] == i {
			out = append(out, d)
		}
	}
	return out
}
