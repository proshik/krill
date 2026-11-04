package dbservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrCloneIsolation = errors.New("existing logical databases permit clone role connections")

// CheckCloneIsolation never changes source ACLs. Before DDL it checks PUBLIC;
// after provisioning it checks the generated role's effective CONNECT access.
// Only modeled tenant databases are protected; the postgres maintenance DB is
// intentionally excluded. CONNECT is not evidence of table read privileges.
func (s *Service) CheckCloneIsolation(ctx context.Context, inst Instance, names []string, username string) error {
	if len(names) == 0 || s.engine == nil {
		return errors.New("clone isolation metadata unavailable")
	}
	literals := make([]string, len(names))
	for i, name := range names {
		if name == "" || strings.ContainsRune(name, 0) {
			return errors.New("invalid protected database name")
		}
		literals[i] = "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	access := "EXISTS (SELECT FROM aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a WHERE a.grantee=0 AND a.privilege_type='CONNECT')"
	if username != "" {
		if !ValidIdent(username) {
			return errors.New("invalid clone role")
		}
		access = "has_database_privilege('" + username + "',d.oid,'CONNECT')"
	}
	script := fmt.Sprintf("SET standard_conforming_strings=on; SET statement_timeout='10s'; SET lock_timeout='5s'; SET search_path=pg_catalog; SELECT count(*)=%d AND NOT COALESCE(bool_or(%s),true) FROM pg_database d WHERE d.datname IN (%s);", len(names), access, strings.Join(literals, ","))
	var out bytes.Buffer
	err := s.engine.Exec(ctx, inst.AppName,
		[]string{"psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", inst.Superuser, "-d", "postgres"},
		[]string{"PGPASSWORD=" + inst.SuperuserPassword}, strings.NewReader(script), &out)
	if err != nil {
		// Exec errors may include SQL or credentials. Do not propagate them.
		return errors.New("clone isolation check unavailable")
	}
	if strings.TrimSpace(out.String()) != "t" {
		return ErrCloneIsolation
	}
	return nil
}
