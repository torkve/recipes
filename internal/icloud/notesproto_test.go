package icloud

import (
	"bytes"
	"compress/gzip"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// buildNoteBlob assembles a minimal Apple Notes protobuf (note_text + attribute
// runs) and gzips it, mirroring TextDataEncrypted.
func buildNoteBlob(text string, runs [][2]int) []byte {
	var note []byte
	note = protowire.AppendTag(note, 2, protowire.BytesType) // note_text
	note = protowire.AppendBytes(note, []byte(text))
	for _, r := range runs { // r = {length, styleType}
		var ps []byte
		ps = protowire.AppendTag(ps, 1, protowire.VarintType) // style_type
		ps = protowire.AppendVarint(ps, uint64(r[1]))
		var run []byte
		run = protowire.AppendTag(run, 1, protowire.VarintType) // length
		run = protowire.AppendVarint(run, uint64(r[0]))
		run = protowire.AppendTag(run, 2, protowire.BytesType) // paragraph_style
		run = protowire.AppendBytes(run, ps)
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
	runs := [][2]int{
		{len([]rune("Брамбораки\n")), 0},   // title
		{len([]rune("Картофель\n")), 103},  // ingredient
		{len([]rune("Мука\n")), 103},       // ingredient
		{len([]rune("Смешать всё\n")), -1}, // step
	}
	blocks, steps, ok := parseNoteBody(buildNoteBlob(text, runs))
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
}

func TestParseNoteBodySubtitle(t *testing.T) {
	text := "Блинчики\nТесто:\nМука\nГотовим\n"
	runs := [][2]int{
		{len([]rune("Блинчики\n")), 0},
		{len([]rune("Тесто:\n")), -1}, // subtitle (ends with ':', precedes checklist)
		{len([]rune("Мука\n")), 103},
		{len([]rune("Готовим\n")), -1},
	}
	blocks, steps, ok := parseNoteBody(buildNoteBlob(text, runs))
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

func TestParseNoteBodyInvalid(t *testing.T) {
	if _, _, ok := parseNoteBody([]byte("not gzip")); ok {
		t.Fatal("expected parse failure on garbage")
	}
}
