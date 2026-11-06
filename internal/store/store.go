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
	`ALTER TABLE sandboxes ADD COLUMN template_name TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE net_slots (
		slot       INTEGER PRIMARY KEY,
		sandbox_id TEXT NOT NULL UNIQUE REFERENCES sandboxes(id) ON DELETE CASCADE
	);
	CREATE TABLE port_forwards (
		sandbox_id TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
		guest_port INTEGER NOT NULL,
		host_port  INTEGER NOT NULL UNIQUE,
		PRIMARY KEY (sandbox_id, guest_port)
	);`,
	`CREATE TABLE layers (
		id             TEXT PRIMARY KEY,
		parent         TEXT NOT NULL DEFAULT '',
		kind           TEXT NOT NULL,
		dir            TEXT NOT NULL,
		mem            TEXT NOT NULL,
		dirty_bytes    INTEGER NOT NULL DEFAULT 0,
		source_sandbox TEXT NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL
	);
	ALTER TABLE sandboxes ADD COLUMN base_layer TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE checkpoints (
		id            TEXT PRIMARY KEY,
		sandbox_id    TEXT NOT NULL,
		name          TEXT NOT NULL,
		layer         TEXT NOT NULL,
		dir           TEXT NOT NULL,
		template      TEXT NOT NULL,
		template_name TEXT NOT NULL,
		created_at    INTEGER NOT NULL,
		UNIQUE (sandbox_id, name)
	);`,
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
	ID           string
	Template     string // template id
	TemplateName string
	Parent       string // sandbox it was forked from (Task 8)
	State        string
	Dir          string // <data>/sandboxes/<id>: disk and per-sandbox files
	Jail         string // the VM's jail directory
	Cgroup       string
	Vsock        string // host path of the vsock socket
	Pid          int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	TTL          time.Duration // 0: no expiry
	Error        string        // why it failed, for StateFailed
	// BaseLayer is the memory layer the VM's next diff is relative to: the
	// template id for a sandbox restored from a template, else a layer id.
	BaseLayer string
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
		(id, template, template_name, parent, state, dir, jail, cgroup, vsock, pid, created_at, updated_at, ttl_s, error, base_layer)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Template, sb.TemplateName, sb.Parent, sb.State, sb.Dir, sb.Jail, sb.Cgroup, sb.Vsock, sb.Pid,
		sb.CreatedAt.UnixNano(), sb.UpdatedAt.UnixNano(), int64(sb.TTL/time.Second), sb.Error, sb.BaseLayer)
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

const selectSandbox = `SELECT id, template, template_name, parent, state, dir, jail, cgroup, vsock, pid, created_at, updated_at, ttl_s, error, base_layer FROM sandboxes`

func scanSandboxes(rows *sql.Rows) ([]*Sandbox, error) {
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		var sb Sandbox
		var created, updated, ttl int64
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.TemplateName, &sb.Parent, &sb.State, &sb.Dir, &sb.Jail, &sb.Cgroup,
			&sb.Vsock, &sb.Pid, &created, &updated, &ttl, &sb.Error, &sb.BaseLayer); err != nil {
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

// ErrNoSlot means the address pool is exhausted.
var ErrNoSlot = errors.New("no free network slot")

// AllocSlot gives a sandbox the lowest free network slot below max. A
// sandbox that already has one gets it back.
func (s *Store) AllocSlot(ctx context.Context, sandboxID string, max int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM net_slots WHERE sandbox_id = ?`, sandboxID).Scan(&slot)
	if err == nil {
		return slot, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// The first gap in the used slots, or 0 if slot 0 is free.
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM net_slots WHERE slot = 0) THEN 0 ELSE
		(SELECT MIN(a.slot) + 1 FROM net_slots a WHERE NOT EXISTS (SELECT 1 FROM net_slots b WHERE b.slot = a.slot + 1)) END`).Scan(&slot)
	if err != nil {
		return 0, err
	}
	if slot >= max {
		return 0, fmt.Errorf("%w: all %d slots are in use", ErrNoSlot, max)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO net_slots (slot, sandbox_id) VALUES (?, ?)`, slot, sandboxID); err != nil {
		return 0, err
	}
	return slot, tx.Commit()
}

// Slot returns a sandbox's network slot, or ErrNotFound.
func (s *Store) Slot(ctx context.Context, sandboxID string) (int, error) {
	var slot int
	err := s.db.QueryRowContext(ctx, `SELECT slot FROM net_slots WHERE sandbox_id = ?`, sandboxID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no network slot for %s", ErrNotFound, sandboxID)
	}
	return slot, err
}

// FreeSlot releases a sandbox's slot (deleting the sandbox also does).
func (s *Store) FreeSlot(ctx context.Context, sandboxID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM net_slots WHERE sandbox_id = ?`, sandboxID)
	return err
}

// PortForward exposes a guest TCP port on a host port.
type PortForward struct {
	SandboxID string
	GuestPort int
	HostPort  int
}

// AddPortForward records a forward. A host port can serve one forward.
func (s *Store) AddPortForward(ctx context.Context, pf PortForward) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO port_forwards (sandbox_id, guest_port, host_port) VALUES (?, ?, ?)`,
		pf.SandboxID, pf.GuestPort, pf.HostPort)
	if isConstraint(err) {
		return fmt.Errorf("%w: port forward %d -> %d", ErrExists, pf.HostPort, pf.GuestPort)
	}
	return err
}

// PortForwards lists forwards; an empty id lists all of them.
func (s *Store) PortForwards(ctx context.Context, sandboxID string) ([]PortForward, error) {
	q, args := `SELECT sandbox_id, guest_port, host_port FROM port_forwards ORDER BY sandbox_id, guest_port`, []any{}
	if sandboxID != "" {
		q, args = `SELECT sandbox_id, guest_port, host_port FROM port_forwards WHERE sandbox_id = ? ORDER BY guest_port`, []any{sandboxID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortForward
	for rows.Next() {
		var pf PortForward
		if err := rows.Scan(&pf.SandboxID, &pf.GuestPort, &pf.HostPort); err != nil {
			return nil, err
		}
		out = append(out, pf)
	}
	return out, rows.Err()
}

// Layer kinds.
const (
	LayerFork = "fork" // a parent's memory at a fork: its base plus a diff
)

// Layer is a memory layer: a full memory file (and the VM state taken with
// it) that children of a fork are restored from, and the base of their next
// diffs. Parent is the layer it was merged from (a template id or another
// layer), which gives the lineage.
type Layer struct {
	ID            string
	Parent        string
	Kind          string
	Dir           string // vmstate, snapshot.json
	Mem           string // the memory file
	DirtyBytes    int64  // pages the diff added over Parent
	SourceSandbox string // the sandbox that was forked
	CreatedAt     time.Time
}

// InsertLayer records a layer.
func (s *Store) InsertLayer(ctx context.Context, l *Layer) error {
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO layers (id, parent, kind, dir, mem, dirty_bytes, source_sandbox, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, l.ID, l.Parent, l.Kind, l.Dir, l.Mem, l.DirtyBytes, l.SourceSandbox, l.CreatedAt.UnixNano())
	if isConstraint(err) {
		return fmt.Errorf("%w: layer %s", ErrExists, l.ID)
	}
	return err
}

// GetLayer returns a layer.
func (s *Store) GetLayer(ctx context.Context, id string) (*Layer, error) {
	var l Layer
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, parent, kind, dir, mem, dirty_bytes, source_sandbox, created_at FROM layers WHERE id = ?`, id).
		Scan(&l.ID, &l.Parent, &l.Kind, &l.Dir, &l.Mem, &l.DirtyBytes, &l.SourceSandbox, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: layer %s", ErrNotFound, id)
	}
	l.CreatedAt = time.Unix(0, created)
	return &l, err
}

// Lineage returns a layer and its ancestors, newest first, stopping at the
// first id that is not a layer (the template).
func (s *Store) Lineage(ctx context.Context, id string) ([]*Layer, error) {
	var out []*Layer
	for id != "" && len(out) < 10000 {
		l, err := s.GetLayer(ctx, id)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, l)
		id = l.Parent
	}
	return out, nil
}

// SetBaseLayer records the layer a sandbox's next diff is relative to.
func (s *Store) SetBaseLayer(ctx context.Context, sandboxID, layer string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET base_layer = ?, updated_at = ? WHERE id = ?`, layer, time.Now().UnixNano(), sandboxID)
	if err != nil {
		return err
	}
	return mustAffect(res, sandboxID)
}

// Checkpoint is a named point a sandbox can be rewound to: a memory layer
// plus a disk copy (in Dir). It outlives its sandbox until deleted, so it
// can still be restored as a new sandbox.
type Checkpoint struct {
	ID           string
	SandboxID    string
	Name         string
	Layer        string
	Dir          string
	Template     string
	TemplateName string
	CreatedAt    time.Time
}

// InsertCheckpoint records a checkpoint; names are unique per sandbox.
func (s *Store) InsertCheckpoint(ctx context.Context, c *Checkpoint) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoints (id, sandbox_id, name, layer, dir, template, template_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.SandboxID, c.Name, c.Layer, c.Dir, c.Template, c.TemplateName, c.CreatedAt.UnixNano())
	if isConstraint(err) {
		return fmt.Errorf("%w: checkpoint %q of %s", ErrExists, c.Name, c.SandboxID)
	}
	return err
}

const selectCheckpoint = `SELECT id, sandbox_id, name, layer, dir, template, template_name, created_at FROM checkpoints`

func scanCheckpoints(rows *sql.Rows) ([]*Checkpoint, error) {
	defer rows.Close()
	var out []*Checkpoint
	for rows.Next() {
		var c Checkpoint
		var created int64
		if err := rows.Scan(&c.ID, &c.SandboxID, &c.Name, &c.Layer, &c.Dir, &c.Template, &c.TemplateName, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(0, created)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetCheckpoint finds a checkpoint by id, or by sandbox and name.
func (s *Store) GetCheckpoint(ctx context.Context, sandboxID, nameOrID string) (*Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, selectCheckpoint+` WHERE id = ? OR (sandbox_id = ? AND name = ?)`, nameOrID, sandboxID, nameOrID)
	if err != nil {
		return nil, err
	}
	list, err := scanCheckpoints(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: checkpoint %q of %s", ErrNotFound, nameOrID, sandboxID)
	}
	return list[0], nil
}

// ListCheckpoints returns a sandbox's checkpoints, oldest first; an empty
// id lists all of them.
func (s *Store) ListCheckpoints(ctx context.Context, sandboxID string) ([]*Checkpoint, error) {
	q, args := selectCheckpoint+` ORDER BY created_at`, []any{}
	if sandboxID != "" {
		q, args = selectCheckpoint+` WHERE sandbox_id = ? ORDER BY created_at`, []any{sandboxID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanCheckpoints(rows)
}

// DeleteCheckpoint removes a checkpoint record.
func (s *Store) DeleteCheckpoint(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, id)
}

// UnreferencedLayers are layers no sandbox has as its base and no
// checkpoint uses: safe to delete. A running VM that still maps a deleted
// layer's memory file keeps working; the kernel frees it when the VM exits.
func (s *Store) UnreferencedLayers(ctx context.Context) ([]*Layer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM layers l
		WHERE NOT EXISTS (SELECT 1 FROM sandboxes s WHERE s.base_layer = l.id)
		  AND NOT EXISTS (SELECT 1 FROM checkpoints c WHERE c.layer = l.id)`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Layer
	for _, id := range ids {
		l, err := s.GetLayer(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// LayerRefs counts the sandboxes and checkpoints that reference a layer.
func (s *Store) LayerRefs(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sandboxes WHERE base_layer = ?) + (SELECT COUNT(*) FROM checkpoints WHERE layer = ?)`, id, id).Scan(&n)
	return n, err
}

// DeleteLayer removes a layer record.
func (s *Store) DeleteLayer(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM layers WHERE id = ?`, id)
	return err
}
