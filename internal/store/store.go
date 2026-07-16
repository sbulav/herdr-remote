// Package store provides the durable, metadata-only SQLite boundary. Its API
// intentionally cannot accept prompt, terminal, input text, or key values.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/pushendpoint"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var ErrDuplicate = errors.New("duplicate action ID")
var ErrUnavailable = errors.New("audit store unavailable")
var ErrEnrollmentUsed = errors.New("enrollment token expired or used")
var ErrAlreadyComplete = errors.New("action is missing or already complete")
var ErrHostLimit = errors.New("host limit reached")
var ErrInvalidDisplayName = errors.New("invalid enrollment display name")
var ErrPushLimit = errors.New("push subscription limit reached")
var ErrInvalidPushSubscription = errors.New("invalid push subscription")
var ErrPushOwnership = errors.New("push subscription is not owned by operator")
var ErrPushConflict = errors.New("push replacement payload conflicts with existing subscription")
var ErrPushMissing = errors.New("push replacement source is missing")

const MaxPushSubscriptionsPerOperator = 20
const MaxPushReplacementSources = 16
const MaxPushDeletionEndpoints = MaxPushReplacementSources + 1

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	migrations := [][]string{{
		`CREATE TABLE action_tombstones(action_id TEXT PRIMARY KEY, first_seen_at TEXT NOT NULL)`,
		`CREATE TABLE actions(action_id TEXT PRIMARY KEY REFERENCES action_tombstones(action_id), operation_type TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL, host_id TEXT NOT NULL, instance_id TEXT NOT NULL, terminal_id TEXT NOT NULL, requested_at TEXT NOT NULL, received_at TEXT, completed_at TEXT, status TEXT, code TEXT, connection_id TEXT, connector_version TEXT, protocol_version INTEGER, text_bytes INTEGER NOT NULL DEFAULT 0, key_count INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX actions_completed_idx ON actions(completed_at)`,
		`CREATE TABLE enrollments(token_hash TEXT PRIMARY KEY, host_id TEXT NOT NULL, display_name TEXT NOT NULL, expires_at TEXT NOT NULL, used_at TEXT)`,
		`CREATE TABLE connector_certificates(serial TEXT PRIMARY KEY, host_id TEXT NOT NULL, fingerprint TEXT NOT NULL UNIQUE, not_before TEXT NOT NULL, not_after TEXT NOT NULL, revoked_at TEXT)`,
		`CREATE INDEX connector_cert_host_idx ON connector_certificates(host_id)`,
		`CREATE TABLE push_subscriptions(id INTEGER PRIMARY KEY AUTOINCREMENT, subject TEXT NOT NULL, endpoint TEXT NOT NULL UNIQUE, p256dh TEXT NOT NULL, auth TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, host_id TEXT, occurred_at TEXT NOT NULL, code TEXT)`,
	}, {`CREATE INDEX actions_incomplete_idx ON actions(completed_at,operation_type)`}, {
		`ALTER TABLE push_subscriptions ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX push_subscriptions_subject_idx ON push_subscriptions(subject)`,
		`CREATE INDEX push_subscriptions_updated_idx ON push_subscriptions(updated_at)`,
		`ALTER TABLE audit_events ADD COLUMN event_id TEXT`,
		`CREATE UNIQUE INDEX audit_events_event_id_idx ON audit_events(event_id) WHERE event_id IS NOT NULL`,
		`CREATE INDEX audit_events_occurred_idx ON audit_events(occurred_at)`,
	}}
	for index, statements := range migrations {
		version := index + 1
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&exists)
		if err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			if !ok {
				_ = tx.Rollback()
			}
		}()
		for _, q := range statements {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d: %w", version, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at)VALUES(?,?)`, version, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		ok = true
	}
	return nil
}

type ActionIntent struct {
	ActionID, OperationType, Issuer, Subject, HostID, InstanceID, TerminalID, ConnectionID, ConnectorVersion string
	ProtocolVersion, TextBytes, KeyCount                                                                     int
	RequestedAt                                                                                              time.Time
}
type ActionStatus struct {
	ActionID, OperationType, Status string
	Code                            *string
	RequestedAt, CompletedAt        time.Time
}

func (s *Store) BeginAction(ctx context.Context, a ActionIntent) error {
	if a.RequestedAt.IsZero() {
		a.RequestedAt = s.now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer tx.Rollback()
	ts := a.RequestedAt.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO action_tombstones(action_id,first_seen_at) VALUES(?,?)`, a.ActionID, ts); err != nil {
		if isConstraint(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO actions(action_id,operation_type,issuer,subject,host_id,instance_id,terminal_id,requested_at,connection_id,connector_version,protocol_version,text_bytes,key_count) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ActionID, a.OperationType, a.Issuer, a.Subject, a.HostID, a.InstanceID, a.TerminalID, ts, a.ConnectionID, a.ConnectorVersion, a.ProtocolVersion, a.TextBytes, a.KeyCount)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}
func (s *Store) Received(ctx context.Context, id string, t time.Time) error {
	if t.IsZero() {
		t = s.now().UTC()
	}
	r, err := s.db.ExecContext(ctx, `UPDATE actions SET received_at=COALESCE(received_at,?) WHERE action_id=?`, t.Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return errors.New("action is missing")
	}
	return nil
}
func (s *Store) Complete(ctx context.Context, id, status string, code *string, t time.Time) error {
	if t.IsZero() {
		t = s.now().UTC()
	}
	r, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,code=?,completed_at=? WHERE action_id=? AND completed_at IS NULL`, status, code, t.Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrAlreadyComplete
	}
	return nil
}
func (s *Store) Action(ctx context.Context, id string) (ActionStatus, error) {
	var a ActionStatus
	var requested, completed string
	err := s.db.QueryRowContext(ctx, `SELECT action_id,operation_type,status,code,requested_at,completed_at FROM actions WHERE action_id=? AND completed_at IS NOT NULL`, id).Scan(&a.ActionID, &a.OperationType, &a.Status, &a.Code, &requested, &completed)
	if err != nil {
		return a, err
	}
	a.RequestedAt, _ = time.Parse(time.RFC3339Nano, requested)
	a.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed)
	return a, nil
}
func (s *Store) Retain(ctx context.Context, retention time.Duration) error {
	cut := s.now().UTC().Add(-retention).Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions WHERE completed_at IS NOT NULL AND completed_at < ?`, cut)
	return err
}
func (s *Store) RecoverIncomplete(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT action_id,operation_type,host_id FROM actions WHERE completed_at IS NULL ORDER BY requested_at`)
	if err != nil {
		return 0, err
	}
	type item struct{ id, op, host string }
	var items []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.op, &v.host); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, v)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	for _, v := range items {
		status, code := "failed", "CONNECTION_LOST"
		if v.op != "agent.read" {
			status, code = "unknown", "OUTCOME_UNKNOWN"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=?,code=?,completed_at=? WHERE action_id=? AND completed_at IS NULL`, status, code, now, v.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(kind,host_id,occurred_at,code)VALUES('action.recovered',?,?,?)`, nullable(v.host), now, code); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return len(items), nil
}

type Enrollment struct {
	HostID, DisplayName string
	ExpiresAt           time.Time
}
type KnownHost struct{ HostID, DisplayName string }

func (s *Store) KnownHosts(ctx context.Context) ([]KnownHost, error) {
	rows, err := s.db.QueryContext(ctx, `WITH host_ids AS (SELECT host_id FROM enrollments WHERE used_at IS NOT NULL UNION SELECT host_id FROM connector_certificates WHERE revoked_at IS NULL) SELECT host_id,COALESCE((SELECT MAX(display_name) FROM enrollments e WHERE e.host_id=host_ids.host_id AND e.used_at IS NOT NULL),host_id) FROM host_ids ORDER BY host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hosts []KnownHost
	for rows.Next() {
		if len(hosts) >= 10 {
			return nil, ErrHostLimit
		}
		var host KnownHost
		if err := rows.Scan(&host.HostID, &host.DisplayName); err != nil {
			return nil, err
		}
		if !validDisplayName(host.DisplayName) {
			return nil, ErrInvalidDisplayName
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func (s *Store) CreateEnrollment(ctx context.Context, tokenHash, hostID, name string, expires time.Time) error {
	if !validDisplayName(name) {
		return ErrInvalidDisplayName
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `DELETE FROM enrollments WHERE used_at IS NULL AND expires_at<=?`, nowText); err != nil {
		return err
	}
	var hosts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT host_id FROM enrollments WHERE used_at IS NOT NULL OR expires_at>? UNION SELECT host_id FROM connector_certificates WHERE revoked_at IS NULL)`, nowText).Scan(&hosts); err != nil {
		return err
	}
	if hosts >= 10 {
		return ErrHostLimit
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO enrollments(token_hash,host_id,display_name,expires_at)VALUES(?,?,?,?)`, tokenHash, hostID, name, expires.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(kind,host_id,occurred_at)VALUES('enrollment.created',?,?)`, hostID, nowText); err != nil {
		return err
	}
	return tx.Commit()
}
func validDisplayName(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 80 {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}
func (s *Store) EnrollmentForToken(ctx context.Context, tokenHash string) (Enrollment, error) {
	var e Enrollment
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT host_id,display_name,expires_at FROM enrollments WHERE token_hash=? AND used_at IS NULL`, tokenHash).Scan(&e.HostID, &e.DisplayName, &expires)
	if err != nil {
		return e, ErrEnrollmentUsed
	}
	e.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	if !s.now().UTC().Before(e.ExpiresAt) {
		return e, ErrEnrollmentUsed
	}
	return e, nil
}

type Certificate struct {
	Serial, HostID, Fingerprint string
	NotBefore, NotAfter         time.Time
	Revoked                     bool
}

func (s *Store) AddCertificate(ctx context.Context, c Certificate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connector_certificates(serial,host_id,fingerprint,not_before,not_after)VALUES(?,?,?,?,?)`, c.Serial, c.HostID, c.Fingerprint, c.NotBefore.UTC().Format(time.RFC3339Nano), c.NotAfter.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) CommitEnrollmentCertificate(ctx context.Context, tokenHash string, c Certificate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var host, expires string
	if err = tx.QueryRowContext(ctx, `SELECT host_id,expires_at FROM enrollments WHERE token_hash=? AND used_at IS NULL`, tokenHash).Scan(&host, &expires); err != nil {
		return ErrEnrollmentUsed
	}
	expiry, _ := time.Parse(time.RFC3339Nano, expires)
	now := s.now().UTC()
	if !now.Before(expiry) || host != c.HostID {
		return ErrEnrollmentUsed
	}
	if err = insertCertificate(ctx, tx, c); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE enrollments SET used_at=? WHERE token_hash=? AND used_at IS NULL`, now.Format(time.RFC3339Nano), tokenHash)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrEnrollmentUsed
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(kind,host_id,occurred_at)VALUES('enrollment.completed',?,?)`, host, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AddRotatedCertificate(ctx context.Context, c Certificate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = insertCertificate(ctx, tx, c); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(kind,host_id,occurred_at)VALUES('certificate.rotated',?,?)`, c.HostID, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertCertificate(ctx context.Context, exec sqlExecer, c Certificate) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO connector_certificates(serial,host_id,fingerprint,not_before,not_after)VALUES(?,?,?,?,?)`, c.Serial, c.HostID, c.Fingerprint, c.NotBefore.UTC().Format(time.RFC3339Nano), c.NotAfter.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) CertificateByFingerprint(ctx context.Context, fp string) (Certificate, error) {
	var c Certificate
	var nb, na string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT serial,host_id,fingerprint,not_before,not_after,revoked_at FROM connector_certificates WHERE fingerprint=?`, fp).Scan(&c.Serial, &c.HostID, &c.Fingerprint, &nb, &na, &revoked)
	if err != nil {
		return c, err
	}
	c.NotBefore, _ = time.Parse(time.RFC3339Nano, nb)
	c.NotAfter, _ = time.Parse(time.RFC3339Nano, na)
	c.Revoked = revoked.Valid
	return c, nil
}
func (s *Store) RevokeHost(ctx context.Context, host string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE connector_certificates SET revoked_at=? WHERE host_id=? AND revoked_at IS NULL`, now, host); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(kind,host_id,occurred_at)VALUES('certificate.revoked',?,?)`, host, now); err != nil {
		return err
	}
	return tx.Commit()
}

type PushSubscription struct{ Subject, Endpoint, P256DH, Auth, UserAgent string }

func (s *Store) UpsertPush(ctx context.Context, p PushSubscription) error {
	if !validPush(p) {
		return ErrInvalidPushSubscription
	}
	nowTime := s.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE updated_at<?`, nowTime.Add(-90*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE subject=? AND endpoint<>?`, p.Subject, p.Endpoint).Scan(&count); err != nil {
		return err
	}
	if count >= MaxPushSubscriptionsPerOperator {
		return ErrPushLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO push_subscriptions(subject,endpoint,p256dh,auth,user_agent,created_at,updated_at)VALUES(?,?,?,?,?,?,?) ON CONFLICT(endpoint) DO UPDATE SET subject=excluded.subject,p256dh=excluded.p256dh,auth=excluded.auth,user_agent=excluded.user_agent,updated_at=excluded.updated_at`, p.Subject, p.Endpoint, p.P256DH, p.Auth, p.UserAgent, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ReplacePush(ctx context.Context, subject string, sourceEndpoints []string, replacement PushSubscription) error {
	if replacement.Subject != subject || !validPush(replacement) || !validPushReplacementSources(sourceEndpoints) {
		return ErrInvalidPushSubscription
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing PushSubscription
	targetExists := true
	err = tx.QueryRowContext(ctx, `SELECT subject,endpoint,p256dh,auth,user_agent FROM push_subscriptions WHERE endpoint=?`, replacement.Endpoint).Scan(&existing.Subject, &existing.Endpoint, &existing.P256DH, &existing.Auth, &existing.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		targetExists = false
	} else if err != nil {
		return err
	}
	if targetExists {
		if existing.Subject != subject {
			return ErrPushOwnership
		}
		if existing.P256DH != replacement.P256DH || existing.Auth != replacement.Auth {
			return ErrPushConflict
		}
	}
	ownedSources := make([]string, 0, len(sourceEndpoints))
	for _, endpoint := range sourceEndpoints {
		if endpoint == replacement.Endpoint {
			continue
		}
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT subject FROM push_subscriptions WHERE endpoint=?`, endpoint).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && owner == subject {
			ownedSources = append(ownedSources, endpoint)
		}
	}
	if !targetExists && len(ownedSources) == 0 {
		return ErrPushMissing
	}
	for _, endpoint := range ownedSources {
		if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE subject=? AND endpoint=?`, subject, endpoint); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE subject=? AND endpoint<>?`, subject, replacement.Endpoint).Scan(&count); err != nil {
		return err
	}
	if count >= MaxPushSubscriptionsPerOperator {
		return ErrPushLimit
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO push_subscriptions(subject,endpoint,p256dh,auth,user_agent,created_at,updated_at)VALUES(?,?,?,?,?,?,?) ON CONFLICT(endpoint) DO UPDATE SET p256dh=excluded.p256dh,auth=excluded.auth,user_agent=excluded.user_agent,updated_at=excluded.updated_at`, replacement.Subject, replacement.Endpoint, replacement.P256DH, replacement.Auth, replacement.UserAgent, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func validPushReplacementSources(sourceEndpoints []string) bool {
	if len(sourceEndpoints) < 1 || len(sourceEndpoints) > MaxPushReplacementSources {
		return false
	}
	seen := make(map[string]struct{}, len(sourceEndpoints))
	for _, endpoint := range sourceEndpoints {
		if len(endpoint) > 2048 {
			return false
		}
		if _, err := pushendpoint.Parse(endpoint); err != nil {
			return false
		}
		if _, exists := seen[endpoint]; exists {
			return false
		}
		seen[endpoint] = struct{}{}
	}
	return true
}
func (s *Store) DeletePush(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint=?`, endpoint)
	return err
}
func (s *Store) DeletePushForSubject(ctx context.Context, subject, endpoint string) error {
	return s.DeletePushEndpointsForSubject(ctx, subject, []string{endpoint})
}
func (s *Store) DeletePushEndpointsForSubject(ctx context.Context, subject string, endpoints []string) error {
	if subject == "" || len(endpoints) < 1 || len(endpoints) > MaxPushDeletionEndpoints {
		return ErrInvalidPushSubscription
	}
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if len(endpoint) > 2048 {
			return ErrInvalidPushSubscription
		}
		if _, err := pushendpoint.Parse(endpoint); err != nil {
			return ErrInvalidPushSubscription
		}
		if _, exists := seen[endpoint]; exists {
			return ErrInvalidPushSubscription
		}
		seen[endpoint] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, endpoint := range endpoints {
		if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE subject=? AND endpoint=?`, subject, endpoint); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) HasPushSubscription(ctx context.Context, subject, endpoint string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM push_subscriptions WHERE subject=? AND endpoint=?)`, subject, endpoint).Scan(&exists)
	return exists, err
}
func (s *Store) PushSubscriptions(ctx context.Context, subject string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject,endpoint,p256dh,auth,user_agent FROM push_subscriptions WHERE subject=? ORDER BY updated_at DESC LIMIT ?`, subject, MaxPushSubscriptionsPerOperator)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Subject, &p.Endpoint, &p.P256DH, &p.Auth, &p.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func validPush(p PushSubscription) bool {
	if _, err := pushendpoint.Parse(p.Endpoint); err != nil || len(p.P256DH) < 1 || len(p.P256DH) > 512 || len(p.Auth) < 1 || len(p.Auth) > 256 || len(p.Subject) < 1 || len(p.Subject) > 256 || len(p.UserAgent) > 256 {
		return false
	}
	for _, value := range []string{p.Endpoint, p.P256DH, p.Auth, p.Subject, p.UserAgent} {
		if strings.IndexFunc(value, func(r rune) bool { return r <= 0x1f || (r >= 0x7f && r <= 0x9f) }) >= 0 {
			return false
		}
	}
	return true
}

func (s *Store) AuditEvent(ctx context.Context, kind, host string, code *string) error {
	id, err := NewAuditEventID()
	if err != nil {
		return err
	}
	return s.AuditEventAt(ctx, id, kind, host, code, s.now().UTC())
}
func NewAuditEventID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
func (s *Store) AuditEventAt(ctx context.Context, eventID, kind, host string, code *string, occurred time.Time) error {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO audit_events(event_id,kind,host_id,occurred_at,code)VALUES(?,?,?,?,?)`, eventID, kind, nullable(host), occurred.UTC().Format(time.RFC3339Nano), code)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		var existingKind string
		if err := s.db.QueryRowContext(ctx, `SELECT kind FROM audit_events WHERE event_id=?`, eventID).Scan(&existingKind); err != nil {
			return err
		}
		if existingKind != kind {
			return errors.New("audit event identity collision")
		}
	}
	return nil
}
func (s *Store) CountAuditEvents(ctx context.Context, kind string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE kind=?`, kind).Scan(&count)
	return count, err
}
func (s *Store) AuditEventOccurrences(ctx context.Context, kind string) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT occurred_at FROM audit_events WHERE kind=? ORDER BY occurred_at`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		value, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func isConstraint(err error) bool {
	return err != nil && (contains(err.Error(), "constraint") || contains(err.Error(), "UNIQUE"))
}
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
