//go:build acceptance

package acceptance

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/test/acceptance/accept"
)

// The state-backup acceptance (docs/superpowers specs, 2026-11-21): the
// state-restore runbooks of docs/install.md run with their own commands on a
// disposable Lima VM. Scenario A: back up, wipe everything Krill installed,
// reinstall a newer release, restore, recover the workloads. Scenario B: a UI
// update whose migration fails, undone from its pre-update snapshot.

const (
	operatorEmail = "operator@accept.test"
	forwardTable  = "acceptance_sbk_forward"
	appDomain     = "web.accept.test"
	sbkTelegram   = "123456:ACCEPT-not-a-real-token"
	pgMajor       = "17"
)

// sbk is the state the ordered steps share.
type sbk struct {
	*suite
	fx                       Fixtures
	recipient, keyLocal      string
	org                      int64
	proj, env, app, domainID int64
	regID, inst, ldb         int64
	dest, backup             int64
	instApp                  string
	ldbUser, ldbPass         string
	appBackupKey             string
	stateKey                 string // the A1 object
	objLocal                 string // its copy on the Mac
	old                      struct {
		email, password, secretKey, cookie, nodeID string
		extraEnv                                   []string // KEY=VALUE lines install.sh does not write
	}
	fp map[string]accept.Fingerprint
	// maxDeployment is the highest deployments.id right after the A8 restore.
	maxDeployment int64
	ran           []ranCmd
}

type ranCmd struct{ id, how string }

func (b *sbk) orgPath() string { return fmt.Sprintf("/orgs/%d", b.org) }
func (b *sbk) envPath() string {
	return fmt.Sprintf("%s/projects/%d/environments/%d", b.orgPath(), b.proj, b.env)
}
func (b *sbk) appPath() string { return fmt.Sprintf("%s/apps/%d", b.envPath(), b.app) }
func (b *sbk) dbPath() string  { return fmt.Sprintf("%s/databases/%d", b.envPath(), b.ldb) }
func (b *sbk) sbkPath() string { return b.orgPath() + "/krill-backups" }

// --- runbook commands ---------------------------------------------------------

// cmd returns a runbook command's text with the placeholders filled in.
func (b *sbk) cmd(id string, subst map[string]string) string {
	return accept.Fill(accept.Lookup(id).Text, subst)
}

// runbook runs a docs/install.md command (after prelude, in one bash script),
// records it and fails the step when it exits non-zero.
func (b *sbk) runbook(t *testing.T, ev *Evidence, id string, subst map[string]string, prelude, how string) string {
	t.Helper()
	script := b.cmd(id, subst)
	if prelude != "" {
		script = prelude + "\n" + script
	}
	out := b.sudo(t, ev, script)
	b.note(id, how)
	return out
}

// note records how a runbook command was run, for the summary.
func (b *sbk) note(id, how string) {
	if how == "" {
		how = accept.Lookup(id).Mode.String()
	}
	b.ran = append(b.ran, ranCmd{id, how})
}

var restoreObj = map[string]string{"<object>": "object.sql.gz.age", "<major>": pgMajor}

const inRestoreDir = "cd " + restoreDir

// --- fingerprints -------------------------------------------------------------

func (b *sbk) fingerprint(t *testing.T, ev *Evidence, name, container, user, db string) accept.Fingerprint {
	t.Helper()
	return b.fingerprintWith(t, ev, name, container, user, db, accept.Options{})
}

// ruleSOptions narrow rule S to what spec §8 allows after the start: the
// status columns (with the updated_at a status write sets), the backup's run result, the network migrator's mark, and
// in deployments (and their releases) only the rows that existed at the
// restore; new deployments are checked on their own.
func ruleSOptions(maxDeployment int64) accept.Options {
	return accept.Options{
		Ignore: map[string][]string{
			// A status write sets updated_at too (UpdateApplicationStatus,
			// UpdateDBInstanceStatus).
			"applications":  {"status", "updated_at"},
			"db_instances":  {"status", "updated_at"},
			"backups":       {"last_error", "last_run_at", "last_status"},
			"organizations": {"network_migrated_at"},
		},
		Where: map[string]string{
			"deployments":         fmt.Sprintf("id <= %d", maxDeployment),
			"deployment_releases": fmt.Sprintf("deployment_id <= %d", maxDeployment),
		},
	}
}

func (b *sbk) fingerprintWith(t *testing.T, ev *Evidence, name, container, user, db string, opts accept.Options) accept.Fingerprint {
	t.Helper()
	tables, err := b.host.PsqlIn(b.ctx, container, user, db, accept.TablesSQL())
	must(t, err, "listing the tables for "+name)
	out, err := b.host.PsqlIn(b.ctx, container, user, db, accept.FingerprintSQL(strings.Fields(tables), []string{forwardTable}, opts))
	must(t, err, "fingerprint "+name)
	fp, err := accept.ParseFingerprint(out)
	must(t, err, "parsing fingerprint "+name)
	names := make([]string, 0, len(fp.Tables))
	for n := range fp.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []string
	for _, n := range names {
		rows = append(rows, fmt.Sprintf("%s %d %s", n, fp.Tables[n].Rows, fp.Tables[n].Hash))
	}
	ev.Output("fingerprint "+name+" ("+container+"/"+db+")", fmt.Sprintf("%d tables, %d sequences, schema %s\n%s",
		len(fp.Tables), len(fp.Sequences), fp.Schema, strings.Join(rows, "\n")))
	b.fp[name] = fp
	return fp
}

// compare fails the step on any difference between a and c that allow does
// not name ("table:x", "sequence:x", "schema"), dumping the rows of every
// unexpected table from dump (container, user, db) without secret columns.
// It returns the allowed differences.
func (b *sbk) compare(t *testing.T, ev *Evidence, rule string, a, c accept.Fingerprint, allow map[string]bool, dump [3]string) []accept.Difference {
	t.Helper()
	var ok, bad []accept.Difference
	for _, d := range accept.Diff(a, c) {
		key := d.Object + ":" + d.Name
		if d.Object == "schema" {
			key = "schema"
		}
		if allow[key] {
			ok = append(ok, d)
		} else {
			bad = append(bad, d)
		}
	}
	for _, d := range ok {
		ev.Note("rule %s: allowed difference %s", rule, d)
	}
	if len(bad) == 0 {
		ev.Note("rule %s: compared %d tables, %d sequences, schema; nothing else differs", rule, len(c.Tables), len(c.Sequences))
		return ok
	}
	for _, d := range bad {
		ev.Note("rule %s: UNEXPECTED difference %s", rule, d)
		if d.Object == "table" && dump[0] != "" {
			cols, err := b.host.PsqlIn(b.ctx, dump[0], dump[1], dump[2], accept.ColumnsSQL(d.Name))
			if err == nil {
				rows, _ := b.host.PsqlIn(b.ctx, dump[0], dump[1], dump[2], accept.RowsSQL(d.Name, strings.Fields(cols)))
				ev.Output("rows of "+d.Name+" (secret columns removed)", rows)
			}
		}
	}
	t.Fatalf("rule %s: %d unexpected differences (see the evidence)", rule, len(bad))
	return nil
}

// samplerTables are written by the metrics sampler on every tick (an insert
// on the first one); between two moments of a running Krill they may differ
// in more than the ignored columns, so rules W and S allow them. Rules O and
// R compare one object with its own restore and never allow them.
var samplerTables = []string{"node_capacity", "node_disks"}

func allowTables(names ...string) map[string]bool {
	names = append(names, samplerTables...)
	m := map[string]bool{}
	for _, n := range names {
		m["table:"+n] = true
		m["sequence:"+n+"_id_seq"] = true
	}
	return m
}

var krillDB = [3]string{pgContainer, "krill", "krill"}

// --- small helpers ---------------------------------------------------------------

func (b *sbk) q(t *testing.T, sql string) string {
	t.Helper()
	out, err := b.host.Psql(b.ctx, sql)
	must(t, err, sql)
	return out
}

func (b *sbk) id(t *testing.T, sql string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(b.q(t, sql), 10, 64)
	must(t, err, "parsing an id from "+sql)
	return n
}

// actionRe is the action part of a form's path: the page a browser posts it
// from is the path without it, and handlers that redirect back go there.
var actionRe = regexp.MustCompile(`/(backups/\d+/(run|restore|toggle)|backups|domains/\d+/exposure|deploy|enabled|run|registry|env)$`)

// post submits a form from the page it lives on (see actionRe) and fails
// unless the request ends with 200.
func (b *sbk) post(t *testing.T, ev *Evidence, path string, form url.Values) Response {
	t.Helper()
	referer := actionRe.ReplaceAllString(path, "")
	resp, err := b.client.Post(b.ctx, path, referer, form)
	must(t, err, "POST "+path)
	ev.Note("POST %s (from %s) -> %d %s", path, referer, resp.Status, resp.URL)
	if resp.Status != http.StatusOK {
		t.Fatalf("POST %s ended with status %d", path, resp.Status)
	}
	return resp
}

// postOK posts a form and fails unless the resulting page carries want.
func (b *sbk) postOK(t *testing.T, ev *Evidence, path string, form url.Values, want string) Response {
	t.Helper()
	resp := b.post(t, ev, path, form)
	if want != "" && !strings.Contains(resp.Text(), want) {
		ev.Output("page after POST "+path, bounded(resp.Text(), 3000))
		t.Fatalf("POST %s: page does not say %q", path, want)
	}
	return resp
}

// waitFor polls cond every interval until it returns true or timeout passes.
func waitFor(t *testing.T, ev *Evidence, what string, timeout, interval time.Duration, cond func() (bool, string)) string {
	t.Helper()
	start := time.Now()
	var last string
	for {
		ok, s := cond()
		last = s
		if ok {
			ev.Note("%s after %s: %s", what, time.Since(start).Round(time.Second), s)
			return s
		}
		if time.Since(start) > timeout {
			t.Fatalf("%s: not reached within %s (last: %s)", what, timeout, last)
		}
		time.Sleep(interval)
	}
}

// stage copies a local file into the VM as root with mode, via /tmp.
func (b *sbk) stageRoot(t *testing.T, ev *Evidence, local, remote, mode string) {
	t.Helper()
	tmp := stageDir + "/" + filepath.Base(remote) + ".in"
	_, err := b.vm.Exec(b.ctx, "mkdir -p "+stageDir)
	must(t, err, "stage dir")
	must(t, b.vm.CopyIn(b.ctx, local, tmp), "copying "+filepath.Base(local))
	b.sudo(t, ev, "mkdir -p "+shQuote(filepath.Dir(remote))+" && install -m "+mode+" "+shQuote(tmp)+" "+shQuote(remote)+" && rm -f "+shQuote(tmp))
}

// writeLocal writes a 0600 file in the workdir.
func (b *sbk) writeLocal(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(b.cfg.Workdir, name)
	must(t, os.WriteFile(p, []byte(body), 0o600), "writing "+name)
	must(t, os.Chmod(p, 0o600), "chmod "+name)
	return p
}

// keyIn puts the age private key into the restore directory; keyOut removes it.
func (b *sbk) keyIn(t *testing.T, ev *Evidence) {
	b.stageRoot(t, ev, b.keyLocal, restoreDir+"/krill-backup-key.txt", "0600")
}

func (b *sbk) keyOut(t *testing.T, ev *Evidence) {
	b.sudo(t, ev, "rm -f "+restoreDir+"/krill-backup-key.txt; ls -la "+restoreDir)
}

// mc runs mc inside the MinIO container (its alias "s3" comes from the
// container's environment).
func (b *sbk) mc(t *testing.T, ev *Evidence, args string) string {
	t.Helper()
	return b.sudo(t, ev, "docker exec "+s3Container+" mc "+args)
}

// fetchObject writes an S3 object of the state bucket to the restore
// directory as object.sql.gz.age.
func (b *sbk) fetchObject(t *testing.T, ev *Evidence, key string) {
	t.Helper()
	b.sudo(t, ev, "mkdir -p "+restoreDir+" && chmod 0700 "+restoreDir+" && docker exec "+s3Container+" mc cat "+shQuote("s3/"+stateBucket+"/"+key)+
		" > "+restoreDir+"/object.sql.gz.age && sha256sum "+restoreDir+"/object.sql.gz.age")
}

// objectKeys lists the keys under a prefix of a bucket.
func (b *sbk) objectKeys(t *testing.T, ev *Evidence, bucket, prefix string) []string {
	t.Helper()
	out := b.mc(t, ev, "ls --recursive --json "+shQuote("s3/"+bucket+"/"+prefix))
	re := regexp.MustCompile(`"key":"([^"]+)"`)
	var keys []string
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		keys = append(keys, prefix+m[1])
	}
	sort.Strings(keys)
	return keys
}

// serviceContainer is the running container of a Swarm service.
func (b *sbk) serviceContainer(t *testing.T, service string) string {
	t.Helper()
	out, err := b.vm.Sudo(b.ctx, "docker ps -q -f label=com.docker.swarm.service.name="+shQuote(service)+" | head -1")
	must(t, err, "finding the container of "+service)
	c := strings.TrimSpace(out)
	if c == "" {
		t.Fatalf("no running container for service %s", service)
	}
	return c
}

// login signs a fresh client in as the operator.
func (b *sbk) loginAs(t *testing.T, ev *Evidence, password string) error {
	t.Helper()
	c := NewClient(t, b.cfg.HostPort)
	err := c.Login(b.ctx, operatorEmail, password)
	if err == nil {
		b.client = c
	}
	return err
}

// stateRuns counts state_backup_runs.
func (b *sbk) stateRuns(t *testing.T) int64 {
	return b.id(t, "select count(*) from state_backup_runs")
}

// backupNow presses Back up now and waits for its run; it returns the
// object key.
func (b *sbk) backupNow(t *testing.T, ev *Evidence) string {
	t.Helper()
	before := b.id(t, "select coalesce(max(id), 0) from state_backup_runs")
	b.postOK(t, ev, b.sbkPath()+"/run", url.Values{}, "Backup of Krill's state started.")
	row := waitFor(t, ev, "state backup run", 5*time.Minute, 3*time.Second, func() (bool, string) {
		out, err := b.host.Psql(b.ctx, fmt.Sprintf("select trigger||'|'||status||'|'||object_key||'|'||size_bytes||'|'||error from state_backup_runs where id > %d order by id desc limit 1", before))
		return err == nil && out != "", out
	})
	f := strings.SplitN(row, "|", 5)
	if len(f) < 5 || f[0] != "manual" || f[1] != "ok" || f[2] == "" {
		t.Fatalf("the state backup run is %q, want manual|ok|<key>", row)
	}
	return f[2]
}

// waitDeploy waits for the app's latest deployment to finish and wants done.
func (b *sbk) waitDeploy(t *testing.T, ev *Evidence) {
	t.Helper()
	st := b.waitDeployStatus(t, ev, 6*time.Minute, func(st string) bool { return st == "done" || st == "error" })
	if st != "done" {
		t.Fatalf("the app's deployment ended %q", st)
	}
}

// waitInstance waits until the DB instance is running and its container is up.
func (b *sbk) waitInstance(t *testing.T, ev *Evidence) {
	t.Helper()
	// 15 minutes: on a fresh VM the first deploy also downloads the image.
	waitFor(t, ev, "DB instance running", 15*time.Minute, 5*time.Second, func() (bool, string) {
		st, err := b.host.Psql(b.ctx, fmt.Sprintf("select status from db_instances where id=%d", b.inst))
		if err != nil || st != "running" {
			return false, st
		}
		out, err := b.vm.Sudo(b.ctx, "docker ps -q -f label=com.docker.swarm.service.name="+shQuote(b.instApp))
		if err != nil || strings.TrimSpace(out) == "" {
			return false, "running, no container yet"
		}
		c := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		r, err := b.vm.Sudo(b.ctx, "docker exec "+c+" pg_isready -h 127.0.0.1 -U postgres")
		return err == nil, strings.TrimSpace(r)
	})
}

// curlApp asks Traefik for the app's domain and wants nginx's page. On a
// fresh VM the gateway's image may still be downloading, so its task gets
// time to come up first.
func (b *sbk) curlApp(t *testing.T, ev *Evidence) {
	t.Helper()
	waitFor(t, ev, "gateway running", 20*time.Minute, 5*time.Second, func() (bool, string) {
		out, err := b.vm.Sudo(b.ctx, "docker service ls --filter name=krill-traefik --format '{{.Replicas}}'")
		out = strings.TrimSpace(out)
		return err == nil && out == "1/1", out
	})
	waitFor(t, ev, "app answering through Traefik", 3*time.Minute, 3*time.Second, func() (bool, string) {
		out, err := b.vm.Sudo(b.ctx, "code=$(curl -s -o /tmp/accept-curl.html -w '%{http_code}' -H 'Host: "+appDomain+"' http://127.0.0.1/); "+
			"if grep -q 'Welcome to nginx' /tmp/accept-curl.html; then echo \"$code nginx\"; else echo \"$code\"; fi")
		out = strings.TrimSpace(out)
		return err == nil && out == "200 nginx", out
	})
}

// marker reads accept_marker as the logical database's owner over TCP with
// its password (what an app linked to it would do).
// The password comes from ldbPassFile on stdin, never on a command line.
func (b *sbk) marker(t *testing.T) (string, error) {
	c := b.serviceContainer(t, b.instApp)
	out, err := b.vm.Sudo(b.ctx, "docker exec -i "+c+" sh -c 'PGPASSWORD=\"$(cat)\" psql -X -At -h 127.0.0.1 -U "+b.ldbUser+
		" -d appdb -c \"select v from accept_marker\"' < "+ldbPassFile)
	return strings.TrimSpace(out), err
}

var connRe = regexp.MustCompile(`postgres(?:ql)?://([^:@/\s"]+):([^@\s"]+)@`)

// readLogicalDB reads the logical database's user and password from its
// page, as runbook A step 8 says.
func (b *sbk) readLogicalDB(t *testing.T, ev *Evidence) {
	t.Helper()
	resp, err := b.client.Get(b.ctx, b.dbPath())
	must(t, err, "database page")
	if resp.Status != http.StatusOK {
		t.Fatalf("database page answered %d", resp.Status)
	}
	m := connRe.FindStringSubmatch(resp.Body)
	if m == nil {
		t.Fatalf("no connection string with a password on the database page")
	}
	b.ldbUser, b.ldbPass = m[1], m[2]
	redactValue(b.ldbPass)
	b.stageRoot(t, ev, b.writeLocal(t, "ldb.pw", b.ldbPass), ldbPassFile, "0600")
	ev.Note("logical database user %s (password read from the page)", b.ldbUser)
}

// envValue reads one KEY= line's value from krill.env. The line is read
// whole, so the harness log masks a secret's value by its variable name
// (secretRe) before the caller registers the bare value for redaction.
func (b *sbk) envValue(t *testing.T, key string) string {
	t.Helper()
	out, err := b.vm.Sudo(b.ctx, "grep '^"+key+"=' "+envFile+" | tail -1")
	must(t, err, "reading "+key)
	v := strings.TrimPrefix(strings.TrimSpace(out), key+"=")
	if v == "" {
		t.Fatalf("no %s in %s", key, envFile)
	}
	return v
}

// --- the test ---------------------------------------------------------------------

func TestStateBackupAcceptance(t *testing.T) {
	b := &sbk{suite: newSuiteFor(t, StateBackupDefaults), fp: map[string]accept.Fingerprint{}}
	b.fx = NewFixtures()

	b.run(t, "build_releases", func(t *testing.T, ev *Evidence) {
		for _, v := range StateBackupVariants {
			start := time.Now()
			rel, err := BuildRelease(b.ctx, t, b.cfg, v)
			must(t, err, "building "+v.Tag)
			ev.Note("built %s (base %d, schema %d) in %s", v.Tag, rel.Base, rel.Schema, time.Since(start).Round(time.Second))
			b.rels[v.Tag] = rel
			if b.base == 0 {
				b.base = rel.Base
			}
		}
		doc, err := os.ReadFile(filepath.Join(b.rels["v0.91.0"].Source, "docs", "install.md"))
		must(t, err, "reading docs/install.md")
		must(t, accept.Check(string(doc), accept.Runbook, accept.Covered), "docs/install.md against the runbook list")
		ev.Note("docs/install.md matches the %d listed runbook commands; base migration %d", len(accept.Runbook), b.base)
		recipient, keyPath, identity, err := accept.NewAgeKey(b.cfg.Workdir)
		must(t, err, "generating the age key")
		redactValue(identity)
		b.recipient, b.keyLocal = recipient, keyPath
		ev.Note("age recipient %s, private key in %s (0600)", recipient, keyPath)
	})

	b.run(t, "vm_up", func(t *testing.T, ev *Evidence) {
		start := time.Now()
		must(t, b.vm.Ensure(b.ctx), "creating/starting the VM")
		ev.Note("VM %s running after %s", b.cfg.VM, time.Since(start).Round(time.Second))
		out, err := b.vm.Exec(b.ctx, `printf '%s|' "a b" 'c'; echo`)
		must(t, err, "quoting probe")
		if strings.TrimSpace(out) != "a b|c|" {
			t.Fatalf("limactl shell mangles arguments: %q", out)
		}
	})

	b.run(t, "reset_and_tools", func(t *testing.T, ev *Evidence) {
		b.resetInstall(t, ev)
		b.sudo(t, ev, strings.Join([]string{
			removeFixturesScript,
			"if command -v docker >/dev/null 2>&1; then",
			"  docker service ls -q | xargs -r docker service rm",
			"  for i in $(seq 60); do [ -z \"$(docker ps -aq --filter label=com.docker.swarm.service.id)\" ] && break; sleep 2; done",
			"  docker swarm leave --force >/dev/null 2>&1 || true",
			"  docker volume ls -q | xargs -r docker volume rm",
			"  docker network rm krill-state docker_gwbridge >/dev/null 2>&1 || true",
			"fi",
			"command -v age >/dev/null || (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq age)",
			"age --version",
		}, "\n"))
	})

	b.run(t, "prefetch_images", func(t *testing.T, ev *Evidence) {
		ev.Note("environment, not the product under test: Docker Hub images come through mirror.gcr.io (dockerd registry-mirrors), and a pull stalled for 5 minutes is retried after a dockerd restart")
		start := time.Now()
		b.sudo(t, ev, prefetchScript())
		ev.Note("all %d images in place after %s", len(prefetchImages), time.Since(start).Round(time.Second))
	})

	b.run(t, "install_v0.91.0", func(t *testing.T, ev *Evidence) {
		b.runInstaller(t, ev, b.rels["v0.91.0"])
		b.sudo(t, ev, fmt.Sprintf("printf '%%s\\n' 'KRILL_ALLOW_PRIVATE_EGRESS=true' 'KRILL_UPDATE_REPO=%s' >> %s && "+
			"sed -i 's#^KRILL_ADMIN_EMAIL=.*#KRILL_ADMIN_EMAIL=%s#' %s && grep -c -e '^KRILL_ALLOW_PRIVATE_EGRESS=true$' -e '^KRILL_ADMIN_EMAIL=%s$' %s && systemctl restart krill",
			b.cfg.Repo, envFile, operatorEmail, envFile, operatorEmail, envFile))
		b.waitServing(t, ev, 10*time.Minute)
		pw, err := b.host.AdminPassword(b.ctx)
		must(t, err, "admin password")
		must(t, b.loginAs(t, ev, pw), "login as the operator")
		b.expectVersion(t, ev, "v0.91.0")
		b.expectSchema(t, ev, b.base)
	})

	b.run(t, "fixtures", func(t *testing.T, ev *Evidence) {
		line, err := accept.Htpasswd(b.fx.RegUser, b.fx.RegPassword)
		must(t, err, "htpasswd")
		b.stageRoot(t, ev, b.writeLocal(t, "s3.env", b.fx.S3EnvFile()), s3EnvFile, "0600")
		b.stageRoot(t, ev, b.writeLocal(t, "registry.pw", b.fx.RegPassword), regPassFile, "0600")
		b.stageRoot(t, ev, b.writeLocal(t, "s3-user.secret", b.fx.S3Secret), s3UserFile, "0600")
		b.stageRoot(t, ev, b.writeLocal(t, "htpasswd", line+"\n"), regData+"/auth/htpasswd", "0644")
		policy := filepath.Join(b.cfg.Workdir, "state-policy.json")
		must(t, os.WriteFile(policy, []byte(StatePolicyJSON()), 0o644), "policy")
		must(t, b.vm.CopyIn(b.ctx, policy, stageDir+"/state-policy.json"), "copying the policy")
		b.sudo(t, ev, b.fx.StartScript())
	})

	b.run(t, "populate", func(t *testing.T, ev *Evidence) { b.populate(t, ev) })

	// --- Scenario A -------------------------------------------------------------

	b.run(t, "A1_backup_now", func(t *testing.T, ev *Evidence) {
		src := b.fingerprint(t, ev, "F_src", pgContainer, "krill", "krill")
		b.stateKey = b.backupNow(t, ev)
		ev.Note("state backup object %s", b.stateKey)
		src2 := b.fingerprint(t, ev, "F_src2", pgContainer, "krill", "krill")
		b.compare(t, ev, "W", src, src2, allowTables("state_backup_runs"), krillDB)
	})

	b.run(t, "A2_fetch_object", func(t *testing.T, ev *Evidence) {
		b.fetchObject(t, ev, b.stateKey)
		out := b.sudo(t, ev, "install -m 0644 -o \"$SUDO_USER\" "+restoreDir+"/object.sql.gz.age "+stageDir+"/object.out && sha256sum "+stageDir+"/object.out")
		b.objLocal = filepath.Join(b.cfg.Workdir, "objects", filepath.Base(b.stateKey))
		must(t, os.MkdirAll(filepath.Dir(b.objLocal), 0o700), "objects dir")
		must(t, b.vm.CopyOut(b.ctx, stageDir+"/object.out", b.objLocal), "copying the object out")
		b.sudo(t, ev, "rm -f "+stageDir+"/object.out")
		sum, err := fileSHA256(b.objLocal)
		must(t, err, "sha256 on the Mac")
		ev.Note("object on the Mac: %s sha256 %s", b.objLocal, sum)
		if !strings.HasPrefix(strings.TrimSpace(out), sum) {
			t.Fatalf("sha256 on the Mac %s differs from the VM's %q", sum, out)
		}
	})

	b.run(t, "A3_restore_test_container", func(t *testing.T, ev *Evidence) {
		b.keyIn(t, ev)
		out := b.runbook(t, ev, "dump.verify", restoreObj, inRestoreDir, "")
		if !strings.Contains(out, "object OK") {
			t.Fatalf("the object does not verify")
		}
		b.runbook(t, ev, "dump.client", restoreObj, inRestoreDir+"\nset -o pipefail", "")
		block := b.cmd("test.block", restoreObj)
		const last = "\ndocker rm -f krill-restore-test"
		if !strings.HasSuffix(block, last) {
			t.Fatalf("the restore-test block no longer ends with %q", last)
		}
		out = b.sudo(t, ev, inRestoreDir+"\n"+strings.TrimSuffix(block, last))
		if !strings.Contains(out, "restore OK") {
			t.Fatalf("the restore into the throwaway container did not print restore OK")
		}
		t.Cleanup(func() { b.vm.Sudo(b.ctx, "docker rm -f krill-restore-test >/dev/null 2>&1 || true") })
		obj := b.fingerprint(t, ev, "F_obj", "krill-restore-test", "postgres", "postgres")
		b.sudo(t, ev, strings.TrimPrefix(last, "\n"))
		b.note("test.block", "verbatim; fingerprint taken before its last line")
		b.compare(t, ev, "O", b.fp["F_src"], obj, nil, [3]string{})
		b.keyOut(t, ev)
	})

	b.run(t, "A4_save_operator_secrets", func(t *testing.T, ev *Evidence) {
		b.old.email = b.envValue(t, "KRILL_ADMIN_EMAIL")
		b.old.password = b.envValue(t, "KRILL_ADMIN_PASSWORD")
		b.old.secretKey = b.envValue(t, "KRILL_SECRET_KEY")
		redactValue(b.old.password)
		redactValue(b.old.secretKey)
		b.old.extraEnv = []string{"KRILL_ALLOW_PRIVATE_EGRESS=" + b.envValue(t, "KRILL_ALLOW_PRIVATE_EGRESS"), "KRILL_UPDATE_REPO=" + b.envValue(t, "KRILL_UPDATE_REPO")}
		b.old.cookie = b.client.Cookie("krill_session")
		redactValue(b.old.cookie)
		if b.old.cookie == "" {
			t.Fatalf("no session cookie to replay later")
		}
		b.old.nodeID = strings.TrimSpace(b.sudo(t, ev, "docker info --format '{{.Swarm.NodeID}}'"))
		ev.Note("saved: admin email %s, its password, KRILL_SECRET_KEY, %d extra krill.env lines, the session cookie; swarm node %s",
			b.old.email, len(b.old.extraEnv), b.old.nodeID)
	})

	b.run(t, "A5_wipe", func(t *testing.T, ev *Evidence) {
		out, err := b.vm.Sudo(b.ctx, b.cmd("uninstall.main", nil))
		ev.Output(b.cmd("uninstall.main", nil), out)
		ev.Note("Uninstall, first block (pasted, no set -e): exit error %v", err)
		b.note("uninstall.main", "verbatim, as pasted (no set -e)")
		b.runbook(t, ev, "uninstall.leftovers", nil, "set -e", "verbatim, under set -e")
		b.sudo(t, ev, strings.Join([]string{
			"docker service ls -q | xargs -r docker service rm",
			"for i in $(seq 90); do [ -z \"$(docker ps -aq --filter label=com.docker.swarm.service.id)\" ] && break; sleep 2; done",
			"docker swarm leave --force",
			"for i in $(seq 30); do docker volume ls -q | xargs -r docker volume rm >/dev/null 2>&1; [ -z \"$(docker volume ls -q)\" ] && break; sleep 2; done",
			"docker network rm docker_gwbridge >/dev/null 2>&1 || true",
			"docker images -q " + appRepo + " | sort -u | xargs -r docker image rm -f",
			"rm -rf " + restoreDir + " " + stageDir + "/object.out",
			"echo wipe-done",
		}, "\n"))
	})

	b.run(t, "A6_no_leftovers", func(t *testing.T, ev *Evidence) { b.noLeftovers(t, ev) })

	b.run(t, "A7_reinstall", func(t *testing.T, ev *Evidence) {
		b.runInstaller(t, ev, b.rels["v0.91.1"])
		b.waitServing(t, ev, 10*time.Minute)
		node := strings.TrimSpace(b.sudo(t, ev, "docker info --format '{{.Swarm.NodeID}}'"))
		ev.Note("new swarm node %s (old %s)", node, b.old.nodeID)
		if node == "" || node == b.old.nodeID {
			t.Fatalf("the swarm is not new: node %q, old %q", node, b.old.nodeID)
		}
		newPW := b.envValue(t, "KRILL_ADMIN_PASSWORD")
		redactValue(newPW)
		if newPW == b.old.password {
			t.Fatalf("the reinstall kept the old admin password")
		}
		b.runbook(t, ev, "a.stop", nil, "", "") // step 2
		// Step 3, with F1: the original email and secret key, and every other
		// line added to the old krill.env.
		edit := b.writeLocal(t, "env-edit.sh", fmt.Sprintf("set -e\nsed -i -e 's#^KRILL_ADMIN_EMAIL=.*#KRILL_ADMIN_EMAIL=%s#' -e 's#^KRILL_SECRET_KEY=.*#KRILL_SECRET_KEY=%s#' %s\nprintf '%%s\\n' %s >> %s\n",
			b.old.email, b.old.secretKey, envFile, shQuote(b.old.extraEnv[0])+" "+shQuote(b.old.extraEnv[1]), envFile))
		b.stageRoot(t, ev, edit, "/root/accept-env-edit.sh", "0700")
		b.sudo(t, ev, "sh /root/accept-env-edit.sh && rm -f /root/accept-env-edit.sh")
		must(t, os.Remove(edit), "removing the local edit script")
		got := b.envValue(t, "KRILL_SECRET_KEY")
		if got != b.old.secretKey || b.envValue(t, "KRILL_ADMIN_EMAIL") != b.old.email || b.envValue(t, "KRILL_ALLOW_PRIVATE_EGRESS") != "true" {
			t.Fatalf("krill.env does not carry the original settings")
		}
		ev.Note("krill.env: original admin email, original KRILL_SECRET_KEY and the extra lines in place")
	})

	b.run(t, "A8_restore", func(t *testing.T, ev *Evidence) {
		b.sudo(t, ev, "mkdir -p "+restoreDir+" && chmod 0700 "+restoreDir)
		b.stageRoot(t, ev, b.objLocal, restoreDir+"/object.sql.gz.age", "0600")
		sum, err := fileSHA256(b.objLocal)
		must(t, err, "sha256")
		out := b.sudo(t, ev, "sha256sum "+restoreDir+"/object.sql.gz.age")
		if !strings.HasPrefix(strings.TrimSpace(out), sum) {
			t.Fatalf("the object copied back differs")
		}
		b.keyIn(t, ev)
		b.restoreDump(t, ev, b.base)
		restored := b.fingerprint(t, ev, "F_restored", pgContainer, "krill", "krill")
		b.compare(t, ev, "R", b.fp["F_obj"], restored, nil, krillDB)
		b.maxDeployment = b.id(t, "select coalesce(max(id), 0) from deployments")
		b.fingerprintWith(t, ev, "F_restored_S", pgContainer, "krill", "krill", ruleSOptions(b.maxDeployment))
		// Step 5, as fixed (F2/F5): pause the schedules, remove the old workers.
		b.runbook(t, ev, "a.pause", nil, "", "")
		b.runbook(t, ev, "a.nodes", nil, "", "")
		if v := b.q(t, "select coalesce(bool_or(enabled), false)::text from backups"); v != "false" {
			t.Fatalf("backups still enabled after the pause: %s", v)
		}
		if v := b.q(t, "select (select count(*) from volume_backups) + (select count(*) from bucket_backups) + (select count(*) from cluster_nodes)"); v != "0" {
			t.Fatalf("volume/bucket backups or cluster nodes left: %s", v)
		}
		b.fingerprint(t, ev, "F_paused", pgContainer, "krill", "krill")
		b.keyOut(t, ev)
	})

	b.run(t, "A9_start", func(t *testing.T, ev *Evidence) {
		cursor := b.journalCursor(t)
		b.runbook(t, ev, "a.start", nil, "", "")
		b.waitServing(t, ev, 5*time.Minute)
		b.expectSchema(t, ev, b.base+1)
		if v := b.q(t, "select to_regclass('"+forwardTable+"') is not null"); v != "t" {
			t.Fatalf("the forward migration did not run")
		}
		newPW := b.envValue(t, "KRILL_ADMIN_PASSWORD")
		if err := b.loginAs(t, ev, newPW); err == nil {
			t.Fatalf("the reinstall's generated password signs in; the restored operator should keep the old one")
		}
		must(t, b.loginAs(t, ev, b.old.password), "login with the old password")
		ev.Note("old password signs in, the reinstall's does not")
		if v := b.q(t, "select string_agg(email||'='||is_admin::text, ',' order by email) from users"); !strings.Contains(v, operatorEmail+"=true") || strings.Contains(v, "admin@krill.local=true") {
			t.Fatalf("instance operators: %s", v)
		} else {
			ev.Note("users: %s", v)
		}
		for _, p := range []string{b.sbkPath(), b.orgPath() + "/nodes"} {
			resp, err := b.client.Get(b.ctx, p)
			must(t, err, p)
			if resp.Status != http.StatusOK {
				t.Fatalf("%s answered %d to the operator", p, resp.Status)
			}
		}
		oldc := NewClientWithCookie(t, b.cfg.HostPort, "krill_session", b.old.cookie)
		resp, err := oldc.Get(b.ctx, b.orgPath())
		must(t, err, "GET with the old cookie")
		ev.Note("the old session cookie lands on %s", resp.URL)
		if !strings.HasSuffix(strings.SplitN(resp.URL, "?", 2)[0], "/login") {
			t.Fatalf("the old session cookie was accepted (landed on %s)", resp.URL)
		}
		j := b.journal(t, ev, cursor)
		for _, bad := range []string{"cannot be decrypted", "undecryptable", "level=ERROR"} {
			if strings.Contains(j, bad) {
				t.Fatalf("the journal since the start contains %q", bad)
			}
		}
	})

	b.run(t, "A10_recover_workloads", func(t *testing.T, ev *Evidence) { b.recoverWorkloads(t, ev) })

	// --- Scenario B -------------------------------------------------------------

	b.run(t, "B1_publish", func(t *testing.T, ev *Evidence) {
		for _, v := range StateBackupVariants {
			must(t, PublishRelease(b.ctx, t, b.cfg, b.rels[v.Tag]), "publishing "+v.Tag)
		}
		b.orgID = b.org
		b.latest(t, ev, "v0.91.2")
	})

	b.run(t, "B2_failed_update", func(t *testing.T, ev *Evidence) {
		pre := b.fingerprint(t, ev, "F_pre", pgContainer, "krill", "krill")
		runsBefore := b.id(t, "select coalesce(max(id), 0) from state_backup_runs")
		cursor := b.journalCursor(t)
		resp, err := b.client.Install(b.ctx, b.org, "v0.91.2")
		must(t, err, "install")
		flashContains(t, ev, "POST install v0.91.2", resp, "The update has started.")
		b.waitInstalled(t, ev, "v0.91.2", 10*time.Minute)
		want := fmt.Sprintf("database schema is dirty at version %d", b.base+2)
		waitFor(t, ev, "v0.91.2 refusing the dirty schema", 4*time.Minute, 5*time.Second, func() (bool, string) {
			j, err := b.host.JournalSince(b.ctx, cursor)
			if err != nil {
				return false, err.Error()
			}
			i := strings.Index(j, "version=v0.91.2")
			return i >= 0 && strings.Contains(j[i:], want), "waiting"
		})
		b.journal(t, ev, cursor)
		v, dirty, err := b.host.SchemaVersion(b.ctx)
		must(t, err, "schema")
		if v != b.base+2 || !dirty {
			t.Fatalf("schema %d dirty=%v, want %d dirty", v, dirty, b.base+2)
		}
		row := b.q(t, fmt.Sprintf("select trigger||'|'||status||'|'||object_key from state_backup_runs where id > %d order by id", runsBefore))
		if !strings.HasPrefix(row, "pre_update|ok|") || strings.Contains(row, "\n") {
			t.Fatalf("pre-update run(s): %q", row)
		}
		var pick []string
		for _, k := range b.objectKeys(t, ev, stateBucket, statePrefix+"/krill-state/pre-update/") {
			if strings.HasSuffix(k, "_v0.91.1_to_v0.91.2.sql.gz.age") {
				pick = append(pick, k)
			}
		}
		if len(pick) != 1 || pick[0] != strings.TrimPrefix(row, "pre_update|ok|") {
			t.Fatalf("pre-update objects %v, run %q", pick, row)
		}
		b.stateKey = pick[0]
		ev.Note("pre-update snapshot %s", b.stateKey)
		pre2 := b.fingerprint(t, ev, "F_pre2", pgContainer, "krill", "krill")
		allow := allowTables("state_backup_runs")
		allow["table:schema_migrations"] = true
		b.compare(t, ev, "W", pre, pre2, allow, krillDB)
	})

	b.run(t, "B3_runbook_b", func(t *testing.T, ev *Evidence) {
		armed, err := b.host.TimerActive(b.ctx, revertTimer)
		must(t, err, "revert timer")
		if !armed {
			t.Fatalf("the revert timer is no longer armed: too slow to exercise runbook B step 1")
		}
		ev.Note("%s armed before step 1", revertTimer)
		out, _ := b.vm.Sudo(b.ctx, b.cmd("b.cancel", nil)) // as pasted: a stop of a unit that is not loaded fails harmlessly
		ev.Output(b.cmd("b.cancel", nil), out)
		b.note("b.cancel", "verbatim, as pasted (no set -e)")
		if !strings.Contains(out, "0 timers listed") {
			t.Fatalf("step 1 left krill-update timers")
		}
		if ok, _ := b.host.FileExists(b.ctx, pendingMarker); ok {
			t.Fatalf("%s still there", pendingMarker)
		}
		b.runbook(t, ev, "b.stop", nil, "", "")
		b.runbook(t, ev, "b.prev", nil, "", "")
		b.fetchObject(t, ev, b.stateKey)
		b.keyIn(t, ev)
		b.restoreDump(t, ev, b.base+1)
		restored := b.fingerprint(t, ev, "F_B", pgContainer, "krill", "krill")
		b.compare(t, ev, "R", b.fp["F_pre"], restored, nil, krillDB)
		b.keyOut(t, ev)
		b.runbook(t, ev, "b.start", nil, "", "")
	})

	b.run(t, "B4_verify_b", func(t *testing.T, ev *Evidence) {
		b.waitServing(t, ev, 5*time.Minute)
		must(t, b.loginAs(t, ev, b.old.password), "login")
		b.expectVersion(t, ev, "v0.91.1")
		want, err := fileSHA256(b.rels["v0.91.1"].Binary)
		must(t, err, "sha256 of v0.91.1")
		got := b.sudo(t, ev, "sha256sum "+binPath)
		if !strings.HasPrefix(strings.TrimSpace(got), want) {
			t.Fatalf("%s is not the v0.91.1 release binary", binPath)
		}
		b.expectPrev(t, ev, "")
		b.expectSchema(t, ev, b.base+1)
		timers, err := b.host.Timers(b.ctx)
		must(t, err, "timers")
		ev.Output("systemctl list-timers --all --no-legend 'krill-update-*'", timers)
		if strings.Contains(timers, "krill-update-") {
			t.Fatalf("krill-update timers left: %s", timers)
		}
		st1, err := b.host.UnitState(b.ctx)
		must(t, err, "unit")
		time.Sleep(60 * time.Second)
		st2, err := b.host.UnitState(b.ctx)
		must(t, err, "unit")
		ev.Output("systemctl show krill (60s apart)", st1.Raw+"\n---\n"+st2.Raw)
		if st2.ActiveState != "active" || st2.NRestarts != st1.NRestarts {
			t.Fatalf("krill not steady: %s -> %s", st1.Raw, st2.Raw)
		}
		b.curlApp(t, ev)
		after := b.fingerprint(t, ev, "F_B2", pgContainer, "krill", "krill")
		b.compare(t, ev, "S", b.fp["F_B"], after, allowTables(), krillDB)
		ev.Output("updates page", b.page(t, ev))
	})

	b.run(t, "B5_secret_scan", func(t *testing.T, ev *Evidence) {
		for _, f := range []string{"report.md", "test.log"} {
			data, err := os.ReadFile(filepath.Join(b.cfg.Workdir, f))
			must(t, err, "reading "+f)
			if hits := accept.ScanForSecrets(string(data), RedactedValues()); len(hits) > 0 {
				t.Fatalf("%s carries secrets: %v", f, hits)
			}
			ev.Note("%s: no secret found (%d values checked, plus the age identity pattern)", f, len(RedactedValues()))
		}
	})

	b.run(t, "summary", func(t *testing.T, ev *Evidence) {
		how := map[string][]string{}
		for _, r := range b.ran {
			how[r.id] = append(how[r.id], r.how)
		}
		for _, c := range accept.Runbook {
			switch {
			case len(how[c.ID]) > 0:
				ev.Note("%-24s %-38s ran: %s", c.ID, c.Section, strings.Join(how[c.ID], "; "))
			case c.Mode == accept.NotRun:
				ev.Note("%-24s %-38s not run: %s", c.ID, c.Section, c.Why)
			default:
				t.Errorf("%s (%s) was neither run nor listed as not run", c.ID, c.Section)
			}
		}
		ev.Note("not covered (spec §2): two hosts / a changed IP or hostname / a fresh Docker and image cache; worker nodes (rejoin, firewall rulesets, pinned placement, a DB instance pinned to a lost node); DNS, Panel domain, the Firewall page; volume and bucket restores; the external-database variant and the old-psql prefix")
	})

	b.run(t, "cleanup", func(t *testing.T, ev *Evidence) {
		if b.cfg.KeepVM {
			ev.Note("KRILL_ACCEPT_KEEP_VM=1: keeping VM %s", b.cfg.VM)
			return
		}
		must(t, b.vm.Delete(b.ctx), "deleting the VM")
		ev.Note("deleted VM %s", b.cfg.VM)
	})
}

// restoreDump runs "Restoring the dump" for the bundled database against
// object.sql.gz.age in the restore directory and checks the result.
func (b *sbk) restoreDump(t *testing.T, ev *Evidence, wantSchema int) {
	t.Helper()
	if out := b.runbook(t, ev, "dump.verify", restoreObj, inRestoreDir, ""); !strings.Contains(out, "object OK") {
		t.Fatalf("the object does not verify")
	}
	b.runbook(t, ev, "dump.client", restoreObj, inRestoreDir+"\nset -o pipefail", "")
	b.runbook(t, ev, "dump.recreate", nil, "", "")
	if out := b.runbook(t, ev, "dump.restore", restoreObj, inRestoreDir+"\nset -o pipefail", "verbatim, in the same bash script as set -o pipefail"); !strings.Contains(out, "restore OK") {
		t.Fatalf("the restore did not print restore OK")
	}
	b.runbook(t, ev, "dump.check", nil, "", "")
	b.expectSchema(t, ev, wantSchema)
	if v := b.q(t, "select count(*) from applications"); v != "1" {
		t.Fatalf("applications after the restore: %s, want 1", v)
	}
	if v := b.q(t, "select (select count(*) from sessions) + (select count(*) from metric_samples)"); v != "0" {
		t.Fatalf("sessions/metric_samples restored with data: %s rows", v)
	}
}

func (b *sbk) populate(t *testing.T, ev *Evidence) {
	b.post(t, ev, "/orgs", url.Values{"name": {"Accept"}})
	b.org = b.id(t, "select id from organizations where name='Accept'")
	b.orgID = b.org
	b.post(t, ev, b.orgPath()+"/projects", url.Values{"name": {"accept"}, "description": {""}})
	b.proj = b.id(t, fmt.Sprintf("select id from projects where organization_id=%d and name='accept'", b.org))
	b.post(t, ev, fmt.Sprintf("%s/projects/%d/environments", b.orgPath(), b.proj), url.Values{"name": {"production"}})
	b.env = b.id(t, fmt.Sprintf("select id from environments where project_id=%d and name='production'", b.proj))

	b.postOK(t, ev, b.orgPath()+"/registries", url.Values{"name": {"accept-registry"}, "registry_url": {registryHost},
		"username": {b.fx.RegUser}, "password": {b.fx.RegPassword}}, "")
	b.regID = b.id(t, fmt.Sprintf("select id from registries where organization_id=%d and name='accept-registry'", b.org))

	b.post(t, ev, b.envPath()+"/apps", url.Values{"name": {"web"}, "source_type": {"image"}, "image": {appRepo}, "tag": {appTag},
		"domain": {appDomain}, "port": {"80"}, "registry_id": {strconv.FormatInt(b.regID, 10)}})
	b.app = b.id(t, fmt.Sprintf("select id from applications where environment_id=%d and name='web'", b.env))
	b.suite.app = appRef{projID: b.proj, envID: b.env, appID: b.app}
	if v := b.q(t, fmt.Sprintf("select coalesce(registry_id, 0) from applications where id=%d", b.app)); v != strconv.FormatInt(b.regID, 10) {
		b.post(t, ev, b.appPath()+"/registry", url.Values{"registry_id": {strconv.FormatInt(b.regID, 10)}})
	}
	b.post(t, ev, b.appPath()+"/env", url.Values{"env": {"ACCEPT_ONE=1\nACCEPT_TWO=two words"}})
	b.domainID = b.id(t, fmt.Sprintf("select id from domains where application_id=%d and host='%s'", b.app, appDomain))
	b.post(t, ev, fmt.Sprintf("%s/domains/%d/exposure", b.appPath(), b.domainID), url.Values{"exposed": {"on"}, "paths": {""}})
	b.post(t, ev, b.appPath()+"/deploy", url.Values{"image": {appRepo}, "tag": {appTag}})
	b.waitDeploy(t, ev)
	b.curlApp(t, ev)

	b.post(t, ev, b.orgPath()+"/notifications", url.Values{"bot_token": {sbkTelegram}, "chat_id": {"1"}, "enabled": {"on"}})
	redactValue(sbkTelegram)
	if v := b.q(t, fmt.Sprintf("select count(*) from notification_channels where org_id=%d", b.org)); v != "1" {
		t.Fatalf("notification channel not saved")
	}

	b.post(t, ev, b.orgPath()+"/db-servers", url.Values{"engine": {"postgres"}, "name": {"accept-pg"}, "version": {""}, "external_port": {""}})
	b.inst = b.id(t, fmt.Sprintf("select id from db_instances where organization_id=%d and name='accept-pg'", b.org))
	b.instApp = b.q(t, fmt.Sprintf("select app_name from db_instances where id=%d", b.inst))
	if v := b.q(t, fmt.Sprintf("select status from db_instances where id=%d", b.inst)); v != "running" {
		b.post(t, ev, fmt.Sprintf("%s/db-servers/%d/deploy", b.orgPath(), b.inst), url.Values{})
	}
	b.waitInstance(t, ev)
	b.post(t, ev, b.envPath()+"/databases", url.Values{"instance_id": {strconv.FormatInt(b.inst, 10)}, "name": {"appdb"}, "db_name": {"appdb"}, "username": {"appdb"}})
	b.ldb = b.id(t, fmt.Sprintf("select id from logical_databases where environment_id=%d and name='appdb'", b.env))
	b.readLogicalDB(t, ev)
	c := b.serviceContainer(t, b.instApp)
	b.sudo(t, ev, "docker exec "+c+" psql -v ON_ERROR_STOP=1 -U "+shQuote(b.ldbUser)+" -d appdb -c \"CREATE TABLE accept_marker(v text); INSERT INTO accept_marker VALUES ('before-backup')\"")
	if m, err := b.marker(t); err != nil || m != "before-backup" {
		t.Fatalf("marker over TCP with the page's password: %q, %v", m, err)
	}

	b.post(t, ev, b.orgPath()+"/destinations", url.Values{"name": {"app-backups"}, "endpoint": {s3Endpoint}, "bucket": {appBucket},
		"region": {"us-east-1"}, "access_key": {b.fx.S3RootUser}, "secret_key": {b.fx.S3RootPassword}})
	b.dest = b.id(t, fmt.Sprintf("select id from destinations where organization_id=%d and name='app-backups'", b.org))
	b.post(t, ev, b.dbPath()+"/backups", url.Values{"destination_id": {strconv.FormatInt(b.dest, 10)}, "schedule_preset": {"daily"},
		"schedule_custom": {""}, "retention": {"3"}, "prefix": {""}})
	b.backup = b.id(t, fmt.Sprintf("select id from backups where logical_database_id=%d", b.ldb))
	b.postOK(t, ev, fmt.Sprintf("%s/backups/%d/run", b.dbPath(), b.backup), url.Values{}, "Backup started")
	waitFor(t, ev, "app DB backup", 5*time.Minute, 3*time.Second, func() (bool, string) {
		st, err := b.host.Psql(b.ctx, fmt.Sprintf("select last_status from backups where id=%d", b.backup))
		return err == nil && (st == "ok" || st == "error"), st
	})
	if st := b.q(t, fmt.Sprintf("select last_status||' '||last_error from backups where id=%d", b.backup)); !strings.HasPrefix(st, "ok") {
		t.Fatalf("app DB backup: %s", st)
	}
	keys := b.objectKeys(t, ev, appBucket, "")
	if len(keys) != 1 {
		t.Fatalf("app DB backup objects: %v, want one", keys)
	}
	b.appBackupKey = keys[0]

	b.postOK(t, ev, b.sbkPath(), url.Values{"endpoint": {s3Endpoint}, "bucket": {stateBucket}, "region": {"us-east-1"},
		"access_key": {b.fx.S3User}, "secret_key": {b.fx.S3Secret}, "prefix": {statePrefix}, "recipient": {b.recipient},
		"schedule_preset": {"daily"}, "schedule_custom": {""}, "retention": {"14"}}, "")
	b.post(t, ev, b.sbkPath()+"/enabled", url.Values{"enabled": {"1"}})
	if v := b.q(t, "select enabled::text||' '||bucket from state_backup_settings"); v != "true "+stateBucket {
		t.Fatalf("state backup settings: %q", v)
	}
	waitFor(t, ev, "quiet before the backup", 3*time.Minute, 3*time.Second, func() (bool, string) {
		n, err := b.host.Psql(b.ctx, "select count(*) from deployments where status='running'")
		if err != nil || n != "0" {
			return false, "deployments running: " + n
		}
		out, err := b.vm.Sudo(b.ctx, "docker service ls --format '{{.Name}} {{.Replicas}}'")
		if err != nil {
			return false, err.Error()
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 {
				r := strings.SplitN(f[1], "/", 2)
				if len(r) == 2 && r[0] != r[1] {
					return false, l
				}
			}
		}
		return true, strings.ReplaceAll(strings.TrimSpace(out), "\n", "; ")
	})
}

func (b *sbk) noLeftovers(t *testing.T, ev *Evidence) {
	checks := []struct{ cmd, want string }{
		{"systemctl list-unit-files 'krill*' --no-legend || true", ""},
		{"systemctl is-active krill || true", "inactive"},
		{"ls -d /usr/local/bin/krill* /etc/krill /etc/systemd/system/krill.service.d /run/krill-update-* 2>/dev/null || true", ""},
		{"docker info --format '{{.Swarm.LocalNodeState}}'", "inactive"},
		{"docker ps -a --format '{{.Names}}' | grep -v -x -e " + s3Container + " -e " + regContainer + " || true", ""},
		{"docker volume ls -q", ""},
		{"docker network ls --format '{{.Name}}' | grep -v -x -e bridge -e host -e none || true", ""},
		{"docker images -q " + appRepo + " || true", ""},
		{"nft list table inet krill >/dev/null 2>&1 && echo present || true", ""},
	}
	var bad []string
	for _, c := range checks {
		out := strings.TrimSpace(b.sudo(t, ev, c.cmd))
		if out != c.want {
			bad = append(bad, fmt.Sprintf("%s -> %q (want %q)", c.cmd, out, c.want))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("leftovers after the wipe:\n%s", strings.Join(bad, "\n"))
	}
	ev.Note("nothing of Krill left; %s and %s still run", s3Container, regContainer)
}

func (b *sbk) recoverWorkloads(t *testing.T, ev *Evidence) {
	// Redeploy the database instance.
	b.post(t, ev, fmt.Sprintf("%s/db-servers/%d/deploy", b.orgPath(), b.inst), url.Values{})
	b.waitInstance(t, ev)
	// Recreate the logical database's user and database by hand.
	b.readLogicalDB(t, ev)
	sql := b.cmd("a.recreate_user", map[string]string{"<user>": b.ldbUser, "<password>": b.ldbPass, "<db>": "appdb"})
	local := b.writeLocal(t, "recreate.sql", sql+"\n")
	b.stageRoot(t, ev, local, restoreDir+"/recreate.sql", "0600")
	must(t, os.Remove(local), "removing the local SQL file")
	c := b.serviceContainer(t, b.instApp)
	b.sudo(t, ev, "docker exec -i "+c+" psql -v ON_ERROR_STOP=1 -U postgres < "+restoreDir+"/recreate.sql; rc=$?; rm -f "+restoreDir+"/recreate.sql; exit $rc")
	b.note("a.recreate_user", "")
	// The redeployed instance started empty: the recreated database has no
	// marker until the backup is restored.
	if m, err := b.marker(t); err == nil || !strings.Contains(m, "accept_marker") {
		t.Fatalf("before the restore the marker query gave %q, %v; want an error naming accept_marker", m, err)
	}
	ev.Note("the recreated database is empty: the marker table does not exist yet")
	// Restore the app database backup.
	b.postOK(t, ev, fmt.Sprintf("%s/backups/%d/restore", b.dbPath(), b.backup), url.Values{"key": {b.appBackupKey}}, "Restore started")
	waitFor(t, ev, "marker row restored", 5*time.Minute, 5*time.Second, func() (bool, string) {
		m, err := b.marker(t)
		if err != nil {
			return false, err.Error()
		}
		return m == "before-backup", m
	})
	// Redeploy the app.
	b.post(t, ev, b.appPath()+"/deploy", url.Values{"image": {appRepo}, "tag": {appTag}})
	b.waitDeploy(t, ev)
	b.curlApp(t, ev)
	env := b.sudo(t, ev, "docker exec "+b.serviceContainer(t, fmt.Sprintf("krill-%d", b.app))+" env | grep '^ACCEPT_' | sort")
	if !strings.Contains(env, "ACCEPT_ONE=1") || !strings.Contains(env, "ACCEPT_TWO=two words") {
		t.Fatalf("the app's environment: %q", env)
	}
	// Resume the paused schedule.
	b.post(t, ev, fmt.Sprintf("%s/backups/%d/toggle", b.dbPath(), b.backup), url.Values{"enabled": {"1"}})
	if v := b.q(t, fmt.Sprintf("select enabled::text from backups where id=%d", b.backup)); v != "true" {
		t.Fatalf("backup schedule not resumed: %s", v)
	}
	// Back up now on the restored system: decrypts the restored S3 secret.
	key := b.backupNow(t, ev)
	b.fetchObject(t, ev, key)
	b.keyIn(t, ev)
	if out := b.runbook(t, ev, "dump.verify", restoreObj, inRestoreDir, ""); !strings.Contains(out, "object OK") {
		t.Fatalf("the new object does not verify")
	}
	b.keyOut(t, ev)
	if v := b.q(t, fmt.Sprintf("select count(*)||'|'||coalesce(bool_and(status = 'done'), false)::text from deployments where id > %d", b.maxDeployment)); v != "1|true" {
		t.Fatalf("new deployments since the restore (count|all done): %s, want 1|true (the app's redeploy)", v)
	}
	if v := b.q(t, "select count(*)||'|'||coalesce(bool_and(trigger = 'manual' and status = 'ok'), false)::text from state_backup_runs"); v != "1|true" {
		t.Fatalf("state backup runs since the restore: %s, want 1|true", v)
	}
	post := b.fingerprintWith(t, ev, "F_post_S", pgContainer, "krill", "krill", ruleSOptions(b.maxDeployment))
	// Changes outside these fail: the new state backup run, the forward
	// migration (its table and the schema_migrations row, whose version A9
	// checked), the new deployment's sequence, and the sampler's tables. The
	// forward table is left out of the schema hash, so the schema must match.
	allow := allowTables("state_backup_runs", "schema_migrations", forwardTable)
	allow["sequence:deployments_id_seq"] = true
	b.compare(t, ev, "S", b.fp["F_restored_S"], post, allow, krillDB)
}
