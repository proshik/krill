package auth

// Role — an organization member's role, ordered: member < admin < owner.
type Role int

const (
	RoleMember Role = iota
	RoleAdmin
	RoleOwner
)

// AtLeast reports whether the role is no lower than the required one.
func (r Role) AtLeast(min Role) bool { return r >= min }

// String returns the string representation for the DB.
func (r Role) String() string {
	switch r {
	case RoleOwner:
		return "owner"
	case RoleAdmin:
		return "admin"
	default:
		return "member"
	}
}

// ParseRole parses a string from the DB into a Role.
func ParseRole(s string) (Role, bool) {
	switch s {
	case "owner":
		return RoleOwner, true
	case "admin":
		return RoleAdmin, true
	case "member":
		return RoleMember, true
	default:
		return RoleMember, false
	}
}
