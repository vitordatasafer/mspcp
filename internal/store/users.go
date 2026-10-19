package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// User é um usuário do login local. O username é o "subject" dos vínculos.
type User struct {
	ID           uuid.UUID  `json:"id"`
	Username     string     `json:"username"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	MustChange   bool       `json:"must_change_password"`
	TOTPEnabled  bool       `json:"totp_enabled"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	PasswordHash string     `json:"-"`
	TOTPSecret   *string    `json:"-"`
	TOTPPending  *string    `json:"-"`
}

const userCols = `id, username, name, email, must_change_password, totp_enabled, disabled, created_at, last_login_at, password_hash, totp_secret, totp_pending`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.MustChange, &u.TOTPEnabled, &u.Disabled, &u.CreatedAt,
		&u.LastLoginAt, &u.PasswordHash, &u.TOTPSecret, &u.TOTPPending)
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, username, name, email, hash string, mustChange bool) (User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, `
		INSERT INTO users (username, name, email, password_hash, must_change_password) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+userCols, username, name, email, hash, mustChange))
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	return u, err
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username = $1`, username))
	return u, notFound(err)
}

func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	return u, notFound(err)
}

// SetPassword troca a senha. mustChange obriga a trocar no próximo acesso.
func (s *Store) SetPassword(ctx context.Context, id uuid.UUID, hash string, mustChange bool) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = $3 WHERE id = $1`, id, hash, mustChange)
	return err
}

func (s *Store) SetTOTPPending(ctx context.Context, id uuid.UUID, secret string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_pending = $2 WHERE id = $1`, id, secret)
	return err
}

func (s *Store) EnableTOTP(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		UPDATE users SET totp_secret = totp_pending, totp_pending = NULL, totp_enabled = true
		WHERE id = $1 AND totp_pending IS NOT NULL`, id)
	return err
}

func (s *Store) DisableTOTP(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_enabled = false WHERE id = $1`, id)
	return err
}

func (s *Store) MarkLogin(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}

// ---------- sessões ----------

// Sessão expira por inatividade (idle) e, de qualquer forma, depois de
// SessionMaxAge desde o login.
const (
	SessionIdle   = 8 * time.Hour
	SessionMaxAge = 7 * 24 * time.Hour
)

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID uuid.UUID, ip, ua string) error {
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at, remote_ip, user_agent) VALUES ($1, $2, now() + $3::interval, $4, $5)`,
		tokenHash, userID, SessionIdle.String(), ip, ua)
	return err
}

// SessionUser devolve o dono de uma sessão válida e renova o prazo de
// inatividade (no máximo uma escrita por minuto por sessão).
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, `
		SELECT u.`+userColsU+` FROM sessions x JOIN users u ON u.id = x.user_id
		WHERE x.token_hash = $1 AND x.expires_at > now() AND x.created_at > now() - $2::interval AND NOT u.disabled`,
		tokenHash, SessionMaxAge.String()))
	if err != nil {
		return u, notFound(err)
	}
	_, err = s.db.Exec(ctx, `
		UPDATE sessions SET last_seen = now(), expires_at = now() + $2::interval
		WHERE token_hash = $1 AND last_seen < now() - interval '1 minute'`, tokenHash, SessionIdle.String())
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// DeleteUserSessions encerra as sessões do usuário, menos `keep` (se dado).
func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID, keep []byte) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND ($2::bytea IS NULL OR token_hash <> $2)`, userID, keep)
	return err
}

// PurgeSessions apaga sessões vencidas (chamado de tempos em tempos).
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR created_at < now() - $1::interval`, SessionMaxAge.String())
	return err
}

const userColsU = `id, u.username, u.name, u.email, u.must_change_password, u.totp_enabled, u.disabled, u.created_at, u.last_login_at, u.password_hash, u.totp_secret, u.totp_pending`

// ---------- vínculos com dados do usuário ----------

func (s *Store) GetMembership(ctx context.Context, id uuid.UUID) (MembershipView, uuid.UUID, error) {
	var m MembershipView
	var tenant uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT id, subject, project_id, role, created_at, tenant_id FROM memberships WHERE id = $1`, id).
		Scan(&m.ID, &m.Subject, &m.ProjectID, &m.Role, &m.CreatedAt, &tenant)
	return m, tenant, notFound(err)
}

func (s *Store) DeleteMembership(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM memberships WHERE id = $1`, id)
	return err
}

// MembershipsOfSubject lista todos os vínculos de um usuário (para checar
// se quem pede um reset de senha administra todos eles).
func (s *Store) MembershipsOfSubject(ctx context.Context, subject string) ([]Membership, error) {
	rows, err := s.db.Query(ctx, `
		SELECT m.id, m.subject, m.tenant_id, t.path, t.kind, m.project_id, m.role
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id WHERE m.subject = $1`, subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) {
		var m Membership
		err := r.Scan(&m.ID, &m.Subject, &m.TenantID, &m.TenantPath, &m.TenantKind, &m.ProjectID, &m.Role)
		return m, err
	})
}
