// Package cattree navigates the category hierarchy (categories with ParentID)
// held as a flat list, as returned by store.ListCategories. Every walk is
// cycle-guarded, so corrupt parent links cannot loop.
package cattree

import "recipes/internal/models"

// Path returns the ancestor chain from the top-level root down to id
// (inclusive). Returns nil if id is absent.
func Path(cats []models.Category, id int64) []models.Category {
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

// DescendantIDs returns id plus the ids of all its descendants, for filtering
// recipes by a category subtree.
func DescendantIDs(cats []models.Category, id int64) []int64 {
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
