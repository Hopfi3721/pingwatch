package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

// TelegramNotifier отправляет уведомления в чат через Bot API.
//
// BaseURL настраивается, потому что в тестах и в закрытых контурах
// Telegram API заменяется на httptest-сервер или локальный прокси.
type TelegramNotifier struct {
	Token   string
	ChatID  string
	BaseURL string
	Client  *http.Client
}

func (n *TelegramNotifier) Notify(ctx context.Context, notification Notification) error {
	if strings.TrimSpace(n.Token) == "" {
		return fmt.Errorf("telegram: token is empty")
	}
	if strings.TrimSpace(n.ChatID) == "" {
		return fmt.Errorf("telegram: chat id is empty")
	}

	baseURL := n.BaseURL
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	payload, err := json.Marshal(map[string]any{
		"chat_id":                  n.ChatID,
		"text":                     Format(notification),
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("telegram: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", baseURL, n.Token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	defer resp.Body.Close()

	// Тело небольшое, но читаем с лимитом: ответ приходит из внешней сети.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("telegram: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var apiResponse struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return fmt.Errorf("telegram: decode response: %w", err)
	}
	if !apiResponse.OK {
		return fmt.Errorf("telegram: api error: %s", apiResponse.Description)
	}

	return nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
