package web

import "recipes/internal/models"

// catNode is a category with its depth in the hierarchy, for indented rendering.
type catNode struct {
	models.Category
	Depth int
}

// categoryTree orders categories parent-before-child (preserving the input order
// among siblings) and assigns each a depth, so templates can render the hierarchy
// (Notes subfolders) with indentation. Categories whose parent is missing are
// treated as top-level.
func categoryTree(cats []models.Category) []catNode {
	present := make(map[int64]bool, len(cats))
	for _, c := range cats {
		present[c.ID] = true
	}
	children := map[int64][]models.Category{}
	for _, c := range cats {
		parent := int64(0)
		if c.ParentID != nil && present[*c.ParentID] {
			parent = *c.ParentID
		}
		children[parent] = append(children[parent], c)
	}

	// visited guards against a parent cycle in the data (e.g. a manual reparent
	// that slipped past validation): each category is emitted at most once, so
	// this renders on every page without risk of infinite recursion.
	visited := make(map[int64]bool, len(cats))
	var out []catNode
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for _, c := range children[parent] {
			if visited[c.ID] {
				continue
			}
			visited[c.ID] = true
			out = append(out, catNode{Category: c, Depth: depth})
			walk(c.ID, depth+1)
		}
	}
	walk(0, 0)
	return out
}
