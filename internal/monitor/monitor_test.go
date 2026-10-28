package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validTarget() Target {
	return Target{
		Name:            "API",
		URL:             "https://example.com/healthz",
		IntervalSeconds: 30,
		TimeoutSeconds:  10,
		Enabled:         true,
	}
}

func TestTargetValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Target)
		want   error
	}{
		{name: "valid", mutate: func(*Target) {}},
		{name: "min interval", mutate: func(target *Target) {
			target.IntervalSeconds = MinIntervalSeconds
			target.TimeoutSeconds = MinTimeoutSeconds
		}},
		{name: "max interval", mutate: func(target *Target) { target.IntervalSeconds = MaxIntervalSeconds }},
		{name: "interval equals timeout plus one", mutate: func(target *Target) { target.IntervalSeconds = 11 }},
		{name: "http", mutate: func(target *Target) { target.URL = "http://localhost:8080/healthz" }},
		{name: "empty name", mutate: func(target *Target) { target.Name = "" }, want: ErrEmptyName},
		{name: "blank name", mutate: func(target *Target) { target.Name = "   " }, want: ErrEmptyName},
		{name: "long name", mutate: func(target *Target) { target.Name = strings.Repeat("я", 101) }, want: ErrNameTooLong},
		{name: "empty url", mutate: func(target *Target) { target.URL = "" }, want: ErrInvalidURL},
		{name: "no scheme", mutate: func(target *Target) { target.URL = "example.com" }, want: ErrInvalidURL},
		{name: "ftp", mutate: func(target *Target) { target.URL = "ftp://example.com/file" }, want: ErrInvalidURL},
		{name: "javascript", mutate: func(target *Target) { target.URL = "javascript:alert(1)" }, want: ErrInvalidURL},
		{name: "no host", mutate: func(target *Target) { target.URL = "http://" }, want: ErrInvalidURL},
		{name: "interval too small", mutate: func(target *Target) { target.IntervalSeconds = MinIntervalSeconds - 1 }, want: ErrInvalidInterval},
		{name: "interval too big", mutate: func(target *Target) { target.IntervalSeconds = MaxIntervalSeconds + 1 }, want: ErrInvalidInterval},
		{name: "zero timeout", mutate: func(target *Target) { target.TimeoutSeconds = 0 }, want: ErrInvalidTimeout},
		{name: "timeout too big", mutate: func(target *Target) { target.TimeoutSeconds = MaxTimeoutSeconds + 1 }, want: ErrInvalidTimeout},
		{name: "timeout equals interval", mutate: func(target *Target) {
			target.IntervalSeconds = 10
			target.TimeoutSeconds = 10
		}, want: ErrInvalidTimeout},
		{name: "timeout bigger than interval", mutate: func(target *Target) {
			target.IntervalSeconds = 10
			target.TimeoutSeconds = 30
		}, want: ErrInvalidTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := validTarget()
			tt.mutate(&target)

			err := target.Validate()
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTargetNormalize(t *testing.T) {
	target := Target{
		Name: "  API  ",
		URL:  "  https://example.com  ",
	}

	target.Normalize()

	if target.Name != "API" {
		t.Errorf("Name = %q, want %q", target.Name, "API")
	}
	if target.URL != "https://example.com" {
		t.Errorf("URL = %q, want %q", target.URL, "https://example.com")
	}
	if target.IntervalSeconds != DefaultIntervalSeconds {
		t.Errorf("IntervalSeconds = %d, want %d", target.IntervalSeconds, DefaultIntervalSeconds)
	}
	if target.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want %d", target.TimeoutSeconds, DefaultTimeoutSeconds)
	}

	if err := target.Validate(); err != nil {
		t.Fatalf("normalized target is invalid: %v", err)
	}
}

func TestTargetDurations(t *testing.T) {
	target := validTarget()
	target.IntervalSeconds = 45
	target.TimeoutSeconds = 7

	if got := target.Interval(); got != 45*time.Second {
		t.Errorf("Interval() = %v, want 45s", got)
	}
	if got := target.Timeout(); got != 7*time.Second {
		t.Errorf("Timeout() = %v, want 7s", got)
	}
}

func TestStatusValid(t *testing.T) {
	for _, status := range []Status{StatusUnknown, StatusUp, StatusDown} {
		if !status.Valid() {
			t.Errorf("Status(%q).Valid() = false, want true", status)
		}
	}

	if Status("broken").Valid() {
		t.Error(`Status("broken").Valid() = true, want false`)
	}
}

func TestIsValidationError(t *testing.T) {
	if !IsValidationError(ErrEmptyName) {
		t.Error("IsValidationError(ErrEmptyName) = false, want true")
	}
	if IsValidationError(errors.New("database is down")) {
		t.Error("IsValidationError(internal error) = true, want false")
	}
}
