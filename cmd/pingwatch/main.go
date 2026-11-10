// Command pingwatch запускает монитор доступности: планировщик проверок,
// SQLite-хранилище, HTTP API и встроенный дашборд в одном процессе.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/alert"
	"github.com/Hopfi3721/pingwatch/internal/api"
	"github.com/Hopfi3721/pingwatch/internal/checker"
	"github.com/Hopfi3721/pingwatch/internal/scheduler"
	"github.com/Hopfi3721/pingwatch/internal/storage"
)

// version подставляется на релизе через -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-version", "--version", "-v":
			fmt.Println("pingwatch", version)
			return
		}
	}
	logger := log.New(os.Stdout, "pingwatch ", log.LstdFlags|log.Lmsgprefix)
	if err := run(logger); err != nil {
		logger.Fatalf("fatal: %v", err)
	}
}

func run(logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := envOr("DB_PATH", "pingwatch.db")
	workers := envInt("WORKERS", 8, logger)
	threshold := envInt("FAILURE_THRESHOLD", 3, logger)
	retention := envInt("RESULTS_RETENTION", 5000, logger)

	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := storage.Migrate(ctx, db); err != nil {
		return err
	}

	store := storage.NewSQLiteStore(db)

	// Состояние алертов восстанавливается из базы: сервис перезапустили,
	// а цель всё ещё лежит — не нужно присылать повторный DOWN.
	alerts := alert.NewStateMachine(threshold)
	latest, err := store.LatestResults(ctx)
	if err != nil {
		return err
	}
	for targetID, result := range latest {
		alerts.Seed(targetID, result.Status)
	}

	notifiers := alert.MultiNotifier{&alert.LogNotifier{Logger: logger}}
	if token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")); token != "" {
		chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
		if chatID == "" {
			return errors.New("TELEGRAM_CHAT_ID must be set together with TELEGRAM_BOT_TOKEN")
		}

		notifiers = append(notifiers, &alert.TelegramNotifier{
			Token:   token,
			ChatID:  chatID,
			BaseURL: envOr("TELEGRAM_API_BASE", ""),
		})
		logger.Print("telegram alerts enabled")
	}

	sched := scheduler.New(
		store,
		checker.NewHTTP(),
		alerts,
		notifiers,
		logger,
		scheduler.Config{
			Workers:    workers,
			Tick:       time.Second,
			Retention:  retention,
			PruneEvery: time.Hour,
		},
	)

	srv := &http.Server{
		Addr:              envOr("ADDR", ":8080"),
		Handler:           api.New(store, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	schedCtx, stopScheduler := context.WithCancel(context.Background())
	var schedulerWG sync.WaitGroup

	schedulerWG.Add(1)
	go func() {
		defer schedulerWG.Done()
		sched.Run(schedCtx)
	}()

	serverErr := make(chan error, 1)
	go func() {
		logger.Printf(
			"listening on %s (db: %s, workers: %d, failure threshold: %d)",
			srv.Addr, dbPath, workers, threshold,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		stopScheduler()
		schedulerWG.Wait()
		return err
	case <-ctx.Done():
	}

	logger.Print("shutting down...")
	stopScheduler()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("server shutdown: %v", err)
	}

	// Планировщик дожидается воркеров и только потом отпускает базу.
	schedulerWG.Wait()

	logger.Print("stopped")

	return nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int, logger *log.Logger) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		logger.Printf("config: %s=%q is not a positive integer, using %d", key, raw, fallback)
		return fallback
	}

	return value
}
