package web

import (
	"bytes"
	"strings"
	"testing"

	"recipes/internal/notesync"
)

// The admin_sync conflicts section renders an enriched ConflictView: recipe title,
// the legend, and the colored local-vs-remote diff.
func TestAdminSyncConflictRenders(t *testing.T) {
	views := []notesync.ConflictView{{
		ID:     7,
		Title:  "Окрошка",
		Detail: "Заметка изменена в iCloud с момента последней синхронизации",
		Diff:   notesync.LineDiff([]string{"Шаги:", "Версия из iCloud."}, []string{"Шаги:", "Версия в приложении."}),
	}}

	tmpls, err := loadTemplates(templateFuncs())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpls["admin_sync"].ExecuteTemplate(&buf, "base", pageData{
		"SiteName": "Тест", "Conflicts": views, "Bound": false,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Окрошка",                                  // title
		`action="/admin/sync/conflicts/7/resolve"`, // resolve form
		"− в iCloud, + в приложении",               // legend
		"diff-del",                                 // remote line
		"diff-add",                                 // local line
		"Версия в приложении.",                     // local content
		"Версия из iCloud.",                        // remote content
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered conflicts missing %q", want)
		}
	}
}
