// Package store keeps Forkbox state in SQLite (WAL mode): sandboxes now,
// network slots and memory layers in later tasks. Every resource a sandbox
// owns is recorded here before it is created, so a reaper can find and
// remove it if its owner dies.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: the daemon stays a static binary
)

// Store is the state database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// migrations run in order; user_version records how many have run.
var migrations = []string{
	`CREATE TABLE sandboxes (
		id         TEXT PRIMARY KEY,
		template   TEXT NOT NULL,
		parent     TEXT NOT NULL DEFAULT '',
		state      TEXT NOT NULL,
		dir        TEXT NOT NULL,
		jail       TEXT NOT NULL,
		cgroup     TEXT NOT NULL,
		vsock      TEXT NOT NULL,
		pid        INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		ttl_s      INTEGER NOT NULL DEFAULT 0,
		error      TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX sandboxes_state ON sandboxes(state);`,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this binary (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Sandbox states.
const (
	StateCreating   = "creating"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateDestroying = "destroying"
	StateFailed     = "failed"
)

// Sandbox is one sandbox record.
type Sandbox struct {
	ID        string
	Template  string // template id
	Parent    string // sandbox it was forked from (Task 8)
	State     string
	Dir       string // <data>/sandboxes/<id>: disk and per-sandbox files
	Jail      string // the VM's jail directory
	Cgroup    string
	Vsock     string // host path of the vsock socket
	Pid       int
	CreatedAt time.Time
	UpdatedAt time.Time
	TTL       time.Duration // 0: no expiry
	Error     string        // why it failed, for StateFailed
}

// Expired reports whether the sandbox's TTL has run out at now.
func (sb *Sandbox) Expired(now time.Time) bool {
	return sb.TTL > 0 && now.After(sb.CreatedAt.Add(sb.TTL))
}

// ErrNotFound means no such sandbox.
var ErrNotFound = errors.New("sandbox not found")

// ErrExists means the id is taken.
var ErrExists = errors.New("sandbox already exists")

// InsertSandbox records a new sandbox.
func (s *Store) InsertSandbox(ctx context.Context, sb *Sandbox) error {
	now := time.Now()
	if sb.CreatedAt.IsZero() {
		sb.CreatedAt = now
	}
	sb.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sandboxes
		(id, template, parent, state, dir, jail, cgroup, vsock, pid, created_at, updated_at, ttl_s, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Template, sb.Parent, sb.State, sb.Dir, sb.Jail, sb.Cgroup, sb.Vsock, sb.Pid,
		sb.CreatedAt.UnixNano(), sb.UpdatedAt.UnixNano(), int64(sb.TTL/time.Second), sb.Error)
	if err != nil && isConstraint(err) {
		return fmt.Errorf("%w: %s", ErrExists, sb.ID)
	}
	return err
}

// SetState updates a sandbox's state, pid and error.
func (s *Store) SetState(ctx context.Context, id, state string, pid int, errText string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET state = ?, pid = ?, error = ?, updated_at = ? WHERE id = ?`,
		state, pid, errText, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	return mustAffect(res, id)
}

// DeleteSandbox removes a record.
func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, id)
}

// GetSandbox returns one record.
func (s *Store) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, selectSandbox+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	list, err := scanSandboxes(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return list[0], nil
}

// ListSandboxes returns every record, oldest first.
func (s *Store) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, selectSandbox+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanSandboxes(rows)
}

const selectSandbox = `SELECT id, template, parent, state, dir, jail, cgroup, vsock, pid, created_at, updated_at, ttl_s, error FROM sandboxes`

func scanSandboxes(rows *sql.Rows) ([]*Sandbox, error) {
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		var sb Sandbox
		var created, updated, ttl int64
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.Parent, &sb.State, &sb.Dir, &sb.Jail, &sb.Cgroup,
			&sb.Vsock, &sb.Pid, &created, &updated, &ttl, &sb.Error); err != nil {
			return nil, err
		}
		sb.CreatedAt, sb.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		sb.TTL = time.Duration(ttl) * time.Second
		out = append(out, &sb)
	}
	return out, rows.Err()
}

func mustAffect(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}
