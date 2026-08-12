package api

import (
	"testing"

	"github.com/proshik/krill/internal/auth"
)

func TestCanWriteRequiresBothLevelAndRole(t *testing.T) {
	cases := []struct {
		name  string
		level Level
		role  auth.Role
		want  bool
	}{
		{"write token, admin", LevelWrite, auth.RoleAdmin, true},
		{"write token, owner", LevelWrite, auth.RoleOwner, true},
		{"write token, demoted to member", LevelWrite, auth.RoleMember, false},
		{"read token, owner", LevelRead, auth.RoleOwner, false},
		{"read token, member", LevelRead, auth.RoleMember, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := Identity{Level: tc.level, Role: tc.role}
			if got := id.CanWrite(); got != tc.want {
				t.Fatalf("CanWrite() = %v, want %v", got, tc.want)
			}
		})
	}
}
