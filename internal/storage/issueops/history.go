package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// HistoryInTx returns the complete version history for an issue by querying
// the dolt_history_issues system table. The result is ordered newest-first.
//
// The subquery wrapper avoids Dolt's max1Row optimization on PK lookup:
// dolt_history_* tables return multiple rows per PK (one per commit), but
// the query planner incorrectly assumes WHERE id=? returns one row.
func HistoryInTx(ctx context.Context, tx DBTX, issueID string) ([]*storage.HistoryEntry, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT
			id, title,
			COALESCE(description, '') AS description,
			COALESCE(design, '') AS design,
			COALESCE(acceptance_criteria, '') AS acceptance_criteria,
			COALESCE(notes, '') AS notes,
			status, priority, issue_type, assignee, owner, created_by,
			estimated_minutes, created_at, updated_at, closed_at, close_reason,
			pinned, mol_type,
			commit_hash, committer, commit_date
		FROM (
			SELECT * FROM dolt_history_issues
		) h
		WHERE h.id = ?
		ORDER BY h.commit_date DESC
	`, issueID)
	if err != nil {
		return nil, fmt.Errorf("failed to get issue history: %w", err)
	}
	defer rows.Close()

	var entries []*storage.HistoryEntry
	for rows.Next() {
		var issue types.Issue
		var createdAtStr, updatedAtStr sql.NullString
		var closedAt sql.NullTime
		var assignee, owner, createdBy, closeReason, molType sql.NullString
		var estimatedMinutes sql.NullInt64
		var pinned sql.NullInt64
		var commitHash, committer string
		var commitDate time.Time

		if err := rows.Scan(
			&issue.ID, &issue.Title, &issue.Description, &issue.Design, &issue.AcceptanceCriteria, &issue.Notes,
			&issue.Status, &issue.Priority, &issue.IssueType, &assignee, &owner, &createdBy,
			&estimatedMinutes, &createdAtStr, &updatedAtStr, &closedAt, &closeReason,
			&pinned, &molType,
			&commitHash, &committer, &commitDate,
		); err != nil {
			return nil, fmt.Errorf("failed to scan history: %w", err)
		}

		if createdAtStr.Valid {
			issue.CreatedAt = ParseTimeString(createdAtStr.String)
		}
		if updatedAtStr.Valid {
			issue.UpdatedAt = ParseTimeString(updatedAtStr.String)
		}
		if closedAt.Valid {
			issue.ClosedAt = &closedAt.Time
		}
		if assignee.Valid {
			issue.Assignee = assignee.String
		}
		if owner.Valid {
			issue.Owner = owner.String
		}
		if createdBy.Valid {
			issue.CreatedBy = createdBy.String
		}
		if estimatedMinutes.Valid {
			mins := int(estimatedMinutes.Int64)
			issue.EstimatedMinutes = &mins
		}
		if closeReason.Valid {
			issue.CloseReason = closeReason.String
		}
		if pinned.Valid && pinned.Int64 != 0 {
			issue.Pinned = true
		}
		if molType.Valid {
			issue.MolType = types.MolType(molType.String)
		}

		entries = append(entries, &storage.HistoryEntry{
			CommitHash: commitHash,
			Committer:  committer,
			CommitDate: commitDate,
			Issue:      &issue,
		})
	}

	return entries, rows.Err()
}

// PreviousExternalRefInTx returns the external_ref value recorded for
// issueID as of the most recent commit at or before asOf, by querying the
// dolt_history_issues system table. found is false if no history entry
// exists for issueID at or before asOf.
//
// The subquery wrapper avoids Dolt's max1Row optimization on PK lookup, for
// the same reason described on HistoryInTx above.
func PreviousExternalRefInTx(ctx context.Context, tx *sql.Tx, issueID string, asOf time.Time) (string, bool, error) {
	var previousRef sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT external_ref
		FROM (
			SELECT id, external_ref, commit_date FROM dolt_history_issues
		) h
		WHERE h.id = ? AND h.commit_date <= ?
		ORDER BY h.commit_date DESC
		LIMIT 1
	`, issueID, asOf.UTC()).Scan(&previousRef)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to get previous external_ref: %w", err)
	}
	return previousRef.String, true, nil
}

// ExternalRefsAsOfInTx returns every issue's external_ref as of the most
// recent commit at or before asOf ("" for NULL). Issues missing from the map
// did not exist at that commit. Equivalent to PreviousExternalRefInTx for all
// issues, in two queries instead of one history scan per issue.
func ExternalRefsAsOfInTx(ctx context.Context, tx *sql.Tx, asOf time.Time) (map[string]string, error) {
	refs := make(map[string]string)
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log WHERE date <= ? ORDER BY date DESC LIMIT 1`, asOf.UTC()).Scan(&hash)
	if err == sql.ErrNoRows {
		return refs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find commit as of %s: %w", asOf.UTC().Format(time.RFC3339), err)
	}
	if !isDoltCommitHash(hash) {
		return nil, fmt.Errorf("unexpected commit hash %q", hash)
	}
	// AS OF takes a literal; the hash is validated above.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT id, external_ref FROM issues AS OF '%s'", hash))
	if err != nil {
		return nil, fmt.Errorf("failed to read external refs as of %s: %w", hash, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref sql.NullString
		if err := rows.Scan(&id, &ref); err != nil {
			return nil, fmt.Errorf("failed to scan external ref: %w", err)
		}
		refs[id] = ref.String
	}
	return refs, rows.Err()
}

// isDoltCommitHash reports whether s looks like a Dolt commit hash (32
// base32 characters, 0-9 and a-v).
func isDoltCommitHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'v')) {
			return false
		}
	}
	return true
}
