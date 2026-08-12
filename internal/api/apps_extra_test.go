package api_test

import (
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
