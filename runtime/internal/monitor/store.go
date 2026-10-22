package monitor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const eventLimit = 512

type Store struct {
	db    *sql.DB
	Epoch string
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	_, err = db.Exec(`PRAGMA busy_timeout=5000;
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources (id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT,type TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS endings (session TEXT NOT NULL,turn TEXT NOT NULL,PRIMARY KEY(session,turn));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// SQLite also enforces the per-user writer lease. It is held on a dedicated
	// second DB file for the entire daemon lifetime, not just a scan transaction.
	if err = db.QueryRow(`SELECT value FROM meta WHERE key='epoch'`).Scan(&s.Epoch); errors.Is(err, sql.ErrNoRows) {
		s.Epoch = digest(path + "/" + randomToken())
		_, err = db.Exec(`INSERT INTO meta VALUES ('epoch',?)`, s.Epoch)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0600)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Load() (map[string]*Source, error) {
	rows, err := s.db.Query(`SELECT data FROM sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]*Source{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var src Source
		if err = json.Unmarshal(b, &src); err != nil {
			return nil, errors.New("invalid_checkpoint")
		}
		result[src.ID] = &src
	}
	return result, rows.Err()
}

func (s *Store) Commit(src *Source, events []Event) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO sources VALUES (?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, src.ID, b); err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == "turn.ended" {
			r, err := tx.Exec(`INSERT OR IGNORE INTO endings VALUES (?,?)`, event.Session.ID, event.Session.TurnID)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				continue
			}
		}
		b, err = json.Marshal(event.Session)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO events(type,data) VALUES (?,?)`, event.Type, b); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM events WHERE id <= (SELECT COALESCE(MAX(id),0)-? FROM events)`, eventLimit); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM endings WHERE rowid NOT IN (SELECT rowid FROM endings ORDER BY rowid DESC LIMIT 4096)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Events(after int64) ([]Event, int64, error) {
	var latest int64
	if err := s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id,type,data FROM events WHERE id>? ORDER BY id`, after)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var b []byte
		if err = rows.Scan(&e.ID, &e.Type, &b); err != nil {
			return nil, 0, err
		}
		if err = json.Unmarshal(b, &e.Session); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, latest, rows.Err()
}

// A long-lived IMMEDIATE transaction on a separate file provides a crash-safe
// cross-platform single-instance lease, including across different TCP ports.
func AcquireLease(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+".lock")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=0; BEGIN IMMEDIATE`); err != nil {
		db.Close()
		return nil, errors.New("runtime_already_running")
	}
	return func() { _, _ = db.Exec(`ROLLBACK`); _ = db.Close() }, nil
}
