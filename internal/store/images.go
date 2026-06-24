package store

import (
	"context"
	"database/sql"
	"time"

	"recipes/internal/models"
)

// AddImage records an uploaded image file for a recipe.
func (s *Store) AddImage(ctx context.Context, recipeID int64, filename, contentType string) (*models.RecipeImage, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO recipe_images (recipe_id, filename, content_type, created_at) VALUES (?, ?, ?, ?)`,
		recipeID, filename, contentType, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &models.RecipeImage{ID: id, RecipeID: recipeID, Filename: filename, ContentType: contentType}, nil
}

// DeleteImageByName removes the image row for a recipe by filename. It is a
// no-op (nil) if no such row exists.
func (s *Store) DeleteImageByName(ctx context.Context, recipeID int64, filename string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM recipe_images WHERE recipe_id = ? AND filename = ?`, recipeID, filename)
	return err
}

// ImagesForRecipe returns the images attached to a recipe, oldest first.
func (s *Store) ImagesForRecipe(ctx context.Context, recipeID int64) ([]models.RecipeImage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, recipe_id, filename, content_type, created_at FROM recipe_images WHERE recipe_id = ? ORDER BY id`, recipeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.RecipeImage
	for rows.Next() {
		var img models.RecipeImage
		var created string
		if err := rows.Scan(&img.ID, &img.RecipeID, &img.Filename, &img.ContentType, &created); err != nil {
			return nil, err
		}
		img.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, img)
	}
	return out, rows.Err()
}

// imageFilenamesTx returns the filenames attached to a recipe, within a tx.
func imageFilenamesTx(ctx context.Context, tx *sql.Tx, recipeID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT filename FROM recipe_images WHERE recipe_id = ?`, recipeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AllImageFilenames returns every referenced image filename (for orphan sweeps).
func (s *Store) AllImageFilenames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT filename FROM recipe_images`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	return out, rows.Err()
}
