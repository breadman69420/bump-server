package db

import (
	"context"
	"database/sql"
	"time"
)

type Queries struct {
	db *sql.DB
}

func NewQueries(db *sql.DB) *Queries {
	return &Queries{db: db}
}

// LogSession records a token request for pattern detection.
func (q *Queries) LogSession(ctx context.Context, deviceHash string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO session_log (device_hash) VALUES ($1)`,
		deviceHash,
	)
	return err
}

// SessionCountLastHour returns how many sessions a device has requested in the past hour.
func (q *Queries) SessionCountLastHour(ctx context.Context, deviceHash string) (int, error) {
	var count int
	err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_log
		 WHERE device_hash = $1 AND requested_at > NOW() - INTERVAL '1 hour'`,
		deviceHash,
	).Scan(&count)
	return count, err
}

// InsertReport records an abuse report. Returns false if this reporter
// already reported this device (duplicate prevention).
func (q *Queries) InsertReport(ctx context.Context, reporterHash, reportedHash, reason string) (bool, error) {
	result, err := q.db.ExecContext(ctx,
		`INSERT INTO reports (reporter_hash, reported_hash, reason) VALUES ($1, $2, $3)
		 ON CONFLICT (reporter_hash, reported_hash) DO NOTHING`,
		reporterHash, reportedHash, reason,
	)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

// ReportCount returns the number of reports against a device hash.
func (q *Queries) ReportCount(ctx context.Context, reportedHash string) (int, error) {
	var count int
	err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM reports WHERE reported_hash = $1`,
		reportedHash,
	).Scan(&count)
	return count, err
}

// AddToBlocklist adds a device hash to the blocklist.
func (q *Queries) AddToBlocklist(ctx context.Context, deviceHash string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO blocklist (device_hash) VALUES ($1) ON CONFLICT DO NOTHING`,
		deviceHash,
	)
	return err
}

// IsBlocked checks if a device hash is on the blocklist.
func (q *Queries) IsBlocked(ctx context.Context, deviceHash string) (bool, error) {
	var exists bool
	err := q.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blocklist WHERE device_hash = $1)`,
		deviceHash,
	).Scan(&exists)
	return exists, err
}

// GetBlocklist returns all blocked device hashes.
func (q *Queries) GetBlocklist(ctx context.Context) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT device_hash FROM blocklist`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// CleanupOldSessions deletes session logs older than 7 days.
func (q *Queries) CleanupOldSessions(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	result, err := q.db.ExecContext(ctx,
		`DELETE FROM session_log WHERE requested_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
