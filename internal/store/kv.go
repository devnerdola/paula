package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// KeyModel holds the model saved for a role.
func KeyModel(role string) string { return "model." + role }

// Get returns the value of a key, and false when it was never set.
func (s *Store) Get(ctx context.Context, key string) (string, bool, error) {
	return value(s.ro.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key))
}

func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
	return err
}

// GetInt returns a number kept in the kv table, and 0 when it was never set.
func (s *Store) GetInt(ctx context.Context, key string) (int64, error) {
	v, ok, err := s.Get(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func (s *Store) SetInt(ctx context.Context, key string, v int64) error {
	return s.Set(ctx, key, strconv.FormatInt(v, 10))
}

func value(row *sql.Row) (string, bool, error) {
	var v string
	err := row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
