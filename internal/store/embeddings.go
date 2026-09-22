package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

// Embedded is a model that a memory has been embedded by: the runner it is
// served through and the id that runner knows it by. Vectors of one model say
// nothing about the vectors of another, so what made each one is kept with it.
type Embedded struct {
	Runner string
	Model  string
}

// MemoriesToEmbed are the memories that still stand and have no vector from
// this model, oldest first, at most limit of them.
//
// A vector as wide as the model answers is one of its own; one of another
// width is not, whatever it was stored under, since a model that answers at
// one width and then another is two models as far as a vector is concerned. So
// a memory holding one of those is waiting, and is written again.
//
// width is how wide the model answered last, which only an answer says. Zero
// is a caller that has not had one yet, and then the newest vector the model
// wrote is what says how wide its vectors are.
func (s *Store) MemoriesToEmbed(ctx context.Context, by Embedded, width, limit int) ([]Memory, error) {
	var wide any
	if width > 0 {
		wide = 4 * width
	}
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+` `+memoriesFrom+`
		  LEFT JOIN memory_embeddings e
		    ON e.memory_id = memories.id AND e.runner = ? AND e.model = ?
		   AND length(e.vector) = coalesce(?,
		         (SELECT length(vector) FROM memory_embeddings
		           WHERE runner = ? AND model = ?
		           ORDER BY memory_id DESC LIMIT 1))
		 WHERE replaced_by IS NULL AND e.memory_id IS NULL
		 ORDER BY memories.id LIMIT ?`,
		by.Runner, by.Model, wide, by.Runner, by.Model, limit)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// Embed stores a vector for each memory, all of them together: a memory left
// without one would be searched for and never found. A memory forgotten while
// it was being embedded takes its vector with it rather than failing the rest.
func (s *Store) Embed(ctx context.Context, by Embedded, vectors map[MemoryID][]float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for id, v := range vectors {
		if len(v) == 0 {
			return fmt.Errorf("memory %d was embedded as nothing", id)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO memory_embeddings
			(memory_id, runner, model, vector)
			SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM memories WHERE id = ?)
			ON CONFLICT(memory_id, runner, model) DO UPDATE SET
				vector = excluded.vector`,
			int64(id), by.Runner, by.Model, vectorBytes(v), int64(id))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NearestMemories are the memories closest in meaning to a vector, the closest
// first, at most limit of them. Only what still stands is searched, and only
// what this model embedded: a vector of another model measures nothing here.
//
// A vector of another width than the one asked about is another model's,
// whatever it was stored under, so it is left out rather than measured. How
// many were left out comes back with them, since a search that quietly
// answered out of a fraction of what she remembers would say nothing about it.
func (s *Store) NearestMemories(ctx context.Context, by Embedded, to []float32, limit int) (found []Memory, elsewhere int, err error) {
	if len(to) == 0 || limit <= 0 {
		return nil, 0, nil
	}
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+`, e.vector `+memoriesFrom+`
		  JOIN memory_embeddings e
		    ON e.memory_id = memories.id AND e.runner = ? AND e.model = ?
		 WHERE replaced_by IS NULL`, by.Runner, by.Model)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// A conversation holds memories in the hundreds, so they are all measured
	// here rather than kept in an index of their own.
	type near struct {
		memory Memory
		like   float64
	}
	var measured []near
	for rows.Next() {
		var raw []byte
		m, err := scanMemory(rows, &raw)
		if err != nil {
			return nil, 0, err
		}
		v, err := vectorOf(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("memory %d: %w", m.ID, err)
		}
		if len(v) != len(to) {
			elsewhere++
			continue
		}
		measured = append(measured, near{memory: *m, like: cosine(v, to)})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	slices.SortStableFunc(measured, func(a, b near) int {
		switch {
		case a.like > b.like:
			return -1
		case a.like < b.like:
			return 1
		}
		return 0
	})
	out := make([]Memory, 0, min(limit, len(measured)))
	for _, n := range measured[:min(limit, len(measured))] {
		out = append(out, n.memory)
	}
	return out, elsewhere, nil
}

// cosine is how alike two vectors point, from 1 for the same way to -1 for
// opposite ways. A vector of no length points nowhere, and is alike nothing.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// vectorBytes is a vector as it is stored: every number in turn, smallest byte
// first, which is how it is read back.
func vectorBytes(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

func vectorOf(b []byte) ([]float32, error) {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil, fmt.Errorf("a vector of %d bytes is not one", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
