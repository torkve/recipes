package icloud

import (
	"bytes"
	"compress/gzip"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// noteRun describes one attribute run for buildNoteBlob: a rune length, a
// paragraph style_type, and (optionally) an inline attachment id + type UTI.
type noteRun struct {
	length    int
	styleType int
	attachID  string
	attachUTI string
}

// buildNoteBlob assembles a minimal Apple Notes protobuf (note_text + attribute
// runs) and gzips it, mirroring TextDataEncrypted.
func buildNoteBlob(text string, runs []noteRun) []byte {
	var note []byte
	note = protowire.AppendTag(note, 2, protowire.BytesType) // note_text
	note = protowire.AppendBytes(note, []byte(text))
	for _, r := range runs {
		var ps []byte
		ps = protowire.AppendTag(ps, 1, protowire.VarintType) // style_type
		ps = protowire.AppendVarint(ps, uint64(r.styleType))
		var run []byte
		run = protowire.AppendTag(run, 1, protowire.VarintType) // length
		run = protowire.AppendVarint(run, uint64(r.length))
		run = protowire.AppendTag(run, 2, protowire.BytesType) // paragraph_style
		run = protowire.AppendBytes(run, ps)
		if r.attachID != "" {
			var ai []byte
			ai = protowire.AppendTag(ai, 1, protowire.BytesType) // attachment_identifier
			ai = protowire.AppendBytes(ai, []byte(r.attachID))
			ai = protowire.AppendTag(ai, 2, protowire.BytesType) // type_uti
			ai = protowire.AppendBytes(ai, []byte(r.attachUTI))
			run = protowire.AppendTag(run, 12, protowire.BytesType) // attachment_info
			run = protowire.AppendBytes(run, ai)
		}
		note = protowire.AppendTag(note, 5, protowire.BytesType) // attribute_run
		note = protowire.AppendBytes(note, run)
	}
	var doc []byte
	doc = protowire.AppendTag(doc, 3, protowire.BytesType) // Document.note
	doc = protowire.AppendBytes(doc, note)
	var top []byte
	top = protowire.AppendTag(top, 2, protowire.BytesType) // NoteStoreProto.document
	top = protowire.AppendBytes(top, doc)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(top)
	_ = gw.Close()
	return buf.Bytes()
}

func TestParseNoteBodyChecklistsAndSteps(t *testing.T) {
	text := "Брамбораки\nКартофель\nМука\nСмешать всё\n"
	runs := []noteRun{
		{length: len([]rune("Брамбораки\n")), styleType: 0},   // title
		{length: len([]rune("Картофель\n")), styleType: 103},  // ingredient
		{length: len([]rune("Мука\n")), styleType: 103},       // ingredient
		{length: len([]rune("Смешать всё\n")), styleType: -1}, // step
	}
	blocks, steps, imgs, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	want := [][]string{{"Картофель", "Мука"}}
	if !reflect.DeepEqual(blocks, want) {
		t.Fatalf("blocks=%v want %v", blocks, want)
	}
	if steps != "Смешать всё" {
		t.Fatalf("steps=%q", steps)
	}
	if len(imgs) != 0 {
		t.Fatalf("unexpected images: %v", imgs)
	}
}

func TestParseNoteBodySubtitle(t *testing.T) {
	text := "Блинчики\nТесто:\nМука\nГотовим\n"
	runs := []noteRun{
		{length: len([]rune("Блинчики\n")), styleType: 0},
		{length: len([]rune("Тесто:\n")), styleType: -1}, // subtitle (ends with ':', precedes checklist)
		{length: len([]rune("Мука\n")), styleType: 103},
		{length: len([]rune("Готовим\n")), styleType: -1},
	}
	blocks, steps, _, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	want := [][]string{{"# Тесто", "Мука"}}
	if !reflect.DeepEqual(blocks, want) {
		t.Fatalf("blocks=%v want %v", blocks, want)
	}
	if steps != "Готовим" {
		t.Fatalf("steps=%q", steps)
	}
}

// An inline image (a U+FFFC in a step paragraph with a public.* attachment)
// becomes an @@IMG:id@@ marker in steps and is reported in imageIDs; a non-image
// attachment (e.g. a table) is dropped without a marker.
func TestParseNoteBodyInlineImage(t *testing.T) {
	text := "Пирог\nПеремешать ￼ дальше\n￼\n"
	runs := []noteRun{
		{length: len([]rune("Пирог\n")), styleType: 0},
		{length: len([]rune("Перемешать ")), styleType: -1},
		{length: 1, styleType: -1, attachID: "IMG-AAA", attachUTI: "public.jpeg"},
		{length: len([]rune(" дальше\n")), styleType: -1},
		{length: 1, styleType: -1, attachID: "TBL-BBB", attachUTI: "com.apple.notes.table"},
		{length: 1, styleType: -1}, // trailing newline of the table line
	}
	blocks, steps, imgs, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	if len(blocks) != 0 {
		t.Fatalf("unexpected blocks: %v", blocks)
	}
	wantSteps := "Перемешать @@IMG:IMG-AAA@@ дальше"
	if steps != wantSteps {
		t.Fatalf("steps=%q want %q", steps, wantSteps)
	}
	if !reflect.DeepEqual(imgs, []string{"IMG-AAA"}) {
		t.Fatalf("imageIDs=%v want [IMG-AAA]", imgs)
	}
}

// A "hero" image at the very top of the note sits in the title paragraph (which
// groupParagraphs skips). Its marker must still be preserved into steps and the
// id reported, otherwise the whole resolve/download chain never runs.
func TestParseNoteBodyTopImagePreserved(t *testing.T) {
	text := "￼\nБрамбораки\nНатереть картофель\n"
	runs := []noteRun{
		{length: 1, styleType: 0, attachID: "HERO-1", attachUTI: "public.jpeg"}, // image
		{length: len([]rune("\nБрамбораки\n")), styleType: 0},                   // title line
		{length: len([]rune("Натереть картофель\n")), styleType: -1},            // step
	}
	_, steps, imgs, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	if !reflect.DeepEqual(imgs, []string{"HERO-1"}) {
		t.Fatalf("imageIDs=%v want [HERO-1]", imgs)
	}
	if !strings.Contains(steps, "@@IMG:HERO-1@@") {
		t.Fatalf("top image marker not preserved in steps: %q", steps)
	}
}

// An image in an ingredient (checklist) line must also be preserved into steps
// rather than stripped, and reported.
func TestParseNoteBodyIngredientImagePreserved(t *testing.T) {
	text := "Торт\nкорж ￼\nИспечь\n"
	runs := []noteRun{
		{length: len([]rune("Торт\n")), styleType: 0},
		{length: len([]rune("корж ")), styleType: 103},
		{length: 1, styleType: 103, attachID: "ING-1", attachUTI: "public.png"},
		{length: len([]rune("\nИспечь\n")), styleType: -1},
	}
	_, steps, imgs, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	if !reflect.DeepEqual(imgs, []string{"ING-1"}) {
		t.Fatalf("imageIDs=%v want [ING-1]", imgs)
	}
	if !strings.Contains(steps, "@@IMG:ING-1@@") {
		t.Fatalf("ingredient image marker not preserved: %q", steps)
	}
}

// A scanned-document (gallery) attachment in the body must emit a marker just
// like a raster image (resolution to its pages happens later, in the provider).
func TestParseNoteBodyGalleryMarker(t *testing.T) {
	text := "Скан\nстраница ￼\n"
	runs := []noteRun{
		{length: len([]rune("Скан\n")), styleType: 0},
		{length: len([]rune("страница ")), styleType: -1},
		{length: 1, styleType: -1, attachID: "GAL-1", attachUTI: "com.apple.notes.gallery"},
		{length: len([]rune("\n")), styleType: -1},
	}
	_, steps, imgs, ok := parseNoteBody(buildNoteBlob(text, runs))
	if !ok {
		t.Fatal("parse failed")
	}
	if !reflect.DeepEqual(imgs, []string{"GAL-1"}) {
		t.Fatalf("imageIDs=%v want [GAL-1]", imgs)
	}
	if !strings.Contains(steps, "@@IMG:GAL-1@@") {
		t.Fatalf("gallery marker not emitted: %q", steps)
	}
}

func TestParseNoteBodyInvalid(t *testing.T) {
	if _, _, _, ok := parseNoteBody([]byte("not gzip")); ok {
		t.Fatal("expected parse failure on garbage")
	}
}
