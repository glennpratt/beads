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

// Pushing hierarchy and link corrections. A pull records what Jira had
// (jira_parents, jira_links); a push compares the bead's local parent-child,
// blocks, related and duplicates dependencies to Jira-linked beads against
// those records and sends only local changes:
//
//   - a parent added/changed/removed locally sets or clears Epic Link
//     (Parent Link for an Epic; parent on Cloud). Sub-task re-parenting
//     needs Jira's Move and is reported, not sent.
//   - a link added locally is created; one removed locally is deleted.
//
// Recorded links beads never materialized (a pair already linked by another
// type, or blocking within one hierarchy, which beads rejects) are not
// treated as local removals, so pushing never deletes them from Jira.

// pushLinkTypes are the Jira link types used to create links per dependency
// type (jira.push_link_type.<blocks|related|duplicates> overrides).
var pushLinkTypes = map[types.DependencyType]string{
	types.DepBlocks:     "Blocks",
	types.DepRelated:    "Relates",
	types.DepDuplicates: "Duplicate",
}

type linkOp struct {
	dep tracker.DependencyInfo
	sig string
}

type relationChanges struct {
	comments     []*types.Comment // local comments not yet pushed
	closeComment string           // close reason to post with a close transition

	parentChanged bool
	newParent     string // "" clears
	parentField   string // custom field ID, or "parent" (Cloud)
	addLinks      []linkOp
	removeLinks   []linkOp
	warnings      []string
}

func (rc *relationChanges) empty() bool {
	return rc == nil || (!rc.parentChanged && len(rc.addLinks) == 0 && len(rc.removeLinks) == 0 && len(rc.comments) == 0)
}

// describe lists the changes for previews, e.g. "epic link KPP-9, +blocks KPP-2<-KPP-1".
func (rc *relationChanges) describe() []string {
	if rc == nil {
		return nil
	}
	var out []string
	if rc.parentChanged {
		label := "parent"
		if rc.parentField != "parent" {
			label = "epic/parent link"
		}
		if rc.newParent == "" {
			out = append(out, label+" cleared")
		} else {
			out = append(out, label+" "+rc.newParent)
		}
	}
	for _, op := range rc.addLinks {
		out = append(out, "+"+describeLink(op.dep))
	}
	for _, op := range rc.removeLinks {
		out = append(out, "-"+describeLink(op.dep))
	}
	for _, c := range rc.comments {
		out = append(out, "+comment: "+snippet(c.Text))
	}
	if rc.closeComment != "" {
		out = append(out, "close comment: "+snippet(rc.closeComment))
	}
	return append(out, rc.warnings...)
}

func describeLink(d tracker.DependencyInfo) string {
	switch types.DependencyType(d.Type) {
	case types.DepBlocks:
		return fmt.Sprintf("%s blocks %s", d.ToExternalID, d.FromExternalID)
	case types.DepDuplicates:
		return fmt.Sprintf("%s duplicates %s", d.FromExternalID, d.ToExternalID)
	}
	return fmt.Sprintf("%s relates to %s", d.FromExternalID, d.ToExternalID)
}

// stringListMeta reads a []string metadata value as stored.
func stringListMeta(raw json.RawMessage, key string) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	list, ok := m[key].([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out, true
}

// jiraKeyOf returns a bead's Jira key, or "" if it is not Jira-linked.
func (t *Tracker) jiraKeyOf(issue *types.Issue) string {
	if issue == nil || issue.ExternalRef == nil || !t.IsExternalRef(*issue.ExternalRef) {
		return ""
	}
	return strings.ToUpper(ExtractJiraKey(*issue.ExternalRef))
}

// relationDiff computes hierarchy and link changes to push for a linked bead.
// Beads without pull records (never pulled since this was added) push none.
func (t *Tracker) relationDiff(ctx context.Context, local *types.Issue) *relationChanges {
	this := t.jiraKeyOf(local)
	if this == "" || t.store == nil {
		return &relationChanges{}
	}
	recParents, haveParents := stringListMeta(local.Metadata, jiraParentsMetadataKey)
	for i, p := range recParents {
		recParents[i] = strings.ToUpper(p)
	}
	recLinks, haveLinks := stringListMeta(local.Metadata, jiraLinksMetadataKey)
	rc := t.relationChangesFor(ctx, local, this, recParents, haveParents, recLinks, haveLinks, false)
	rc.comments = t.pendingComments(ctx, local)
	return rc
}

// createRelations returns the links to create (and comments to post) for a
// bead just created in Jira as key: every local link to a Jira-linked bead,
// since nothing has been recorded yet and the other end has no record of
// it either. The parent is placed by the create itself.
func (t *Tracker) createRelations(ctx context.Context, local *types.Issue, key string) *relationChanges {
	if t.store == nil {
		return &relationChanges{}
	}
	rc := t.relationChangesFor(ctx, local, strings.ToUpper(key), nil, false, []string{}, true, true)
	rc.comments = t.pendingComments(ctx, local)
	return rc
}

// relationChangesFor diffs local relationships of the bead (as Jira key this)
// against the given records. ownAll creates links regardless of which end
// would normally own them (used for new issues).
func (t *Tracker) relationChangesFor(ctx context.Context, local *types.Issue, this string, recParents []string, haveParents bool, recLinks []string, haveLinks, ownAll bool) *relationChanges {
	rc := &relationChanges{}
	if !haveParents && !haveLinks {
		return rc
	}

	outgoing, err := t.store.GetDependenciesWithMetadata(ctx, local.ID)
	if err != nil {
		debug.Logf("jira: relation diff %s: %v\n", local.ID, err)
		return rc
	}
	incoming, err := t.store.GetDependentsWithMetadata(ctx, local.ID)
	if err != nil {
		debug.Logf("jira: relation diff %s: %v\n", local.ID, err)
		return rc
	}

	recordedLinkSigs := map[string]bool{}
	for _, sig := range recLinks {
		recordedLinkSigs[sig] = true
	}
	var localParents []string
	parentBeads := map[string]*types.IssueWithDependencyMetadata{}
	localLinks := map[string]tracker.DependencyInfo{}
	addLink := func(from, to string, typ types.DependencyType) {
		if typ == types.DepRelated && from > to {
			from, to = to, from
		}
		d := tracker.DependencyInfo{FromExternalID: from, ToExternalID: to, Type: string(typ), Source: tracker.DependencySourceRelation}
		localLinks[linkSignature(d)] = d
	}
	for _, d := range outgoing {
		other := t.jiraKeyOf(&d.Issue)
		if other == "" {
			continue // personal/local-only bead: never pushed
		}
		switch d.DependencyType {
		case types.DepParentChild:
			if recordedLinkSigs[string(types.DepParentChild)+"|"+this+"|"+other] {
				continue // from a Jira "Parent-Child" link, not Epic/Parent Link
			}
			localParents = append(localParents, other)
			parentBeads[other] = d
		case types.DepBlocks, types.DepRelated, types.DepDuplicates:
			addLink(this, other, d.DependencyType)
		}
	}
	for _, d := range incoming {
		other := t.jiraKeyOf(&d.Issue)
		if other == "" {
			continue
		}
		switch d.DependencyType {
		case types.DepBlocks, types.DepRelated, types.DepDuplicates:
			addLink(other, this, d.DependencyType)
		}
	}

	if haveParents {
		t.parentChange(ctx, local, rc, localParents, recParents, parentBeads)
	}
	if haveLinks {
		recorded := map[string]bool{}
		for _, sig := range recLinks {
			recorded[sig] = true
		}
		// The owning end acts, so a link is created or deleted once.
		owns := func(d tracker.DependencyInfo) bool { return ownAll || strings.EqualFold(d.FromExternalID, this) }
		for sig, d := range localLinks {
			if !recorded[sig] && owns(d) {
				rc.addLinks = append(rc.addLinks, linkOp{dep: d, sig: sig})
			}
		}
		for _, sig := range recLinks {
			if _, ok := localLinks[sig]; ok {
				continue
			}
			parts := strings.SplitN(sig, "|", 3)
			if len(parts) != 3 {
				continue
			}
			d := tracker.DependencyInfo{FromExternalID: parts[1], ToExternalID: parts[2], Type: parts[0], Source: tracker.DependencySourceRelation}
			if owns(d) && t.locallyRemoved(ctx, d) {
				rc.removeLinks = append(rc.removeLinks, linkOp{dep: d, sig: sig})
			}
		}
		sort.Slice(rc.addLinks, func(i, j int) bool { return rc.addLinks[i].sig < rc.addLinks[j].sig })
		sort.Slice(rc.removeLinks, func(i, j int) bool { return rc.removeLinks[i].sig < rc.removeLinks[j].sig })
	}
	return rc
}

// parentChange compares local Jira parents with the recorded ones.
func (t *Tracker) parentChange(ctx context.Context, local *types.Issue, rc *relationChanges, localParents, recParents []string, beads map[string]*types.IssueWithDependencyMetadata) {
	inLocal, inRec := map[string]bool{}, map[string]bool{}
	for _, p := range localParents {
		inLocal[p] = true
	}
	for _, p := range recParents {
		inRec[p] = true
	}
	var added, removed []string
	for p := range inLocal {
		if !inRec[p] {
			added = append(added, p)
		}
	}
	for p := range inRec {
		if !inLocal[p] {
			removed = append(removed, p)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	sort.Strings(added)
	if len(added) > 1 {
		rc.warnings = append(rc.warnings, fmt.Sprintf("several new Jira parents %v; parent not changed", added))
		return
	}
	if len(added) == 0 && len(inLocal) > 0 {
		return // a recorded parent was dropped but another remains: nothing to set
	}

	thisIsEpic := local.IssueType == types.TypeEpic || strings.EqualFold(metadataString(local.Metadata, "jira_type"), "Epic")
	if t.isSubtaskType(ctx, metadataString(local.Metadata, "jira_type")) {
		rc.warnings = append(rc.warnings, "sub-task parent changes need Jira's Move; not sent")
		return
	}
	t.resolveHierarchyFields(ctx)
	newParent := ""
	if len(added) == 1 {
		newParent = added[0]
		p := beads[newParent]
		parentIsEpic := p != nil && (p.IssueType == types.TypeEpic || strings.EqualFold(metadataString(p.Metadata, "jira_type"), "Epic"))
		if !thisIsEpic && !parentIsEpic {
			rc.warnings = append(rc.warnings, fmt.Sprintf("new parent %s is not an epic; parent not changed", newParent))
			return
		}
	}
	switch {
	case thisIsEpic && t.parentLinkField != "":
		rc.parentField = t.parentLinkField
	case !thisIsEpic && t.epicLinkField != "":
		rc.parentField = t.epicLinkField
	case t.epicLinkField == "" && t.parentLinkField == "":
		rc.parentField = "parent" // Cloud
	default:
		rc.warnings = append(rc.warnings, "no Jira field for this parent change; not sent")
		return
	}
	rc.parentChanged, rc.newParent = true, newParent
}

// locallyRemoved reports whether a recorded link missing locally was
// removed by the user, rather than never created by beads: if the pair has
// another dependency, or a blocking link joins an issue to its own
// ancestor (which beads rejects), the absence is not a removal.
func (t *Tracker) locallyRemoved(ctx context.Context, d tracker.DependencyInfo) bool {
	from := t.beadByKey(ctx, d.FromExternalID)
	to := t.beadByKey(ctx, d.ToExternalID)
	if from == nil || to == nil {
		return false
	}
	for _, pair := range [][2]string{{from.ID, to.ID}, {to.ID, from.ID}} {
		deps, err := t.store.GetDependenciesWithMetadata(ctx, pair[0])
		if err != nil {
			return false
		}
		for _, x := range deps {
			if x.ID == pair[1] {
				return false
			}
		}
	}
	if types.DependencyType(d.Type) == types.DepBlocks && (t.isAncestor(ctx, from.ID, to.ID) || t.isAncestor(ctx, to.ID, from.ID)) {
		return false
	}
	return true
}

func (t *Tracker) beadByKey(ctx context.Context, key string) *types.Issue {
	if t.jiraURL == "" {
		return nil
	}
	issue, err := t.store.GetIssueByExternalRef(ctx, t.jiraURL+"/browse/"+strings.ToUpper(key))
	if err != nil {
		return nil
	}
	return issue
}

// isAncestor reports whether anc is a parent-child ancestor of id.
func (t *Tracker) isAncestor(ctx context.Context, anc, id string) bool {
	seen := map[string]bool{}
	frontier := []string{id}
	for depth := 0; depth < 10 && len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			deps, err := t.store.GetDependenciesWithMetadata(ctx, cur)
			if err != nil {
				return false
			}
			for _, d := range deps {
				if d.DependencyType != types.DepParentChild || seen[d.ID] {
					continue
				}
				if d.ID == anc {
					return true
				}
				seen[d.ID] = true
				next = append(next, d.ID)
			}
		}
		frontier = next
	}
	return false
}

// applyRelationLinks creates and deletes links for externalID. The parent
// change is sent with the field update by UpdateIssue.
func (t *Tracker) applyRelationLinks(ctx context.Context, externalID string, rc *relationChanges) error {
	if len(rc.removeLinks) > 0 {
		current, err := t.client.GetIssue(ctx, externalID)
		if err != nil {
			return err
		}
		mapper := &jiraFieldMapper{linkMap: t.linkMap}
		for _, op := range rc.removeLinks {
			id := ""
			for _, l := range current.Fields.IssueLinks {
				one := &Issue{Key: current.Key, Fields: IssueFields{IssueLinks: []IssueLink{l}}}
				for _, d := range mapper.linkDependencies(one) {
					if linkSignature(d) == op.sig {
						id = l.ID
					}
				}
			}
			if id == "" {
				debug.Logf("jira: link %s already gone from %s\n", op.sig, externalID)
				continue
			}
			if err := t.client.DeleteIssueLink(ctx, id); err != nil {
				return err
			}
		}
	}
	var failures []string
	var added []linkOp
	for _, op := range rc.addLinks {
		if err := t.createLink(ctx, op.dep); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		added = append(added, op)
	}
	rc.addLinks = added // only links that exist are recorded; failed ones retry next push
	if len(failures) > 0 {
		return &linkFailures{failures}
	}
	return nil
}

// linkFailures reports links that could not be created; the rest were.
type linkFailures struct{ msgs []string }

func (e *linkFailures) Error() string { return strings.Join(e.msgs, "; ") }

// createLink creates the Jira link for a dependency. The link type's kind
// decides direction (Jira's REST API puts the outward description on the
// inward issue). A blocking link refused for lack of permission on the
// blocker (e.g. a service-desk project) is retried as the waiting issue's
// "depends on" link (jira.push_link_type.blocks_fallback, default
// "Dependent"), which needs permission only on the waiting issue.
func (t *Tracker) createLink(ctx context.Context, d tracker.DependencyInfo) error {
	typ := types.DependencyType(d.Type)
	create := func(typeName string) error {
		inward, outward := d.FromExternalID, d.ToExternalID
		switch linkKind(t.linkMap, typeName) {
		case linkBlocks: // inward blocks outward: the blocker (To) is inward
			inward, outward = d.ToExternalID, d.FromExternalID
		case linkParent: // inward is the parent of outward
			inward, outward = d.ToExternalID, d.FromExternalID
		}
		return t.client.CreateIssueLink(ctx, typeName, inward, outward)
	}
	typeName := t.pushLinkType(ctx, typ)
	err := create(typeName)
	if err == nil || typ != types.DepBlocks || !isPermissionError(err) {
		return err
	}
	fallback := "Dependent"
	if t.store != nil {
		if v, _ := t.getConfig(ctx, "jira.push_link_type.blocks_fallback", ""); strings.TrimSpace(v) != "" {
			fallback = strings.TrimSpace(v)
		}
	}
	if strings.EqualFold(fallback, typeName) || linkKind(t.linkMap, fallback) != linkDependsOn {
		return err
	}
	debug.Logf("jira: %s %s %s refused (%v); retrying as %s\n", d.ToExternalID, typeName, d.FromExternalID, err, fallback)
	if ferr := create(fallback); ferr != nil {
		return fmt.Errorf("%v; fallback %s: %w", err, fallback, ferr)
	}
	return nil
}

func isPermissionError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "returned 401") || strings.Contains(msg, "returned 403") || strings.Contains(strings.ToLower(msg), "permission")
}

func (t *Tracker) pushLinkType(ctx context.Context, typ types.DependencyType) string {
	if t.store != nil {
		if v, _ := t.getConfig(ctx, "jira.push_link_type."+string(typ), ""); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return pushLinkTypes[typ]
}

// jiraCommentsMetadataKey lists the IDs of local comments already pushed.
const jiraCommentsMetadataKey = "jira_pushed_comments"

// pendingComments returns the bead's comments not yet pushed to Jira.
// Comments are shared (pushed); notes (bd note) stay local.
// jira.push_comments=false disables this.
func (t *Tracker) pendingComments(ctx context.Context, local *types.Issue) []*types.Comment {
	if v, _ := t.getConfig(ctx, "jira.push_comments", "JIRA_PUSH_COMMENTS"); strings.EqualFold(strings.TrimSpace(v), "false") {
		return nil
	}
	reader, ok := t.store.(tracker.CommentReader)
	if !ok {
		return nil
	}
	comments, err := reader.GetIssueComments(ctx, local.ID)
	if err != nil {
		debug.Logf("jira: reading comments of %s: %v\n", local.ID, err)
		return nil
	}
	pushed, _ := stringListMeta(local.Metadata, jiraCommentsMetadataKey)
	done := map[string]bool{}
	for _, id := range pushed {
		done[id] = true
	}
	var out []*types.Comment
	for _, c := range comments {
		if c != nil && !done[c.ID] && strings.TrimSpace(c.Text) != "" {
			out = append(out, c)
		}
	}
	return out
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// applyComments posts pending comments to externalID.
func (t *Tracker) applyComments(ctx context.Context, externalID string, rc *relationChanges) error {
	for _, c := range rc.comments {
		if err := t.client.AddComment(ctx, externalID, c.Text); err != nil {
			return err
		}
	}
	if rc.closeComment != "" {
		if err := t.client.AddComment(ctx, externalID, rc.closeComment); err != nil {
			return err
		}
	}
	return nil
}

// recordPushedRelations updates the bead's pull records after a push so a
// repeated push sends nothing until the next pull refreshes them.
func (t *Tracker) recordPushedRelations(ctx context.Context, local *types.Issue, rc *relationChanges) {
	if (rc.empty() && rc.closeComment == "") || t.store == nil {
		return
	}
	meta := map[string]interface{}{}
	if len(local.Metadata) > 0 && json.Unmarshal(local.Metadata, &meta) != nil {
		return
	}
	if rc.parentChanged {
		if rc.newParent == "" {
			meta[jiraParentsMetadataKey] = []string{}
		} else {
			meta[jiraParentsMetadataKey] = []string{rc.newParent}
		}
	}
	links, _ := stringListMeta(local.Metadata, jiraLinksMetadataKey)
	set := map[string]bool{}
	for _, s := range links {
		set[s] = true
	}
	for _, op := range rc.addLinks {
		set[op.sig] = true
	}
	for _, op := range rc.removeLinks {
		delete(set, op.sig)
	}
	record := make([]string, 0, len(set))
	for s := range set {
		record = append(record, s)
	}
	sort.Strings(record)
	if _, had := stringListMeta(local.Metadata, jiraLinksMetadataKey); had || len(rc.addLinks)+len(rc.removeLinks) > 0 {
		meta[jiraLinksMetadataKey] = record
	}
	if rc.closeComment != "" {
		meta[jiraCloseCommentMetadataKey] = fieldHash(normalizeText(rc.closeComment))
	}
	if len(rc.comments) > 0 {
		ids, _ := stringListMeta(local.Metadata, jiraCommentsMetadataKey)
		for _, c := range rc.comments {
			ids = append(ids, c.ID)
		}
		meta[jiraCommentsMetadataKey] = ids
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return
	}
	if err := t.store.UpdateIssue(ctx, local.ID, map[string]interface{}{"metadata": json.RawMessage(raw)}, "jira-sync"); err != nil {
		debug.Logf("jira: recording pushed relations for %s: %v\n", local.ID, err)
	}
}
