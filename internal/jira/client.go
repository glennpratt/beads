package jira

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/debug"
)

// Issue represents a Jira issue from the REST API.
type Issue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Self   string      `json:"self"`
	Fields IssueFields `json:"fields"`
}

// IssueFields contains the fields of a Jira issue.
type IssueFields struct {
	Summary     string           `json:"summary"`
	Description json.RawMessage  `json:"description"` // ADF (Atlassian Document Format) or plain text
	Status      *StatusField     `json:"status"`
	Priority    *PriorityField   `json:"priority"`
	IssueType   *IssueTypeField  `json:"issuetype"`
	Project     *ProjectField    `json:"project"`
	Assignee    *UserField       `json:"assignee"`
	Labels      []string         `json:"labels"`
	Created     string           `json:"created"`
	Updated     string           `json:"updated"`
	Resolution  *ResolutionField `json:"resolution"`
	Parent      *ParentField     `json:"parent,omitempty"` // sub-task parent; on Jira Cloud also the epic

	// Custom holds raw "customfield_*" values (e.g. Epic Link, Parent Link),
	// keyed by field ID. Populated on unmarshal only.
	Custom map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the known fields and keeps customfield_* values in Custom.
func (f *IssueFields) UnmarshalJSON(data []byte) error {
	type plain IssueFields
	if err := json.Unmarshal(data, (*plain)(f)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for k, v := range all {
		if !strings.HasPrefix(k, "customfield_") || string(v) == "null" {
			continue
		}
		if f.Custom == nil {
			f.Custom = make(map[string]json.RawMessage)
		}
		f.Custom[k] = v
	}
	return nil
}

// ParentField references an issue's parent (sub-task parent, or on Jira
// Cloud any hierarchy parent such as the epic).
type ParentField struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// CustomFieldKey returns the issue key held in a custom field, for issue
// picker fields such as Epic Link and Parent Link. Server/DC returns a bare
// key string; object forms carrying a "key" are also accepted.
func (f *IssueFields) CustomFieldKey(fieldID string) string {
	raw, ok := f.Custom[fieldID]
	if fieldID == "" || !ok {
		return ""
	}
	var key string
	if err := json.Unmarshal(raw, &key); err == nil {
		return strings.TrimSpace(key)
	}
	var obj struct {
		Key  string `json:"key"`
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Key != "" {
			return strings.TrimSpace(obj.Key)
		}
		return strings.TrimSpace(obj.Data.Key)
	}
	return ""
}

// StatusField represents a Jira issue status.
type StatusField struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	StatusCategory *StatusCategoryField `json:"statusCategory,omitempty"`
}

// StatusCategoryField is Jira's fixed grouping of workflow statuses. Its Key
// is one of "new", "indeterminate" or "done", regardless of how a workflow
// names its statuses.
type StatusCategoryField struct {
	Key string `json:"key"`
}

// PriorityField represents a Jira issue priority.
type PriorityField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IssueTypeField represents a Jira issue type.
type IssueTypeField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectField represents a Jira project.
type ProjectField struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// UserField represents a Jira user.
type UserField struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// ResolutionField represents a Jira resolution.
type ResolutionField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Transition represents a Jira workflow transition.
type Transition struct {
	ID     string                     `json:"id"`
	Name   string                     `json:"name"`
	To     StatusField                `json:"to"`
	Fields map[string]TransitionField `json:"fields,omitempty"` // screen fields (expand=transitions.fields)
}

// TransitionField is a field on a transition screen.
type TransitionField struct {
	Required      bool `json:"required"`
	AllowedValues []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"allowedValues"`
}

// TransitionsResult is the response from GET /issue/{key}/transitions.
type TransitionsResult struct {
	Transitions []Transition `json:"transitions"`
}

// SearchResult represents a Jira JQL search response.
type SearchResult struct {
	StartAt       int     `json:"startAt"`
	MaxResults    int     `json:"maxResults"`
	Total         int     `json:"total"`
	NextPageToken string  `json:"nextPageToken"`
	IsLast        bool    `json:"isLast"`
	Issues        []Issue `json:"issues"`
}

// Client provides HTTP access to a Jira instance.
type Client struct {
	URL        string
	Username   string
	APIToken   string
	APIVersion string // "2" or "3" (default: "3")
	HTTPClient *http.Client

	// ExtraFields are additional field IDs (e.g. Epic Link / Parent Link
	// custom fields) requested alongside searchFields.
	ExtraFields []string
}

// NewClient creates a new Jira client.
func NewClient(url, username, apiToken string) *Client {
	return &Client{
		URL:        strings.TrimSuffix(url, "/"),
		Username:   username,
		APIToken:   apiToken,
		APIVersion: "3",
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ConfigureTLS sets up mutual TLS and/or a custom CA bundle for Jira instances
// behind client-certificate authentication (common for self-hosted Jira
// Server/Data Center). certFile and keyFile must be set together; caFile is
// optional and is added to the system roots rather than replacing them.
// Paths may start with "~/". A call with all arguments empty is a no-op.
func (c *Client) ConfigureTLS(certFile, keyFile, caFile string) error {
	if certFile == "" && keyFile == "" && caFile == "" {
		return nil
	}
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("jira client certificate and key must be configured together")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(expandHome(certFile), expandHome(keyFile))
		if err != nil {
			return fmt.Errorf("load jira client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		// Some Jira front ends (e.g. per-path client auth on Apache or F5)
		// request the client certificate via TLS 1.2 renegotiation after the
		// initial handshake, which Go refuses by default.
		tlsConfig.Renegotiation = tls.RenegotiateOnceAsClient
	}

	if caFile != "" {
		pem, err := os.ReadFile(expandHome(caFile))
		if err != nil {
			return fmt.Errorf("read jira CA certificate: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("no certificates found in jira CA file %s", caFile)
		}
		tlsConfig.RootCAs = pool
	}

	// Clone the default transport so proxy and timeout settings are kept.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	c.HTTPClient.Transport = transport
	return nil
}

// expandHome expands a leading "~/" to the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}

// apiBase returns the versioned REST API base URL, e.g. "https://host/rest/api/3".
func (c *Client) apiBase() string {
	v := c.APIVersion
	if v == "" {
		v = "3"
	}
	return c.URL + "/rest/api/" + v
}

// FetchIssueTimestamp fetches the updated timestamp for a single Jira issue.
func (c *Client) FetchIssueTimestamp(ctx context.Context, jiraKey string) (time.Time, error) {
	var zero time.Time

	apiURL := fmt.Sprintf("%s/issue/%s?fields=updated", c.apiBase(), url.PathEscape(jiraKey))

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return zero, fmt.Errorf("fetch issue %s: %w", jiraKey, err)
	}

	var result struct {
		Fields struct {
			Updated string `json:"updated"`
		} `json:"fields"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return zero, fmt.Errorf("parse Jira response: %w", err)
	}

	updated, err := ParseTimestamp(result.Fields.Updated)
	if err != nil {
		return zero, fmt.Errorf("parse Jira timestamp: %w", err)
	}

	return updated, nil
}

// searchFields is the default set of fields to request in search/get queries.
const searchFields = "summary,description,status,priority,issuetype,project,assignee,labels,created,updated,resolution,parent"

// requestFields returns searchFields plus any configured ExtraFields.
func (c *Client) requestFields() string {
	if len(c.ExtraFields) == 0 {
		return searchFields
	}
	return searchFields + "," + strings.Join(c.ExtraFields, ",")
}

// Field describes a Jira field from GET /field.
type Field struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
	Schema struct {
		Type   string `json:"type"`
		Custom string `json:"custom"`
	} `json:"schema"`
}

// Custom field plugin types for Jira Server/DC hierarchy links.
const (
	epicLinkFieldType   = "com.pyxis.greenhopper.jira:gh-epic-link"   // Story -> Epic
	epicNameFieldType   = "com.pyxis.greenhopper.jira:gh-epic-label"  // Required when creating an Epic
	parentLinkFieldType = "com.atlassian.jpo:jpo-custom-field-parent" // Advanced Roadmaps: Epic -> higher levels
)

// HierarchyFields holds the Server/DC custom field IDs used for issue
// hierarchy. Each is empty when the instance does not have it (Jira Cloud
// uses the standard parent field instead).
type HierarchyFields struct {
	EpicLink   string
	EpicName   string
	ParentLink string
}

// GetFields lists all Jira fields (system and custom).
func (c *Client) GetFields(ctx context.Context) ([]Field, error) {
	body, err := c.doRequest(ctx, "GET", c.apiBase()+"/field", nil)
	if err != nil {
		return nil, fmt.Errorf("list fields: %w", err)
	}
	var fields []Field
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("parse fields response: %w", err)
	}
	return fields, nil
}

// DiscoverHierarchyFields finds the Epic Link, Epic Name and Parent Link
// custom field IDs by plugin type.
func (c *Client) DiscoverHierarchyFields(ctx context.Context) (HierarchyFields, error) {
	var hf HierarchyFields
	fields, err := c.GetFields(ctx)
	if err != nil {
		return hf, err
	}
	for _, f := range fields {
		switch f.Schema.Custom {
		case epicLinkFieldType:
			hf.EpicLink = f.ID
		case epicNameFieldType:
			hf.EpicName = f.ID
		case parentLinkFieldType:
			hf.ParentLink = f.ID
		}
	}
	return hf, nil
}

// SearchIssues queries Jira using JQL and returns all matching issues, handling pagination.
func (c *Client) SearchIssues(ctx context.Context, jql string) ([]Issue, error) {
	var allIssues []Issue
	startAt := 0
	nextPageToken := ""
	maxResults := 100
	page := 0
	useV2Pagination := c.APIVersion == "2"

	for {
		select {
		case <-ctx.Done():
			return allIssues, ctx.Err()
		default:
		}

		page++
		if page > MaxPages {
			return nil, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
		}

		params := url.Values{
			"jql":        {jql},
			"fields":     {c.requestFields()},
			"maxResults": {fmt.Sprintf("%d", maxResults)},
		}
		if useV2Pagination {
			params.Set("startAt", fmt.Sprintf("%d", startAt))
		} else if nextPageToken != "" {
			params.Set("nextPageToken", nextPageToken)
		}

		// v3 uses /search/jql; v2 uses /search (both accept jql as a query param)
		searchPath := "search/jql"
		if useV2Pagination {
			searchPath = "search"
		}
		apiURL := fmt.Sprintf("%s/%s?%s", c.apiBase(), searchPath, params.Encode())

		body, err := c.doRequest(ctx, "GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("search issues: %w", err)
		}

		var result SearchResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse search response: %w", err)
		}

		allIssues = append(allIssues, result.Issues...)

		if len(result.Issues) == 0 {
			break
		}
		if useV2Pagination {
			if startAt+len(result.Issues) >= result.Total {
				break
			}
			startAt += len(result.Issues)
			continue
		}
		if result.IsLast || result.NextPageToken == "" {
			break
		}
		nextPageToken = result.NextPageToken
	}

	return allIssues, nil
}

// GetIssue fetches a single Jira issue by key (e.g., "PROJ-123").
func (c *Client) GetIssue(ctx context.Context, key string) (*Issue, error) {
	apiURL := fmt.Sprintf("%s/issue/%s?fields=%s", c.apiBase(), url.PathEscape(key), url.QueryEscape(c.requestFields()))

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get issue %s: %w", key, err)
	}

	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("parse issue response: %w", err)
	}

	return &issue, nil
}

// CreateIssue creates a new issue in Jira.
// fields should include "project", "summary", "issuetype", and optionally other fields.
func (c *Client) CreateIssue(ctx context.Context, fields map[string]interface{}) (*Issue, error) {
	payload := map[string]interface{}{"fields": fields}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal create request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue", c.apiBase())

	body, err := c.doRequest(ctx, "POST", apiURL, data)
	if err != nil {
		return nil, fmt.Errorf("create issue: %w", err)
	}

	// Create response only returns id, key, self. Fetch the full issue.
	var created struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self string `json:"self"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}

	return c.GetIssue(ctx, created.Key)
}

// UpdateIssue updates an existing Jira issue by key.
func (c *Client) UpdateIssue(ctx context.Context, key string, fields map[string]interface{}) error {
	payload := map[string]interface{}{"fields": fields}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal update request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue/%s", c.apiBase(), url.PathEscape(key))

	_, err = c.doRequest(ctx, "PUT", apiURL, data)
	if err != nil {
		return fmt.Errorf("update issue %s: %w", key, err)
	}

	return nil
}

// GetIssueTransitions fetches the available workflow transitions for a Jira issue.
func (c *Client) GetIssueTransitions(ctx context.Context, key string) ([]Transition, error) {
	apiURL := fmt.Sprintf("%s/issue/%s/transitions?expand=transitions.fields", c.apiBase(), url.PathEscape(key))

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get transitions for %s: %w", key, err)
	}

	var result TransitionsResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse transitions response: %w", err)
	}

	return result.Transitions, nil
}

// TransitionIssue moves a Jira issue to a new status using the given transition ID.
func (c *Client) TransitionIssue(ctx context.Context, key, transitionID string) error {
	return c.TransitionIssueWithFields(ctx, key, transitionID, nil)
}

// TransitionIssueWithFields applies a transition, setting screen fields such
// as a required resolution.
func (c *Client) TransitionIssueWithFields(ctx context.Context, key, transitionID string, fields map[string]interface{}) error {
	payload := map[string]interface{}{
		"transition": map[string]string{"id": transitionID},
	}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal transition request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue/%s/transitions", c.apiBase(), url.PathEscape(key))

	_, err = c.doRequest(ctx, "POST", apiURL, data)
	if err != nil {
		return fmt.Errorf("transition issue %s: %w", key, err)
	}

	return nil
}

// doRequest executes an authenticated HTTP request and returns the response body.
func (c *Client) doRequest(ctx context.Context, method, apiURL string, body []byte) ([]byte, error) {
	debug.Logf("jira: %s %s\n", method, apiURL)

	if c.URL == "" {
		return nil, fmt.Errorf("jira URL not configured")
	}
	if c.APIToken == "" {
		return nil, fmt.Errorf("jira API token not configured")
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		// Reset body reader at top of loop so retries after network errors
		// don't send empty bodies (the reader may be at EOF).
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, apiURL, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		c.setAuth(req)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "bd-jira-sync/1.0")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		start := time.Now()
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			debug.Logf("jira: %s %s failed after %s (attempt %d/%d): %v\n", method, apiURL, time.Since(start).Round(time.Millisecond), attempt+1, MaxRetries+1, err)
			lastErr = fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
			continue
		}
		debug.Logf("jira: %s %s -> %d in %s (attempt %d/%d)\n", method, apiURL, resp.StatusCode, time.Since(start).Round(time.Millisecond), attempt+1, MaxRetries+1)

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
			continue
		}

		// PUT returns 204 No Content on success
		if resp.StatusCode == http.StatusNoContent {
			return nil, nil
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		// Permanent failures — no retry.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, string(respBody))
		}

		// Retry on rate-limiting and server errors with exponential backoff.
		retriable := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusInternalServerError ||
			resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout

		if retriable {
			delay := RetryDelay * time.Duration(1<<uint(attempt))
			useServerDelay := false

			// Use Retry-After header if present (no jitter — respect server-mandated delay)
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil {
					delay = time.Duration(seconds) * time.Second
					useServerDelay = true
				}
			}

			// Only add jitter to our own exponential backoff, not server-mandated delays
			if !useServerDelay {
				if half := int64(delay / 2); half > 0 {
					delay += time.Duration(rand.Int64N(half)) //nolint:gosec // G404: jitter for retry backoff does not need crypto rand
				}
			}

			lastErr = fmt.Errorf("transient error %d (attempt %d/%d)", resp.StatusCode, attempt+1, MaxRetries+1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				continue
			}
		}

		return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", MaxRetries+1, lastErr)
}

// setAuth sets the appropriate authentication header on the request.
func (c *Client) setAuth(req *http.Request) {
	isCloud := strings.Contains(c.URL, "atlassian.net")
	if (isCloud || c.Username != "") && c.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.APIToken))
		req.Header.Set("Authorization", "Basic "+auth)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.APIToken)
	}
}

// DescriptionToPlainText extracts plain text from Jira's ADF (Atlassian Document Format).
// Jira v3 API returns descriptions as ADF JSON, not plain text.
func DescriptionToPlainText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Try to parse as ADF document
	var doc struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not JSON - treat as plain text string
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return string(raw)
	}

	if doc.Type != "doc" {
		// Not ADF - try plain string
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return string(raw)
	}

	// Extract text from ADF nodes
	var parts []string
	for _, block := range doc.Content {
		var line []string
		for _, inline := range block.Content {
			if inline.Text != "" {
				line = append(line, inline.Text)
			}
		}
		if len(line) > 0 {
			parts = append(parts, strings.Join(line, ""))
		}
	}

	return strings.Join(parts, "\n")
}

// PlainTextToADF converts plain text to Jira's ADF (Atlassian Document Format).
func PlainTextToADF(text string) json.RawMessage {
	if text == "" {
		return nil
	}

	paragraphs := strings.Split(text, "\n")
	var content []interface{}
	for _, para := range paragraphs {
		if para == "" {
			content = append(content, map[string]interface{}{
				"type":    "paragraph",
				"content": []interface{}{},
			})
			continue
		}
		content = append(content, map[string]interface{}{
			"type": "paragraph",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": para,
				},
			},
		})
	}

	doc := map[string]interface{}{
		"type":    "doc",
		"version": 1,
		"content": content,
	}

	data, _ := json.Marshal(doc)
	return data
}
