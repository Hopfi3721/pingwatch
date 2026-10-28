// Package checker выполняет одну HTTP-проверку цели.
package checker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

// HTTPChecker проверяет доступность URL.
//
// Успех — любой ответ 2xx или 3xx: редирект на странице логина или
// 204 от healthz одинаково означают "сервис жив". 4xx/5xx — падение,
// потому что для пользователя это нерабочий сервис.
type HTTPChecker struct {
	client *http.Client
}

func NewHTTP() *HTTPChecker {
	return &HTTPChecker{
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
			// Свой таймаут не задаём: дедлайн приходит из контекста,
			// чтобы он был один, а не два конкурирующих.
		},
	}
}

// Check никогда не возвращает error: неудача проверки — это тоже результат,
// который нужно сохранить и показать. Ошибка транспорта живёт в Result.Error.
func (c *HTTPChecker) Check(ctx context.Context, target monitor.Target) monitor.Result {
	result := monitor.Result{
		TargetID:  target.ID,
		Status:    monitor.StatusDown,
		CheckedAt: time.Now(),
	}

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		result.Error = err.Error()
		result.LatencyMS = time.Since(start).Milliseconds()
		return result
	}
	req.Header.Set("User-Agent", "pingwatch/0.1 (+https://github.com/Hopfi3721/pingwatch)")

	resp, err := c.client.Do(req)
	if err != nil {
		result.Error = describeError(ctx, err, target.Timeout())
		result.LatencyMS = time.Since(start).Milliseconds()
		return result
	}
	defer resp.Body.Close()

	// Дочитываем немного тела, чтобы соединение вернулось в пул.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))

	result.LatencyMS = time.Since(start).Milliseconds()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.Status = monitor.StatusUp
		return result
	}

	result.Error = fmt.Sprintf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))

	return result
}

// describeError превращает ошибку транспорта в текст, который полезно
// увидеть в дашборде и в Telegram. "context deadline exceeded" сам по себе
// ничего не говорит дежурному.
func describeError(ctx context.Context, err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("timeout after %s", timeout)
	}
	if errors.Is(err, context.Canceled) {
		return "check canceled"
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		// net/url.Error дублирует метод и URL, они и так известны из цели.
		return urlErr.Err.Error()
	}

	return err.Error()
}
