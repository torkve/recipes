package notesync

import (
	"reflect"
	"testing"

	"recipes/internal/models"
)

func TestChecklistsToIngredientsRoundTrip(t *testing.T) {
	checklists := [][]string{
		{"# Тесто", "200 г муки", "щепотка соли"},
		{"3 яйца", "", "  100 г сахара  "},
	}
	ings := ChecklistsToIngredients(checklists)
	// Subtitle marker "# Тесто" stays as an item on import (only the reverse
	// mapping interprets it); empties are trimmed/dropped.
	want := []models.IngredientBlock{
		{Items: []string{"# Тесто", "200 г муки", "щепотка соли"}},
		{Items: []string{"3 яйца", "100 г сахара"}},
	}
	if !reflect.DeepEqual(ings, want) {
		t.Fatalf("got %+v, want %+v", ings, want)
	}
}

func TestIngredientsToChecklistsPreservesSubtitle(t *testing.T) {
	blocks := []models.IngredientBlock{
		{Subtitle: "Начинка", Items: []string{"яблоки", "корица"}},
		{Items: []string{"мука"}},
	}
	got := IngredientsToChecklists(blocks)
	want := [][]string{
		{"# Начинка", "яблоки", "корица"},
		{"мука"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSortFoldersTopologically(t *testing.T) {
	folders := []Folder{
		{ID: "c", ParentID: "a", Name: "Холодные"},
		{ID: "a", ParentID: "", Name: "Супы"},
		{ID: "b", ParentID: "", Name: "Салаты"},
		{ID: "d", ParentID: "c", Name: "Гаспачо"},
	}
	got := SortFoldersTopologically(folders)
	// Parents must precede children; ties broken by name.
	pos := map[FolderID]int{}
	for i, f := range got {
		pos[f.ID] = i
	}
	if pos["a"] > pos["c"] || pos["c"] > pos["d"] {
		t.Fatalf("parent did not precede child: %+v", got)
	}
	if pos["b"] != 0 && pos["a"] != 0 {
		t.Fatalf("a root must come first, got %+v", got)
	}
}
