package auth

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// CookieName — имя cookie сессии.
const CookieName = "krill_session"

type ctxKey int

const (
	userIDKey ctxKey = iota
	orgIDKey
	roleKey
)

// Validator проверяет токен сессии.
type Validator interface {
	Validate(ctx context.Context, token string) (int64, bool)
}

// MemberResolver сообщает роль пользователя в организации.
type MemberResolver interface {
	Membership(ctx context.Context, userID, orgID int64) (Role, bool)
}

// RequireAuth пропускает запрос только при валидной сессии, иначе → /login.
func RequireAuth(v Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			uid, ok := v.Validate(r.Context(), c.Value)
			if !ok {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, uid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireOrgMember проверяет членство пользователя в org из пути {orgID}.
// Не член (или org нет) → 404. Кладёт orgID и роль в контекст.
func RequireOrgMember(m MemberResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			orgID, err := strconv.ParseInt(chi.URLParam(r, "orgID"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			role, ok := m.Membership(r.Context(), UserID(r.Context()), orgID)
			if !ok {
				http.NotFound(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), orgIDKey, orgID)
			ctx = context.WithValue(ctx, roleKey, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole требует роль не ниже min, иначе 403.
func RequireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !RoleOf(r.Context()).AtLeast(min) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserID достаёт id пользователя из контекста (0, если нет).
func UserID(ctx context.Context) int64 {
	if v, ok := ctx.Value(userIDKey).(int64); ok {
		return v
	}
	return 0
}

// OrgID достаёт id активной организации из контекста (0, если нет).
func OrgID(ctx context.Context) int64 {
	if v, ok := ctx.Value(orgIDKey).(int64); ok {
		return v
	}
	return 0
}

// RoleOf достаёт роль пользователя в активной org из контекста (RoleMember по умолчанию).
func RoleOf(ctx context.Context) Role {
	if v, ok := ctx.Value(roleKey).(Role); ok {
		return v
	}
	return RoleMember
}
