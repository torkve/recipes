package notesync

import (
	"sort"
	"strings"

	"recipes/internal/models"
)

// ChecklistsToIngredients maps each note checklist to one ingredient block.
// Items are kept verbatim (free-text, no quantity parsing — per spec). A leading
// "# <text>" item is taken as the block subtitle (symmetric with
// IngredientsToChecklists). Empty checklists are dropped.
func ChecklistsToIngredients(checklists [][]string) []models.IngredientBlock {
	var out []models.IngredientBlock
	for _, c := range checklists {
		var block models.IngredientBlock
		for _, it := range c {
			v := strings.TrimSpace(it)
			if v == "" {
				continue
			}
			if sub, ok := strings.CutPrefix(v, "# "); ok && block.Subtitle == "" && len(block.Items) == 0 {
				block.Subtitle = strings.TrimSpace(sub)
				continue
			}
			block.Items = append(block.Items, v)
		}
		if block.Subtitle != "" || len(block.Items) > 0 {
			out = append(out, block)
		}
	}
	return out
}

// IngredientsToChecklists is the reverse mapping for pushing a recipe back to a
// note: each block becomes one checklist. A block subtitle is preserved as a
// leading "# <subtitle>" item so it survives the round trip.
func IngredientsToChecklists(blocks []models.IngredientBlock) [][]string {
	var out [][]string
	for _, b := range blocks {
		var items []string
		if s := strings.TrimSpace(b.Subtitle); s != "" {
			items = append(items, "# "+s)
		}
		items = append(items, b.Items...)
		if len(items) > 0 {
			out = append(out, items)
		}
	}
	return out
}

// SortFoldersTopologically orders folders so that every parent appears before
// its children (and stably by name within a depth), so the engine can resolve
// parent categories before creating children.
func SortFoldersTopologically(folders []Folder) []Folder {
	byID := make(map[FolderID]Folder, len(folders))
	for _, f := range folders {
		byID[f.ID] = f
	}

	depth := make(map[FolderID]int, len(folders))
	var depthOf func(id FolderID, seen map[FolderID]bool) int
	depthOf = func(id FolderID, seen map[FolderID]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		f, ok := byID[id]
		if !ok || f.ParentID == "" || seen[id] {
			depth[id] = 0
			return 0
		}
		seen[id] = true
		d := depthOf(f.ParentID, seen) + 1
		depth[id] = d
		return d
	}

	out := make([]Folder, len(folders))
	copy(out, folders)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := depthOf(out[i].ID, map[FolderID]bool{}), depthOf(out[j].ID, map[FolderID]bool{})
		if di != dj {
			return di < dj
		}
		return out[i].Name < out[j].Name
	})
	return out
}
