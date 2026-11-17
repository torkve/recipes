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
	// ErrConflict is returned when a conditional update finds the row at a
	// different revision than the caller expected.
	ErrConflict = errors.New("store: revision conflict")
	// ErrTokenReuse is returned when an already-rotated refresh token is
	// presented again; the grant it belonged to has been revoked.
	ErrTokenReuse = errors.New("store: refresh token reused")
	// ErrForeignImage is returned when a recipe write would attach an image
	// that is attached to another recipe.
	ErrForeignImage = errors.New("store: image belongs to another recipe")
	// ErrClientLimit is returned when registering an OAuth client would exceed
	// the client cap and no unused client can be evicted.
	ErrClientLimit = errors.New("store: OAuth client limit reached")
)
