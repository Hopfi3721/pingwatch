package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"

	_ "modernc.org/sqlite"
)

const (
	defaultResultsLimit = 50
	maxResultsLimit     = 1000
)

// Open открывает SQLite-базу и проверяет соединение.
//
// WAL + busy_timeout — чтобы чтение дашборда не блокировало запись
// результатов, а редкие конфликты записи не падали с SQLITE_BUSY.
// MaxOpenConns(1) сознательно сериализует доступ: нагрузка монитора —
// единицы записей в секунду, зато нет "database is locked" в тестах.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
		dsn = "file:" + filepath.ToSlash(path) +
			"?_pragma=busy_timeout(5000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=foreign_keys(1)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return db, nil
}

// SQLiteStore — реализация Store поверх database/sql.
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

func (s *SQLiteStore) CreateTarget(ctx context.Context, target *monitor.Target) error {
	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO targets (name, url, interval_seconds, timeout_seconds, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		target.Name,
		target.URL,
		target.IntervalSeconds,
		target.TimeoutSeconds,
		target.Enabled,
		target.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert target: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("target id: %w", err)
	}
	target.ID = id

	return nil
}

func (s *SQLiteStore) GetTarget(ctx context.Context, id int64) (*monitor.Target, error) {
	const query = `
SELECT id, name, url, interval_seconds, timeout_seconds, enabled, created_at
FROM targets
WHERE id = ?`

	var (
		target    monitor.Target
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&target.ID,
		&target.Name,
		&target.URL,
		&target.IntervalSeconds,
		&target.TimeoutSeconds,
		&target.Enabled,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, monitor.ErrTargetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get target: %w", err)
	}

	target.CreatedAt = time.UnixMilli(createdAt)
	return &target, nil
}

func (s *SQLiteStore) ListTargets(ctx context.Context) ([]monitor.Target, error) {
	const query = `
SELECT id, name, url, interval_seconds, timeout_seconds, enabled, created_at
FROM targets
ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	defer rows.Close()

	targets := make([]monitor.Target, 0)
	for rows.Next() {
		var (
			target    monitor.Target
			createdAt int64
		)
		if err := rows.Scan(
			&target.ID,
			&target.Name,
			&target.URL,
			&target.IntervalSeconds,
			&target.TimeoutSeconds,
			&target.Enabled,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}

		target.CreatedAt = time.UnixMilli(createdAt)
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate targets: %w", err)
	}

	return targets, nil
}

func (s *SQLiteStore) DeleteTarget(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM targets WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete target: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete target rows: %w", err)
	}
	if affected == 0 {
		return monitor.ErrTargetNotFound
	}

	return nil
}

func (s *SQLiteStore) SaveResult(ctx context.Context, result *monitor.Result) error {
	if !result.Status.Valid() {
		return monitor.ErrInvalidStatus
	}

	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO check_results (target_id, status, status_code, latency_ms, error, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		result.TargetID,
		string(result.Status),
		result.StatusCode,
		result.LatencyMS,
		result.Error,
		result.CheckedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert result: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("result id: %w", err)
	}
	result.ID = id

	return nil
}

func (s *SQLiteStore) ListResults(ctx context.Context, targetID int64, limit int) ([]monitor.Result, error) {
	if limit <= 0 {
		limit = defaultResultsLimit
	}
	if limit > maxResultsLimit {
		limit = maxResultsLimit
	}

	const query = `
SELECT id, target_id, status, status_code, latency_ms, error, checked_at
FROM check_results
WHERE target_id = ?
ORDER BY id DESC
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, targetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list results: %w", err)
	}
	defer rows.Close()

	results := make([]monitor.Result, 0, limit)
	for rows.Next() {
		result, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate results: %w", err)
	}

	return results, nil
}

func (s *SQLiteStore) LatestResult(ctx context.Context, targetID int64) (*monitor.Result, error) {
	const query = `
SELECT id, target_id, status, status_code, latency_ms, error, checked_at
FROM check_results
WHERE target_id = ?
ORDER BY id DESC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, query, targetID)
	result, err := scanResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, monitor.ErrResultNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("latest result: %w", err)
	}

	return &result, nil
}

func (s *SQLiteStore) LatestResults(ctx context.Context) (map[int64]monitor.Result, error) {
	const query = `
SELECT id, target_id, status, status_code, latency_ms, error, checked_at
FROM check_results
WHERE id IN (SELECT MAX(id) FROM check_results GROUP BY target_id)`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("latest results: %w", err)
	}
	defer rows.Close()

	latest := make(map[int64]monitor.Result)
	for rows.Next() {
		result, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		latest[result.TargetID] = result
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest results: %w", err)
	}

	return latest, nil
}

func (s *SQLiteStore) Stats(ctx context.Context, targetID int64) (*monitor.Stats, error) {
	const aggregate = `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN status = 'up' THEN 1 ELSE 0 END), 0),
       COALESCE(AVG(latency_ms), 0),
       MIN(checked_at),
       MAX(checked_at)
FROM check_results
WHERE target_id = ?`

	var (
		firstChecked sql.NullInt64
		lastChecked  sql.NullInt64
		avgLatency   float64
	)
	stats := monitor.Stats{TargetID: targetID}

	err := s.db.QueryRowContext(ctx, aggregate, targetID).Scan(
		&stats.Total,
		&stats.Up,
		&avgLatency,
		&firstChecked,
		&lastChecked,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate stats: %w", err)
	}

	if stats.Total == 0 {
		return &stats, nil
	}

	stats.AvgLatencyMS = int64(math.Round(avgLatency))
	stats.Down = stats.Total - stats.Up
	stats.UptimePercent = math.Round(float64(stats.Up)/float64(stats.Total)*10000) / 100
	stats.FirstCheckedAt = time.UnixMilli(firstChecked.Int64)
	stats.LastCheckedAt = time.UnixMilli(lastChecked.Int64)

	// P95 считаем по фактическим значениям: SQLite не умеет percentile,
	// а тянуть все строки в память незачем — нужен один OFFSET.
	offset := int(math.Ceil(0.95*float64(stats.Total))) - 1
	if offset < 0 {
		offset = 0
	}

	const percentile = `
SELECT latency_ms
FROM check_results
WHERE target_id = ?
ORDER BY latency_ms
LIMIT 1 OFFSET ?`

	if err := s.db.QueryRowContext(ctx, percentile, targetID, offset).Scan(&stats.P95LatencyMS); err != nil {
		return nil, fmt.Errorf("p95 latency: %w", err)
	}

	return &stats, nil
}

func (s *SQLiteStore) PruneResults(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, fmt.Errorf("keep must be positive, got %d", keep)
	}

	const query = `
DELETE FROM check_results
WHERE id IN (
	SELECT id FROM (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY target_id ORDER BY id DESC) AS rn
		FROM check_results
	)
	WHERE rn > ?
)`

	res, err := s.db.ExecContext(ctx, query, keep)
	if err != nil {
		return 0, fmt.Errorf("prune results: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune rows: %w", err)
	}

	return affected, nil
}

// scanner объединяет *sql.Row и *sql.Rows: у обоих есть Scan.
type scanner interface {
	Scan(dest ...any) error
}

func scanResult(s scanner) (monitor.Result, error) {
	var (
		result    monitor.Result
		status    string
		checkedAt int64
	)

	if err := s.Scan(
		&result.ID,
		&result.TargetID,
		&status,
		&result.StatusCode,
		&result.LatencyMS,
		&result.Error,
		&checkedAt,
	); err != nil {
		return monitor.Result{}, err
	}

	result.Status = monitor.Status(status)
	result.CheckedAt = time.UnixMilli(checkedAt)

	return result, nil
}
