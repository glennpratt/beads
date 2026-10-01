package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// BoardConfig is the subset of a Jira Software board configuration used to
// derive board membership (GET /rest/agile/1.0/board/{id}/configuration).
type BoardConfig struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"` // "kanban" or "scrum"
	Filter struct {
		ID string `json:"id"`
	} `json:"filter"`
	SubQuery struct {
		Query string `json:"query"`
	} `json:"subQuery"`
	Ranking struct {
		RankCustomFieldID int `json:"rankCustomFieldId"`
	} `json:"ranking"`
	ColumnConfig struct {
		Columns []struct {
			Name     string `json:"name"`
			Statuses []struct {
				ID string `json:"id"`
			} `json:"statuses"`
		} `json:"columns"`
	} `json:"columnConfig"`
}

// RankField returns the rank custom field ID (e.g. "customfield_13900"), or "".
func (b *BoardConfig) RankField() string {
	if b.Ranking.RankCustomFieldID == 0 {
		return ""
	}
	return fmt.Sprintf("customfield_%d", b.Ranking.RankCustomFieldID)
}

// ColumnForStatus maps a status ID to the board column that shows it.
func (b *BoardConfig) ColumnForStatus(statusID string) string {
	for _, c := range b.ColumnConfig.Columns {
		for _, s := range c.Statuses {
			if s.ID == statusID {
				return c.Name
			}
		}
	}
	return ""
}

// CreateIssueLink links two issues. For a link type whose outward text is
// "blocks", inwardKey blocks outwardKey (Jira's REST API puts the outward
// description on the inward issue).
func (c *Client) CreateIssueLink(ctx context.Context, typeName, inwardKey, outwardKey string) error {
	body, err := json.Marshal(map[string]interface{}{
		"type":         map[string]string{"name": typeName},
		"inwardIssue":  map[string]string{"key": inwardKey},
		"outwardIssue": map[string]string{"key": outwardKey},
	})
	if err != nil {
		return err
	}
	if _, err := c.doRequest(ctx, "POST", c.apiBase()+"/issueLink", body); err != nil {
		return fmt.Errorf("link %s %s %s: %w", inwardKey, typeName, outwardKey, err)
	}
	return nil
}

// AddComment adds a comment to an issue (plain text on v2, ADF on v3).
func (c *Client) AddComment(ctx context.Context, key, text string) error {
	var body interface{} = text
	if c.APIVersion != "2" {
		body = PlainTextToADF(text)
	}
	payload, err := json.Marshal(map[string]interface{}{"body": body})
	if err != nil {
		return err
	}
	if _, err := c.doRequest(ctx, "POST", fmt.Sprintf("%s/issue/%s/comment", c.apiBase(), url.PathEscape(key)), payload); err != nil {
		return fmt.Errorf("comment on %s: %w", key, err)
	}
	return nil
}

// DeleteIssueLink removes an issue link by ID.
func (c *Client) DeleteIssueLink(ctx context.Context, linkID string) error {
	if _, err := c.doRequest(ctx, "DELETE", c.apiBase()+"/issueLink/"+url.PathEscape(linkID), nil); err != nil {
		return fmt.Errorf("delete link %s: %w", linkID, err)
	}
	return nil
}

func (c *Client) agileBase() string { return c.URL + "/rest/agile/1.0" }

// GetBoardConfig fetches a board's configuration.
func (c *Client) GetBoardConfig(ctx context.Context, boardID string) (*BoardConfig, error) {
	body, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/board/%s/configuration", c.agileBase(), url.PathEscape(boardID)), nil)
	if err != nil {
		return nil, fmt.Errorf("get board %s configuration: %w", boardID, err)
	}
	var cfg BoardConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse board configuration: %w", err)
	}
	return &cfg, nil
}

// GetFilterJQL returns a saved filter's JQL without its ORDER BY clause.
func (c *Client) GetFilterJQL(ctx context.Context, filterID string) (string, error) {
	body, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/filter/%s", c.apiBase(), url.PathEscape(filterID)), nil)
	if err != nil {
		return "", fmt.Errorf("get filter %s: %w", filterID, err)
	}
	var f struct {
		JQL string `json:"jql"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return "", fmt.Errorf("parse filter: %w", err)
	}
	return stripOrderBy(f.JQL), nil
}

// stripOrderBy removes a trailing ORDER BY clause from JQL.
func stripOrderBy(jql string) string {
	if i := strings.LastIndex(strings.ToUpper(jql), "ORDER BY"); i >= 0 {
		jql = jql[:i]
	}
	return strings.TrimSpace(jql)
}

// GetBoardIssues lists every issue on a board (including its backlog) with
// the given fields, following pagination.
func (c *Client) GetBoardIssues(ctx context.Context, boardID string, fields []string) ([]Issue, error) {
	return c.getAgileIssues(ctx, fmt.Sprintf("%s/board/%s/issue", c.agileBase(), url.PathEscape(boardID)), fields)
}

// GetBoardBacklog lists issues in a board's backlog. Boards without a
// backlog (e.g. kanban with the backlog disabled) return an error, which the
// caller treats as an empty backlog.
func (c *Client) GetBoardBacklog(ctx context.Context, boardID string) ([]Issue, error) {
	return c.getAgileIssues(ctx, fmt.Sprintf("%s/board/%s/backlog", c.agileBase(), url.PathEscape(boardID)), []string{"status"})
}

// GetKanbanBacklogColumns returns the names of a kanban board's backlog
// ("Kanplan") columns. Jira Server/DC's public API does not expose the kanban
// backlog ("The backlog is not available on Kanban boards"), so this reads the
// board's edit model from the internal greenhopper API.
func (c *Client) GetKanbanBacklogColumns(ctx context.Context, boardID string) ([]string, error) {
	apiURL := fmt.Sprintf("%s/rest/greenhopper/1.0/rapidviewconfig/editmodel.json?rapidViewId=%s", c.URL, url.QueryEscape(boardID))
	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get board %s edit model: %w", boardID, err)
	}
	var model struct {
		IsKanPlanEnabled bool `json:"isKanPlanEnabled"`
		RapidListConfig  struct {
			MappedColumns []struct {
				Name            string `json:"name"`
				IsKanPlanColumn bool   `json:"isKanPlanColumn"`
			} `json:"mappedColumns"`
		} `json:"rapidListConfig"`
	}
	if err := json.Unmarshal(body, &model); err != nil {
		return nil, fmt.Errorf("parse board edit model: %w", err)
	}
	if !model.IsKanPlanEnabled {
		return nil, nil
	}
	var cols []string
	for _, col := range model.RapidListConfig.MappedColumns {
		if col.IsKanPlanColumn {
			cols = append(cols, col.Name)
		}
	}
	return cols, nil
}

func (c *Client) getAgileIssues(ctx context.Context, endpoint string, fields []string) ([]Issue, error) {
	var all []Issue
	startAt := 0
	for page := 0; page < MaxPages; page++ {
		params := url.Values{
			"startAt":    {fmt.Sprintf("%d", startAt)},
			"maxResults": {"500"},
			"fields":     {strings.Join(fields, ",")},
		}
		body, err := c.doRequest(ctx, "GET", endpoint+"?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var result SearchResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse board issues: %w", err)
		}
		all = append(all, result.Issues...)
		startAt += len(result.Issues)
		if len(result.Issues) == 0 || startAt >= result.Total {
			return all, nil
		}
	}
	return nil, fmt.Errorf("pagination limit exceeded for %s", endpoint)
}
