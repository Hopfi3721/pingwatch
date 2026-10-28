package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return NewSQLiteStore(db)
}

func newTarget(name string) *monitor.Target {
	return &monitor.Target{
		Name:            name,
		URL:             "https://example.com/healthz",
		IntervalSeconds: 30,
		TimeoutSeconds:  10,
		Enabled:         true,
		CreatedAt:       time.Now().UTC(),
	}
}

func newResult(targetID int64, status monitor.Status, latencyMS int64, checkedAt time.Time) *monitor.Result {
	return &monitor.Result{
		TargetID:  targetID,
		Status:    status,
		LatencyMS: latencyMS,
		CheckedAt: checkedAt,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}

	var applied int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied migrations = %d, want 1", applied)
	}
}

func TestTargetCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	target.Enabled = false

	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if target.ID == 0 {
		t.Fatal("CreateTarget did not assign ID")
	}

	got, err := store.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if got.Name != target.Name || got.URL != target.URL {
		t.Errorf("GetTarget = %+v, want %+v", got, target)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false")
	}
	if got.IntervalSeconds != target.IntervalSeconds || got.TimeoutSeconds != target.TimeoutSeconds {
		t.Errorf("intervals = %d/%d, want %d/%d",
			got.IntervalSeconds, got.TimeoutSeconds, target.IntervalSeconds, target.TimeoutSeconds)
	}
	if got.CreatedAt.UnixMilli() != target.CreatedAt.UnixMilli() {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, target.CreatedAt)
	}

	second := newTarget("Web")
	if err := store.CreateTarget(ctx, second); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	targets, err := store.ListTargets(ctx)
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("len(targets) = %d, want 2", len(targets))
	}
	if targets[0].ID != target.ID || targets[1].ID != second.ID {
		t.Errorf("targets order = %d,%d, want %d,%d", targets[0].ID, targets[1].ID, target.ID, second.ID)
	}

	if err := store.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := store.GetTarget(ctx, target.ID); err != monitor.ErrTargetNotFound {
		t.Errorf("GetTarget after delete error = %v, want ErrTargetNotFound", err)
	}
	if err := store.DeleteTarget(ctx, target.ID); err != monitor.ErrTargetNotFound {
		t.Errorf("DeleteTarget twice error = %v, want ErrTargetNotFound", err)
	}
}

func TestResultLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Millisecond)

	first := newResult(target.ID, monitor.StatusUp, 12, base)
	first.StatusCode = 200
	if err := store.SaveResult(ctx, first); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}

	second := newResult(target.ID, monitor.StatusDown, 5000, base.Add(time.Second))
	second.StatusCode = 500
	second.Error = "unexpected status 500 Internal Server Error"
	if err := store.SaveResult(ctx, second); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}

	if first.ID == 0 || second.ID <= first.ID {
		t.Fatalf("result ids = %d, %d; want increasing", first.ID, second.ID)
	}

	latest, err := store.LatestResult(ctx, target.ID)
	if err != nil {
		t.Fatalf("LatestResult: %v", err)
	}
	if latest.ID != second.ID || latest.Status != monitor.StatusDown {
		t.Errorf("latest = %+v, want second result", latest)
	}
	if latest.CheckedAt.UnixMilli() != second.CheckedAt.UnixMilli() {
		t.Errorf("CheckedAt = %v, want %v", latest.CheckedAt, second.CheckedAt)
	}
	if latest.Error != second.Error {
		t.Errorf("Error = %q, want %q", latest.Error, second.Error)
	}

	results, err := store.ListResults(ctx, target.ID, 1)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}
	if len(results) != 1 || results[0].ID != second.ID {
		t.Fatalf("ListResults limit=1 = %+v, want only second result", results)
	}

	all, err := store.ListResults(ctx, target.ID, 10)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}
	if all[0].ID != second.ID || all[1].ID != first.ID {
		t.Error("ListResults must be ordered newest first")
	}

	latestMap, err := store.LatestResults(ctx)
	if err != nil {
		t.Fatalf("LatestResults: %v", err)
	}
	if got, ok := latestMap[target.ID]; !ok || got.ID != second.ID {
		t.Errorf("LatestResults[%d] = %+v (ok=%v), want second result", target.ID, got, ok)
	}
}

func TestLatestResultNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	if _, err := store.LatestResult(ctx, target.ID); err != monitor.ErrResultNotFound {
		t.Errorf("error = %v, want ErrResultNotFound", err)
	}
}

func TestSaveResultRejectsInvalidStatus(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	result := newResult(target.ID, monitor.Status("maybe"), 1, time.Now())
	if err := store.SaveResult(ctx, result); err != monitor.ErrInvalidStatus {
		t.Errorf("error = %v, want ErrInvalidStatus", err)
	}
}

func TestStats(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Millisecond)

	for i := 1; i <= 100; i++ {
		status := monitor.StatusUp
		if i == 100 {
			status = monitor.StatusDown
		}
		if err := store.SaveResult(ctx, newResult(target.ID, status, int64(i*10), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("SaveResult: %v", err)
		}
	}

	stats, err := store.Stats(ctx, target.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Total != 100 {
		t.Errorf("Total = %d, want 100", stats.Total)
	}
	if stats.Up != 99 || stats.Down != 1 {
		t.Errorf("Up/Down = %d/%d, want 99/1", stats.Up, stats.Down)
	}
	if stats.UptimePercent != 99 {
		t.Errorf("UptimePercent = %v, want 99", stats.UptimePercent)
	}
	if stats.AvgLatencyMS != 505 {
		t.Errorf("AvgLatencyMS = %d, want 505", stats.AvgLatencyMS)
	}
	if stats.P95LatencyMS != 950 {
		t.Errorf("P95LatencyMS = %d, want 950", stats.P95LatencyMS)
	}
	if stats.FirstCheckedAt.UnixMilli() != base.Add(time.Second).UnixMilli() {
		t.Errorf("FirstCheckedAt = %v, want %v", stats.FirstCheckedAt, base.Add(time.Second))
	}
	if stats.LastCheckedAt.UnixMilli() != base.Add(100*time.Second).UnixMilli() {
		t.Errorf("LastCheckedAt = %v, want %v", stats.LastCheckedAt, base.Add(100*time.Second))
	}
}

func TestStatsEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	stats, err := store.Stats(ctx, target.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Total != 0 || stats.Up != 0 || stats.Down != 0 || stats.UptimePercent != 0 {
		t.Errorf("empty stats = %+v, want zeroes", stats)
	}
	if stats.AvgLatencyMS != 0 || stats.P95LatencyMS != 0 {
		t.Errorf("empty latencies = %d/%d, want 0/0", stats.AvgLatencyMS, stats.P95LatencyMS)
	}
}

func TestPruneResults(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := newTarget("First")
	second := newTarget("Second")
	for _, target := range []*monitor.Target{first, second} {
		if err := store.CreateTarget(ctx, target); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}

	base := time.Now()
	for i := 0; i < 10; i++ {
		if err := store.SaveResult(ctx, newResult(first.ID, monitor.StatusUp, 1, base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("SaveResult: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := store.SaveResult(ctx, newResult(second.ID, monitor.StatusUp, 1, base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("SaveResult: %v", err)
		}
	}

	deleted, err := store.PruneResults(ctx, 5)
	if err != nil {
		t.Fatalf("PruneResults: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}

	firstResults, err := store.ListResults(ctx, first.ID, 100)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}
	if len(firstResults) != 5 {
		t.Errorf("len(first results) = %d, want 5", len(firstResults))
	}

	secondResults, err := store.ListResults(ctx, second.ID, 100)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}
	if len(secondResults) != 3 {
		t.Errorf("len(second results) = %d, want 3 (below retention)", len(secondResults))
	}

	if _, err := store.PruneResults(ctx, 0); err == nil {
		t.Error("PruneResults(keep=0) returned nil error, want validation error")
	}
}

func TestDeleteTargetCascadesResults(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := newTarget("API")
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := store.SaveResult(ctx, newResult(target.ID, monitor.StatusUp, 1, time.Now())); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}

	if err := store.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}

	results, err := store.ListResults(ctx, target.ID, 10)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0 after cascade delete", len(results))
	}
}

func TestContextCancellation(t *testing.T) {
	store := newTestStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.ListTargets(ctx); err == nil {
		t.Error("ListTargets with cancelled context returned nil error")
	}
}
