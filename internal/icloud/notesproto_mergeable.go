package icloud

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// This file encodes the Apple Notes "mergeable" (CRDT) note body that modern
// Notes clients actually render — the legacy NoteStoreProto (notesproto_encode.go)
// is accepted by the server but ignored on devices. The byte layout was reverse-
// engineered from real iCloud web-app saves (a plain create and a checklist edit).
//
// note proto: f2=note_text, f3=REPEATED run, f4=object table, f5=REPEATED paragraph
// style (a run-length encoding over the text). We synthesize a fresh single-replica
// document; updates are handled by delete+create (see PushNote), so we never have to
// maintain CRDT version vectors across edits.
//
// run: f1=charRef{f1=id,f2=pos}, f2=length, f3=paraRef{f1=id,f2=selector}, f5=op
//   anchor:     charRef{0,0}      len0  paraRef{0,0}      op1
//   content:    charRef{1,start}  len   paraRef{1,sel}    opN   (sel 1 = checklist)
//   terminator: charRef{0,2^32-1} len0  paraRef{0,2^32-1} (no op)
// f5 entry: f1=runeLength, optional f2=paraStyle{f1=style_type, f2=4,
//   optional f5={f1=16-byte object UUID, f2=0}}. Default paragraphs are bare
//   {len}; each checklist line is its own {len,{103,...,uuid}}; the final empty
//   paragraph is the explicit tail {1,{0,4}}.
// object table f4: f1={f1=16-byte replica UUID, f2={f1=textRunes}, f2={f1=objCount}}.

const eosSentinel = 0xFFFFFFFF // end-of-sequence position in anchor/terminator refs

// newNoteUUID returns a fresh 16-byte object/replica id. It is a seam so tests can
// inject deterministic ids and byte-match real captures.
var newNoteUUID = func() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// mPara is one paragraph with its style class.
type mPara struct {
	text      string
	checklist bool
}

// mergeableParagraphs maps a recipe to ordered paragraphs: title, then ingredient
// blocks (a "# subtitle" becomes a non-checklist "subtitle:" line, items are
// checklist lines), then step lines. Mirrors encodeNoteBody / groupParagraphs.
func mergeableParagraphs(title string, ingredientBlocks [][]string, steps string) []mPara {
	paras := []mPara{{title, false}}
	for _, block := range ingredientBlocks {
		for _, item := range block {
			if sub, ok := strings.CutPrefix(item, "# "); ok {
				paras = append(paras, mPara{sub + ":", false})
				continue
			}
			paras = append(paras, mPara{item, true})
		}
	}
	for _, line := range strings.Split(steps, "\n") {
		if strings.TrimSpace(line) != "" {
			paras = append(paras, mPara{line, false})
		}
	}
	return paras
}

func putRef(f1, f2 uint64) []byte {
	var r []byte
	r = protowire.AppendTag(r, 1, protowire.VarintType)
	r = protowire.AppendVarint(r, f1)
	r = protowire.AppendTag(r, 2, protowire.VarintType)
	r = protowire.AppendVarint(r, f2)
	return r
}

func appendRun(note []byte, charID, charPos, length, paraID, paraSel uint64, op uint64, hasOp bool) []byte {
	var run []byte
	run = protowire.AppendTag(run, 1, protowire.BytesType)
	run = protowire.AppendBytes(run, putRef(charID, charPos))
	run = protowire.AppendTag(run, 2, protowire.VarintType)
	run = protowire.AppendVarint(run, length)
	run = protowire.AppendTag(run, 3, protowire.BytesType)
	run = protowire.AppendBytes(run, putRef(paraID, paraSel))
	if hasOp {
		run = protowire.AppendTag(run, 5, protowire.VarintType)
		run = protowire.AppendVarint(run, op)
	}
	note = protowire.AppendTag(note, 3, protowire.BytesType)
	return protowire.AppendBytes(note, run)
}

// appendParaStyle appends one f5 entry covering length runes. styleType < 0 emits a
// bare default entry (no paragraph style); otherwise it emits {len,{styleType,4[,uuid]}}.
func appendParaStyle(note []byte, length uint64, styleType int, uuid []byte) []byte {
	var entry []byte
	entry = protowire.AppendTag(entry, 1, protowire.VarintType)
	entry = protowire.AppendVarint(entry, length)
	if styleType >= 0 {
		var style []byte
		style = protowire.AppendTag(style, 1, protowire.VarintType)
		style = protowire.AppendVarint(style, uint64(styleType))
		style = protowire.AppendTag(style, 2, protowire.VarintType)
		style = protowire.AppendVarint(style, 4)
		if uuid != nil {
			var obj []byte
			obj = protowire.AppendTag(obj, 1, protowire.BytesType)
			obj = protowire.AppendBytes(obj, uuid)
			obj = protowire.AppendTag(obj, 2, protowire.VarintType)
			obj = protowire.AppendVarint(obj, 0)
			style = protowire.AppendTag(style, 5, protowire.BytesType)
			style = protowire.AppendBytes(style, obj)
		}
		entry = protowire.AppendTag(entry, 2, protowire.BytesType)
		entry = protowire.AppendBytes(entry, style)
	}
	note = protowire.AppendTag(note, 5, protowire.BytesType)
	return protowire.AppendBytes(note, entry)
}

func appendObjectTable(note []byte, uuid []byte, textRunes, objCount uint64) []byte {
	counter := func(v uint64) []byte {
		var c []byte
		c = protowire.AppendTag(c, 1, protowire.VarintType)
		return protowire.AppendVarint(c, v)
	}
	var entry []byte
	entry = protowire.AppendTag(entry, 1, protowire.BytesType)
	entry = protowire.AppendBytes(entry, uuid)
	entry = protowire.AppendTag(entry, 2, protowire.BytesType)
	entry = protowire.AppendBytes(entry, counter(textRunes))
	entry = protowire.AppendTag(entry, 2, protowire.BytesType)
	entry = protowire.AppendBytes(entry, counter(objCount))

	var tbl []byte
	tbl = protowire.AppendTag(tbl, 1, protowire.BytesType)
	tbl = protowire.AppendBytes(tbl, entry)
	note = protowire.AppendTag(note, 4, protowire.BytesType)
	return protowire.AppendBytes(note, tbl)
}

// buildMergeableNoteProto returns the uncompressed top-level mergeable note proto.
// It is separated from compression so tests can byte-match it against real captures
// (zlib output itself is not reproducible).
func buildMergeableNoteProto(title string, ingredientBlocks [][]string, steps string) []byte {
	paras := mergeableParagraphs(title, ingredientBlocks, steps)

	// note_text: each paragraph + "\n", then one trailing "\n" (empty paragraph).
	var sb strings.Builder
	type seg struct {
		runes     int
		checklist bool
	}
	var segs []seg
	for _, p := range paras {
		line := p.text + "\n"
		sb.WriteString(line)
		segs = append(segs, seg{utf8.RuneCountInString(line), p.checklist})
	}
	sb.WriteString("\n")
	text := sb.String()
	textRunes := utf8.RuneCountInString(text)

	var note []byte
	note = protowire.AppendTag(note, 2, protowire.BytesType)
	note = protowire.AppendBytes(note, []byte(text)) // note_text

	// Runs (f3): anchor, then RLE of segments by class (the trailing empty paragraph
	// is default and merges into the last run), terminator.
	note = appendRun(note, 0, 0, 0, 0, 0, 1, true)
	op := uint64(2)
	pos := uint64(0)
	runSegs := append(append([]seg{}, segs...), seg{1, false}) // + trailing empty paragraph
	for i := 0; i < len(runSegs); {
		cls := runSegs[i].checklist
		total := 0
		j := i
		for j < len(runSegs) && runSegs[j].checklist == cls {
			total += runSegs[j].runes
			j++
		}
		sel := uint64(0)
		if cls {
			sel = 1
		}
		note = appendRun(note, 1, pos, uint64(total), 1, sel, op, true)
		pos += uint64(total)
		op++
		i = j
	}
	note = appendRun(note, 0, eosSentinel, 0, 0, eosSentinel, 0, false)

	// Object table (f4): replica UUID + [textRunes, objCount].
	objCount := 0
	for _, s := range segs {
		if s.checklist {
			objCount++
		}
	}
	if objCount == 0 {
		objCount = 1
	}
	note = appendObjectTable(note, newNoteUUID(), uint64(textRunes), uint64(objCount))

	// Style table (f5): default groups as bare entries, each checklist line as its
	// own object entry, then the explicit tail for the trailing empty paragraph.
	for k := 0; k < len(segs); {
		if segs[k].checklist {
			note = appendParaStyle(note, uint64(segs[k].runes), checklistStyleType, newNoteUUID())
			k++
			continue
		}
		total := 0
		for k < len(segs) && !segs[k].checklist {
			total += segs[k].runes
			k++
		}
		note = appendParaStyle(note, uint64(total), -1, nil) // bare default
	}
	note = appendParaStyle(note, 1, 0, nil) // trailing empty paragraph, explicit type 0

	// Wrap: document{f1=0,f2=0,f3=note}, top{f1=0,f2=document}.
	var doc []byte
	doc = protowire.AppendTag(doc, 1, protowire.VarintType)
	doc = protowire.AppendVarint(doc, 0)
	doc = protowire.AppendTag(doc, 2, protowire.VarintType)
	doc = protowire.AppendVarint(doc, 0)
	doc = protowire.AppendTag(doc, 3, protowire.BytesType)
	doc = protowire.AppendBytes(doc, note)

	var top []byte
	top = protowire.AppendTag(top, 1, protowire.VarintType)
	top = protowire.AppendVarint(top, 0)
	top = protowire.AppendTag(top, 2, protowire.BytesType)
	top = protowire.AppendBytes(top, doc)
	return top
}

// encodeMergeableNoteBody builds the zlib-compressed mergeable note body for a recipe.
func encodeMergeableNoteBody(title string, ingredientBlocks [][]string, steps string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(buildMergeableNoteProto(title, ingredientBlocks, steps)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parseMergeableNoteBody decodes a mergeable note body back into ingredient blocks
// and steps (test/round-trip helper; the live pull path uses parseNoteBody). It maps
// the f5 paragraph-style RLE to a per-rune style array and reuses groupParagraphs.
func parseMergeableNoteBody(blob []byte) (blocks [][]string, steps string, ok bool) {
	data, err := inflate(blob)
	if err != nil {
		return nil, "", false
	}
	note := pbBytes(pbBytes(data, 2), 3)
	if note == nil {
		return nil, "", false
	}
	text := string(pbBytes(note, 2))
	if text == "" {
		return nil, "", false
	}
	runes := []rune(text)
	styleAt := make([]int, len(runes))
	for i := range styleAt {
		styleAt[i] = -1
	}
	pos := 0
	pbEachBytes(note, 5, func(entry []byte) {
		length := int(pbVarint(entry, 1))
		style := -1
		if st := pbBytes(entry, 2); st != nil {
			style = int(pbVarint(st, 1))
		}
		for j := 0; j < length && pos < len(styleAt); j++ {
			styleAt[pos] = style
			pos++
		}
	})
	return groupParagraphs(runes, styleAt, map[int]string{})
}
