package notify

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

const telegramAPIBase = "https://api.telegram.org"

var httpClient = &http.Client{Timeout: 10 * time.Second}

// sendTelegram posts a message via the Telegram Bot API.
func sendTelegram(ctx context.Context, token, chatID, text string) error {
	return sendTelegramTo(ctx, telegramAPIBase, token, chatID, text)
}

// sendTelegramTo is sendTelegram with an overridable base URL (for tests).
func sendTelegramTo(ctx context.Context, base, token, chatID, text string) error {
	url := base + "/bot" + token + "/sendMessage"
	payload, _ := json.Marshal(map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return redactToken(err.Error(), token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// url.Error embeds the request URL (and thus the token) — redact it.
		return redactToken(err.Error(), token)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &parsed)
	if resp.StatusCode != http.StatusOK || !parsed.OK {
		desc := parsed.Description
		if desc == "" {
			desc = resp.Status
		}
		return fmt.Errorf("telegram: %s", desc) // never includes the token
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

// redactToken removes the bot token from an arbitrary string (e.g. a url.Error).
func redactToken(s, token string) errString {
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return errString(s)
}
