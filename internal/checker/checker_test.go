package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

func targetFor(rawURL string) monitor.Target {
	return monitor.Target{
		ID:              1,
		Name:            "test",
		URL:             rawURL,
		IntervalSeconds: 30,
		TimeoutSeconds:  2,
		Enabled:         true,
	}
}

func TestCheckUp(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "200", statusCode: http.StatusOK},
		{name: "204", statusCode: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "pingwatch/") {
					t.Errorf("User-Agent = %q, want pingwatch/...", ua)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			result := NewHTTP().Check(context.Background(), targetFor(srv.URL))

			if result.Status != monitor.StatusUp {
				t.Fatalf("Status = %q, want up (error: %q)", result.Status, result.Error)
			}
			if result.StatusCode != tt.statusCode {
				t.Errorf("StatusCode = %d, want %d", result.StatusCode, tt.statusCode)
			}
			if result.Error != "" {
				t.Errorf("Error = %q, want empty", result.Error)
			}
			if result.TargetID != 1 {
				t.Errorf("TargetID = %d, want 1", result.TargetID)
			}
			if result.CheckedAt.IsZero() {
				t.Error("CheckedAt is zero")
			}
			if result.LatencyMS < 0 {
				t.Errorf("LatencyMS = %d, want >= 0", result.LatencyMS)
			}
		})
	}
}

func TestCheckFollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := NewHTTP().Check(context.Background(), targetFor(srv.URL+"/old"))

	if result.Status != monitor.StatusUp {
		t.Fatalf("Status = %q, want up (error: %q)", result.Status, result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 after redirect", result.StatusCode)
	}
}

func TestCheckServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	result := NewHTTP().Check(context.Background(), targetFor(srv.URL))

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down", result.Status)
	}
	if result.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", result.StatusCode)
	}
	if !strings.Contains(result.Error, "unexpected status 500") {
		t.Errorf("Error = %q, want mention of status 500", result.Error)
	}
}

func TestCheckClientErrorIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	result := NewHTTP().Check(context.Background(), targetFor(srv.URL))

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down: 404 — это нерабочий сервис", result.Status)
	}
	if result.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", result.StatusCode)
	}
}

func TestCheckTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	target := targetFor(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := NewHTTP().Check(ctx, target)
	elapsed := time.Since(start)

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down", result.Status)
	}
	if !strings.Contains(result.Error, "timeout after 2s") {
		t.Errorf("Error = %q, want readable timeout message", result.Error)
	}
	if elapsed > time.Second {
		t.Errorf("Check took %v, want it to respect the 50ms context deadline", elapsed)
	}
}

func TestCheckCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := NewHTTP().Check(ctx, targetFor("http://127.0.0.1:1/"))

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down", result.Status)
	}
	if result.Error != "check canceled" {
		t.Errorf("Error = %q, want %q", result.Error, "check canceled")
	}
}

func TestCheckInvalidURL(t *testing.T) {
	result := NewHTTP().Check(context.Background(), targetFor("://bad-url"))

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down", result.Status)
	}
	if result.Error == "" {
		t.Error("Error is empty, want request error")
	}
}

func TestCheckConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	result := NewHTTP().Check(context.Background(), targetFor(url))

	if result.Status != monitor.StatusDown {
		t.Fatalf("Status = %q, want down", result.Status)
	}
	if result.Error == "" {
		t.Error("Error is empty, want connection error")
	}
	// net/url.Error оборачивает причину и дублирует URL — в тексте ошибки
	// он не нужен, там и так есть сама цель.
	if strings.Contains(result.Error, "Get \""+url+"\"") {
		t.Errorf("Error = %q, want it without duplicated method and URL", result.Error)
	}
}
