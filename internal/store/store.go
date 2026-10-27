// Package store is the laavud database: who can sign in and where. Nothing else.
//
// Users, passkeys, sign-in sessions, single-use links, agent access and push
// subscriptions. No tmux sessions or agent files here: if the database is lost,
// passkeys have to be registered again, but the agents' work is unaffected.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	handle BLOB NOT NULL UNIQUE,
	role TEXT NOT NULL CHECK (role IN ('admin','member')),
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id BLOB PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	data BLOB NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS login_sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	user_agent TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS invites (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER,            -- NULL: new user; otherwise a new device
	role TEXT NOT NULL DEFAULT 'member',
	name TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS access (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	agent TEXT NOT NULL,
	PRIMARY KEY (user_id, agent)
);
CREATE TABLE IF NOT EXISTS push_subs (
	endpoint TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	data TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kv (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

// Open opens or creates the database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, Now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// User is an app user. It has no counterpart in the system.
type User struct {
	ID      int64
	Name    string
	Handle  []byte // WebAuthn user handle, random
	Role    string
	Created time.Time
}

func (u *User) Admin() bool { return u.Role == "admin" }

// RandomToken returns a random token for links and cookies.
func RandomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewHandle returns a random WebAuthn user handle.
func NewHandle() []byte {
	h := make([]byte, 32)
	rand.Read(h)
	return h
}

func (s *Store) CreateUser(name, role string) (*User, error) {
	return s.CreateUserWithHandle(name, role, NewHandle())
}

func (s *Store) CreateUserWithHandle(name, role string, h []byte) (*User, error) {
	now := s.Now()
	res, err := s.db.Exec(`INSERT INTO users(name, handle, role, created) VALUES (?,?,?,?)`, name, h, role, now.Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Name: name, Handle: h, Role: role, Created: now}, nil
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var created int64
	if err := row.Scan(&u.ID, &u.Name, &u.Handle, &u.Role, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Created = time.Unix(created, 0)
	return &u, nil
}

func (s *Store) UserByID(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id, name, handle, role, created FROM users WHERE id=?`, id))
}

func (s *Store) UserByHandle(h []byte) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id, name, handle, role, created FROM users WHERE handle=?`, h))
}

func (s *Store) UserByName(name string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id, name, handle, role, created FROM users WHERE name=?`, name))
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name, handle, role, created FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var c int64
		if err := rows.Scan(&u.ID, &u.Name, &u.Handle, &u.Role, &c); err != nil {
			return nil, err
		}
		u.Created = time.Unix(c, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
	return err
}

// Credential is a stored passkey (a serialized webauthn.Credential).
type Credential struct {
	ID       []byte
	UserID   int64
	Data     []byte
	Label    string
	Created  time.Time
	LastUsed time.Time
}

func (s *Store) AddCredential(c Credential) error {
	_, err := s.db.Exec(`INSERT INTO credentials(id, user_id, data, label, created) VALUES (?,?,?,?,?)`,
		c.ID, c.UserID, c.Data, c.Label, s.Now().Unix())
	return err
}

func (s *Store) UpdateCredential(id, data []byte) error {
	_, err := s.db.Exec(`UPDATE credentials SET data=?, last_used=? WHERE id=?`, data, s.Now().Unix(), id)
	return err
}

func (s *Store) Credentials(userID int64) ([]Credential, error) {
	rows, err := s.db.Query(`SELECT id, user_id, data, label, created, last_used FROM credentials WHERE user_id=? ORDER BY created`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		var cr, lu int64
		if err := rows.Scan(&c.ID, &c.UserID, &c.Data, &c.Label, &cr, &lu); err != nil {
			return nil, err
		}
		c.Created, c.LastUsed = time.Unix(cr, 0), time.Unix(lu, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCredential(userID int64, id []byte) error {
	_, err := s.db.Exec(`DELETE FROM credentials WHERE user_id=? AND id=?`, userID, id)
	return err
}

// DeleteAllAuth deletes all passkeys and sign-in sessions, for reset-auth.
func (s *Store) DeleteAllAuth() error {
	_, err := s.db.Exec(`DELETE FROM credentials; DELETE FROM login_sessions; DELETE FROM invites;`)
	return err
}

// Invite is a single-use passkey registration link.
type Invite struct {
	UserID *int64 // nil means a new user
	Role   string
	Name   string
}

// CreateInvite returns the link token; only its hash is stored.
func (s *Store) CreateInvite(inv Invite, ttl time.Duration) (string, error) {
	tok := RandomToken()
	now := s.Now()
	_, err := s.db.Exec(`INSERT INTO invites(token_hash, user_id, role, name, created, expires) VALUES (?,?,?,?,?,?)`,
		hash(tok), inv.UserID, inv.Role, inv.Name, now.Unix(), now.Add(ttl).Unix())
	return tok, err
}

// PeekInvite validates a link without consuming it.
func (s *Store) PeekInvite(tok string) (*Invite, error) {
	var inv Invite
	var uid sql.NullInt64
	var exp int64
	err := s.db.QueryRow(`SELECT user_id, role, name, expires FROM invites WHERE token_hash=?`, hash(tok)).Scan(&uid, &inv.Role, &inv.Name, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp < s.Now().Unix()) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if uid.Valid {
		inv.UserID = &uid.Int64
	}
	return &inv, nil
}

// ConsumeInvite consumes a link.
func (s *Store) ConsumeInvite(tok string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE token_hash=? AND expires>=?`, hash(tok), s.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateLogin creates a sign-in session and returns the cookie token.
func (s *Store) CreateLogin(userID int64, ttl time.Duration, ua string) (string, error) {
	tok := RandomToken()
	now := s.Now()
	_, err := s.db.Exec(`INSERT INTO login_sessions(token_hash, user_id, created, expires, user_agent) VALUES (?,?,?,?,?)`,
		hash(tok), userID, now.Unix(), now.Add(ttl).Unix(), ua)
	return tok, err
}

// LoginUser returns the user for a cookie token.
func (s *Store) LoginUser(tok string) (*User, error) {
	var uid, exp int64
	err := s.db.QueryRow(`SELECT user_id, expires FROM login_sessions WHERE token_hash=?`, hash(tok)).Scan(&uid, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp < s.Now().Unix()) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.UserByID(uid)
}

func (s *Store) DeleteLogin(tok string) error {
	_, err := s.db.Exec(`DELETE FROM login_sessions WHERE token_hash=?`, hash(tok))
	return err
}

func (s *Store) CleanupExpired() {
	now := s.Now().Unix()
	s.db.Exec(`DELETE FROM login_sessions WHERE expires<?`, now)
	s.db.Exec(`DELETE FROM invites WHERE expires<?`, now)
}

func (s *Store) GrantAccess(userID int64, agent string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO access(user_id, agent) VALUES (?,?)`, userID, agent)
	return err
}

func (s *Store) RevokeAccess(userID int64, agent string) error {
	_, err := s.db.Exec(`DELETE FROM access WHERE user_id=? AND agent=?`, userID, agent)
	return err
}

func (s *Store) HasAccess(userID int64, agent string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM access WHERE user_id=? AND agent=?`, userID, agent).Scan(&n)
	return n > 0, err
}

// UsersWithAccess returns users with access to the agent. An admin sees all agents.
func (s *Store) UsersWithAccess(agent string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM users WHERE role='admin' UNION SELECT user_id FROM access WHERE agent=?`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) AddPushSub(userID int64, endpoint, data string) error {
	// Someone else's subscription cannot be reassigned: only your own is updated.
	res, err := s.db.Exec(`INSERT INTO push_subs(endpoint, user_id, data, created) VALUES (?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET data=excluded.data WHERE push_subs.user_id=excluded.user_id`, endpoint, userID, data, s.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("subscription belongs to another user")
	}
	return nil
}

// DeleteUserPushSub deletes a subscription only if it belongs to this user.
func (s *Store) DeleteUserPushSub(userID int64, endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subs WHERE endpoint=? AND user_id=?`, endpoint, userID)
	return err
}

func (s *Store) DeletePushSub(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subs WHERE endpoint=?`, endpoint)
	return err
}

// PushSubs returns the user's subscriptions (JSON PushSubscription).
func (s *Store) PushSubs(userID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT data FROM push_subs WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Get(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) Set(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv(k, v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}
