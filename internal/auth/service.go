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

// SeedAdmin создаёт администратора, если пользователя с таким email ещё нет.
func (s *Service) SeedAdmin(ctx context.Context, email, password string) error {
	_, err := s.q.GetUserByEmail(ctx, email)
	if err == nil {
		return nil // уже существует — не трогаем
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.q.CreateUser(ctx, db.CreateUserParams{Email: email, PasswordHash: hash})
	return err
}

// ErrInvalidCredentials — неверный email или пароль.
var ErrInvalidCredentials = errors.New("invalid credentials")
