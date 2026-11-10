// Package store persists jobs, the run ledger, revisions and templates in
// SQLite. The CLI and the daemon share one database file; every
// read-modify-write goes through an IMMEDIATE transaction so the two never
// clobber each other.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sernik77/CR0N0S/internal/model"
)

// ErrNotFound is returned when a job or template does not exist.
var ErrNotFound = errors.New("not found")

// ErrReasonRequired is returned when an audited edit has no reason.
var ErrReasonRequired = errors.New("a reason is required to edit a job (--reason)")

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	Path string
}

// Audit describes why a job is being changed.
type Audit struct {
	Action     string // register, modify, reschedule, cancel, arm, disarm, pause, resume
	Reason     string
	ReasonDesc string
}

// reasonRequired lists actions that change a job's definition.
var reasonRequired = map[string]bool{"modify": true, "reschedule": true}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	uuid          TEXT PRIMARY KEY,
	title         TEXT NOT NULL,
	path          TEXT NOT NULL,
	command       TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	next_run      INTEGER,
	registered_at INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	spec          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_next_run ON jobs(status, next_run);
CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	job_uuid    TEXT NOT NULL,
	trigger     TEXT NOT NULL,
	started_at  INTEGER NOT NULL,
	finished_at INTEGER NOT NULL,
	status      TEXT NOT NULL,
	exit_code   INTEGER NOT NULL,
	output      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_job ON runs(job_uuid, started_at);
CREATE TABLE IF NOT EXISTS revisions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	job_uuid    TEXT NOT NULL,
	at          INTEGER NOT NULL,
	action      TEXT NOT NULL,
	reason      TEXT NOT NULL,
	reason_desc TEXT NOT NULL DEFAULT '',
	actor       TEXT NOT NULL,
	before      TEXT NOT NULL DEFAULT '',
	after       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS revisions_job ON revisions(job_uuid, at);
CREATE TABLE IF NOT EXISTS templates (
	name        TEXT PRIMARY KEY,
	description TEXT NOT NULL DEFAULT '',
	spec        TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
`

// DefaultPath returns the database path: $CR0_DB, the system path for root
// (or when system is true), otherwise $XDG_DATA_HOME/cr0/cr0.db.
func DefaultPath(system bool) string {
	if p := os.Getenv("CR0_DB"); p != "" {
		return p
	}
	if system || os.Geteuid() == 0 {
		return "/var/lib/cr0/cr0.db"
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "cr0", "cr0.db")
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db, Path: path}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func unix(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func actor() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return fmt.Sprintf("uid:%d", os.Getuid())
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func writeJob(e execer, j *model.Job) error {
	spec, err := json.Marshal(j)
	if err != nil {
		return err
	}
	cmd := j.Command
	if cmd == "" {
		cmd = j.Script
	}
	_, err = e.Exec(`INSERT INTO jobs(uuid,title,path,command,status,next_run,registered_at,updated_at,spec)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uuid) DO UPDATE SET title=excluded.title, path=excluded.path, command=excluded.command,
			status=excluded.status, next_run=excluded.next_run, updated_at=excluded.updated_at, spec=excluded.spec`,
		j.UUID, j.Title, j.Path, cmd, j.Status, unix(j.NextRun), j.RegisteredAt.Unix(), time.Now().Unix(), string(spec))
	return err
}

func writeRevision(e execer, uuid string, a Audit, before, after *model.Job) error {
	enc := func(j *model.Job) string {
		if j == nil {
			return ""
		}
		b, _ := json.Marshal(j)
		return string(b)
	}
	reason := a.Reason
	if reason == "" {
		reason = "-"
	}
	_, err := e.Exec(`INSERT INTO revisions(job_uuid,at,action,reason,reason_desc,actor,before,after) VALUES(?,?,?,?,?,?,?,?)`,
		uuid, time.Now().Unix(), a.Action, reason, a.ReasonDesc, actor(), enc(before), enc(after))
	return err
}

// Register inserts a new job, assigning a UUID and registration time.
func (s *Store) Register(j *model.Job) error {
	if j.UUID == "" {
		j.UUID = uuid.NewString()
	}
	if j.RegisteredAt.IsZero() {
		j.RegisteredAt = time.Now().Truncate(time.Second)
	}
	if j.Status == "" {
		j.Status = model.StatusActive
	}
	if err := j.Validate(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeJob(tx, j); err != nil {
		return err
	}
	if err := writeRevision(tx, j.UUID, Audit{Action: "register", Reason: "registered"}, nil, j); err != nil {
		return err
	}
	return tx.Commit()
}

func scanJob(row interface{ Scan(...any) error }) (*model.Job, error) {
	var spec string
	if err := row.Scan(&spec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var j model.Job
	if err := json.Unmarshal([]byte(spec), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Resolve finds a job by full UUID or unique UUID prefix (at least 4 chars).
func (s *Store) Resolve(id string) (string, error) {
	if len(id) < 4 {
		return "", fmt.Errorf("job id %q is too short (min 4 chars)", id)
	}
	rows, err := s.db.Query(`SELECT uuid FROM jobs WHERE uuid LIKE ? LIMIT 2`, strings.ReplaceAll(id, "%", "")+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return "", err
		}
		ids = append(ids, u)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("job %q: %w", id, ErrNotFound)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("job id %q is ambiguous", id)
}

// Get loads a job by id or prefix.
func (s *Store) Get(id string) (*model.Job, error) {
	u, err := s.Resolve(id)
	if err != nil {
		return nil, err
	}
	return scanJob(s.db.QueryRow(`SELECT spec FROM jobs WHERE uuid=?`, u))
}

// Update atomically loads a job, applies fn and saves it, recording a
// revision. Actions that change the definition require a reason.
func (s *Store) Update(id string, a Audit, fn func(*model.Job) error) (*model.Job, error) {
	if reasonRequired[a.Action] && strings.TrimSpace(a.Reason) == "" {
		return nil, ErrReasonRequired
	}
	return s.mutate(id, &a, fn)
}

// UpdateState atomically changes runtime state (next run, last status...)
// without writing a revision. Used by the daemon.
func (s *Store) UpdateState(id string, fn func(*model.Job) error) (*model.Job, error) {
	return s.mutate(id, nil, fn)
}

func (s *Store) mutate(id string, a *Audit, fn func(*model.Job) error) (*model.Job, error) {
	u, err := s.Resolve(id)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := scanJob(tx.QueryRow(`SELECT spec FROM jobs WHERE uuid=?`, u))
	if err != nil {
		return nil, err
	}
	after := before.Clone()
	if err := fn(after); err != nil {
		return nil, err
	}
	after.UUID, after.RegisteredAt = before.UUID, before.RegisteredAt
	if err := after.Validate(); err != nil {
		return nil, err
	}
	if err := writeJob(tx, after); err != nil {
		return nil, err
	}
	if a != nil {
		if err := writeRevision(tx, u, *a, before, after); err != nil {
			return nil, err
		}
	}
	return after, tx.Commit()
}

// List returns all jobs ordered by next run (unscheduled last).
func (s *Store) List() ([]*model.Job, error) {
	rows, err := s.db.Query(`SELECT spec FROM jobs ORDER BY next_run IS NULL, next_run, registered_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Active returns jobs with status active.
func (s *Store) Active() ([]*model.Job, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, j := range all {
		if j.Status == model.StatusActive {
			out = append(out, j)
		}
	}
	return out, nil
}

// AddRun appends a run to the ledger.
func (s *Store) AddRun(r *model.Run) error {
	res, err := s.db.Exec(`INSERT INTO runs(job_uuid,trigger,started_at,finished_at,status,exit_code,output,error) VALUES(?,?,?,?,?,?,?,?)`,
		r.JobUUID, r.Trigger, r.StartedAt.UnixMilli(), r.FinishedAt.UnixMilli(), r.Status, r.ExitCode, r.Output, r.Error)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// Runs returns the ledger, newest first. An empty uuid returns all jobs' runs.
func (s *Store) Runs(uuid string, limit int) ([]model.Run, error) {
	q := `SELECT id,job_uuid,trigger,started_at,finished_at,status,exit_code,output,error FROM runs`
	var args []any
	if uuid != "" {
		q += ` WHERE job_uuid=?`
		args = append(args, uuid)
	}
	q += ` ORDER BY started_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		var r model.Run
		var st, fin int64
		if err := rows.Scan(&r.ID, &r.JobUUID, &r.Trigger, &st, &fin, &r.Status, &r.ExitCode, &r.Output, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = time.UnixMilli(st), time.UnixMilli(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revisions returns the audit trail, newest first.
func (s *Store) Revisions(uuid string, limit int) ([]model.Revision, error) {
	q := `SELECT id,job_uuid,at,action,reason,reason_desc,actor,before,after FROM revisions`
	var args []any
	if uuid != "" {
		q += ` WHERE job_uuid=?`
		args = append(args, uuid)
	}
	q += ` ORDER BY at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Revision
	for rows.Next() {
		var r model.Revision
		var at int64
		if err := rows.Scan(&r.ID, &r.JobUUID, &at, &r.Action, &r.Reason, &r.ReasonDesc, &r.Actor, &r.Before, &r.After); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveTemplate creates or replaces a template.
func (s *Store) SaveTemplate(t *model.Template) error {
	if t.Name == "" {
		return fmt.Errorf("template name is required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	spec, err := json.Marshal(t.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO templates(name,description,spec,created_at) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET description=excluded.description, spec=excluded.spec`,
		t.Name, t.Description, string(spec), t.CreatedAt.Unix())
	return err
}

// Template loads a template by name.
func (s *Store) Template(name string) (*model.Template, error) {
	var t model.Template
	var spec string
	var at int64
	err := s.db.QueryRow(`SELECT name,description,spec,created_at FROM templates WHERE name=?`, name).Scan(&t.Name, &t.Description, &spec, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("template %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(at, 0)
	return &t, json.Unmarshal([]byte(spec), &t.Spec)
}

// Templates lists all templates by name.
func (s *Store) Templates() ([]model.Template, error) {
	rows, err := s.db.Query(`SELECT name FROM templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	out := make([]model.Template, 0, len(names))
	for _, n := range names {
		t, err := s.Template(n)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

// DeleteTemplate removes a template.
func (s *Store) DeleteTemplate(name string) error {
	res, err := s.db.Exec(`DELETE FROM templates WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("template %q: %w", name, ErrNotFound)
	}
	return nil
}
