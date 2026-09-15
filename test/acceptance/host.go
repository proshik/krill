//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Host paths and names install.sh and internal/selfupdate use on the VM.
const (
	binPath       = "/usr/local/bin/krill"
	prevPath      = binPath + ".prev"
	envFile       = "/etc/krill/krill.env"
	dropInPath    = "/etc/systemd/system/krill.service.d/10-krill-update.conf"
	pendingMarker = "/run/krill-update-pending"
	revertedMark  = "/run/krill-update-reverted"
	revertTimer   = "krill-update-revert.timer"
	restartTimer  = "krill-update-restart.timer"
	pgContainer   = "krill-postgres"
)

// Host asserts over the VM's real state.
type Host struct {
	vm *VM
}

// NewHost wraps vm.
func NewHost(vm *VM) *Host { return &Host{vm: vm} }

// Psql runs one SQL statement against Krill's own database and returns the
// unaligned, tuples-only output.
func (h *Host) Psql(ctx context.Context, sql string) (string, error) {
	out, err := h.vm.Sudo(ctx, "docker exec "+pgContainer+" psql -U krill -d krill -Atc "+shQuote(sql))
	return strings.TrimSpace(out), err
}

// binaryVersion runs `<path> --version` and parses "krill vX.Y.Z".
func (h *Host) binaryVersion(ctx context.Context, path string) (string, error) {
	out, err := h.vm.Sudo(ctx, shQuote(path)+" --version")
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	v, ok := strings.CutPrefix(line, "krill ")
	if !ok {
		return "", fmt.Errorf("unexpected --version output from %s: %q", path, out)
	}
	return v, nil
}

// KrillVersion is the installed binary's version.
func (h *Host) KrillVersion(ctx context.Context) (string, error) {
	return h.binaryVersion(ctx, binPath)
}

// BinaryVersion is the version of any binary on the VM.
func (h *Host) BinaryVersion(ctx context.Context, path string) (string, error) {
	return h.binaryVersion(ctx, path)
}

// PrevVersion is krill.prev's version; false when there is no krill.prev.
func (h *Host) PrevVersion(ctx context.Context) (string, bool, error) {
	ok, err := h.FileExists(ctx, prevPath)
	if err != nil || !ok {
		return "", false, err
	}
	v, err := h.binaryVersion(ctx, prevPath)
	return v, err == nil, err
}

// SchemaVersion reads schema_migrations.
func (h *Host) SchemaVersion(ctx context.Context) (int, bool, error) {
	out, err := h.Psql(ctx, "select version, dirty from schema_migrations")
	if err != nil {
		return 0, false, err
	}
	parts := strings.Split(out, "|")
	if len(parts) != 2 {
		return 0, false, fmt.Errorf("unexpected schema_migrations output %q", out)
	}
	v, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false, fmt.Errorf("unexpected schema version %q", out)
	}
	return v, parts[1] == "t", nil
}

// UnitState is `systemctl show krill -p ActiveState,SubState,NRestarts,Result`.
type UnitState struct {
	ActiveState, SubState, Result string
	NRestarts                     int
	Raw                           string
}

// UnitState reads the krill unit's state.
func (h *Host) UnitState(ctx context.Context) (UnitState, error) {
	out, err := h.vm.Sudo(ctx, "systemctl show krill -p ActiveState,SubState,NRestarts,Result")
	if err != nil {
		return UnitState{}, err
	}
	st := UnitState{Raw: strings.TrimSpace(out)}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			st.ActiveState = v
		case "SubState":
			st.SubState = v
		case "Result":
			st.Result = v
		case "NRestarts":
			st.NRestarts, _ = strconv.Atoi(v)
		}
	}
	return st, nil
}

// Timers lists the krill-update-* timers, loaded or not.
func (h *Host) Timers(ctx context.Context) (string, error) {
	out, err := h.vm.Sudo(ctx, "systemctl list-timers --all --no-legend 'krill-update-*'")
	return strings.TrimSpace(out), err
}

// TimerActive reports whether a timer unit is active (armed).
func (h *Host) TimerActive(ctx context.Context, unit string) (bool, error) {
	out, err := h.vm.Sudo(ctx, "systemctl is-active "+shQuote(unit)+" || true")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "active", nil
}

// DropInPresent reports whether the self-update drop-in exists.
func (h *Host) DropInPresent(ctx context.Context) (bool, error) {
	return h.FileExists(ctx, dropInPath)
}

// FileExists tests a path on the VM as root.
func (h *Host) FileExists(ctx context.Context, path string) (bool, error) {
	out, err := h.vm.Sudo(ctx, "if [ -e "+shQuote(path)+" ]; then echo yes; else echo no; fi")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// JournalCursor is the krill unit journal's current cursor, for JournalSince.
func (h *Host) JournalCursor(ctx context.Context) (string, error) {
	out, err := h.vm.Sudo(ctx, "journalctl -u krill -n 0 --show-cursor --no-pager")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(line), "-- cursor: "); ok {
			return c, nil
		}
	}
	// An empty journal has no cursor yet; JournalSince("") reads it all.
	return "", nil
}

// JournalSince returns the krill unit's journal after cursor ("" = all).
func (h *Host) JournalSince(ctx context.Context, cursor string) (string, error) {
	cmd := "journalctl -u krill --no-pager -o short-iso"
	if cursor != "" {
		cmd += " --after-cursor=" + shQuote(cursor)
	}
	return h.vm.Sudo(ctx, cmd)
}

// AdminPassword reads the generated admin password from krill.env. The line
// is read whole, so the logged command output carries the variable name and
// is masked by redact; the bare value is masked from then on.
func (h *Host) AdminPassword(ctx context.Context) (string, error) {
	out, err := h.vm.Sudo(ctx, "grep '^KRILL_ADMIN_PASSWORD=' "+envFile)
	if err != nil {
		return "", err
	}
	pw := strings.TrimPrefix(strings.TrimSpace(out), "KRILL_ADMIN_PASSWORD=")
	redactValue(pw)
	if pw == "" {
		return "", fmt.Errorf("no KRILL_ADMIN_PASSWORD in %s", envFile)
	}
	return pw, nil
}

// AdminEmail reads the admin email from krill.env.
func (h *Host) AdminEmail(ctx context.Context) (string, error) {
	out, err := h.vm.Sudo(ctx, "sed -n 's#^KRILL_ADMIN_EMAIL=##p' "+envFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DefaultOrgID resolves the seeded default organization.
func (h *Host) DefaultOrgID(ctx context.Context) (int64, error) {
	out, err := h.Psql(ctx, "select id from organizations where slug='default'")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}
