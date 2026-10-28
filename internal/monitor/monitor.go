// Package monitor содержит доменные сущности pingwatch: цель проверки,
// результат отдельной проверки и агрегированную статистику.
//
// Пакет намеренно не знает ни про HTTP, ни про SQLite: это чистые данные
// и правила их валидации.
package monitor

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Status — состояние цели. Отдельный тип, а не bool: "ещё не проверяли"
// и "проверили, всё плохо" — это разные вещи, и путать их нельзя.
type Status string

const (
	StatusUnknown Status = "unknown"
	StatusUp      Status = "up"
	StatusDown    Status = "down"
)

// Valid сообщает, известно ли системе такое состояние.
func (s Status) Valid() bool {
	switch s {
	case StatusUnknown, StatusUp, StatusDown:
		return true
	default:
		return false
	}
}

// Границы и значения по умолчанию для параметров цели.
const (
	DefaultIntervalSeconds = 30
	MinIntervalSeconds     = 5
	MaxIntervalSeconds     = 86400

	DefaultTimeoutSeconds = 10
	MinTimeoutSeconds     = 1
	MaxTimeoutSeconds     = 60

	maxNameLen = 100
)

var (
	ErrEmptyName       = errors.New("name must not be empty")
	ErrNameTooLong     = errors.New("name must not exceed 100 characters")
	ErrInvalidURL      = errors.New("url must be an absolute http(s) url")
	ErrInvalidInterval = errors.New("interval_seconds must be between 5 and 86400")
	ErrInvalidTimeout  = errors.New("timeout_seconds must be between 1 and 60 and less than interval_seconds")
	ErrInvalidStatus   = errors.New("unknown status")
	ErrTargetNotFound  = errors.New("target not found")
	ErrResultNotFound  = errors.New("result not found")
	ErrInvalidLimit    = errors.New("limit must be a positive integer")
)

// Target — то, что пользователь просит проверять.
type Target struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	IntervalSeconds int       `json:"interval_seconds"`
	TimeoutSeconds  int       `json:"timeout_seconds"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

// Normalize проставляет значения по умолчанию и убирает лишние пробелы.
// Вызывается до Validate, чтобы пустой interval означал "30 секунд",
// а не ошибку валидации.
func (t *Target) Normalize() {
	t.Name = strings.TrimSpace(t.Name)
	t.URL = strings.TrimSpace(t.URL)

	if t.IntervalSeconds == 0 {
		t.IntervalSeconds = DefaultIntervalSeconds
	}
	if t.TimeoutSeconds == 0 {
		t.TimeoutSeconds = DefaultTimeoutSeconds
	}
}

// Validate проверяет, что цель можно поставить в расписание.
func (t *Target) Validate() error {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return ErrEmptyName
	}
	if len([]rune(name)) > maxNameLen {
		return ErrNameTooLong
	}

	parsed, err := url.Parse(strings.TrimSpace(t.URL))
	if err != nil {
		return ErrInvalidURL
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return ErrInvalidURL
	}

	if t.IntervalSeconds < MinIntervalSeconds || t.IntervalSeconds > MaxIntervalSeconds {
		return ErrInvalidInterval
	}

	if t.TimeoutSeconds < MinTimeoutSeconds || t.TimeoutSeconds > MaxTimeoutSeconds {
		return ErrInvalidTimeout
	}
	if t.TimeoutSeconds >= t.IntervalSeconds {
		return ErrInvalidTimeout
	}

	return nil
}

// Interval возвращает интервал проверки в виде time.Duration.
func (t Target) Interval() time.Duration {
	return time.Duration(t.IntervalSeconds) * time.Second
}

// Timeout возвращает таймаут одной проверки в виде time.Duration.
func (t Target) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

// Result — одна проверка цели. Ошибка хранится строкой, а не error:
// результат живёт в базе дольше, чем живёт конкретный тип ошибки.
type Result struct {
	ID         int64     `json:"id"`
	TargetID   int64     `json:"target_id"`
	Status     Status    `json:"status"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Stats — агрегат по истории проверок одной цели.
type Stats struct {
	TargetID       int64     `json:"target_id"`
	Total          int       `json:"total"`
	Up             int       `json:"up"`
	Down           int       `json:"down"`
	UptimePercent  float64   `json:"uptime_percent"`
	AvgLatencyMS   int64     `json:"avg_latency_ms"`
	P95LatencyMS   int64     `json:"p95_latency_ms"`
	FirstCheckedAt time.Time `json:"first_checked_at,omitempty"`
	LastCheckedAt  time.Time `json:"last_checked_at,omitempty"`
}

// IsValidationError отвечает на вопрос "это ошибка ввода или сбой сервера".
// Одна точка правды: HTTP-слой не перечисляет ошибки модели вручную.
func IsValidationError(err error) bool {
	for _, target := range []error{
		ErrEmptyName,
		ErrNameTooLong,
		ErrInvalidURL,
		ErrInvalidInterval,
		ErrInvalidTimeout,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}
