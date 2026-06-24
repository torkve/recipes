package store

import "errors"

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrDuplicate is returned on a uniqueness violation (e.g. category name).
	ErrDuplicate = errors.New("store: duplicate")
	// ErrCategoryInUse is returned when deleting a category that still has recipes.
	ErrCategoryInUse = errors.New("store: category in use")
	// ErrCycle is returned when setting a category parent would create a cycle
	// (the parent is the category itself or one of its descendants).
	ErrCycle = errors.New("store: category cycle")
)
