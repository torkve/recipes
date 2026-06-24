package store

import "errors"

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrDuplicate is returned on a uniqueness violation (e.g. category name).
	ErrDuplicate = errors.New("store: duplicate")
	// ErrCategoryInUse is returned when deleting a category that still has recipes.
	ErrCategoryInUse = errors.New("store: category in use")
)
