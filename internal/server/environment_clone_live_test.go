//go:build integration

package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envclone"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
	"github.com/proshik/krill/internal/traefik"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The adapter only adds the fixture's provider label and refuses non-owned
// service names. Builds, Docker APIs, scheduling, predeploy and exec stay real.
type cloneLiveEngine struct {
	docker.Engine
	network, token string
	mu             sync.Mutex
	owned          map[string]bool
}

func (e *cloneLiveEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	e.mu.Lock()
	owned := e.owned[s.Name]
	e.mu.Unlock()
	if !owned || s.Network != e.network {
		return errors.New("fixture refused non-owned service/network")
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels["krill.clone-fixture"] = e.token
	return e.Engine.ServiceDeploy(ctx, s)
}
func (e *cloneLiveEngine) LocalImageID(ctx context.Context, ref string) (string, error) {
	return e.Engine.(docker.PredeployRunner).LocalImageID(ctx, ref)
}
func (e *cloneLiveEngine) RunPredeploy(ctx context.Context, s docker.PredeploySpec, out io.Writer) error {
	return e.Engine.(docker.PredeployRunner).RunPredeploy(ctx, s, out)
}
func (e *cloneLiveEngine) CleanupPredeploy(context.Context) error {
	return errors.New("fixture forbids global predeploy cleanup")
}

const cloneHTTPApp = `package main
import("context";"encoding/json";"net/http";"os";"fmt";"github.com/jackc/pgx/v5/pgxpool")
func main(){
 ctx:=context.Background();pool,err:=pgxpool.New(ctx,os.Getenv("DATABASE_URL"));if err!=nil{panic("database config")};defer pool.Close()
 if len(os.Args)>1&&os.Args[1]=="--migrate"{
 if _,err=pool.Exec(ctx,"CREATE TABLE messages (value text NOT NULL)");err!=nil{panic("database migration")}
 return
 }
 var ready int
 if err=pool.QueryRow(ctx,"SELECT count(*) FROM messages").Scan(&ready);err!=nil{panic("predeploy migration missing")}
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){json.NewEncoder(w).Encode(map[string]any{"public":os.Getenv("PUBLIC"),"secret_set":os.Getenv("TOKEN")!="","metrics_set":os.Getenv("KRILL_METRICS_TOKEN")!=""})})
 http.HandleFunc("/count",func(w http.ResponseWriter,r *http.Request){var n int;if pool.QueryRow(ctx,"SELECT count(*) FROM messages").Scan(&n)!=nil{http.Error(w,"database read failed",500);return};fmt.Fprint(w,n)})
 http.HandleFunc("/write",func(w http.ResponseWriter,r *http.Request){if r.Method!="POST"{w.WriteHeader(405);return};if _,err:=pool.Exec(ctx,"INSERT INTO messages VALUES ($1)",r.FormValue("value"));err!=nil{http.Error(w,"database write failed",500);return};w.WriteHeader(204)})
 if err:=http.ListenAndServe(":8080",nil);err!=nil{panic("http listener")}
}`

// Opt-in browser acceptance. No main startup, shared gateway, host ports 80/443,
// source user services or global reconcilers are invoked. Drive the printed URL
// with scripts/environment-clone.browser.js, then POST /_test/finish.
func TestEnvironmentCloneLive(t *testing.T) {
	if os.Getenv("KRILL_CLONE_LIVE") != "1" {
		t.Skip("opt-in real HTTP clone acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random[:])
	network := "krill-clone-live-" + token
	engine, err := docker.NewEngine(os.Getenv("DOCKER_HOST"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.NetworkEnsure(ctx, network)
	if err != nil || !created {
		t.Fatal("could not allocate unique owned network")
	}
	e := &cloneLiveEngine{Engine: engine, network: network, token: token, owned: map[string]bool{}}
	var cleanupVolumes, cleanupImages []string
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		e.mu.Lock()
		names := []string{}
		for name := range e.owned {
			names = append(names, name)
		}
		e.mu.Unlock()
		for _, name := range names {
			if err := engine.ServiceRemove(c, name); err != nil {
				t.Logf("owned service cleanup: %s: %v", name, err)
			}
		}
		for _, vol := range cleanupVolumes {
			for attempt := 0; attempt < 30; attempt++ {
				if err := engine.VolumeRemove(c, vol); err == nil {
					break
				}
				time.Sleep(300 * time.Millisecond)
			}
		}
		for _, tag := range cleanupImages {
			_ = exec.CommandContext(c, "docker", "image", "rm", tag).Run()
		}
		for attempt := 0; attempt < 30; attempt++ {
			if err := engine.NetworkRemove(c, network); err == nil {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Error("owned overlay network cleanup did not finish")
	})
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	osvc := org.NewService(q)
	owner := mkUser(t, q, "clone-live@fixture.test")
	o, err := osvc.CreateOrg(ctx, owner, "Clone live")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetOrganizationNetwork(ctx, db.SetOrganizationNetworkParams{ID: o.ID, NetworkName: network}); err != nil {
		t.Fatal(err)
	}
	p, err := osvc.CreateProject(ctx, o.ID, "HTTP fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	source, err := osvc.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	baseID := 1000000000 + (int64(random[0]) << 16) + (int64(random[1]) << 8) + int64(random[2])
	for _, id := range []int64{baseID, baseID + 1} {
		st, err := engine.ServiceState(ctx, docker.ServiceName(id))
		if err != nil || st.Found {
			t.Fatal("fixture app ID collides with existing service")
		}
	}
	if _, err := pool.Exec(ctx, "SELECT setval('applications_id_seq',$1,false)", baseID); err != nil {
		t.Fatal(err)
	}
	hub := deploy.NewLogHub()
	dbsvc := dbservice.New(e, dbservice.NewDBStore(q), hub, network)
	pgName := network + "-pg"
	e.owned[pgName] = true
	cleanupVolumes = append(cleanupVolumes, pgName+"-data")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "fixture-pg", AppName: pgName, Image: "postgres:16-alpine", Superuser: "fixture", SuperuserPassword: "fixture_server_password"})
	if err != nil {
		t.Fatal(err)
	}
	di := drivers.Instance{ID: inst.ID, OrganizationID: o.ID, Engine: "postgres", AppName: pgName, Image: inst.Image, Superuser: inst.Superuser, SuperuserPassword: inst.SuperuserPassword}
	spec := drivers.Registry.MustGet("postgres").BuildSpec(di, network)
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := engine.Exec(ctx, pgName, []string{"pg_isready", "-h", "127.0.0.1", "-U", "fixture"}, nil, nil, io.Discard); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned PG did not start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	ld, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: source.ID, Name: "data", DbName: "source_data", Username: "source_owner", Password: "fixture_source_password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := dbsvc.ProvisionLogicalDB(ctx, di, dbservice.LogicalDB{DBName: ld.DbName, Username: ld.Username, Password: ld.Password}); err != nil {
		t.Fatal("source DB provisioning failed")
	}
	query := func(database, statement string) (string, error) {
		var out bytes.Buffer
		err := engine.Exec(ctx, pgName, []string{"psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", "fixture", "-d", database}, nil, strings.NewReader(statement), &out)
		return strings.TrimSpace(out.String()), err
	}
	if _, err := query(ld.DbName, "CREATE TABLE messages(value text NOT NULL); INSERT INTO messages VALUES ('production-sentinel');"); err != nil {
		t.Fatal("source sentinel setup failed")
	}

	// Smart HTTP serves a real bare repo. Dependencies are vendored from the
	// existing Go module cache, so Docker build needs no extra package manager.
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", cloneHTTPApp)
	write("go.mod", "module clonefixture\n\ngo 1.26.1\n\nrequire github.com/jackc/pgx/v5 v5.9.2\n")
	write("Dockerfile", "FROM golang:1.26.4 AS build\nWORKDIR /src\nCOPY . .\nRUN CGO_ENABLED=0 go build -mod=vendor -o /app .\nFROM postgres:16-alpine\nCOPY --from=build /app /app\nENTRYPOINT [\"/app\"]\n")
	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture %s: %v: %s", name, err, output)
		}
	}
	run(work, "go", "mod", "tidy")
	run(work, "go", "mod", "vendor")
	run(work, "git", "init", "-b", "main")
	run(work, "git", "add", ".")
	run(work, "git", "-c", "user.name=Fixture", "-c", "user.email=fixture@local.test", "commit", "-m", "fixture: HTTP Postgres app")
	run(root, "git", "clone", "--bare", work, filepath.Join(root, "repo.git"))
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Root: "/", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var gitRequests atomic.Int64
	gitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "git-upload-pack") {
			gitRequests.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	defer gitServer.Close()
	app, err := hostshare.NewStore(pool).CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: source.ID, Name: "web", SourceType: "dockerfile", GitUrl: gitServer.URL + "/repo.git", GitBranch: "main", DockerfilePath: "Dockerfile", Port: 8080, Domain: "source-" + token + ".test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: app.ID, EnvText: "# fixture\nPUBLIC=source-public\nTOKEN=production-secret\nDATABASE_URL=production-fallback", EnvPublicKeys: []string{"PUBLIC", "DATABASE_URL"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, LogicalDatabaseID: &ld.ID, VarName: "DATABASE_URL", Scheme: "postgresql", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationAdvanced(ctx, db.UpdateApplicationAdvancedParams{ID: app.ID, Replicas: 1, RestartCondition: "any", PredeployCommand: "/app --migrate"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "files", MountPath: "/files"}); err != nil {
		t.Fatal(err)
	}
	oldMetrics := "production-metrics"
	if err := q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: app.ID, MetricsToken: &oldMetrics}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: app.ID, MetricsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: app.ID, Port: 8080, Path: "/metrics", Job: "fixture"}); err != nil {
		t.Fatal(err)
	}

	gateway, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{
		Image: "traefik:" + traefik.TraefikVersion, Networks: []string{network}, ExposedPorts: []string{"18080/tcp"},
		Mounts:     testcontainers.Mounts(testcontainers.BindMount("/var/run/docker.sock", "/var/run/docker.sock")),
		Cmd:        []string{"--entrypoints.web.address=:18080", "--providers.swarm.endpoint=unix:///var/run/docker.sock", "--providers.swarm.exposedbydefault=false", "--providers.swarm.network=" + network, "--providers.swarm.constraints=Label(`krill.clone-fixture`,`" + token + "`)", "--providers.swarm.refreshseconds=1"},
		WaitingFor: wait.ForListeningPort("18080/tcp")}, Started: true})
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, gateway)
	gatewayURL, err := gateway.PortEndpoint(ctx, "18080/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	dep := deploy.New(e, builder.New(os.Getenv("DOCKER_HOST"), true), deploy.NewDBStore(q, pool), hub, network)
	dep.Start(ctx)
	defer dep.Stop()
	cloner := envclone.New(pool, dbsvc, e, "clone.test")
	defer cloner.Shutdown(context.Background())
	cfg := config.Config{BaseDomain: "clone.test", Network: network, AllowPrivateEgress: true}
	newRouter := func() http.Handler {
		srv := server.New(cfg, auth.NewService(q), osvc, q, dep, e, hub, dbsvc)
		srv.SetHostShare(hostshare.NewStore(pool))
		srv.SetEnvironmentCloner(cloner)
		return srv.Router()
	}
	var routerMu sync.RWMutex
	router := newRouter()
	sourceURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(source.ID)
	done := make(chan struct{}, 1)
	var cloned db.Application
	var clonedDB db.ListLogicalDatabasesByEnvironmentRow
	api := http.NewServeMux()
	respond := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	var heldCloneGuard bool
	var guardMu sync.Mutex
	defer func() {
		guardMu.Lock()
		defer guardMu.Unlock()
		if heldCloneGuard {
			oplock.Release(oplock.CloneProject(p.ID))
		}
	}()
	api.HandleFunc("POST /_test/clone-lock", func(w http.ResponseWriter, r *http.Request) {
		guardMu.Lock()
		defer guardMu.Unlock()
		key := oplock.CloneProject(p.ID)
		if r.URL.Query().Get("held") == "1" {
			if !heldCloneGuard {
				heldCloneGuard = oplock.TryAcquire(key)
			}
		} else if heldCloneGuard {
			oplock.Release(key)
			heldCloneGuard = false
		}
		respond(w, map[string]bool{"held": heldCloneGuard}, nil)
	})
	api.HandleFunc("POST /_test/check-created", func(w http.ResponseWriter, r *http.Request) {
		envs, err := q.ListEnvironments(ctx, p.ID)
		if err != nil || len(envs) != 2 {
			respond(w, nil, errors.New("expected one cloned environment"))
			return
		}
		apps, err := q.ListApplicationsByEnvironment(ctx, envs[1].ID)
		if err != nil || len(apps) != 1 {
			respond(w, nil, errors.New("cloned app missing"))
			return
		}
		cloned = apps[0]
		if cloned.PredeployCommand != "/app --migrate" || cloned.AutoDeploy || cloned.Status != "idle" || cloned.EnvText != "# fixture\nPUBLIC=source-public\nTOKEN=\nDATABASE_URL=" || cloned.MetricsToken == nil || *cloned.MetricsToken == oldMetrics {
			respond(w, nil, errors.New("clone configuration unsafe"))
			return
		}
		st, err := engine.ServiceState(ctx, docker.ServiceName(cloned.ID))
		if err != nil || st.Found {
			respond(w, nil, errors.New("clone service exists before explicit Deploy"))
			return
		}
		dbs, err := q.ListLogicalDatabasesByEnvironment(ctx, envs[1].ID)
		if err != nil || len(dbs) != 1 {
			respond(w, nil, errors.New("clone database missing"))
			return
		}
		clonedDB = dbs[0]
		empty, err := query(clonedDB.DbName, "SELECT count(*) FROM pg_tables WHERE schemaname='public';")
		if err != nil || empty != "0" {
			respond(w, nil, errors.New("clone database is not initially empty"))
			return
		}
		allowed, err := query("postgres", fmt.Sprintf("SELECT has_database_privilege('%s','source_data','CONNECT');", clonedDB.Username))
		if err != nil || allowed != "f" {
			respond(w, nil, errors.New("clone role can access production"))
			return
		}
		if clonedDB.Password == ld.Password || clonedDB.Username == ld.Username {
			respond(w, nil, errors.New("database credentials reused"))
			return
		}
		domains, err := q.ListDomainsByApplication(ctx, cloned.ID)
		if err != nil || len(domains) != 1 || domains[0].Exposed {
			respond(w, nil, errors.New("clone domain not internal"))
			return
		}
		e.mu.Lock()
		e.owned[docker.ServiceName(cloned.ID)] = true
		e.mu.Unlock()
		cleanupVolumes = append(cleanupVolumes, docker.VolumeName(cloned.ID, "files"))
		// This disposable state has no earlier deployments. Track the image before
		// building so a failed acceptance also removes its owned tag.
		cleanupImages = append(cleanupImages, docker.BuildImageTag(cloned.ID, 1))
		appURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(envs[1].ID) + "/apps/" + i64(cloned.ID)
		respond(w, map[string]any{"app_url": appURL, "host": cloned.Domain, "domain_id": domains[0].ID, "gateway": gatewayURL}, nil)
	})
	api.HandleFunc("GET /_test/deployment", func(w http.ResponseWriter, r *http.Request) {
		rows, err := q.ListDeploymentsByApplication(ctx, cloned.ID)
		if err != nil || len(rows) == 0 {
			respond(w, map[string]string{"status": "waiting"}, err)
			return
		}
		d := rows[0]
		if d.Status == "error" {
			t.Logf("owned deployment failure: %s\n%s", d.ErrorMessage, d.Log)
		}
		respond(w, map[string]any{"status": d.Status, "error": d.ErrorMessage}, nil)
	})
	api.HandleFunc("POST /_test/reopen", func(w http.ResponseWriter, r *http.Request) {
		routerMu.Lock()
		router = newRouter()
		routerMu.Unlock()
		respond(w, map[string]bool{"reopened": true}, nil)
	})
	api.HandleFunc("POST /_test/finish", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("KRILL_CLONE_VISUAL") == "1" {
			if cloned.ID == 0 {
				respond(w, nil, errors.New("clone checks not completed"))
				return
			}
			t.Log("PASS visual-only fixture; no build/deploy acceptance claimed")
			respond(w, map[string]bool{"passed": true}, nil)
			select {
			case done <- struct{}{}:
			default:
			}
			return
		}
		count, err := query(clonedDB.DbName, "SELECT count(*) FROM messages WHERE value='staging-write';")
		if err != nil || count != "1" {
			respond(w, nil, errors.New("clone write did not persist"))
			return
		}
		value, err := query(ld.DbName, "SELECT string_agg(value,',') FROM messages;")
		if err != nil || value != "production-sentinel" {
			respond(w, nil, errors.New("source sentinel changed"))
			return
		}
		if gitRequests.Load() == 0 {
			respond(w, nil, errors.New("no real Git smart HTTP upload-pack request"))
			return
		}
		rows, err := q.ListDeploymentsByApplication(ctx, cloned.ID)
		if err != nil || len(rows) != 1 || rows[0].Status != "done" {
			respond(w, nil, errors.New("real deploy did not finish"))
			return
		}
		if !strings.Contains(rows[0].Log, "git clone") || !strings.Contains(rows[0].Log, "docker build") {
			respond(w, nil, errors.New("real build log missing"))
			return
		}
		t.Logf("PASS real Git smart HTTP (%d upload-pack requests), Docker build, predeploy, HTTP/PG writes, empty clone, source isolation, router reopen", gitRequests.Load())
		respond(w, map[string]bool{"passed": true}, nil)
		select {
		case done <- struct{}{}:
		default:
		}
	})
	api.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routerMu.RLock()
		h := router
		routerMu.RUnlock()
		h.ServeHTTP(w, r)
	}))
	panel := httptest.NewServer(api)
	defer panel.Close()
	t.Logf("CLONE_LIVE_URL=%s%s LOGIN=clone-live@fixture.test PASSWORD=pw", panel.URL, sourceURL)
	t.Logf("OWNED_NETWORK=%s OWNED_PG=%s GATEWAY=%s SOURCE_APP_ID=%s", network, pgName, gatewayURL, strconv.FormatInt(app.ID, 10))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("live browser acceptance timed out")
	}
}
