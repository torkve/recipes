package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"recipes/internal/models"
)

// RecipeInput carries the writable fields of a recipe.
type RecipeInput struct {
	Title       string
	CategoryID  int64
	Ingredients []models.IngredientBlock
	StepsHTML   string

	// Optional iCloud linkage / ownership.
	ICloudNoteID *string
	ICloudEtag   *string
	OwnerID      *int64
}

func marshalIngredients(blocks []models.IngredientBlock) (string, error) {
	if blocks == nil {
		blocks = []models.IngredientBlock{}
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ftsUpsert refreshes the full-text row for a recipe within a transaction.
func ftsUpsert(ctx context.Context, tx *sql.Tx, id int64, title string, ingredients []models.IngredientBlock, stepsHTML string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recipes_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO recipes_fts(rowid, title, ingredients, steps) VALUES (?, ?, ?, ?)`,
		id, title, ingredientsToText(ingredients), htmlToText(stepsHTML))
	return err
}

// CreateRecipe inserts a recipe and its full-text index entry atomically.
func (s *Store) CreateRecipe(ctx context.Context, in RecipeInput) (*models.Recipe, error) {
	ingJSON, err := marshalIngredients(in.Ingredients)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO recipes (title, category_id, ingredients_json, steps_html, created_at, updated_at, icloud_note_id, icloud_etag, owner_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Title, in.CategoryID, ingJSON, in.StepsHTML, now, now, in.ICloudNoteID, in.ICloudEtag, in.OwnerID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := ftsUpsert(ctx, tx, id, in.Title, in.Ingredients, in.StepsHTML); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetRecipe(ctx, id)
}

// UpdateRecipe updates a recipe and its full-text entry atomically.
func (s *Store) UpdateRecipe(ctx context.Context, id int64, in RecipeInput) error {
	ingJSON, err := marshalIngredients(in.Ingredients)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE recipes
		SET title = ?, category_id = ?, ingredients_json = ?, steps_html = ?, updated_at = ?,
		    icloud_note_id = ?, icloud_etag = ?, owner_id = ?
		WHERE id = ?`,
		in.Title, in.CategoryID, ingJSON, in.StepsHTML, now,
		in.ICloudNoteID, in.ICloudEtag, in.OwnerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := ftsUpsert(ctx, tx, id, in.Title, in.Ingredients, in.StepsHTML); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRecipe removes a recipe, its images rows (via cascade) and its FTS entry.
// It returns the image filenames that were attached so callers can delete files.
func (s *Store) DeleteRecipe(ctx context.Context, id int64) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	files, err := imageFilenamesTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM recipes WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recipes_fts WHERE rowid = ?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return files, nil
}

func scanRecipeRow(row interface{ Scan(...any) error }) (*models.Recipe, error) {
	var r models.Recipe
	var ingJSON, created, updated string
	var noteID, etag sql.NullString
	var owner sql.NullInt64
	if err := row.Scan(&r.ID, &r.Title, &r.CategoryID, &ingJSON, &r.StepsHTML,
		&created, &updated, &noteID, &etag, &owner); err != nil {
		return nil, err
	}
	if ingJSON != "" {
		if err := json.Unmarshal([]byte(ingJSON), &r.Ingredients); err != nil {
			return nil, err
		}
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339, created)
	r.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	if noteID.Valid {
		r.ICloudNoteID = &noteID.String
	}
	if etag.Valid {
		r.ICloudEtag = &etag.String
	}
	if owner.Valid {
		r.OwnerID = &owner.Int64
	}
	return &r, nil
}

const recipeCols = `id, title, category_id, ingredients_json, steps_html, created_at, updated_at, icloud_note_id, icloud_etag, owner_id`

// GetRecipe loads a recipe with its category and images.
func (s *Store) GetRecipe(ctx context.Context, id int64) (*models.Recipe, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+recipeCols+` FROM recipes WHERE id = ?`, id)
	r, err := scanRecipeRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.Category, err = s.GetCategory(ctx, r.CategoryID); err != nil {
		return nil, err
	}
	if r.Images, err = s.ImagesForRecipe(ctx, id); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRecipeIDsByOwner returns the ids of recipes owned by a user (used by the
// sync push to find that user's recipes to send back to iCloud).
func (s *Store) ListRecipeIDsByOwner(ctx context.Context, ownerID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recipes WHERE owner_id = ? ORDER BY id`, ownerID)
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

// GetRecipeByNoteID loads the recipe linked to an iCloud note, or ErrNotFound.
func (s *Store) GetRecipeByNoteID(ctx context.Context, noteID string) (*models.Recipe, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+recipeCols+` FROM recipes WHERE icloud_note_id = ?`, noteID)
	r, err := scanRecipeRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListRecipes returns recipes newest-first, optionally restricted to a set of
// category ids (e.g. a category and its subtree), each with its Category
// populated (id + name). A nil/empty set lists all; pass limit <= 0 for no limit.
func (s *Store) ListRecipes(ctx context.Context, categoryIDs []int64, limit, offset int) ([]models.Recipe, error) {
	q := `
		SELECT r.id, r.title, r.category_id, r.created_at, c.name
		FROM recipes r JOIN categories c ON c.id = r.category_id`
	args := []any{}
	if len(categoryIDs) > 0 {
		q += ` WHERE r.category_id IN (` + placeholders(len(categoryIDs)) + `)`
		for _, id := range categoryIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY r.created_at DESC, r.id DESC`
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	return s.queryRecipeList(ctx, q, args...)
}

// RecipesByIDs returns the lightweight recipes (id, title, category) for the
// given ids, in unspecified order (callers re-order). Used to materialize
// semantic-search hits not already in the lexical result set.
func (s *Store) RecipesByIDs(ctx context.Context, ids []int64) ([]models.Recipe, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `
		SELECT r.id, r.title, r.category_id, r.created_at, c.name
		FROM recipes r JOIN categories c ON c.id = r.category_id
		WHERE r.id IN (` + placeholders(len(ids)) + `)`
	return s.queryRecipeList(ctx, q, args...)
}

func (s *Store) queryRecipeList(ctx context.Context, q string, args ...any) ([]models.Recipe, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Recipe
	for rows.Next() {
		var r models.Recipe
		var created, catName string
		if err := rows.Scan(&r.ID, &r.Title, &r.CategoryID, &created, &catName); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, created)
		r.Category = &models.Category{ID: r.CategoryID, Name: catName}
		out = append(out, r)
	}
	return out, rows.Err()
}
