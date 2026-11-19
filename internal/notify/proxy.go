package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ProxyEnv names the setting that routes Telegram messages through a proxy.
const ProxyEnv = "KRILL_TELEGRAM_PROXY"

// parseTelegramProxy validates a proxy URL for the Telegram sender. Its
// errors never repeat the value: it may carry credentials.
func parseTelegramProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("not a valid URL (check the scheme, the host and the port)")
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		// The scheme is not repeated: in "user:pw@host:1080" it is the user.
		return nil, errors.New("unsupported or missing scheme: want socks5h://, socks5://, http:// or https://")
	}
	if u.Hostname() == "" {
		return nil, errors.New("no proxy host")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("a proxy URL has no path, query or fragment")
	}
	return u, nil
}

// proxyTestTransport lets tests adjust the proxied transport: a local TLS
// server standing in for api.telegram.org, and a dialer that refuses
// anything but loopback, so no regression can reach the real Bot API.
var proxyTestTransport func(*http.Transport)

// proxyLabel is a proxy URL without its credentials, for the log.
func proxyLabel(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// telegramClient is an HTTP client that reaches the Bot API only through
// proxy. It is its own transport: no other client of Krill sees the proxy,
// and HTTPS_PROXY/ALL_PROXY no longer apply to Telegram once it is set.
// Go resolves the target name on the proxy for socks5 and socks5h alike.
func telegramClient(proxy *url.URL) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyURL(proxy)
	if proxyTestTransport != nil {
		proxyTestTransport(t)
	}
	return &http.Client{Timeout: httpClient.Timeout, Transport: t}
}

// SetTelegramProxy routes every message of s (alerts, test messages, the
// digest) through the proxy URL raw; empty leaves them direct. An invalid
// value never falls back to sending directly: every send then fails with
// the reason, and the error is returned for the startup log. On success it
// returns the proxy as scheme://host:port, credentials left out. Call it
// before s sends anything (main does, at startup).
func (s *Service) SetTelegramProxy(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	u, err := parseTelegramProxy(raw)
	if err != nil {
		err = fmt.Errorf("%s is invalid: %w", ProxyEnv, err)
		s.send = func(context.Context, string, string, string) error {
			return fmt.Errorf("telegram: not sent: %w", err)
		}
		return "", err
	}
	client := telegramClient(u)
	secret := ""
	if pw, ok := u.User.Password(); ok {
		secret = pw
	}
	s.send = func(ctx context.Context, token, chatID, text string) error {
		err := sendTelegramWith(ctx, client, telegramAPIBase, token, chatID, text)
		if err != nil && secret != "" {
			err = redactToken(err.Error(), secret)
		}
		return err
	}
	return proxyLabel(u), nil
}
