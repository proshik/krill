package api_test

import (
	"bytes"
	"encoding/json"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

func TestWhoami(t *testing.T) {
	f := newAPIFixture(t)

	who, err := f.svc.Whoami(t.Context(), f.ident)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if who.OrgName != "acme" {
		t.Fatalf("org name: want acme, got %q", who.OrgName)
	}
	if who.Level != "write" || who.Role != "owner" || !who.CanWrite {
		t.Fatalf("unexpected write identity: %+v", who)
	}

	readWho, err := f.svc.Whoami(t.Context(), f.readIdent)
	if err != nil {
		t.Fatalf("whoami (read): %v", err)
	}
	if readWho.Level != "read" || readWho.CanWrite {
		t.Fatalf("read token must not report can_write: %+v", readWho)
	}
}

func TestListAppsBatchesAcrossProjectsAndIncludesDomains(t *testing.T) {
	f := newAPIFixture(t)
	orgSvc := org.NewService(f.q)

	// A second project/env/app in the same org, to exercise the batched walk
	// across more than one project.
	proj2, err := orgSvc.CreateProject(t.Context(), f.ident.OrgID, "acme-proj-2", "")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	env2, err := orgSvc.CreateEnvironment(t.Context(), proj2.ID, "staging")
	if err != nil {
		t.Fatalf("env2: %v", err)
	}
	app2, err := f.q.CreateApplication(t.Context(), db.CreateApplicationParams{
		EnvironmentID: env2.ID,
		Name:          "worker",
		Image:         "redis",
		Tag:           "7",
		Domain:        "worker.example.test",
		Port:          6379,
		EnvText:       "",
		SourceType:    "image",
	})
	if err != nil {
		t.Fatalf("app2: %v", err)
	}
	if _, err := f.q.CreateDomain(t.Context(), db.CreateDomainParams{
		ApplicationID: f.appID, Host: "bot.example.test", Tls: false, IsPrimary: true, Exposed: false, Paths: "",
	}); err != nil {
		t.Fatalf("domain: %v", err)
	}

	apps, err := f.svc.ListApps(t.Context(), f.ident)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("want 2 apps, got %d: %+v", len(apps), apps)
	}

	byID := map[int64]struct {
		Path    string
		Domains []string
	}{}
	for _, a := range apps {
		byID[a.ID] = struct {
			Path    string
			Domains []string
		}{a.Path, a.Domains}
	}
	bot, ok := byID[f.appID]
	if !ok {
		t.Fatalf("bot app missing from list: %+v", apps)
	}
	if bot.Path != "acme-proj/production/bot" {
		t.Fatalf("bot path: got %q", bot.Path)
	}
	if len(bot.Domains) != 1 || bot.Domains[0] != "bot.example.test" {
		t.Fatalf("bot domains: got %+v", bot.Domains)
	}
	worker, ok := byID[app2.ID]
	if !ok {
		t.Fatalf("worker app missing from list: %+v", apps)
	}
	if worker.Path != "acme-proj-2/staging/worker" {
		t.Fatalf("worker path: got %q", worker.Path)
	}
	if len(worker.Domains) != 0 {
		t.Fatalf("worker should have no domains: got %+v", worker.Domains)
	}

	// A different org sees none of this — org isolation, not just app isolation.
	rivalApps, err := f.svc.ListApps(t.Context(), f.otherIdent)
	if err != nil {
		t.Fatalf("list apps (rival): %v", err)
	}
	if len(rivalApps) != 0 {
		t.Fatalf("rival org should see no apps, got %+v", rivalApps)
	}
}

// TestListEnvLabelsDBLinkSourceAndNeverLeaksItsValue exercises the merge
// semantics ListEnv implements but the given fixture never creates an
// app_db_links row for: a DB-link-sourced variable overrides a same-named
// literal's label, and a DB-link variable absent from env_text still appears.
// It also confirms the value-leak property holds on this path specifically —
// a linked variable's value is resolved live at deploy time (never stored in
// env_text) and must never reach the result either.
func TestListEnvLabelsDBLinkSourceAndNeverLeaksItsValue(t *testing.T) {
	f := newAPIFixture(t)

	// A redis instance to link against — a redis link points straight at the
	// instance (no logical_databases row needed, unlike postgres, which links
	// via a logical database). The password is a distinctive marker so the
	// leak check below actually proves something.
	const linkedPassword = "s3cr3t-redis-instance-pw"
	inst, err := f.q.CreateDBInstance(t.Context(), db.CreateDBInstanceParams{
		OrganizationID:    f.ident.OrgID,
		Engine:            "redis",
		Name:              "cache",
		AppName:           "krill-redis-fixture-1",
		Image:             "redis:7-alpine",
		Superuser:         "",
		SuperuserPassword: linkedPassword,
		NodeHostname:      "",
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}

	// PORT already exists as a literal in env_text (fixture: "PORT=8080") — a
	// link on the same var name must override its label, not duplicate the
	// key (deploy.GetApplication applies the link's value last, so env_text's
	// literal is shadowed). REDIS_URL has no env_text line at all: its value
	// is injected only at deploy time, so it must still surface as a key.
	if _, err := f.q.CreateDBLink(t.Context(), db.CreateDBLinkParams{
		ApplicationID: f.appID, InstanceID: &inst.ID, VarName: "PORT", Scheme: "redis", Field: "url",
	}); err != nil {
		t.Fatalf("create db link (PORT): %v", err)
	}
	if _, err := f.q.CreateDBLink(t.Context(), db.CreateDBLinkParams{
		ApplicationID: f.appID, InstanceID: &inst.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url",
	}); err != nil {
		t.Fatalf("create db link (REDIS_URL): %v", err)
	}

	keys, err := f.svc.ListEnv(t.Context(), f.ident, f.appIDString)
	if err != nil {
		t.Fatalf("list env: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("want 3 keys (PORT, SECRET_TOKEN, REDIS_URL), got %d: %+v", len(keys), keys)
	}
	bySource := map[string]string{}
	for _, k := range keys {
		bySource[k.Key] = k.Source
	}
	if bySource["PORT"] != "db-link" {
		t.Fatalf("PORT: want db-link (link overrides the same-named literal), got %q", bySource["PORT"])
	}
	if bySource["SECRET_TOKEN"] != "literal" {
		t.Fatalf("SECRET_TOKEN: want literal, got %q", bySource["SECRET_TOKEN"])
	}
	if bySource["REDIS_URL"] != "db-link" {
		t.Fatalf("REDIS_URL: want db-link (injected, absent from env_text), got %q", bySource["REDIS_URL"])
	}

	blob, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(blob, []byte(linkedPassword)) {
		t.Fatalf("db instance password leaked into the API response: %s", blob)
	}
	if bytes.Contains(blob, []byte("hunter2")) {
		t.Fatalf("literal env value leaked into the API response: %s", blob)
	}
}
