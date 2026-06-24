package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"recipes/internal/models"
)

// isUniqueErr reports whether err is a SQLite UNIQUE-constraint violation.
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isForeignKeyErr reports whether err is a SQLite FOREIGN KEY violation.
func isForeignKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func scanCategory(row interface{ Scan(...any) error }) (*models.Category, error) {
	var c models.Category
	var parent sql.NullInt64
	var created string
	if err := row.Scan(&c.ID, &c.Name, &c.NameNorm, &parent, &c.Source, &created); err != nil {
		return nil, err
	}
	if parent.Valid {
		c.ParentID = &parent.Int64
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &c, nil
}

// GetCategory returns a category by id.
func (s *Store) GetCategory(ctx context.Context, id int64) (*models.Category, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, name_norm, parent_id, source, created_at FROM categories WHERE id = ?`, id)
	c, err := scanCategory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// CategoryByNorm returns a category by its normalized name, or ErrNotFound.
func (s *Store) CategoryByNorm(ctx context.Context, norm string) (*models.Category, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, name_norm, parent_id, source, created_at FROM categories WHERE name_norm = ?`, norm)
	c, err := scanCategory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListCategories returns all categories ordered by name, each with its recipe count.
func (s *Store) ListCategories(ctx context.Context) ([]models.Category, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.name, c.name_norm, c.parent_id, c.source, c.created_at,
		       (SELECT COUNT(*) FROM recipes r WHERE r.category_id = c.id) AS cnt
		FROM categories c
		ORDER BY c.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Category
	for rows.Next() {
		var c models.Category
		var parent sql.NullInt64
		var created string
		if err := rows.Scan(&c.ID, &c.Name, &c.NameNorm, &parent, &c.Source, &created, &c.RecipeCount); err != nil {
			return nil, err
		}
		if parent.Valid {
			c.ParentID = &parent.Int64
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetOrCreateCategory returns the category whose normalized name matches the
// given (possibly new) name, creating it with the given source if absent.
// This backs the "free-text new category" flow on the recipe form.
func (s *Store) GetOrCreateCategory(ctx context.Context, name, source string) (*models.Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("store: empty category name")
	}
	norm := NormalizeName(name)

	if c, err := s.CategoryByNorm(ctx, norm); err == nil {
		return c, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO categories (name, name_norm, parent_id, source, created_at) VALUES (?, ?, NULL, ?, ?)`,
		name, norm, source, now)
	if err != nil {
		if isUniqueErr(err) {
			// Lost a race; fetch the existing row.
			return s.CategoryByNorm(ctx, norm)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &models.Category{ID: id, Name: name, NameNorm: norm, Source: source}, nil
}

// CreateCategoryWithParent inserts a category under an optional parent. Returns
// ErrDuplicate if the normalized name already exists.
func (s *Store) CreateCategoryWithParent(ctx context.Context, name string, parentID *int64, source string) (*models.Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("store: empty category name")
	}
	norm := NormalizeName(name)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO categories (name, name_norm, parent_id, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, norm, parentID, source, now)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &models.Category{ID: id, Name: name, NameNorm: norm, ParentID: parentID, Source: source}, nil
}

// SetCategoryParent sets (parentID != nil) or clears (parentID == nil) a
// category's parent. Returns ErrNotFound if id (or a non-nil parent) does not
// exist, or ErrCycle if the new parent is the category itself or one of its
// descendants. The descendant check and the update run in one transaction so a
// concurrent reparent cannot race a cycle in.
func (s *Store) SetCategoryParent(ctx context.Context, id int64, parentID *int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var tmp int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM categories WHERE id = ?`, id).Scan(&tmp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if parentID != nil {
		// Walk up the ancestor chain from the proposed parent: reaching id means
		// the parent is a descendant of id (a cycle). The seen-set also stops a
		// pre-existing corrupt cycle from looping forever.
		seen := map[int64]bool{}
		for cur := *parentID; ; {
			if cur == id {
				return ErrCycle
			}
			if seen[cur] {
				break
			}
			seen[cur] = true
			var next sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT parent_id FROM categories WHERE id = ?`, cur).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound // proposed parent does not exist
			}
			if err != nil {
				return err
			}
			if !next.Valid {
				break
			}
			cur = next.Int64
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE categories SET parent_id = ? WHERE id = ?`, parentID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameCategory changes a category's display name and normalized key.
// Returns ErrDuplicate if the new normalized name collides with another category.
func (s *Store) RenameCategory(ctx context.Context, id int64, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("store: empty category name")
	}
	norm := NormalizeName(newName)
	res, err := s.db.ExecContext(ctx,
		`UPDATE categories SET name = ?, name_norm = ? WHERE id = ?`, newName, norm, id)
	if isUniqueErr(err) {
		return ErrDuplicate
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCategory removes a category that has no recipes. The recipes→category
// foreign key is ON DELETE RESTRICT, so the single DELETE is atomic: a category
// still referenced by recipes raises a FOREIGN KEY violation, which we translate
// to ErrCategoryInUse. This avoids the TOCTOU window of a separate count query.
func (s *Store) DeleteCategory(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM categories WHERE id = ?`, id)
	if err != nil {
		if isForeignKeyErr(err) {
			return ErrCategoryInUse
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
