package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db    *sql.DB
	Epoch string
	mu    sync.Mutex
}

var ErrConflict = errors.New("event_id_conflict")

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
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY,data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS facts(id INTEGER PRIMARY KEY AUTOINCREMENT,data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS evidence(key TEXT PRIMARY KEY,source TEXT NOT NULL,data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS dedup(producer TEXT,event TEXT,hash TEXT NOT NULL,fact_id INTEGER NOT NULL,stored_at INTEGER NOT NULL,PRIMARY KEY(producer,event));
 CREATE INDEX IF NOT EXISTS dedup_time ON dedup(stored_at);
 CREATE TABLE IF NOT EXISTS sequences(producer TEXT,stream TEXT,highest INTEGER NOT NULL,PRIMARY KEY(producer,stream));`)
	if err == nil {
		err = db.QueryRow(`SELECT value FROM meta WHERE key='epoch'`).Scan(&s.Epoch)
		if errors.Is(err, sql.ErrNoRows) {
			s.Epoch = randomToken()
			_, err = db.Exec(`INSERT INTO meta VALUES ('epoch',?)`, s.Epoch)
		}
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
		var src Source
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if json.Unmarshal(b, &src) != nil {
			return nil, errors.New("invalid_checkpoint")
		}
		result[src.ID] = &src
	}
	return result, rows.Err()
}

func evidenceKey(f Fact) string {
	return digest(f.SourceID + "\x00" + f.Agent + "\x00" + f.Namespace + "\x00" + f.SessionID)
}

func (s *Store) commit(ctx context.Context, src *Source, facts []Fact, external bool) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var fingerprint string
	if external {
		input := facts[0].InputEvent
		b, _ := json.Marshal(input)
		fingerprint = digest(string(b))
		var prior string
		var id int64
		err = tx.QueryRowContext(ctx, `SELECT hash,fact_id FROM dedup WHERE producer=? AND event=? AND stored_at>=?`, input.ProducerID, input.EventID, time.Now().Add(-7*24*time.Hour).Unix()).Scan(&prior, &id)
		if err == nil {
			if prior != fingerprint {
				return 0, false, ErrConflict
			}
			return id, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		if input.SourceSequence != nil {
			var highest int64
			err = tx.QueryRowContext(ctx, `SELECT highest FROM sequences WHERE producer=? AND stream=?`, input.ProducerID, input.StreamID).Scan(&highest)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, false, err
			}
			if err == nil && *input.SourceSequence != highest+1 {
				facts[0].SequenceGap = true
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO sequences VALUES(?,?,?) ON CONFLICT(producer,stream) DO UPDATE SET highest=MAX(highest,excluded.highest)`, input.ProducerID, input.StreamID, *input.SourceSequence)
			if err != nil {
				return 0, false, err
			}
		}
	}
	if src != nil {
		var old []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM sources WHERE id=?`, src.ID).Scan(&old); err == nil {
			var previous Source
			_ = json.Unmarshal(old, &previous)
			if previous.Incarnation != src.Incarnation {
				if _, err = tx.ExecContext(ctx, `DELETE FROM evidence WHERE source=?`, src.ID); err != nil {
					return 0, false, err
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		b, _ := json.Marshal(src)
		if _, err = tx.ExecContext(ctx, `INSERT INTO sources VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, src.ID, b); err != nil {
			return 0, false, err
		}
	}
	ledgers := map[string]*EvidenceSession{}
	var last int64
	for _, f := range facts {
		f.ReceivedAt = time.Now().UTC()
		result, err := tx.ExecContext(ctx, `INSERT INTO facts(data) VALUES ('{}')`)
		if err != nil {
			return 0, false, err
		}
		f.ID, err = result.LastInsertId()
		if err != nil {
			return 0, false, err
		}
		last = f.ID
		b, _ := json.Marshal(f)
		if _, err = tx.ExecContext(ctx, `UPDATE facts SET data=? WHERE id=?`, b, f.ID); err != nil {
			return 0, false, err
		}
		key := evidenceKey(f)
		ledger := ledgers[key]
		if ledger == nil {
			ledger = &EvidenceSession{Agent: f.Agent, Namespace: f.Namespace, SessionID: f.SessionID, SourceID: f.SourceID, Origin: f.Origin, Facts: []Fact{}, Coverage: "complete", IncompleteOperations: []string{}}
			var prior []byte
			err = tx.QueryRowContext(ctx, `SELECT data FROM evidence WHERE key=?`, key).Scan(&prior)
			if err == nil {
				if json.Unmarshal(prior, ledger) != nil {
					return 0, false, errors.New("invalid_evidence")
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return 0, false, err
			}
			ledgers[key] = ledger
		}
		retainEvidence(ledger, f)
	}
	for key, ledger := range ledgers {
		b, _ := json.Marshal(ledger)
		if _, err = tx.ExecContext(ctx, `INSERT INTO evidence VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, ledger.SourceID, b); err != nil {
			return 0, false, err
		}
	}
	if external {
		input := facts[0].InputEvent
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO dedup VALUES(?,?,?,?,?)`, input.ProducerID, input.EventID, fingerprint, last, time.Now().Unix()); err != nil {
			return 0, false, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM dedup WHERE stored_at<? OR rowid IN (SELECT rowid FROM dedup ORDER BY rowid DESC LIMIT -1 OFFSET 100000)`, time.Now().Add(-7*24*time.Hour).Unix()); err != nil {
			return 0, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM facts WHERE id <= (SELECT COALESCE(MAX(id),0)-? FROM facts)`, EventLimit); err != nil {
		return 0, false, err
	}
	return last, false, tx.Commit()
}
func (s *Store) Commit(src *Source, facts []Fact) error {
	_, _, err := s.commit(context.Background(), src, facts, false)
	return err
}
func (s *Store) Ingest(ctx context.Context, input InputEvent) (int64, bool, error) {
	return s.commit(ctx, nil, []Fact{{InputEvent: input, Origin: "external", SourceID: "external:" + digest(input.ProducerID)}}, true)
}
func revision(tx *sql.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='facts'),0)`).Scan(&n)
	return n, err
}
func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	n, err := revision(tx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Schema: 2, Epoch: s.Epoch, Revision: n, Sessions: []EvidenceSession{}, Coverage: "complete"}
	rows, err := tx.Query(`SELECT data FROM evidence ORDER BY key`)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var b []byte
		var e EvidenceSession
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &e); err != nil {
			break
		}
		snapshot.Sessions = append(snapshot.Sessions, e)
		if e.Coverage != "complete" {
			snapshot.Coverage = "partial"
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	boundSnapshot(&snapshot)
	return snapshot, err
}

// Bound a recovery response as well as the incremental journal. Truncation is
// explicit evidence loss, never an implicit "all tools finished" assertion.
func boundSnapshot(snapshot *Snapshot) {
	type ref struct {
		session, index, size int
		id                   int64
	}
	refs := []ref{}
	budget := MaxSnapshotBytes - (1 << 20)
	if len(snapshot.Sessions) > 4096 {
		snapshot.Sessions = snapshot.Sessions[:4096]
		snapshot.Coverage = "partial"
	}
	identities := snapshot.Sessions[:0]
	for _, s := range snapshot.Sessions {
		identity := s
		identity.Facts = []Fact{}
		b, _ := json.Marshal(identity)
		if len(b) > budget {
			snapshot.Coverage = "partial"
			continue
		}
		budget -= len(b)
		identities = append(identities, s)
	}
	snapshot.Sessions = identities
	for i, s := range snapshot.Sessions {
		for j, f := range s.Facts {
			b, _ := json.Marshal(f)
			refs = append(refs, ref{i, j, len(b) + 1, f.ID})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].id > refs[j].id })
	keep := map[[2]int]bool{}
	for _, r := range refs {
		if budget >= r.size {
			budget -= r.size
			keep[[2]int{r.session, r.index}] = true
		} else {
			snapshot.Sessions[r.session].Coverage = "partial"
			snapshot.Sessions[r.session].IncompleteOperations = []string{"*"}
			snapshot.Coverage = "partial"
		}
	}
	for i := range snapshot.Sessions {
		old := snapshot.Sessions[i].Facts
		snapshot.Sessions[i].Facts = []Fact{}
		for j, f := range old {
			if keep[[2]int{i, j}] {
				snapshot.Sessions[i].Facts = append(snapshot.Sessions[i].Facts, f)
			}
		}
	}
}
func (s *Store) Events(after int64) ([]Fact, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	n, err := revision(tx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(`SELECT data FROM facts WHERE id>? ORDER BY id`, after)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	facts := []Fact{}
	for rows.Next() {
		var b []byte
		var f Fact
		if err = rows.Scan(&b); err != nil {
			return nil, 0, err
		}
		if err = json.Unmarshal(b, &f); err != nil {
			return nil, 0, err
		}
		facts = append(facts, f)
	}
	return facts, n, rows.Err()
}

// The original lease path is retained so v0.1 and v0.2 cannot own one data dir.
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
