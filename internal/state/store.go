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

const upsert = "INSERT INTO records(kind,id,body) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET body=excluded.body"

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
	db, err := openDB(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return &Store{db: db, lock: lock}, nil
}

func openDB(file string) (_ *sql.DB, err error) {
	path, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "foreign_keys(ON)", "secure_delete(ON)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != 0 && version != 1 {
		return nil, fmt.Errorf("unsupported state schema version %d", version)
	}
	_, err = db.Exec(`BEGIN IMMEDIATE;
 CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, id TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(kind,id));
 PRAGMA user_version=1;
 COMMIT;`)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }

func (s *Store) Put(kind, id string, value any) error {
	return s.Update(func(tx *Tx) error { return tx.Put(kind, id, value) })
}

type Tx struct {
	tx      *sql.Tx
	deleted bool
}

// Update leaves no copy of a deleted record on disk: secure_delete zeroes its freed pages, and
// truncating the write-ahead log after commit discards the frames that still hold it.
func (s *Store) Update(fn func(*Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t := &Tx{tx: tx}
	if err := fn(t); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if t.deleted {
		_, err = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return err
}

func (t *Tx) Put(kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(upsert, kind, id, data)
	return err
}

func (t *Tx) Delete(kind, id string) error {
	t.deleted = true
	_, err := t.tx.Exec("DELETE FROM records WHERE kind=? AND id=?", kind, id)
	return err
}

func (s *Store) Get(kind, id string, value any) error {
	var data []byte
	if err := s.db.QueryRow("SELECT body FROM records WHERE kind=? AND id=?", kind, id).Scan(&data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func List[T any](s *Store, kind string) ([]T, error) {
	rows, err := s.db.Query("SELECT body FROM records WHERE kind=? ORDER BY id", kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []T
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var value T
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
