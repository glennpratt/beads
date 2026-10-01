package jira

import (
	"context"
	"encoding/json"
	"time"

	"github.com/steveyegge/beads/internal/debug"
)

// Jira metadata that rarely changes (field IDs, board configuration, filter
// JQL, kanban backlog columns) is cached in local metadata so routine pulls
// do not re-fetch it. Full pulls (--full) refresh it.
const metaCacheTTL = 24 * time.Hour

type metaCacheEntry struct {
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

func (t *Tracker) cacheGet(ctx context.Context, key string, v interface{}) bool {
	if t.refreshMeta || t.store == nil {
		return false
	}
	raw, err := t.store.GetLocalMetadata(ctx, "jira.cache."+key)
	if err != nil || raw == "" {
		return false
	}
	var e metaCacheEntry
	if json.Unmarshal([]byte(raw), &e) != nil || time.Since(e.At) > metaCacheTTL {
		return false
	}
	return json.Unmarshal(e.Data, v) == nil
}

func (t *Tracker) cachePut(ctx context.Context, key string, v interface{}) {
	if t.store == nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	raw, err := json.Marshal(metaCacheEntry{At: time.Now(), Data: data})
	if err != nil {
		return
	}
	if err := t.store.SetLocalMetadata(ctx, "jira.cache."+key, string(raw)); err != nil {
		debug.Logf("jira: caching %s: %v\n", key, err)
	}
}
