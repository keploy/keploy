package flakiness

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

type TestHistory struct {
	TestName      string    `json:"testName"`
	TotalRuns     int       `json:"totalRuns"`
	Passes        int       `json:"passes"`
	Failures      int       `json:"failures"`
	FlakinessRate float64   `json:"flakinessRate"`
	LastResults   []bool    `json:"lastResults"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
}

type FlakinessDB struct {
	conn   *sql.DB
	logger *zap.Logger
}

func New(logger *zap.Logger, path string) (*FlakinessDB, error) {

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open flakiness db: %w", err)
	}

	fdb := &FlakinessDB{conn: db, logger: logger}
	if err := fdb.init(); err != nil {
		return nil, err
	}
	return fdb, nil
}

func (fdb *FlakinessDB) init() error {

	query := `
	CREATE TABLE IF NOT EXISTS test_executions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		test_name TEXT NOT NULL,
		passed BOOLEAN NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_test_name_ts ON test_executions(test_name, timestamp DESC);
	`
	_, err := fdb.conn.Exec(query)
	return err
}

func (fdb *FlakinessDB) RecordResult(ctx context.Context, testName string, passed bool) error {
	query := `INSERT INTO test_executions (test_name, passed, timestamp) VALUES (?, ?, ?)`
	_, err := fdb.conn.ExecContext(ctx, query, testName, passed, time.Now())
	return err
}

func (fdb *FlakinessDB) GetFlakyTests(ctx context.Context, threshold float64) ([]*TestHistory, error) {
	query := `
		SELECT 
			test_name,
			COUNT(*) as total_runs,
			SUM(CASE WHEN passed THEN 1 ELSE 0 END) as passes,
			MIN(timestamp) as first_seen,
			MAX(timestamp) as last_seen
		FROM test_executions
		GROUP BY test_name
		HAVING 
			passes > 0 AND 
			(total_runs - passes) > 0 -- Must have at least one fail and one pass to be "flaky"
	`

	rows, err := fdb.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var flakyTests []*TestHistory

	for rows.Next() {
		var h TestHistory
		if err := rows.Scan(&h.TestName, &h.TotalRuns, &h.Passes, &h.FirstSeen, &h.LastSeen); err != nil {
			return nil, err
		}

		h.Failures = h.TotalRuns - h.Passes
		h.FlakinessRate = float64(h.Failures) / float64(h.TotalRuns)

		if h.FlakinessRate < threshold {
			continue
		}

		lastResults, err := fdb.getLastResults(ctx, h.TestName, 10)
		if err != nil {
			fdb.logger.Warn("failed to fetch last results", zap.String("test", h.TestName), zap.Error(err))
		}
		h.LastResults = lastResults

		flakyTests = append(flakyTests, &h)
	}

	return flakyTests, nil
}

func (fdb *FlakinessDB) getLastResults(ctx context.Context, testName string, limit int) ([]bool, error) {
	query := `SELECT passed FROM test_executions WHERE test_name = ? ORDER BY timestamp DESC LIMIT ?`
	rows, err := fdb.conn.QueryContext(ctx, query, testName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []bool
	for rows.Next() {
		var p bool
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}

		results = append(results, p)
	}
	return results, nil
}
