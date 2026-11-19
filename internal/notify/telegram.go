package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	return sendTelegramWith(ctx, httpClient, base, token, chatID, text)
}

// sendTelegramWith posts a message through client.
func sendTelegramWith(ctx context.Context, client *http.Client, base, token, chatID, text string) error {
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
	resp, err := client.Do(req)
	if err != nil {
		// url.Error embeds the request URL (and thus the token) — redact it.
		return redactToken(err.Error(), token)
	}
	defer resp.Body.Close()
	return telegramResult(resp)
}

// telegramReplyMax caps how much of a Bot API reply is read. sendMessage
// echoes the whole message back (text and entities), so a long digest's
// reply runs to several KiB; a successful reply is never near this.
const telegramReplyMax = 1 << 20

// telegramResult reads a Bot API reply. A refusal (any status but 200)
// fails with Telegram's description, or the status without one. On 200 the
// message was accepted: Telegram answers 200 only with "ok": true, so a body
// that cannot be read or parsed (cut at telegramReplyMax, a proxy closing
// early) still counts as sent, with a warning — calling it a failure would
// make the digest resend a message that arrived. JSON without "ok": true on
// 200 (an explicit false, or "{}" from something that is not the Bot API)
// fails.
func telegramResult(resp *http.Response) error {
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, telegramReplyMax))
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	perr := json.Unmarshal(body, &parsed)
	if resp.StatusCode != http.StatusOK {
		desc := resp.Status
		if perr == nil && parsed.Description != "" {
			desc = parsed.Description
		}
		return fmt.Errorf("telegram: %s", desc) // never includes the token
	}
	if perr != nil || rerr != nil {
		slog.Warn("notify: Telegram accepted a message (200) with a reply that could not be read; counting it as sent",
			"read_err", rerr, "parse_err", perr, "bytes", len(body))
		return nil
	}
	if !parsed.OK {
		desc := parsed.Description
		if desc == "" {
			desc = "ok: false"
		}
		return fmt.Errorf("telegram: %s", desc)
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
