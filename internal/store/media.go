package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
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

// AddPhoto records a picture she took under the call that took it, and returns
// its number. A picture recorded before keeps its number and the call that
// took it first.
func (s *Store) AddPhoto(ctx context.Context, sha256 string, call int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO media (sha256, tool_call_id) VALUES (?, ?)
		ON CONFLICT(sha256) DO UPDATE SET tool_call_id = coalesce(tool_call_id, excluded.tool_call_id)
		RETURNING id`, sha256, call).Scan(&id)
	return id, err
}

// Photo is the picture of that number she took, whether or not a message has
// carried it yet.
func (s *Store) Photo(ctx context.Context, id int64) (*Media, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE id = ? AND tool_call_id IS NOT NULL`, id)
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

// SetCaptionError records why the host refused to describe an image, so it is
// not sent again to be described. There is no second try after a while.
func (s *Store) SetCaptionError(ctx context.Context, sha256, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET caption_error = ? WHERE sha256 = ?`, reason, sha256)
	return err
}

// Image is a picture as it was sent: the number it is looked up by, which is
// the one its file was recorded under, the message it came in, who sent that
// and when, and what it showed.
type Image struct {
	ID        int64
	SHA256    string
	Caption   string
	MessageID MessageID
	Role      string
	SentAt    time.Time
}

// imagesFrom is every picture a message carries, with what is known of its
// file. A picture sent twice is one file, and two pictures.
const imagesFrom = `SELECT media.id, media.sha256, media.caption, messages.id, messages.role, messages.created_at
	  FROM messages, json_each(messages.parts_json) AS part
	  JOIN media ON media.sha256 = json_extract(part.value, '$.sha256')
	 WHERE json_extract(part.value, '$.type') = 'image'`

// Images are every picture of the conversation, newest first.
func (s *Store) Images(ctx context.Context) ([]Image, error) {
	rows, err := s.ro.QueryContext(ctx, imagesFrom+` ORDER BY messages.id DESC, part.key`)
	if err != nil {
		return nil, err
	}
	return scanImages(rows)
}

// ImagesFrom are the pictures of the conversation, newest first, from the one
// at from on and at most limit of them.
func (s *Store) ImagesFrom(ctx context.Context, from, limit int) ([]Image, error) {
	rows, err := s.ro.QueryContext(ctx, imagesFrom+` ORDER BY messages.id DESC, part.key LIMIT ? OFFSET ?`, limit, from)
	if err != nil {
		return nil, err
	}
	return scanImages(rows)
}

func scanImages(rows *sql.Rows) ([]Image, error) {
	defer rows.Close()
	var out []Image
	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *img)
	}
	return out, rows.Err()
}

// Image is the picture of that number, as the message it first came in sent it.
func (s *Store) Image(ctx context.Context, id int64) (*Image, error) {
	row := s.ro.QueryRowContext(ctx, imagesFrom+` AND media.id = ? ORDER BY messages.id LIMIT 1`, id)
	img, err := scanImage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return img, err
}

func scanImage(row scanner) (*Image, error) {
	var img Image
	var sent int64
	if err := row.Scan(&img.ID, &img.SHA256, &img.Caption, &img.MessageID, &img.Role, &sent); err != nil {
		return nil, err
	}
	img.SentAt = time.Unix(0, sent)
	return &img, nil
}

func scanMedia(row scanner) (*Media, error) {
	var m Media
	if err := row.Scan(&m.SHA256, &m.Caption, &m.CaptionError); err != nil {
		return nil, err
	}
	return &m, nil
}
