package store

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"

	"recipes/internal/models"
)

// RecipeVector is a recipe's stored embedding plus its category, for the
// in-memory semantic-search snapshot (category enables subtree filtering).
type RecipeVector struct {
	ID         int64
	CategoryID int64
	Vec        []float32
}

// encodeVec serializes a float32 vector as little-endian bytes (4 bytes/float).
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec reverses encodeVec. Trailing bytes that don't form a full float32
// are ignored.
func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// UpsertEmbedding stores (or replaces) a recipe's embedding for a model,
// computed from the recipe content at the given revision. It is a no-op when
// the recipe is gone or its current revision differs from revision.
func (s *Store) UpsertEmbedding(ctx context.Context, recipeID, revision int64, model string, dim int, vec []float32) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO recipe_embeddings(recipe_id, model, dim, vec, updated_at, revision)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE (SELECT revision FROM recipes WHERE id = ?) = ?
		ON CONFLICT(recipe_id) DO UPDATE SET
		  model = excluded.model, dim = excluded.dim, vec = excluded.vec,
		  updated_at = excluded.updated_at, revision = excluded.revision`,
		recipeID, model, dim, encodeVec(vec), now, revision, recipeID, revision)
	return err
}

// RecipeIDsMissingEmbedding returns recipe ids with no current embedding for the
// given model — absent, stored under a different model, or computed from an
// older recipe revision — oldest first.
func (s *Store) RecipeIDsMissingEmbedding(ctx context.Context, model string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id FROM recipes r
		LEFT JOIN recipe_embeddings e ON e.recipe_id = r.id AND e.model = ?
		WHERE e.recipe_id IS NULL OR e.revision != r.revision
		ORDER BY r.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// EmbeddingsForModel returns every stored vector for the model with its recipe's
// category, for loading the semantic snapshot into memory.
func (s *Store) EmbeddingsForModel(ctx context.Context, model string) ([]RecipeVector, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.recipe_id, r.category_id, e.vec
		FROM recipe_embeddings e JOIN recipes r ON r.id = e.recipe_id
		WHERE e.model = ?`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecipeVector
	for rows.Next() {
		var rv RecipeVector
		var b []byte
		if err := rows.Scan(&rv.ID, &rv.CategoryID, &b); err != nil {
			return nil, err
		}
		rv.Vec = decodeVec(b)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// EmbedText builds the plain-text representation of a recipe for embedding — the
// same flattening the FTS index uses (title + ingredients + steps).
func EmbedText(title string, ingredients []models.IngredientBlock, stepsHTML string) string {
	return strings.TrimSpace(title + "\n" + ingredientsToText(ingredients) + "\n" + htmlToText(stepsHTML))
}

// RecipeEmbedInput returns the embedding input text for a recipe id and the
// recipe revision it reflects; ok is false when no recipe has that id.
func (s *Store) RecipeEmbedInput(ctx context.Context, id int64) (text string, revision int64, ok bool, err error) {
	r, err := s.GetRecipe(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return EmbedText(r.Title, r.Ingredients, r.StepsHTML), r.Revision, true, nil
}
