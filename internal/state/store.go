// Package state owns a single process's durable state on a local filesystem.
package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	lock *os.File
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("state directory must be private (mode 0700)")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("state directory already owned: %w", err), lock.Close())
	}
	path, err := filepath.Abs(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "foreign_keys(ON)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		_ = s.Close()
		return nil, err
	}
	if version != 0 && version != 1 {
		_ = s.Close()
		return nil, fmt.Errorf("unsupported state schema version %d", version)
	}
	_, err = db.Exec(`BEGIN IMMEDIATE;
 CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, id TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(kind,id));
 PRAGMA user_version=1;
 COMMIT;`)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }

// Put commits an aggregate (including its reservations/attempt history) atomically.
func (s *Store) Put(kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO records(kind,id,body) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET body=excluded.body", kind, id, data)
	return err
}

// PutMany publishes a whole workflow atomically, never a partially visible graph.
func (s *Store) PutMany(kind string, values map[string]any) error {
	encoded := map[string][]byte{}
	for id, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		encoded[id] = data
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for id, data := range encoded {
		if _, err := tx.Exec("INSERT INTO records(kind,id,body) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET body=excluded.body", kind, id, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Get(kind, id string, value any) error {
	var data []byte
	if err := s.db.QueryRow("SELECT body FROM records WHERE kind=? AND id=?", kind, id).Scan(&data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (s *Store) List(kind string) ([][]byte, error) {
	rows, err := s.db.Query("SELECT body FROM records WHERE kind=? ORDER BY id", kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}
