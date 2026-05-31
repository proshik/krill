package auth

import "testing"

func TestRoleAtLeast(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) {
		t.Error("owner >= admin")
	}
	if !RoleAdmin.AtLeast(RoleMember) {
		t.Error("admin >= member")
	}
	if RoleMember.AtLeast(RoleAdmin) {
		t.Error("member should NOT be >= admin")
	}
	if !RoleMember.AtLeast(RoleMember) {
		t.Error("member >= member")
	}
}

func TestParseRole(t *testing.T) {
	r, ok := ParseRole("admin")
	if !ok || r != RoleAdmin {
		t.Errorf("ParseRole(admin) = %v %v", r, ok)
	}
	if _, ok := ParseRole("bogus"); ok {
		t.Error("bogus role must be rejected")
	}
}
