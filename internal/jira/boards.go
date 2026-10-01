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

// Pull scopes and boards.
//
//	jira.scope.<name> = <JQL>   an extra pull scope (full JQL, no project prefix)
//	jira.board.<name> = <id>    a Jira Software board: its filter is a pull
//	                            scope, and membership is mirrored on each pull
//
// Board membership is labels plus metadata, rebuilt on every pull:
//
//	board:<name>    issue is shown on the board
//	backlog:<name>  issue is in the board's backlog
//	metadata jira_boards = {<name>: {column, rank, on_board}}
//	metadata jira_column / jira_rank from the first board (by name) with the issue
//
// These labels are local: never sent to Jira.

const (
	boardLabelPrefix   = "board:"
	backlogLabelPrefix = "backlog:"
)

type namedScope struct {
	name string
	jql  string
}

type boardSpec struct {
	name     string
	id       string
	cfg      *BoardConfig
	scopeJQL string
}

// boardEntry is an issue's place on one board.
type boardEntry struct {
	Column  string `json:"column,omitempty"`
	Rank    string `json:"rank,omitempty"`
	OnBoard bool   `json:"on_board"`
}

// loadScopeConfig reads jira.scope.* and jira.board.* from all config.
func (t *Tracker) loadScopeConfig(allConfig map[string]string) {
	t.scopes, t.boards = nil, nil
	for key, val := range allConfig {
		val = strings.TrimSpace(val)
		switch {
		case strings.HasPrefix(key, "jira.scope.") && val != "":
			t.scopes = append(t.scopes, namedScope{name: strings.TrimPrefix(key, "jira.scope."), jql: val})
		case strings.HasPrefix(key, "jira.board.") && val != "":
			t.boards = append(t.boards, &boardSpec{name: strings.TrimPrefix(key, "jira.board."), id: val})
		}
	}
	sort.Slice(t.scopes, func(i, j int) bool { return t.scopes[i].name < t.scopes[j].name })
	sort.Slice(t.boards, func(i, j int) bool { return t.boards[i].name < t.boards[j].name })
}

// resolveBoards loads each board's configuration and filter once.
func (t *Tracker) resolveBoards(ctx context.Context) error {
	if t.boardsResolved {
		return t.boardsErr
	}
	t.boardsResolved = true
	for _, b := range t.boards {
		cfg, err := t.client.GetBoardConfig(ctx, b.id)
		if err != nil {
			t.boardsErr = fmt.Errorf("board %s (%s): %w", b.name, b.id, err)
			return t.boardsErr
		}
		filterJQL, err := t.client.GetFilterJQL(ctx, cfg.Filter.ID)
		if err != nil {
			t.boardsErr = fmt.Errorf("board %s (%s): %w", b.name, b.id, err)
			return t.boardsErr
		}
		b.cfg = cfg
		b.scopeJQL = "(" + filterJQL + ")"
		if sub := strings.TrimSpace(cfg.SubQuery.Query); sub != "" {
			b.scopeJQL = "(" + b.scopeJQL + " AND (" + sub + "))"
		}
		debug.Logf("jira: board %s = %q (%s), scope %s\n", b.name, cfg.Name, cfg.Type, b.scopeJQL)
	}
	return nil
}

// scopeJQL returns the pull query (without state/since/order clauses): the
// project clause with jira.pull_jql (the original behavior, used alone when
// no scopes or boards are configured), OR-ed with each scope and board filter.
func (t *Tracker) scopeJQL(ctx context.Context) (string, error) {
	var parts []string
	pullJQL, _ := t.getConfig(ctx, "jira.pull_jql", "JIRA_PULL_JQL")
	if pullJQL != "" || (len(t.scopes) == 0 && len(t.boards) == 0) {
		clause := t.projectClause()
		if pullJQL != "" {
			clause += " AND (" + pullJQL + ")" // parenthesize: user JQL may contain OR
		}
		parts = append(parts, clause)
	}
	for _, s := range t.scopes {
		parts = append(parts, "("+s.jql+")")
	}
	if len(t.boards) > 0 {
		if err := t.resolveBoards(ctx); err != nil {
			return "", err
		}
		for _, b := range t.boards {
			parts = append(parts, b.scopeJQL)
		}
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

func (t *Tracker) projectClause() string {
	if len(t.projectKeys) == 1 {
		return fmt.Sprintf("project = %q", t.projectKeys[0])
	}
	quoted := make([]string, len(t.projectKeys))
	for i, k := range t.projectKeys {
		quoted[i] = fmt.Sprintf("%q", k)
	}
	return fmt.Sprintf("project IN (%s)", strings.Join(quoted, ", "))
}

// loadBoardMembership fetches every configured board's issues and backlog
// once per run and records each issue's column, rank and on-board state.
func (t *Tracker) loadBoardMembership(ctx context.Context) error {
	if t.membersLoaded {
		return t.membersErr
	}
	t.membersLoaded = true
	if err := t.resolveBoards(ctx); err != nil {
		t.membersErr = err
		return err
	}
	members := make(map[string]map[string]boardEntry)
	for _, b := range t.boards {
		fields := []string{"status"}
		rankField := b.cfg.RankField()
		if rankField != "" {
			fields = append(fields, rankField)
		}
		issues, err := t.client.GetBoardIssues(ctx, b.id, fields)
		if err != nil {
			t.membersErr = fmt.Errorf("board %s issues: %w", b.name, err)
			return t.membersErr
		}
		backlog, backlogCols := t.boardBacklog(ctx, b)
		for _, i := range issues {
			key := strings.ToUpper(i.Key)
			e := boardEntry{Rank: i.Fields.CustomFieldKey(rankField)}
			if i.Fields.Status != nil {
				e.Column = b.cfg.ColumnForStatus(i.Fields.Status.ID)
			}
			e.OnBoard = !backlog[key] && !backlogCols[e.Column]
			if members[key] == nil {
				members[key] = make(map[string]boardEntry)
			}
			members[key][b.name] = e
		}
		onBoard := 0
		for key := range members {
			if e, ok := members[key][b.name]; ok && e.OnBoard {
				onBoard++
			}
		}
		debug.Logf("jira: board %s: %d issues, %d on the board\n", b.name, len(issues), onBoard)
	}
	t.boardMembers = members
	return nil
}

// boardBacklog returns the board's backlog as issue keys (public backlog
// endpoint: scrum boards, Cloud kanban) or, for Server/DC kanban boards with
// the kanban backlog enabled, as backlog column names. A kanban board whose
// edit model is unavailable falls back to a first column named "Backlog".
func (t *Tracker) boardBacklog(ctx context.Context, b *boardSpec) (map[string]bool, map[string]bool) {
	keys, cols := map[string]bool{}, map[string]bool{}
	bl, err := t.client.GetBoardBacklog(ctx, b.id)
	if err == nil {
		for _, i := range bl {
			keys[strings.ToUpper(i.Key)] = true
		}
		return keys, cols
	}
	if !strings.EqualFold(b.cfg.Type, "kanban") {
		debug.Logf("jira: board %s backlog unavailable (%v); all issues count as on the board\n", b.name, err)
		return keys, cols
	}
	names, err := t.client.GetKanbanBacklogColumns(ctx, b.id)
	if err != nil {
		debug.Logf("jira: board %s kanban backlog lookup failed (%v); falling back to a first column named Backlog\n", b.name, err)
		if cs := b.cfg.ColumnConfig.Columns; len(cs) > 0 && strings.EqualFold(cs[0].Name, "Backlog") {
			names = []string{cs[0].Name}
		}
	}
	for _, n := range names {
		cols[n] = true
	}
	debug.Logf("jira: board %s backlog columns: %v\n", b.name, names)
	return keys, cols
}

// boardDriftKeys returns linked issues whose board membership (on board,
// backlog, column or rank) differs from what the bead records, and board
// members not yet pulled, excluding keys already fetched. Board-only changes
// such as rank or aging out of a filter do not bump the issue's updated
// time, so an incremental pull would otherwise miss them.
func (t *Tracker) boardDriftKeys(ctx context.Context, fetched map[string]bool) []string {
	if t.store == nil || t.boardMembers == nil {
		return nil
	}
	local, err := t.store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		debug.Logf("jira: board drift: listing local issues: %v\n", err)
		return nil
	}
	seen := make(map[string]bool)
	var drift []string
	consider := func(key, have string) {
		if key == "" || seen[key] || fetched[key] {
			return
		}
		seen[key] = true
		if have != boardSignature(t.boardMembers[key]) {
			drift = append(drift, key)
		}
	}
	for _, issue := range local {
		if issue == nil || issue.ExternalRef == nil || !t.IsExternalRef(*issue.ExternalRef) {
			continue
		}
		consider(strings.ToUpper(ExtractJiraKey(*issue.ExternalRef)), boardSignature(storedBoards(issue.Metadata)))
	}
	for key := range t.boardMembers {
		consider(key, "")
	}
	sort.Strings(drift)
	return drift
}

func storedBoards(raw json.RawMessage) map[string]boardEntry {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Boards map[string]boardEntry `json:"jira_boards"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Boards
}

func boardSignature(entries map[string]boardEntry) string {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		e := entries[n]
		fmt.Fprintf(&b, "%s|%t|%s|%s;", n, e.OnBoard, e.Column, e.Rank)
	}
	return b.String()
}

// applyBoardMembership adds board/backlog labels to the converted issue's
// Jira-owned labels and sets board metadata. If membership could not be
// loaded, the bead's existing board labels and metadata are kept.
func (t *Tracker) applyBoardMembership(ctx context.Context, extIssue *tracker.TrackerIssue, conv *tracker.IssueConversion, existing *types.Issue) {
	if len(t.boards) == 0 || extIssue == nil || conv == nil || conv.Issue == nil {
		return
	}
	if extIssue.Metadata == nil {
		extIssue.Metadata = map[string]interface{}{}
	}
	if err := t.loadBoardMembership(ctx); err != nil {
		debug.Logf("jira: keeping existing board membership: %v\n", err)
		if existing != nil {
			for _, l := range existing.Labels {
				if strings.HasPrefix(l, boardLabelPrefix) || strings.HasPrefix(l, backlogLabelPrefix) {
					conv.Issue.Labels = append(conv.Issue.Labels, l)
				}
			}
			if boards := storedBoards(existing.Metadata); boards != nil {
				extIssue.Metadata["jira_boards"] = boards
			}
		}
		return
	}

	entries := t.boardMembers[strings.ToUpper(extIssue.Identifier)]
	if len(entries) == 0 {
		extIssue.Metadata["jira_boards"] = nil
		extIssue.Metadata["jira_column"] = nil
		extIssue.Metadata["jira_rank"] = nil
		return
	}
	extIssue.Metadata["jira_boards"] = entries
	first := true
	for _, b := range t.boards { // sorted by name
		e, ok := entries[b.name]
		if !ok {
			continue
		}
		if e.OnBoard {
			conv.Issue.Labels = append(conv.Issue.Labels, boardLabelPrefix+b.name)
		} else {
			conv.Issue.Labels = append(conv.Issue.Labels, backlogLabelPrefix+b.name)
		}
		if first {
			extIssue.Metadata["jira_column"] = e.Column
			extIssue.Metadata["jira_rank"] = e.Rank
			first = false
		}
	}
}

// searchKeys fetches issues by key in chunks.
func (t *Tracker) searchKeys(ctx context.Context, keys []string) ([]Issue, error) {
	var out []Issue
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		issues, err := t.client.SearchIssues(ctx, "key in ("+strings.Join(keys[start:end], ", ")+")")
		if err != nil {
			return out, err
		}
		out = append(out, issues...)
	}
	return out, nil
}
