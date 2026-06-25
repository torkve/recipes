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

// categoryPath returns the ancestor chain from the top-level root down to id
// (inclusive), for breadcrumbs. Returns nil if id is absent. Cycle-guarded.
func categoryPath(cats []models.Category, id int64) []models.Category {
	byID := make(map[int64]models.Category, len(cats))
	for _, c := range cats {
		byID[c.ID] = c
	}
	var rev []models.Category
	seen := map[int64]bool{}
	for cur := id; cur != 0 && !seen[cur]; {
		c, ok := byID[cur]
		if !ok {
			break
		}
		seen[cur] = true
		rev = append(rev, c)
		if c.ParentID == nil {
			break
		}
		cur = *c.ParentID
	}
	// rev is leaf→root; reverse to root→leaf.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// categoryDescendantIDs returns id plus the ids of all its descendants, for
// filtering recipes by a category subtree. Cycle-guarded.
func categoryDescendantIDs(cats []models.Category, id int64) []int64 {
	children := map[int64][]int64{}
	for _, c := range cats {
		if c.ParentID != nil {
			children[*c.ParentID] = append(children[*c.ParentID], c.ID)
		}
	}
	var out []int64
	seen := map[int64]bool{}
	var walk func(n int64)
	walk = func(n int64) {
		if seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
		for _, ch := range children[n] {
			walk(ch)
		}
	}
	walk(id)
	return out
}
