package agent

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ExecutionStart is the row written when a command/process begins
// (minus stdout/stderr/stdin, which are never persisted).
type ExecutionStart struct {
	ExecutionID     string
	ProcessID       string
	DeviceID        string
	Mode            string // "read" | "write" | "process"
	Command         string
	Cwd             string
	StartedAt       time.Time
	ClientContextID string // best-effort; empty means NULL
	ClientSubject   string
}

// Execution is one row of the executions table.
type Execution struct {
	ExecutionID     string
	ProcessID       string
	DeviceID        string
	Mode            string
	Command         string
	Cwd             string
	StartedAt       time.Time
	EndedAt         *time.Time
	ExitCode        *int
	ClientContextID string
	ClientSubject   string
}

const schema = `
CREATE TABLE IF NOT EXISTS executions (
  execution_id      TEXT PRIMARY KEY,
  process_id        TEXT,
  device_id         TEXT NOT NULL,
  mode              TEXT NOT NULL,
  command           TEXT NOT NULL,
  cwd               TEXT,
  started_at        TEXT NOT NULL,
  ended_at          TEXT,
  exit_code         INTEGER,
  client_context_id TEXT,
  client_subject    TEXT
);
CREATE INDEX IF NOT EXISTS idx_executions_started_at ON executions(started_at);
CREATE INDEX IF NOT EXISTS idx_executions_process_id ON executions(process_id);
CREATE INDEX IF NOT EXISTS idx_executions_client_context_id ON executions(client_context_id);
`

type HistoryStore struct {
	db *sql.DB
}

// historyTimeLayout formats timestamps with a fixed-width, always-present
// 9-digit fractional second, unlike time.RFC3339Nano (which omits the
// fraction entirely when it is zero). List/PurgeOlderThan compare these
// strings lexicographically, which only matches chronological order
// when every value has the same width; time.Parse(time.RFC3339Nano, ...)
// still reads this format back correctly.
const historyTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func OpenHistoryStore(path string) (*HistoryStore, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection avoids SQLITE_BUSY between writer goroutines
	// entirely: SQLite's busy_timeout is per-connection, so a
	// pool-generated connection beyond the first would not reliably
	// inherit it, and History writes are short enough that they don't
	// need write concurrency in the first place.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &HistoryStore{db: db}, nil
}

func (s *HistoryStore) Close() error { return s.db.Close() }

func (s *HistoryStore) RecordStart(e ExecutionStart) error {
	_, err := s.db.Exec(
		`INSERT INTO executions
		 (execution_id, process_id, device_id, mode, command, cwd, started_at, client_context_id, client_subject)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ExecutionID, e.ProcessID, e.DeviceID, e.Mode, e.Command, e.Cwd,
		e.StartedAt.UTC().Format(historyTimeLayout),
		nullIfEmpty(e.ClientContextID), nullIfEmpty(e.ClientSubject),
	)
	return err
}

const (
	// recordEndMaxRetries and recordEndRetryDelay bound RecordEnd's
	// SQLITE_BUSY retry: busy_timeout (set via DSN in OpenHistoryStore)
	// already blocks for up to 5s inside a single db.Exec call, so the
	// extra retries only need to cover the rare case where that single
	// wait still wasn't enough — 3 retries * 100ms keeps the worst-case
	// added latency under a second rather than compounding into tens
	// of seconds.
	recordEndMaxRetries = 3
	recordEndRetryDelay = 100 * time.Millisecond
)

// isSQLiteBusy reports whether err is SQLITE_BUSY (result code 5), the
// only error RecordEnd retries — anything else (constraint violation,
// I/O error, corruption) is returned to the caller immediately instead
// of being silently retried away.
func isSQLiteBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code() == sqlite3.SQLITE_BUSY
}

func (s *HistoryStore) RecordEnd(executionID string, endedAt time.Time, exitCode *int) error {
	var err error
	for attempt := 0; attempt <= recordEndMaxRetries; attempt++ {
		_, err = s.db.Exec(
			`UPDATE executions SET ended_at = ?, exit_code = ? WHERE execution_id = ?`,
			endedAt.UTC().Format(historyTimeLayout), exitCode, executionID,
		)
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		if attempt < recordEndMaxRetries {
			time.Sleep(recordEndRetryDelay)
		}
	}
	return err
}

func (s *HistoryStore) Get(executionID string) (*Execution, error) {
	row := s.db.QueryRow(
		`SELECT execution_id, process_id, device_id, mode, command, cwd, started_at, ended_at, exit_code, client_context_id, client_subject
		 FROM executions WHERE execution_id = ?`, executionID)
	return scanExecution(row)
}

func (s *HistoryStore) List(limit int) ([]Execution, error) {
	rows, err := s.db.Query(
		`SELECT execution_id, process_id, device_id, mode, command, cwd, started_at, ended_at, exit_code, client_context_id, client_subject
		 FROM executions ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Execution
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanExecution(row scanner) (*Execution, error) {
	var (
		e                              Execution
		startedAt                      string
		endedAt, clientCtx, clientSubj sql.NullString
		exitCode                       sql.NullInt64
	)
	if err := row.Scan(&e.ExecutionID, &e.ProcessID, &e.DeviceID, &e.Mode, &e.Command, &e.Cwd,
		&startedAt, &endedAt, &exitCode, &clientCtx, &clientSubj); err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return nil, err
	}
	e.StartedAt = t
	if endedAt.Valid {
		et, err := time.Parse(time.RFC3339Nano, endedAt.String)
		if err != nil {
			return nil, err
		}
		e.EndedAt = &et
	}
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		e.ExitCode = &ec
	}
	e.ClientContextID = clientCtx.String
	e.ClientSubject = clientSubj.String
	return &e, nil
}

// PurgeOlderThan deletes executions that started before cutoff.
func (s *HistoryStore) PurgeOlderThan(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM executions WHERE started_at < ?`, cutoff.UTC().Format(historyTimeLayout))
	return err
}

// StartGC periodically purges executions older than retention. Call
// once per Agent process.
func (s *HistoryStore) StartGC(ctx context.Context, retention, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.PurgeOlderThan(time.Now().Add(-retention)); err != nil {
					log.Printf("agent: history retention purge failed: %v", err)
				}
			}
		}
	}()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
