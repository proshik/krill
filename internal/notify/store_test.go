package notify_test

import (
	"context"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

func TestChannelsForOrgDecryptsToken(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")

	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	ctx := context.Background()

	ownerID := mkUser(t, q, "n-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")

	stored, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
		OrgID: o.ID, Type: "telegram", Enabled: true,
		BotToken: secret.Enc("super-token"), ChatID: "42",
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !strings.HasPrefix(stored.BotToken, "enc:v1:") {
		t.Fatalf("token not encrypted at rest: %q", stored.BotToken)
	}

	st := notify.NewDBStore(q)
	chans, err := st.ChannelsForOrg(ctx, o.ID)
	if err != nil || len(chans) != 1 {
		t.Fatalf("ChannelsForOrg: %v, n=%d", err, len(chans))
	}
	if chans[0].BotToken != "super-token" {
		t.Fatalf("token not decrypted: %q", chans[0].BotToken)
	}
}

func mkUser(t *testing.T, q *db.Queries, email string) int64 {
	t.Helper()
	u, err := q.CreateUser(context.Background(), db.CreateUserParams{Email: email, PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}
