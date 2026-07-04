package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/volume"
	"github.com/proshik/krill/internal/web"
	"github.com/proshik/krill/internal/web/i18n"
)

// Server holds the HTTP layer dependencies.
type Server struct {
	cfg      config.Config
	auth     *auth.Service
	org      *org.Service
	q        *db.Queries
	deployer *deploy.Deployer
	engine   docker.Engine
	logHub   *deploy.DeployLogHub
	dbsvc    *dbservice.Service

	backupSvc     *backup.Service
	reloadBackups func()

	volumeSvc           *volume.VolumeService
	reloadVolumeBackups func()

	notify  *notify.Service
	metrics metrics.Store

	// selfComponentFn returns the control-plane component key (supplied by the
	// metrics sampler, which learns it while sampling — no per-request docker scan).
	selfComponentFn func() string

	// monCache memoizes the /monitoring/data response per (org, range) for one
	// sampler interval: the underlying samples only change that often, so 10s
	// polls don't re-scan thousands of rows from Postgres each time.
	monMu    sync.Mutex
	monCache map[string]monCacheEntry

	// logSem caps concurrent live log-follow WebSockets (each opens a dockerd
	// follow stream + goroutines). A buffered channel used as a counting
	// semaphore so one member can't exhaust the daemon by opening thousands.
	logSem chan struct{}
}

// maxLiveLogStreams bounds concurrent docker log-follow WebSockets host-wide.
const maxLiveLogStreams = 24

type monCacheEntry struct {
	data []byte
	at   time.Time
}

func New(cfg config.Config, authSvc *auth.Service, orgSvc *org.Service, q *db.Queries, d *deploy.Deployer, e docker.Engine, hub *deploy.DeployLogHub, dbSvc *dbservice.Service) *Server {
	return &Server{
		cfg: cfg, auth: authSvc, org: orgSvc, q: q, deployer: d, engine: e, logHub: hub, dbsvc: dbSvc,
		logSem: make(chan struct{}, maxLiveLogStreams),
	}
}

// acquireLogSlot reserves a live-log-stream slot without blocking. The returned
// release func must be called when the stream ends; ok is false when the cap is
// reached (the caller should reject the connection).
func (s *Server) acquireLogSlot() (release func(), ok bool) {
	select {
	case s.logSem <- struct{}{}:
		return func() { <-s.logSem }, true
	default:
		return func() {}, false
	}
}

// SetBackups wires the backup service and a reload hook (re-reads the cron
// schedule) into the server without changing New's signature.
func (s *Server) SetBackups(svc *backup.Service, reload func()) {
	s.backupSvc = svc
	s.reloadBackups = reload
}

// SetVolumeBackups wires the volume-backup service + a scheduler reload closure.
func (s *Server) SetVolumeBackups(svc *volume.VolumeService, reload func()) {
	s.volumeSvc = svc
	s.reloadVolumeBackups = reload
}

// SetNotify wires the notification service (used by the test-message handler).
func (s *Server) SetNotify(n *notify.Service) { s.notify = n }

// SetMetrics wires the metrics store (used by the monitoring handler).
func (s *Server) SetMetrics(st metrics.Store) { s.metrics = st }

// SetSelfComponentFn wires the control-plane component provider (the metrics
// sampler), so the monitoring handler need not scan docker itself.
func (s *Server) SetSelfComponentFn(fn func() string) { s.selfComponentFn = fn }

// Router assembles the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(csrfGuard)

	// locale middleware: the krill_lang cookie (set via the Settings language
	// selector) is authoritative; with no valid cookie, fall back to the browser's
	// Accept-Language header, then to the default.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			loc := i18n.DefaultLocale
			if c, err := req.Cookie("krill_lang"); err == nil && i18n.Supported(c.Value) {
				loc = c.Value
			} else if m := i18n.MatchAcceptLanguage(req.Header.Get("Accept-Language")); m != "" {
				loc = m
			}
			ctx := i18n.WithLocale(req.Context(), loc)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	r.Get("/login", s.loginPage)
	// Rate-limit login attempts per source IP to bound online password guessing.
	loginLimiter := newLoginRateLimiter(10, time.Minute)
	r.With(loginLimiter.middleware).Post("/login", s.loginSubmit)
	// POST so csrfGuard + SameSite cover it: a GET /logout is vulnerable to a
	// cross-site top-level navigation terminating the victim's session.
	r.Post("/logout", s.logout)

	// Public, unauthenticated auto-deploy webhooks (verified by a per-app secret).
	r.Route("/webhooks", func(r chi.Router) {
		r.Post("/github/{appID}", s.githubWebhook)
		r.Post("/deploy/{appID}", s.deployHook)
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(s.auth))
		r.Use(auth.WithInstanceAdmin(s.auth))
		r.Use(s.flashMiddleware)

		r.Get("/", s.home)
		r.Get("/orgs", s.listOrgs)
		r.Get("/orgs/switcher", s.orgSwitcher)
		r.Post("/orgs", s.createOrg)
		r.Post("/lang", s.setLanguage)

		r.Route("/orgs/{orgID}", func(r chi.Router) {
			r.Use(auth.RequireOrgMember(s.org))

			r.Get("/", s.orgDashboard)
			r.Get("/members", s.listMembers)
			r.Get("/destinations", s.listDestinations)
			r.Get("/registries", s.listRegistries)
			r.Get("/git-credentials", s.listGitCredentials)
			r.Get("/db-servers", s.listDBInstances)
			r.Get("/db-servers/{instID}", s.dbInstanceDetail)
			r.Get("/db-servers/{instID}/status", s.dbInstanceStatus)
			r.Get("/db-servers/{instID}/logs", s.dbInstanceLogs)
			r.Get("/db-servers/{instID}/deploy-logs", s.dbInstanceDeployLogs)

			// Global infrastructure: cluster nodes and the host-wide monitoring
			// view span every tenant, so they are gated on the instance-operator
			// flag, NOT org-scoped RoleAdmin (which any user can self-grant by
			// creating an org via POST /orgs).
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireInstanceAdmin())
				r.Get("/nodes", s.listNodes)
				r.Post("/nodes", s.addNode)
				r.Post("/nodes/{nodeID}/availability", s.setNodeAvailability)
				r.Post("/nodes/{nodeID}/remove", s.removeNode)
				r.Post("/nodes/{nodeID}/label", s.setNodeLabel)
				r.Get("/monitoring", s.monitoring)
				r.Get("/monitoring/data", s.monitoringData)
			})

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole(auth.RoleAdmin))
				r.Get("/topology", s.topology)
				r.Get("/topology/data", s.topologyData)
				r.Post("/members", s.createMember)
				r.Post("/members/{mID}/role", s.updateMemberRole)
				r.Post("/members/{mID}/remove", s.removeMember)
				r.Post("/destinations", s.createDestination)
				r.Post("/destinations/{destID}/delete", s.deleteDestination)
				r.Post("/registries", s.createRegistry)
				r.Post("/registries/{regID}/delete", s.deleteRegistry)
				r.Post("/git-credentials", s.createGitCredential)
				r.Post("/git-credentials/{gcID}/delete", s.deleteGitCredential)
				r.Post("/db-servers", s.createDBInstance)
				r.Post("/db-servers/{instID}/deploy", s.deployDBInstance)
				r.Post("/db-servers/{instID}/start", s.startDBInstance)
				r.Post("/db-servers/{instID}/stop", s.stopDBInstance)
				r.Post("/db-servers/{instID}/version", s.versionDBInstance)
				r.Post("/db-servers/{instID}/node", s.setDBInstanceNode)
				r.Post("/db-servers/{instID}/delete", s.deleteDBInstance)
				r.Get("/notifications", s.listNotifications)
				r.Post("/notifications", s.saveNotifications)
				r.Post("/notifications/test", s.testNotification)
				r.Post("/projects", s.createProject)
				r.Post("/projects/{projID}/delete", s.deleteProject)
				r.Post("/projects/{projID}/rename", s.renameProject)
				r.Post("/projects/{projID}/environments", s.createEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/delete", s.deleteEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/apps", s.createApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/delete", s.deleteApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/registry", s.setAppRegistry)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/git-credential", s.setAppGitCredential)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/build", s.saveBuild)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/placement", s.savePlacement)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains", s.addDomain)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/tls", s.toggleDomainTLS)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/exposure", s.setDomainExposure)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/delete", s.deleteDomain)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/basic-auth", s.addDomainBasicAuthUser)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/basic-auth/delete", s.deleteDomainBasicAuthUser)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/allowed-ips", s.setDomainAllowedIPs)
				r.Post("/projects/{projID}/environments/{envID}/databases", s.createLogicalDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/delete", s.deleteLogicalDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/backups", s.addBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/delete", s.deleteBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/toggle", s.toggleBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/run", s.runBackupNow)
				r.Post("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/restore", s.restoreBackup)
				r.Get("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/objects", s.backupObjects)
				r.Get("/projects/{projID}/environments/{envID}/databases/{dbID}/backups/{backupID}/download", s.downloadBackup)
			})

			r.Get("/projects/{projID}", s.projectPage)
			r.Get("/projects/{projID}/environments/{envID}", s.projectPage)
			r.Get("/projects/{projID}/environments/{envID}/statuses", s.envStatuses)

			r.Route("/projects/{projID}/environments/{envID}/apps/{appID}", func(r chi.Router) {
				r.Get("/", s.appDetail)
				r.Get("/status", s.appStatus)
				r.Get("/tags", s.appTags)
				r.Get("/logs", s.appLogs)
				r.Get("/deployments-list", s.listDeployments)
				r.Get("/deployments/{deployID}", s.deploymentLogPage)
				r.Get("/deployments/{deployID}/logs", s.deploymentLogWS)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequireRole(auth.RoleAdmin))
					r.Post("/deploy", s.deployApp)
					r.Post("/rebuild", s.rebuildApp)
					r.Post("/reload", s.reloadApp)
					r.Post("/stop", s.stopApp)
					r.Post("/env", s.saveEnv)
					r.Post("/advanced", s.saveAdvanced)
					r.Post("/volumes", s.addVolume)
					r.Post("/volumes/{volID}/delete", s.deleteVolume)
					r.Post("/volumes/{volID}/owner", s.setVolumeOwner)
					r.Post("/db-links", s.addDBLink)
					r.Post("/db-links/{linkID}/delete", s.deleteDBLink)
					r.Post("/volumes/{volID}/backups", s.addVolumeBackup)
					r.Post("/volumes/backups/{vbID}/toggle", s.toggleVolumeBackup)
					r.Post("/volumes/backups/{vbID}/delete", s.deleteVolumeBackup)
					r.Post("/volumes/backups/{vbID}/run", s.runVolumeBackupNow)
					r.Post("/volumes/backups/{vbID}/restore", s.restoreVolumeBackup)
					r.Get("/volumes/backups/{vbID}/objects", s.volumeBackupObjects)
					r.Get("/volumes/backups/{vbID}/download", s.downloadVolumeBackup)
					r.Post("/ports", s.addAppPort)
					r.Post("/ports/{portID}/delete", s.deleteAppPort)
					r.Post("/autodeploy/enable", s.enableAutoDeploy)
					r.Post("/autodeploy/disable", s.disableAutoDeploy)
					r.Post("/autodeploy/regenerate", s.regenerateWebhookSecret)
					r.Get("/terminal/ws", s.appTerminal)
				})
			})

			r.Get("/projects/{projID}/environments/{envID}/databases/{dbID}", s.logicalDatabaseDetail)
		})
	})

	return r
}
