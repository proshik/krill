package auth

import (
	"context"
	"net/http"
)

// CookieName — имя cookie сессии.
const CookieName = "krill_session"

type ctxKey int

const userIDKey ctxKey = 0

// Validator — то, что нужно middleware для проверки токена (реализует *Service).
type Validator interface {
	Validate(ctx context.Context, token string) (int64, bool)
}

// RequireAuth пропускает запрос дальше только при валидной сессии, иначе → /login.
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

// UserID достаёт id пользователя из контекста (0, если нет).
func UserID(ctx context.Context) int64 {
	if v, ok := ctx.Value(userIDKey).(int64); ok {
		return v
	}
	return 0
}
