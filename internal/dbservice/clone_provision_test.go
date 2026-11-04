package dbservice

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type secretErrorEngine struct{ *mockEngine }

func (e secretErrorEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return errors.New("CREATE USER clone PASSWORD 'do-not-log-this'")
}

func TestCloneExecDoesNotExposeSQLBearingError(t *testing.T) {
	svc := New(secretErrorEngine{newMockEngine()}, nil, nil, "")
	c := CloneDatabase{Operation: strings.Repeat("a", 32), DBName: "clone_db", Username: "clone_user", Deadline: time.Now().Add(time.Minute)}
	err := svc.ProvisionCloneDatabase(t.Context(), Instance{AppName: "fixture"}, c, "fixture_password_for_clone")
	if err == nil || strings.Contains(err.Error(), "do-not-log") || errors.Unwrap(err) != nil {
		t.Fatal("raw SQL error was exposed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := svc.CleanupCloneDatabase(ctx, Instance{AppName: "fixture"}, c); !errors.Is(err, context.Canceled) {
		t.Fatal("safe cancellation classification was lost")
	}
}
