package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
)

// SessionTTL — срок жизни сессии.
const SessionTTL = 7 * 24 * time.Hour

// Service инкапсулирует операции аутентификации поверх sqlc-запросов.
type Service struct {
	q *db.Queries
}

func NewService(q *db.Queries) *Service { return &Service{q: q} }

// Authenticate проверяет email+пароль и при успехе создаёт сессию, возвращая токен.
func (s *Service) Authenticate(ctx context.Context, email, password string) (string, error) {
	u, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		return "", ErrInvalidCredentials
	}
	if !CheckPassword(u.PasswordHash, password) {
		return "", ErrInvalidCredentials
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	err = s.q.CreateSession(ctx, db.CreateSessionParams{
		Token:     token,
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(SessionTTL),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Validate возвращает userID, если токен валиден и не истёк.
func (s *Service) Validate(ctx context.Context, token string) (int64, bool) {
	sess, err := s.q.GetSession(ctx, token)
	if err != nil {
		return 0, false
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.q.DeleteSession(ctx, token)
		return 0, false
	}
	return sess.UserID, true
}

// Logout удаляет сессию.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.q.DeleteSession(ctx, token)
}

// SeedAdmin создаёт администратора и дефолтную организацию, если их ещё нет.
// Идемпотентно: повторный старт не дублирует и не перезатирает.
func (s *Service) SeedAdmin(ctx context.Context, email, password string) error {
	u, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		hash, herr := HashPassword(password)
		if herr != nil {
			return herr
		}
		u, err = s.q.CreateUser(ctx, db.CreateUserParams{Email: email, PasswordHash: hash})
	}
	if err != nil {
		return err
	}

	// Дефолтная организация: если у админа ещё нет членства нигде — создаём.
	orgs, err := s.q.ListOrganizationsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	if len(orgs) > 0 {
		return nil
	}
	o, err := s.q.CreateOrganization(ctx, db.CreateOrganizationParams{
		Name: "Default", Slug: "default", OwnerID: u.ID,
	})
	if err != nil {
		return err
	}
	_, err = s.q.CreateMember(ctx, db.CreateMemberParams{
		OrganizationID: o.ID, UserID: u.ID, Role: "owner",
	})
	return err
}

// ErrInvalidCredentials — неверный email или пароль.
var ErrInvalidCredentials = errors.New("invalid credentials")
