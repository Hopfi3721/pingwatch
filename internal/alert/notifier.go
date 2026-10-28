package alert

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// LogNotifier пишет уведомления в лог. Всегда включён: это бесплатный
// журнал переходов, по которому можно восстановить картину инцидента.
type LogNotifier struct {
	Logger *log.Logger
}

func (n *LogNotifier) Notify(_ context.Context, notification Notification) error {
	logger := n.Logger
	if logger == nil {
		logger = log.Default()
	}

	logger.Printf("alert: %s", strings.ReplaceAll(Format(notification), "\n", " | "))
	return nil
}

// MultiNotifier рассылает уведомление во все каналы и возвращает
// объединённую ошибку. Одна упавшая интеграция не должна мешать остальным.
type MultiNotifier []Notifier

func (m MultiNotifier) Notify(ctx context.Context, notification Notification) error {
	var errs []error

	for _, notifier := range m {
		if notifier == nil {
			continue
		}
		if err := notifier.Notify(ctx, notification); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Format собирает текст уведомления. Одна функция на все каналы:
// лог и Telegram не должны рассказывать разные истории.
func Format(notification Notification) string {
	var b strings.Builder

	switch notification.Event {
	case EventDown:
		fmt.Fprintf(&b, "DOWN: %s\n", notification.Target.Name)
	case EventUp:
		fmt.Fprintf(&b, "RECOVERED: %s\n", notification.Target.Name)
	default:
		fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(string(notification.Event)), notification.Target.Name)
	}

	fmt.Fprintf(&b, "URL: %s\n", notification.Target.URL)

	if notification.Result.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP: %d\n", notification.Result.StatusCode)
	}
	if notification.Result.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", notification.Result.Error)
	}
	if notification.Event == EventDown && notification.ConsecutiveFailures > 0 {
		fmt.Fprintf(&b, "Failed checks: %d\n", notification.ConsecutiveFailures)
	}
	if notification.Result.LatencyMS > 0 {
		fmt.Fprintf(&b, "Latency: %d ms\n", notification.Result.LatencyMS)
	}

	checkedAt := notification.Result.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	fmt.Fprintf(&b, "Time: %s", checkedAt.UTC().Format(time.RFC3339))

	return b.String()
}
