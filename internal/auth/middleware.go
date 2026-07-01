package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// CookieName — name of the session cookie.
const CookieName = "krill_session"

type ctxKey int

const (
	userIDKey ctxKey = iota
	orgIDKey
	roleKey
	instanceAdminKey
)

// Validator validates a session token.
type Validator interface {
	Validate(ctx context.Context, token string) (int64, bool)
}

// MemberResolver reports a user's role in an organization.
type MemberResolver interface {
	Membership(ctx context.Context, userID, orgID int64) (Role, bool)
}

// RequireAuth allows the request through only with a valid session, otherwise → /login.
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

// RequireOrgMember checks the user's membership in the org from the {orgID} path.
// Not a member (or org missing) → 404. Puts orgID and role into the context.
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
				slog.Info("org access denied (not a member)", "user_id", UserID(r.Context()), "org_id", orgID, "path", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), orgIDKey, orgID)
			ctx = context.WithValue(ctx, roleKey, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// InstanceAdminResolver reports whether a user is an instance-level operator.
type InstanceAdminResolver interface {
	IsInstanceAdmin(ctx context.Context, userID int64) (bool, error)
}

// WithInstanceAdmin loads the caller's instance-admin flag once (via the
// resolver) and stashes it in the context so both RequireInstanceAdmin and the
// layout can read it without re-querying. Runs inside the authenticated group,
// after RequireAuth. A resolver error is treated as not-admin (fail closed).
func WithInstanceAdmin(res InstanceAdminResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isAdmin := false
			if uid := UserID(r.Context()); uid != 0 {
				if ok, err := res.IsInstanceAdmin(r.Context(), uid); err == nil {
					isAdmin = ok
				}
			}
			ctx := context.WithValue(r.Context(), instanceAdminKey, isAdmin)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireInstanceAdmin gates global infrastructure (cluster nodes, host-wide
// monitoring) to instance operators. It reads the flag stashed by
// WithInstanceAdmin; a non-operator gets 404 so the route is indistinguishable
// from a non-existent one (no capability disclosure).
func RequireInstanceAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !IsInstanceAdmin(r.Context()) {
				slog.Warn("instance-admin denied", "user_id", UserID(r.Context()),
					"org_id", OrgID(r.Context()), "method", r.Method, "path", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole requires a role no lower than min, otherwise 403.
func RequireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !RoleOf(r.Context()).AtLeast(min) {
				slog.Warn("rbac denied", "user_id", UserID(r.Context()), "org_id", OrgID(r.Context()),
					"have", RoleOf(r.Context()).String(), "required", min.String(), "method", r.Method, "path", r.URL.Path)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserID extracts the user id from the context (0 if absent).
func UserID(ctx context.Context) int64 {
	if v, ok := ctx.Value(userIDKey).(int64); ok {
		return v
	}
	return 0
}

// OrgID extracts the active organization id from the context (0 if absent).
func OrgID(ctx context.Context) int64 {
	if v, ok := ctx.Value(orgIDKey).(int64); ok {
		return v
	}
	return 0
}

// RoleOf extracts the user's role in the active org from the context (RoleMember by default).
func RoleOf(ctx context.Context) Role {
	if v, ok := ctx.Value(roleKey).(Role); ok {
		return v
	}
	return RoleMember
}

// IsInstanceAdmin reports whether the caller is an instance-level operator
// (flag stashed by WithInstanceAdmin; false when absent).
func IsInstanceAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(instanceAdminKey).(bool)
	return v
}
