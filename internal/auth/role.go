package auth

// Role — роль участника организации, упорядочена: member < admin < owner.
type Role int

const (
	RoleMember Role = iota
	RoleAdmin
	RoleOwner
)

// AtLeast сообщает, что роль не ниже требуемой.
func (r Role) AtLeast(min Role) bool { return r >= min }

// String возвращает строковое представление для БД.
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

// ParseRole разбирает строку из БД в Role.
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
