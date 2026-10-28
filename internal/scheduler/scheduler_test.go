package scheduler

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/alert"
	"github.com/Hopfi3721/pingwatch/internal/monitor"
	"github.com/Hopfi3721/pingwatch/internal/storage"
)

type fakeChecker struct {
	mu     sync.Mutex
	calls  int
	status monitor.Status
}

func (f *fakeChecker) Check(_ context.Context, target monitor.Target) monitor.Result {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	return monitor.Result{
		TargetID:  target.ID,
		Status:    f.status,
		LatencyMS: 5,
		CheckedAt: time.Now(),
	}
}

func (f *fakeChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

type recordingNotifier struct {
	mu            sync.Mutex
	notifications []alert.Notification
}

func (n *recordingNotifier) Notify(_ context.Context, notification alert.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.notifications = append(n.notifications, notification)
	return nil
}

func (n *recordingNotifier) all() []alert.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]alert.Notification(nil), n.notifications...)
}

type harness struct {
	scheduler *Scheduler
	store     *storage.SQLiteStore
	checker   *fakeChecker
	notifier  *recordingNotifier
}

func newHarness(t *testing.T, threshold int, status monitor.Status) *harness {
	t.Helper()

	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := storage.NewSQLiteStore(db)
	checker := &fakeChecker{status: status}
	notifier := &recordingNotifier{}

	scheduler := New(
		store,
		checker,
		alert.NewStateMachine(threshold),
		notifier,
		log.New(testWriter{t}, "", 0),
		Config{
			Workers:    2,
			Tick:       20 * time.Millisecond,
			Retention:  100,
			PruneEvery: time.Hour,
		},
	)

	return &harness{
		scheduler: scheduler,
		store:     store,
		checker:   checker,
		notifier:  notifier,
	}
}

// testWriter уводит логи планировщика в вывод теста, чтобы упавший тест
// показывал причину, а не молчал.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func createTarget(t *testing.T, store *storage.SQLiteStore, name string, enabled bool) *monitor.Target {
	t.Helper()

	target := &monitor.Target{
		Name:            name,
		URL:             "https://example.com/healthz",
		IntervalSeconds: 1,
		TimeoutSeconds:  1,
		Enabled:         enabled,
		CreatedAt:       time.Now().UTC(),
	}

	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	return target
}

func runScheduler(t *testing.T, s *Scheduler) (cancel func(), stopped <-chan struct{}) {
	t.Helper()

	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		s.Run(ctx)
		close(done)
	}()

	return cancelCtx, done
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timeout after %s: %s", timeout, message)
}

func TestSchedulerSavesResultsAndStops(t *testing.T) {
	h := newHarness(t, 1, monitor.StatusUp)
	target := createTarget(t, h.store, "API", true)

	cancel, stopped := runScheduler(t, h.scheduler)

	waitFor(t, 3*time.Second, func() bool {
		stats, err := h.store.Stats(context.Background(), target.ID)
		return err == nil && stats.Total >= 2
	}, "expected at least two checks to be stored")

	cancel()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if got := h.checker.callCount(); got < 2 {
		t.Errorf("checker calls = %d, want >= 2", got)
	}
	if notifications := h.notifier.all(); len(notifications) != 0 {
		t.Errorf("notifications = %+v, want none for a healthy target", notifications)
	}

	latest, err := h.store.LatestResult(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("LatestResult: %v", err)
	}
	if latest.Status != monitor.StatusUp || latest.TargetID != target.ID {
		t.Errorf("latest result = %+v, want up for target %d", latest, target.ID)
	}
}

func TestSchedulerNotifiesOnDown(t *testing.T) {
	h := newHarness(t, 1, monitor.StatusDown)
	target := createTarget(t, h.store, "API", true)

	cancel, stopped := runScheduler(t, h.scheduler)
	defer func() {
		cancel()
		<-stopped
	}()

	waitFor(t, 3*time.Second, func() bool {
		return len(h.notifier.all()) >= 1
	}, "expected a DOWN notification")

	notifications := h.notifier.all()
	if notifications[0].Event != alert.EventDown {
		t.Errorf("event = %q, want down", notifications[0].Event)
	}
	if notifications[0].Target.ID != target.ID {
		t.Errorf("target = %d, want %d", notifications[0].Target.ID, target.ID)
	}

	// Уведомление должно быть одно, а проверок — много.
	waitFor(t, 2*time.Second, func() bool {
		stats, err := h.store.Stats(context.Background(), target.ID)
		return err == nil && stats.Total >= 2
	}, "expected checks to continue after the alert")

	if got := len(h.notifier.all()); got != 1 {
		t.Errorf("notifications = %d, want exactly 1", got)
	}
}

func TestSchedulerSkipsDisabledTargets(t *testing.T) {
	h := newHarness(t, 1, monitor.StatusUp)
	target := createTarget(t, h.store, "Disabled", false)

	cancel, stopped := runScheduler(t, h.scheduler)
	defer func() {
		cancel()
		<-stopped
	}()

	time.Sleep(200 * time.Millisecond)

	stats, err := h.store.Stats(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Total != 0 {
		t.Errorf("checks for disabled target = %d, want 0", stats.Total)
	}
	if got := h.checker.callCount(); got != 0 {
		t.Errorf("checker calls = %d, want 0", got)
	}
}

func TestSchedulerForgetsDeletedTargets(t *testing.T) {
	h := newHarness(t, 1, monitor.StatusUp)
	target := createTarget(t, h.store, "API", true)

	cancel, stopped := runScheduler(t, h.scheduler)
	defer func() {
		cancel()
		<-stopped
	}()

	waitFor(t, 3*time.Second, func() bool {
		stats, err := h.store.Stats(context.Background(), target.ID)
		return err == nil && stats.Total >= 1
	}, "expected the first check")

	if err := h.store.DeleteTarget(context.Background(), target.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		h.scheduler.mu.Lock()
		defer h.scheduler.mu.Unlock()

		_, tracked := h.scheduler.lastCheck[target.ID]
		return !tracked
	}, "scheduler must drop in-memory state of a deleted target")

	if status := h.scheduler.alerts.Status(target.ID); status != monitor.StatusUnknown {
		t.Errorf("alert state after delete = %q, want unknown", status)
	}
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	cfg.setDefaults()

	if cfg.Workers != 1 {
		t.Errorf("Workers = %d, want 1", cfg.Workers)
	}
	if cfg.Tick != time.Second {
		t.Errorf("Tick = %v, want 1s", cfg.Tick)
	}
	if cfg.Retention != 5000 {
		t.Errorf("Retention = %d, want 5000", cfg.Retention)
	}
	if cfg.PruneEvery != time.Hour {
		t.Errorf("PruneEvery = %v, want 1h", cfg.PruneEvery)
	}
}
