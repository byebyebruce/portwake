// Package store persists configuration, events and hourly stats in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"dcpentry/internal/config"
	"dcpentry/internal/events"
)

const (
	EventRetention = 30 * 24 * time.Hour
	StatsRetention = 365 * 24 * time.Hour
)

const schema = `
CREATE TABLE IF NOT EXISTS projects (
	pos  INTEGER NOT NULL,
	name TEXT PRIMARY KEY,
	data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      INTEGER NOT NULL,
	project TEXT NOT NULL DEFAULT '',
	port    INTEGER NOT NULL DEFAULT 0,
	type    TEXT NOT NULL,
	data    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS events_project_ts ON events(project, ts);
CREATE TABLE IF NOT EXISTS stats_hourly (
	hour       INTEGER NOT NULL,
	project    TEXT NOT NULL,
	port       INTEGER NOT NULL,
	conns      INTEGER NOT NULL DEFAULT 0,
	requests   INTEGER NOT NULL DEFAULT 0,
	bytes_in   INTEGER NOT NULL DEFAULT 0,
	bytes_out  INTEGER NOT NULL DEFAULT 0,
	starts     INTEGER NOT NULL DEFAULT 0,
	start_fail INTEGER NOT NULL DEFAULT 0,
	start_ms   INTEGER NOT NULL DEFAULT 0,
	runtime_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, project, port)
);
`

type Store struct {
	db *sql.DB

	errMu   sync.Mutex
	dropped int
	lastErr error
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) LoadProjects() ([]config.Project, error) {
	rows, err := s.db.Query(`SELECT data FROM projects ORDER BY pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.Project{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var p config.Project
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplaceProjects atomically replaces the whole configuration.
func (s *Store) ReplaceProjects(projects []config.Project) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM projects`); err != nil {
		return err
	}
	for i, p := range projects {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO projects(pos, name, data) VALUES (?, ?, ?)`, i, p.Name, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Persisted reports whether an event type is stored.
func Persisted(typ string) bool { return typ != events.State }

// Consume writes events from ch until it is closed.
func (s *Store) Consume(ch <-chan events.Event) {
	for e := range ch {
		batch := []events.Event{e}
	drain:
		for len(batch) < 500 {
			select {
			case e, ok := <-ch:
				if !ok {
					break drain
				}
				batch = append(batch, e)
			default:
				break drain
			}
		}
		s.write(batch)
	}
}

// write stores a batch in one transaction. An event that fails to insert is
// skipped (via a savepoint) instead of losing the batch; errors are counted
// and the last one is kept for WriteErrors.
func (s *Store) write(batch []events.Event) {
	tx, err := s.db.Begin()
	if err != nil {
		s.noteErr(err, len(batch))
		return
	}
	defer tx.Rollback()
	for _, e := range batch {
		if !Persisted(e.Type) {
			continue
		}
		if err := writeOne(tx, e); err != nil {
			s.noteErr(err, 1)
		}
	}
	if err := tx.Commit(); err != nil {
		s.noteErr(err, len(batch))
	}
}

func writeOne(tx *sql.Tx, e events.Event) error {
	if _, err := tx.Exec(`SAVEPOINT ev`); err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	_, err := tx.Exec(`INSERT INTO events(ts, project, port, type, data) VALUES (?, ?, ?, ?, ?)`,
		e.Time.UnixMilli(), e.Project, e.Port, e.Type, string(b))
	if err == nil {
		err = addStats(tx, e)
	}
	if err != nil {
		tx.Exec(`ROLLBACK TO ev`)
	}
	tx.Exec(`RELEASE ev`)
	return err
}

func (s *Store) noteErr(err error, n int) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	s.dropped += n
	s.lastErr = err
}

// WriteErrors reports how many events failed to persist and the last error.
func (s *Store) WriteErrors() (int, error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.dropped, s.lastErr
}

func addStats(tx *sql.Tx, e events.Event) error {
	if e.Project == "" {
		return nil
	}
	var col string
	var val int64 = 1
	port := e.Port
	var extra []string
	switch e.Type {
	case events.ConnOpen:
		col = "conns"
	case events.ConnClose:
		col, val = "bytes_in", e.BytesIn
		extra = []string{fmt.Sprintf("bytes_out = bytes_out + %d", e.BytesOut)}
	case events.HTTPRequest:
		col = "requests"
		extra = []string{fmt.Sprintf("bytes_in = bytes_in + %d", e.BytesIn), fmt.Sprintf("bytes_out = bytes_out + %d", e.BytesOut)}
	case events.StartOK:
		col, port = "starts", 0
		extra = []string{fmt.Sprintf("start_ms = start_ms + %d", e.DurationMs)}
	case events.StartFail:
		col, port = "start_fail", 0
	case events.StopDone:
		col, val, port = "runtime_ms", e.DurationMs, 0
	default:
		return nil
	}
	hour := e.Time.Truncate(time.Hour).Unix()
	set := append([]string{fmt.Sprintf("%s = %s + %d", col, col, val)}, extra...)
	_, err := tx.Exec(`INSERT INTO stats_hourly(hour, project, port) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, hour, e.Project, port)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE stats_hourly SET `+strings.Join(set, ", ")+` WHERE hour = ? AND project = ? AND port = ?`, hour, e.Project, port)
	return err
}

// EventQuery filters events; zero values mean "any".
type EventQuery struct {
	Project  string
	Types    []string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int
}

// Events returns matching events, newest first.
func (s *Store) Events(q EventQuery) ([]events.Event, error) {
	where := []string{"1=1"}
	var args []any
	if q.Project != "" {
		where = append(where, "project = ?")
		args = append(args, q.Project)
	}
	if len(q.Types) > 0 {
		where = append(where, "type IN (?"+strings.Repeat(",?", len(q.Types)-1)+")")
		for _, t := range q.Types {
			args = append(args, t)
		}
	}
	if !q.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, q.Until.UnixMilli())
	}
	if q.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, q.BeforeID)
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	args = append(args, q.Limit)
	rows, err := s.db.Query(`SELECT id, data FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []events.Event{}
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var e events.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			continue
		}
		e.ID = id
		out = append(out, e)
	}
	return out, rows.Err()
}

// Totals are aggregated counters.
type Totals struct {
	Conns      int64 `json:"conns"`
	Requests   int64 `json:"requests"`
	BytesIn    int64 `json:"bytes_in"`
	BytesOut   int64 `json:"bytes_out"`
	Starts     int64 `json:"starts"`
	StartFails int64 `json:"start_fails"`
	StartMs    int64 `json:"start_ms"`
	RuntimeMs  int64 `json:"runtime_ms"`
}

// ProjectStats is the summary of one project over a time range.
type ProjectStats struct {
	Project string `json:"project"`
	Totals
	AvgStartMs int64        `json:"avg_start_ms"`
	Series     []HourBucket `json:"series"`
	Ports      []PortTotals `json:"ports"`
}

// PortTotals is the traffic of one entry port over the range.
type PortTotals struct {
	Port     int   `json:"port"`
	Conns    int64 `json:"conns"`
	Requests int64 `json:"requests"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

type HourBucket struct {
	Hour int64 `json:"hour"` // unix seconds
	Totals
}

// Stats summarizes [from, to) per project, with an hourly series.
func (s *Store) Stats(project string, from, to time.Time) ([]ProjectStats, error) {
	q := `SELECT project, hour, SUM(conns), SUM(requests), SUM(bytes_in), SUM(bytes_out), SUM(starts), SUM(start_fail), SUM(start_ms), SUM(runtime_ms)
		FROM stats_hourly WHERE hour >= ? AND hour < ?`
	args := []any{from.Truncate(time.Hour).Unix(), to.Unix()}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` GROUP BY project, hour ORDER BY project, hour`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectStats{}
	idx := map[string]int{}
	for rows.Next() {
		var name string
		var b HourBucket
		if err := rows.Scan(&name, &b.Hour, &b.Conns, &b.Requests, &b.BytesIn, &b.BytesOut, &b.Starts, &b.StartFails, &b.StartMs, &b.RuntimeMs); err != nil {
			return nil, err
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, ProjectStats{Project: name, Series: []HourBucket{}, Ports: []PortTotals{}})
		}
		ps := &out[i]
		ps.Series = append(ps.Series, b)
		t := &ps.Totals
		t.Conns += b.Conns
		t.Requests += b.Requests
		t.BytesIn += b.BytesIn
		t.BytesOut += b.BytesOut
		t.Starts += b.Starts
		t.StartFails += b.StartFails
		t.StartMs += b.StartMs
		t.RuntimeMs += b.RuntimeMs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Starts > 0 {
			out[i].AvgStartMs = out[i].StartMs / out[i].Starts
		}
	}

	pq := `SELECT project, port, SUM(conns), SUM(requests), SUM(bytes_in), SUM(bytes_out)
		FROM stats_hourly WHERE port > 0 AND hour >= ? AND hour < ?`
	if project != "" {
		pq += ` AND project = ?`
	}
	pq += ` GROUP BY project, port ORDER BY project, port`
	prows, err := s.db.Query(pq, args...)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var name string
		var pt PortTotals
		if err := prows.Scan(&name, &pt.Port, &pt.Conns, &pt.Requests, &pt.BytesIn, &pt.BytesOut); err != nil {
			return nil, err
		}
		if i, ok := idx[name]; ok {
			out[i].Ports = append(out[i].Ports, pt)
		}
	}
	return out, prows.Err()
}

// Prune deletes data past its retention.
func (s *Store) Prune(now time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, now.Add(-EventRetention).UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM stats_hourly WHERE hour < ?`, now.Add(-StatsRetention).Unix())
	return err
}
