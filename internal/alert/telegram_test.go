package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramNotifierSendsMessage(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotBody = body

		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()

	notifier := &TelegramNotifier{
		Token:   "123:secret",
		ChatID:  "-100500",
		BaseURL: srv.URL + "/",
		Client:  srv.Client(),
	}

	err := notifier.Notify(context.Background(), Notification{
		Event:  EventDown,
		Target: testTarget(1, "API"),
		Result: downResult(1, 500),
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotPath != "/bot123:secret/sendMessage" {
		t.Errorf("path = %q, want /bot123:secret/sendMessage", gotPath)
	}
	if gotBody["chat_id"] != "-100500" {
		t.Errorf("chat_id = %v, want -100500", gotBody["chat_id"])
	}

	text, _ := gotBody["text"].(string)
	if !strings.Contains(text, "DOWN: API") {
		t.Errorf("text = %q, want DOWN message", text)
	}
	if gotBody["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview = %v, want true", gotBody["disable_web_page_preview"])
	}
}

func TestTelegramNotifierAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()

	notifier := &TelegramNotifier{Token: "1:x", ChatID: "1", BaseURL: srv.URL, Client: srv.Client()}

	err := notifier.Notify(context.Background(), Notification{Event: EventUp, Target: testTarget(1, "API")})
	if err == nil {
		t.Fatal("Notify returned nil error for ok=false")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error = %v, want Telegram description", err)
	}
}

func TestTelegramNotifierHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	notifier := &TelegramNotifier{Token: "1:x", ChatID: "1", BaseURL: srv.URL, Client: srv.Client()}

	err := notifier.Notify(context.Background(), Notification{Event: EventUp, Target: testTarget(1, "API")})
	if err == nil {
		t.Fatal("Notify returned nil error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want HTTP status", err)
	}
}

func TestTelegramNotifierMissingCredentials(t *testing.T) {
	tests := []struct {
		name     string
		notifier *TelegramNotifier
	}{
		{name: "no token", notifier: &TelegramNotifier{ChatID: "1"}},
		{name: "no chat id", notifier: &TelegramNotifier{Token: "1:x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.notifier.Notify(context.Background(), Notification{}); err == nil {
				t.Fatal("Notify returned nil error, want credentials error")
			}
		})
	}
}
