package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// Store is the slice of the generated queries the gateway secret needs.
type Store interface {
	InitPanelGateway(ctx context.Context, secret string) error
	GetPanelGateway(ctx context.Context) (db.PanelGateway, error)
	SetPanelGatewaySecret(ctx context.Context, secret string) error
}

// EnsureSecret returns the stored gateway secret, creating it on first use.
//
// A secret that can no longer be decrypted — KRILL_SECRET_KEY changed — is
// replaced rather than reported: nothing outside this instance holds it, so a
// new one costs a single gateway redeploy (the provider arguments change),
// while refusing would leave the panel domain unroutable until someone edits
// the database by hand.
func EnsureSecret(ctx context.Context, q Store) (string, error) {
	fresh, err := randomSecret()
	if err != nil {
		return "", err
	}
	if err := q.InitPanelGateway(ctx, secret.Enc(fresh)); err != nil {
		return "", fmt.Errorf("init panel gateway: %w", err)
	}
	row, err := q.GetPanelGateway(ctx)
	if err != nil {
		return "", fmt.Errorf("read panel gateway: %w", err)
	}
	plain, err := secret.Dec(row.Secret)
	if errors.Is(err, secret.ErrUndecryptable) {
		slog.Warn("panel gateway secret cannot be decrypted with the current KRILL_SECRET_KEY; replacing it (the gateway will be redeployed once)")
		if err := q.SetPanelGatewaySecret(ctx, secret.Enc(fresh)); err != nil {
			return "", fmt.Errorf("replace panel gateway secret: %w", err)
		}
		return fresh, nil
	}
	if err != nil {
		return "", err
	}
	if plain == "" {
		return "", errors.New("panel gateway secret is empty")
	}
	return plain, nil
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate panel gateway secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SettingsFromRow converts the stored row into the routing view.
func SettingsFromRow(row db.PanelGateway) Settings {
	return Settings{Host: row.Host, State: row.State, AllowedIPs: SplitLines(row.AllowedIps)}
}

// SplitLines parses a stored newline-separated list into trimmed, non-empty
// entries.
func SplitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
