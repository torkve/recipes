package notesync

import (
	"testing"

	"recipes/internal/models"
)

func TestClassifyDecisionTable(t *testing.T) {
	cases := []struct {
		local, remote bool
		want          Decision
	}{
		{false, false, DecisionNoOp},
		{false, true, DecisionApplyRemote},
		{true, false, DecisionApplyLocal},
		{true, true, DecisionConflict},
	}
	for _, c := range cases {
		if got := Classify(c.local, c.remote); got != c.want {
			t.Errorf("Classify(%v,%v)=%v want %v", c.local, c.remote, got, c.want)
		}
	}
}

func TestHashRecipeNoteEquivalence(t *testing.T) {
	// A recipe and the note it was imported from must hash equal, despite the
	// recipe steps being HTML and the note body being raw text.
	r := &models.Recipe{
		Title:       "Борщ",
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла", "капуста"}}},
		StepsHTML:   "<p>Варить  бульон.</p>",
	}
	n := Note{
		Title:      "Борщ",
		Checklists: [][]string{{"свёкла", "капуста"}},
		BodyHTML:   "Варить бульон.",
	}
	if HashRecipe(r) != HashNote(n) {
		t.Fatalf("hash mismatch:\n recipe=%s\n note  =%s", HashRecipe(r), HashNote(n))
	}
}

func TestHashRecipeNoteEquivalenceWithImage(t *testing.T) {
	// An imaged note carries an "@@IMG:<id>@@" marker in its body; the imported
	// recipe carries the re-hosted <img> tag instead. Image markers must be
	// excluded from the fingerprint on both sides, so the two still hash equal —
	// otherwise every imaged recipe would diverge from its note on the next pull.
	r := &models.Recipe{
		Title:       "Брамбораки",
		Ingredients: []models.IngredientBlock{{Items: []string{"картофель"}}},
		StepsHTML:   `<p>Натереть.</p><img src="/uploads/abc.jpg" alt=""><p>Жарить.</p>`,
	}
	// The note carries its inline image in Images (as recordToNote populates it from
	// the body markers); both sides then have one image, so they hash equal.
	n := Note{
		Title:      "Брамбораки",
		Checklists: [][]string{{"картофель"}},
		BodyHTML:   "Натереть.\n@@IMG:CF8663E7-8ED6-44DA-8B6A-B508AAFBC808@@\nЖарить.",
		Images:     []NoteImage{{ID: "CF8663E7-8ED6-44DA-8B6A-B508AAFBC808"}},
	}
	if HashRecipe(r) != HashNote(n) {
		t.Fatalf("imaged hash mismatch:\n recipe=%s\n note  =%s", HashRecipe(r), HashNote(n))
	}
}

// Adding an image is a detected change (the user's case): the same recipe with vs
// without an inline <img> must hash differently so the push is triggered.
func TestHashRecipeImageCountChange(t *testing.T) {
	base := &models.Recipe{Title: "Торт", StepsHTML: "<p>Испечь.</p>"}
	withImg := &models.Recipe{Title: "Торт", StepsHTML: `<p>Испечь.</p><img src="/uploads/x.jpg">`}
	if HashRecipe(base) == HashRecipe(withImg) {
		t.Fatal("adding an image must change the recipe hash")
	}
	// The image-less hash must be unchanged by the count addition (no rebaseline):
	// it equals the hash of the equivalent note with no images.
	n := Note{Title: "Торт", BodyHTML: "Испечь."}
	if HashRecipe(base) != HashNote(n) {
		t.Fatalf("image-less hash drifted:\n recipe=%s\n note  =%s", HashRecipe(base), HashNote(n))
	}
}

func TestHashChangesWithContent(t *testing.T) {
	r := &models.Recipe{Title: "A", StepsHTML: "<p>x</p>"}
	r2 := &models.Recipe{Title: "A", StepsHTML: "<p>y</p>"}
	if HashRecipe(r) == HashRecipe(r2) {
		t.Fatal("expected different hashes for different steps")
	}
}
