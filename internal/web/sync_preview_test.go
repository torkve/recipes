package web

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"recipes/internal/notesync"
)

// The push-preview route is auth-gated and, when sync is disabled (engine nil,
// as in the test server), 404s — matching the other /admin/sync handlers.
func TestSyncPushPreviewRouteGuards(t *testing.T) {
	ts, _ := testServer(t)

	t.Run("requires auth", func(t *testing.T) {
		c := newClient(t)
		resp, err := c.Get(ts.URL + "/admin/sync/push/preview")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/login" {
			t.Fatalf("got %d -> %q, want 303 -> /admin/login", resp.StatusCode, resp.Header.Get("Location"))
		}
	})

	t.Run("404 when sync disabled", func(t *testing.T) {
		c := newClient(t)
		login(t, c, ts.URL)
		resp, err := c.Get(ts.URL + "/admin/sync/push/preview")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("got %d, want 404", resp.StatusCode)
		}
	})
}

// pushItem builds a preview item with a real diff via the exported LineDiff.
func pushItem(op, title string, remote, local []string) notesync.PushItem {
	return notesync.PushItem{Title: title, Op: op, Diff: notesync.LineDiff(remote, local)}
}

func renderPushPreview(t *testing.T, data pageData) string {
	t.Helper()
	tmpls, err := loadTemplates(templateFuncs())
	if err != nil {
		t.Fatal(err)
	}
	tmpl, ok := tmpls["admin_sync_push_preview"]
	if !ok {
		t.Fatal("admin_sync_push_preview template not found")
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestPushPreviewTemplateWithChanges(t *testing.T) {
	missing := pushItem("update", "Окрошка", nil, []string{"Шаги:", "Новый."})
	missing.Note = "будет создана заново"
	preview := notesync.PushPreview{
		Items: []notesync.PushItem{
			pushItem("create", "Винегрет", nil, []string{"Название: Винегрет", "Шаги:", "Нарезать."}),
			pushItem("update", "Борщ", []string{"Шаги:", "Варить."}, []string{"Шаги:", "Варить два часа."}),
			missing,
		},
		Skipped: 3,
	}
	out := renderPushPreview(t, pageData{"SiteName": "Тест", "Title": "Предпросмотр", "Preview": preview})

	for _, want := range []string{
		"Будут созданы заметки",
		"Будут перезаписаны заметки",
		"Подтвердить отправку",
		`action="/admin/sync/push"`,
		"Удаление заметок в iCloud не выполняется",
		"Нарезать.",          // create content
		"Варить два часа.",   // update addition
		"будет создана заново", // missing-remote flag
		"Без изменений: 3",
		"diff-add",
		"diff-del",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered preview missing %q", want)
		}
	}
}

func TestPushPreviewTemplateEmpty(t *testing.T) {
	out := renderPushPreview(t, pageData{"SiteName": "Тест", "Preview": notesync.PushPreview{Skipped: 5}})
	if !strings.Contains(out, "Нечего отправлять") {
		t.Errorf("empty preview should show nothing-to-send message:\n%s", out)
	}
	if strings.Contains(out, "Подтвердить отправку") {
		t.Errorf("empty preview must not offer a confirm button")
	}
}
