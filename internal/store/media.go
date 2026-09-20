package store

import (
	"context"
	"database/sql"
	"errors"
)

// Media is what is known about a stored image. The file itself is named by the
// digest and is always a JPEG, so nothing of that is here.
type Media struct {
	SHA256       string
	Caption      string
	CaptionError string
}

const mediaColumns = `sha256, caption, caption_error`

// AddMedia records an image, keeping what is already known about one stored
// before.
func (s *Store) AddMedia(ctx context.Context, sha256 string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO media (sha256) VALUES (?) ON CONFLICT(sha256) DO NOTHING`, sha256)
	return err
}

func (s *Store) Media(ctx context.Context, sha256 string) (*Media, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE sha256 = ?`, sha256)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// SetCaption stores a caption and clears an earlier failure.
func (s *Store) SetCaption(ctx context.Context, sha256, caption string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET caption = ?, caption_error = '' WHERE sha256 = ?`,
		caption, sha256)
	return err
}

// SetCaptionError records that describing an image failed, so it is not sent
// again to be described. There is no second try after a while.
func (s *Store) SetCaptionError(ctx context.Context, sha256, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET caption_error = ? WHERE sha256 = ?`, reason, sha256)
	return err
}

func scanMedia(row scanner) (*Media, error) {
	var m Media
	if err := row.Scan(&m.SHA256, &m.Caption, &m.CaptionError); err != nil {
		return nil, err
	}
	return &m, nil
}
