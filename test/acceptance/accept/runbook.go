package accept

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode says how the state-backup acceptance runs a runbook command.
type Mode int

const (
	// Verbatim: run as the doc writes it, placeholders filled in.
	Verbatim Mode = iota
	// Adapted: run with a stated change.
	Adapted
	// NotRun: not run, for a stated reason.
	NotRun
)

func (m Mode) String() string {
	switch m {
	case Verbatim:
		return "verbatim"
	case Adapted:
		return "adapted"
	}
	return "not run"
}

// Command is one command of docs/install.md the acceptance knows about. Text
// is the doc's exact text without indentation: one line of a fenced block, a
// run of consecutive lines of one (joined with "\n"), or a command written as
// inline code in the prose.
type Command struct {
	ID, Section, Text string
	Mode              Mode
	Why               string // required unless Verbatim
}

// Covered are the docs/install.md sections whose fenced commands must all
// be listed in Runbook.
var Covered = []string{
	"Restoring the dump",
	"A. Lost host, new host",
	"B. Failed upgrade on the same host",
	"Test a restore regularly",
	"Uninstall",
}

const (
	secDump      = "Restoring the dump"
	secA         = "A. Lost host, new host"
	secB         = "B. Failed upgrade on the same host"
	secTest      = "Test a restore regularly"
	secUninstall = "Uninstall"

	whyExternal = "not covered: the VM uses the bundled database (spec §2)"
	whyWorkers  = "not covered: the VM is a single node (spec §2)"
)

// Runbook is every command of the covered sections. TestCheckRepoInstallDoc
// pins it to docs/install.md, so editing a command there fails `make test`
// until this list follows.
var Runbook = []Command{
	{ID: "dump.verify", Section: secDump, Text: "set -o pipefail\n" +
		`age -d -i krill-backup-key.txt <object> | gunzip > /dev/null && echo "object OK"`},
	{ID: "dump.recreate", Section: secDump, Text: `docker exec krill-postgres psql -U krill -d postgres -c 'DROP DATABASE krill' -c 'CREATE DATABASE krill OWNER krill'`},
	{ID: "dump.restore", Section: secDump, Text: `age -d -i krill-backup-key.txt <object> | gunzip | docker exec -i krill-postgres psql -v ON_ERROR_STOP=1 --single-transaction -U krill -d krill \` + "\n" +
		`&& echo "restore OK"`},
	{ID: "dump.check", Section: secDump, Text: `docker exec krill-postgres psql -U krill -d krill -c 'SELECT version, dirty FROM schema_migrations' -c 'SELECT count(*) FROM applications'`},
	{ID: "dump.client", Section: secDump, Text: `age -d -i krill-backup-key.txt <object> | gunzip | grep -m1 '^\\restrict'` + "\n" +
		`age -d -i krill-backup-key.txt <object> | gunzip | sed -n '1,12p'` + "\n" +
		`docker exec krill-postgres psql --version   # an external database: the psql you restore with`},
	{ID: "dump.external_env", Section: secDump, Mode: NotRun, Why: whyExternal, Text: `export PGHOST=db.example.com PGPORT=5432 PGUSER=<owner> PGSSLMODE=verify-full` + "\n" +
		`export PGPASSFILE="$HOME/.krill-pgpass"`},
	{ID: "dump.external_pgpass", Section: secDump, Mode: NotRun, Why: whyExternal, Text: `( umask 077; read -rs -p 'Database password: ' PW && echo \` + "\n" +
		`&& PW=$(printf '%s' "$PW" | sed 's/[\\:]/\\&/g') \` + "\n" +
		`&& printf '%s:%s:*:%s:%s\n' "$PGHOST" "$PGPORT" "$PGUSER" "$PW" > "$PGPASSFILE" ); unset PW`},
	{ID: "dump.external_recreate", Section: secDump, Mode: NotRun, Why: whyExternal, Text: `psql -d postgres -c 'DROP DATABASE "<db>"' -c 'CREATE DATABASE "<db>" OWNER "<owner>"'`},
	{ID: "dump.external_schemas", Section: secDump, Mode: NotRun, Why: whyExternal, Text: `psql -d <db> -c '\dn'` + "\n" +
		`age -d -i krill-backup-key.txt <object> | gunzip | grep '^CREATE SCHEMA'`},

	{ID: "a.stop", Section: secA, Text: "systemctl stop krill"},
	{ID: "a.start", Section: secA, Text: "systemctl start krill"},
	{ID: "a.pause", Section: secA, Text: `docker exec -i krill-postgres psql -v ON_ERROR_STOP=1 -U krill -d krill -c 'UPDATE backups SET enabled = false' \` + "\n" +
		`-c 'UPDATE volume_backups SET enabled = false' -c 'UPDATE bucket_backups SET enabled = false'`},
	{ID: "a.nodes", Section: secA, Text: `docker exec -i krill-postgres psql -U krill -d krill -c 'SELECT id, name, swarm_node_id FROM cluster_nodes'` + "\n" +
		`docker exec -i krill-postgres psql -U krill -d krill -c "DELETE FROM node_labels WHERE swarm_node_id IN (SELECT swarm_node_id FROM cluster_nodes WHERE swarm_node_id <> '')" \` + "\n" +
		`-c "DELETE FROM firewall_checks WHERE node LIKE 'node-%'" -c 'DELETE FROM cluster_nodes'`},
	{ID: "a.worker_firewall", Section: secA, Mode: NotRun, Why: whyWorkers, Text: `systemctl stop krill-fw-revert.timer 2>/dev/null; nft delete table inet krill 2>/dev/null; rm -f /etc/krill/firewall.nft; systemctl disable krill-firewall.service`},
	{ID: "a.node_hostname", Section: secA, Mode: NotRun, Why: whyWorkers, Text: `UPDATE db_instances SET node_hostname = '<hostname>' WHERE id = <id>;`},
	{ID: "a.recreate_user", Section: secA, Mode: Adapted, Why: "the SQL goes to `docker exec -i <instance container> psql -U postgres` from a 0600 file on stdin, so the password is never in argv", Text: `CREATE USER "<user>" PASSWORD '<password>';` + "\n" +
		`CREATE DATABASE "<db>" OWNER "<user>";` + "\n" +
		`REVOKE CONNECT ON DATABASE "<db>" FROM PUBLIC;`},

	{ID: "b.cancel", Section: secB, Text: `systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null` + "\n" +
		`systemctl stop krill-update-revert.service krill-update-restart.service 2>/dev/null` + "\n" +
		`rm -f /run/krill-update-pending` + "\n" +
		`systemctl list-timers 'krill-update-*'    # must list nothing`},
	{ID: "b.stop", Section: secB, Text: "systemctl stop krill"},
	{ID: "b.prev", Section: secB, Text: "mv /usr/local/bin/krill.prev /usr/local/bin/krill"},
	{ID: "b.start", Section: secB, Text: "systemctl start krill"},

	{ID: "test.block", Section: secTest, Text: `docker run -d --rm --name krill-restore-test -e POSTGRES_PASSWORD=x postgres:<major>-alpine` + "\n" +
		`until docker exec krill-restore-test pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done` + "\n" +
		`set -o pipefail` + "\n" +
		`age -d -i krill-backup-key.txt <object> | gunzip | docker exec -i krill-restore-test psql -v ON_ERROR_STOP=1 --single-transaction -U postgres \` + "\n" +
		`&& echo "restore OK"` + "\n" +
		`docker exec krill-restore-test psql -U postgres -c 'SELECT version, dirty FROM schema_migrations' -c 'SELECT count(*) FROM applications'` + "\n" +
		`docker rm -f krill-restore-test`},

	{ID: "uninstall.main", Section: secUninstall, Text: `systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null   # first: a timer could restart krill` + "\n" +
		`systemctl disable --now krill` + "\n" +
		`rm -f /etc/systemd/system/krill.service /usr/local/bin/krill /usr/local/bin/krill.prev` + "\n" +
		`rm -f /etc/systemd/system/krill.service.d/10-krill-update.conf` + "\n" +
		`rmdir /etc/systemd/system/krill.service.d 2>/dev/null` + "\n" +
		`systemctl daemon-reload` + "\n" +
		`rm -rf /etc/krill` + "\n" +
		`docker rm -f krill-postgres && docker volume rm krill-pg-data   # destroys Krill's state` + "\n" +
		`docker network rm krill-state                                   # after the container is gone`},
	{ID: "uninstall.leftovers", Section: secUninstall, Text: `docker service rm krill-alloy-node 2>/dev/null || true` + "\n" +
		`docker config ls -q --filter label=krill.observability=node-agent | xargs -r docker config rm` + "\n" +
		`docker secret ls -q --filter label=krill.observability=node-agent | xargs -r docker secret rm` + "\n" +
		`docker volume rm krill-alloy-node-data 2>/dev/null || true   # on every node, once the agent's task is gone`},
}

// Lookup returns the Runbook command with id; an unknown id is a bug in the
// test, so it panics.
func Lookup(id string) Command {
	for _, c := range Runbook {
		if c.ID == id {
			return c
		}
	}
	panic("accept: no runbook command " + id)
}

// Fill replaces each placeholder (a key such as "<object>") in text.
func Fill(text string, subst map[string]string) string {
	for k, v := range subst {
		text = strings.ReplaceAll(text, k, v)
	}
	return text
}

var headingRe = regexp.MustCompile(`^(#{1,6}) +(.*?)\s*$`)

// Section returns the body of the markdown section titled heading: the lines
// after its heading up to the next heading of the same or a higher level.
// Lines inside fenced blocks are never headings (a shell comment is not).
func Section(doc, heading string) (string, error) {
	lines := strings.Split(doc, "\n")
	start, level := -1, 0
	fence := false
	var body []string
	for i, l := range lines {
		if isFence(l) {
			fence = !fence
		}
		m := headingRe.FindStringSubmatch(l)
		if fence || isFence(l) || m == nil {
			if start >= 0 {
				body = append(body, l)
			}
			continue
		}
		if start >= 0 {
			if len(m[1]) <= level {
				break
			}
			body = append(body, l)
			continue
		}
		if m[2] == heading {
			start, level = i, len(m[1])
		}
	}
	if start < 0 {
		return "", fmt.Errorf("docs/install.md has no section %q", heading)
	}
	return strings.Join(body, "\n"), nil
}

func isFence(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "```") }

// FencedCommandLines returns the trimmed, non-blank lines inside the fenced
// blocks of body that are not comments.
func FencedCommandLines(body string) []string {
	var out []string
	fence := false
	for _, l := range strings.Split(body, "\n") {
		if isFence(l) {
			fence = !fence
			continue
		}
		t := strings.TrimSpace(l)
		if fence && t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

// Check reports every listed command whose text is no longer in its section
// and every fenced command line of a covered section that no command lists.
// The acceptance passes Covered.
func Check(doc string, cmds []Command, covered []string) error {
	var problems []string
	bySection := map[string][]Command{}
	sections := append([]string(nil), covered...)
	for _, c := range cmds {
		if _, ok := bySection[c.Section]; !ok && !contains(sections, c.Section) {
			sections = append(sections, c.Section)
		}
		bySection[c.Section] = append(bySection[c.Section], c)
	}
	for _, sec := range sections {
		body, err := Section(doc, sec)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		lines := FencedCommandLines(body)
		listed := make([]bool, len(lines))
		for _, c := range bySection[sec] {
			want := strings.Split(c.Text, "\n")
			for i := range want {
				want[i] = strings.TrimSpace(want[i])
			}
			found := false
			for i := 0; i+len(want) <= len(lines); i++ {
				if equal(lines[i:i+len(want)], want) {
					found = true
					for j := range want {
						listed[i+j] = true
					}
				}
			}
			if !found && (len(want) > 1 || !strings.Contains(body, c.Text)) {
				problems = append(problems, fmt.Sprintf("%s: %s (%s) is not in the doc any more", sec, c.ID, firstLine(c.Text)))
			}
		}
		for i, l := range lines {
			if !listed[i] {
				problems = append(problems, fmt.Sprintf("%s: unlisted command line: %s", sec, l))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("docs/install.md and the acceptance runbook list disagree:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }
