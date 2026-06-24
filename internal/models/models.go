// Package models holds the domain types shared across the service.
package models

import "time"

// Category source values.
const (
	SourceBuiltin = "builtin" // seeded default categories
	SourceManual  = "manual"  // created by a user (incl. on-the-fly)
	SourceICloud  = "icloud"  // imported from an iCloud notes subfolder
)

// Category is a recipe category. Categories may be hierarchical (ParentID)
// to mirror iCloud notes subfolders.
type Category struct {
	ID        int64
	Name      string // display name as entered
	NameNorm  string // normalized key (lowercased, collapsed) — uniqueness guard
	ParentID  *int64
	Source    string
	CreatedAt time.Time

	// RecipeCount is populated by listing queries that aggregate usage; it is
	// not a stored column.
	RecipeCount int
}

// User is an authorized family member who can edit recipes.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

// IngredientBlock is one (optionally titled) unordered ingredient list,
// e.g. "Тесто" with its items.
type IngredientBlock struct {
	Subtitle string   `json:"subtitle"`
	Items    []string `json:"items"`
}

// Recipe is a text recipe document.
type Recipe struct {
	ID          int64
	Title       string
	CategoryID  int64
	Category    *Category // optionally joined
	Ingredients []IngredientBlock
	StepsHTML   string // sanitized HTML body (text + inline <img> previews)
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// iCloud sync linkage (nil when not synced).
	ICloudNoteID *string
	ICloudEtag   *string
	OwnerID      *int64

	Images []RecipeImage // optionally joined
}

// RecipeImage references an uploaded image file stored under the uploads dir.
type RecipeImage struct {
	ID          int64
	RecipeID    int64
	Filename    string // basename within the uploads dir
	ContentType string
	CreatedAt   time.Time
}

// ICloudAccount is a user's binding to their Apple iCloud Notes for sync.
// SessionBlob holds the encrypted auth session (never plaintext).
type ICloudAccount struct {
	ID          int64
	UserID      int64
	AppleID     string
	SessionBlob []byte
	NotesFolder string // chosen root folder id/name to sync
	CreatedAt   time.Time
}

// SyncState records the last-synced fingerprints linking a recipe to a note,
// forming the common ancestor for three-way conflict detection.
type SyncState struct {
	RecipeID     int64
	NoteID       string
	LastSyncedAt time.Time
	LocalHash    string // recipe hash at last sync
	RemoteHash   string // note hash at last sync (base)
}

// Sync conflict kinds.
const (
	ConflictBothChanged    = "both_changed"
	ConflictNoteUnparsable = "note_unparseable"
	ConflictDupTitle       = "dup_title"
	ConflictFolderAmbig    = "folder_ambiguous"
)

// SyncConflict is an ambiguity that requires manual resolution.
type SyncConflict struct {
	ID        int64
	RecipeID  *int64
	NoteID    *string
	Kind      string
	Detail    string
	CreatedAt time.Time
	Resolved  bool
}
