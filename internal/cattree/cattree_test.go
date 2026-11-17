package cattree

import (
	"testing"

	"recipes/internal/models"
)

func TestPathAndDescendants(t *testing.T) {
	one, two := int64(1), int64(2)
	cats := []models.Category{
		{ID: 1, Name: "Супы"},
		{ID: 2, Name: "Холодные", ParentID: &one},
		{ID: 3, Name: "Гаспачо", ParentID: &two},
		{ID: 4, Name: "Десерты"},
	}
	path := Path(cats, 3) // root -> leaf
	if len(path) != 3 || path[0].ID != 1 || path[1].ID != 2 || path[2].ID != 3 {
		t.Fatalf("Path(3) = %+v, want [1 2 3]", path)
	}
	if got := Path(cats, 99); got != nil {
		t.Fatalf("Path(missing) = %+v, want nil", got)
	}
	ds := DescendantIDs(cats, 1) // 1 + all descendants
	set := map[int64]bool{}
	for _, id := range ds {
		set[id] = true
	}
	if len(ds) != 3 || !set[1] || !set[2] || !set[3] || set[4] {
		t.Fatalf("DescendantIDs(1) = %v, want {1,2,3}", ds)
	}
}

func TestCycleGuard(t *testing.T) {
	one, two := int64(1), int64(2)
	cats := []models.Category{
		{ID: 1, Name: "a", ParentID: &two},
		{ID: 2, Name: "b", ParentID: &one},
	}
	if p := Path(cats, 1); len(p) != 2 {
		t.Fatalf("Path on cycle = %+v, want 2 nodes", p)
	}
	if d := DescendantIDs(cats, 1); len(d) != 2 {
		t.Fatalf("DescendantIDs on cycle = %v, want 2 ids", d)
	}
}
