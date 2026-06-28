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

// titleStyleType is the paragraph style_type Apple Notes uses for a note's title
// (its first paragraph), rendered as the large Title heading. Verified against real
// device notes: the title's f5 entry is {len,{style_type:0, f2:4}} with no object uuid.
const titleStyleType = 0

// newNoteUUID returns a fresh 16-byte object/replica id. It is a seam so tests can
// inject deterministic ids and byte-match real captures.
var newNoteUUID = func() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// mImage is one inline image to splice into the note body: an Attachment record id
// (referenced from the body) and its type UTI (e.g. "public.jpeg").
type mImage struct {
	attachmentID string
	uti          string
}

// mPara is one paragraph. An image paragraph (image != nil) is the single U+FFFC
// object-replacement rune that Apple Notes renders as the inline attachment.
type mPara struct {
	text      string
	checklist bool
	image     *mImage
}

// objReplacement is the U+FFFC placeholder that marks an inline attachment position.
const objReplacement = "￼"

// mergeableParagraphs maps a recipe to ordered paragraphs: title, then ingredient
// blocks (a "# subtitle" becomes a non-checklist "subtitle:" line, items are
// checklist lines), then step lines. Inline-image markers (@@IMG:...@@, imgTokenRE)
// in the steps become their own image paragraphs, consuming images in order.
// Mirrors encodeNoteBody / groupParagraphs (and parseNoteBody on the read side).
func mergeableParagraphs(title string, ingredientBlocks [][]string, steps string, images map[string]mImage) []mPara {
	paras := []mPara{{text: title}}
	for _, block := range ingredientBlocks {
		for _, item := range block {
			if sub, ok := strings.CutPrefix(item, "# "); ok {
				paras = append(paras, mPara{text: sub + ":"})
				continue
			}
			paras = append(paras, mPara{text: item, checklist: true})
		}
	}
	for _, line := range strings.Split(steps, "\n") {
		parts := imgTokenRE.Split(line, -1)
		markers := imgTokenRE.FindAllString(line, -1)
		for i, part := range parts {
			if strings.TrimSpace(part) != "" {
				paras = append(paras, mPara{text: part})
			}
			if i < len(markers) {
				// Marker is @@IMG:<id>@@; look the image up by id. An unknown id (e.g.
				// the file was missing or its upload failed) drops the marker silently.
				id := strings.TrimSuffix(strings.TrimPrefix(markers[i], "@@IMG:"), "@@")
				if img, ok := images[id]; ok {
					img := img
					paras = append(paras, mPara{text: objReplacement, image: &img})
				}
			}
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

// appendAttachmentEntry appends an f5 entry for a one-rune (U+FFFC) inline image:
// {f1:1, f12:{f1:attachmentID, f2:type_uti}} — exactly what parseNoteBody reads as an
// attachment_info on a field-5 entry.
func appendAttachmentEntry(note []byte, attachmentID, uti string) []byte {
	var ai []byte
	ai = protowire.AppendTag(ai, 1, protowire.BytesType)
	ai = protowire.AppendBytes(ai, []byte(attachmentID))
	ai = protowire.AppendTag(ai, 2, protowire.BytesType)
	ai = protowire.AppendBytes(ai, []byte(uti))

	var entry []byte
	entry = protowire.AppendTag(entry, 1, protowire.VarintType)
	entry = protowire.AppendVarint(entry, 1) // the single U+FFFC rune
	entry = protowire.AppendTag(entry, 12, protowire.BytesType)
	entry = protowire.AppendBytes(entry, ai)

	note = protowire.AppendTag(note, 5, protowire.BytesType)
	return protowire.AppendBytes(note, entry)
}

// objTableEntry builds one f4 object-table entry for a replica:
// {f1:16-byte replicaUUID, f2:{f1:counter1}, f2:{f1:counter2}}.
func objTableEntry(uuid []byte, c1, c2 uint64) []byte {
	counter := func(v uint64) []byte {
		var c []byte
		c = protowire.AppendTag(c, 1, protowire.VarintType)
		return protowire.AppendVarint(c, v)
	}
	var entry []byte
	entry = protowire.AppendTag(entry, 1, protowire.BytesType)
	entry = protowire.AppendBytes(entry, uuid)
	entry = protowire.AppendTag(entry, 2, protowire.BytesType)
	entry = protowire.AppendBytes(entry, counter(c1))
	entry = protowire.AppendTag(entry, 2, protowire.BytesType)
	entry = protowire.AppendBytes(entry, counter(c2))
	return entry
}

// appendObjectTable appends note.f4 (the per-replica version-vector table) from the
// given entries. Each entry is either freshly built by objTableEntry or a foreign
// replica entry passed through verbatim (preserving another device's clock on an
// in-place update).
func appendObjectTable(note []byte, entries [][]byte) []byte {
	var tbl []byte
	for _, e := range entries {
		tbl = protowire.AppendTag(tbl, 1, protowire.BytesType)
		tbl = protowire.AppendBytes(tbl, e)
	}
	note = protowire.AppendTag(note, 4, protowire.BytesType)
	return protowire.AppendBytes(note, tbl)
}

// replicaState is the f4 object-table state to emit on an in-place update: the
// foreign replica entries to preserve verbatim (other devices' clocks), plus our own
// replica's UUID and the base counters to advance from (our replica's current values
// in the note, or zero if absent). The encoder sets our emitted counters to
// base + current content size, which is strictly greater than the base — so the
// resulting version vector dominates the device's last-seen state and the update
// propagates. A nil *replicaState means a fresh document (single new replica clocked
// at the current content size) — the create path.
type replicaState struct {
	foreign        [][]byte // opaque foreign replica entries, emitted byte-for-byte
	uuid           []byte   // our replica UUID
	baseC1, baseC2 uint64   // our replica's current counters (advanced by content size)
}

// buildMergeableNoteProto returns the uncompressed top-level mergeable note proto.
// It is separated from compression so tests can byte-match it against real captures
// (zlib output itself is not reproducible). rep is nil for a create (fresh single
// replica) or carries the preserved+advanced version vector for an in-place update.
func buildMergeableNoteProto(title string, ingredientBlocks [][]string, steps string, images map[string]mImage, rep *replicaState) []byte {
	paras := mergeableParagraphs(title, ingredientBlocks, steps, images)

	// note_text: each paragraph + "\n", then one trailing "\n" (empty paragraph).
	var sb strings.Builder
	type seg struct {
		runes     int
		checklist bool
		image     *mImage
	}
	var segs []seg
	for _, p := range paras {
		line := p.text + "\n"
		sb.WriteString(line)
		segs = append(segs, seg{utf8.RuneCountInString(line), p.checklist, p.image})
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
	runSegs := append(append([]seg{}, segs...), seg{1, false, nil}) // + trailing empty paragraph
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

	// Object table (f4): replica UUID + [textRunes, objCount]. Each checklist item and
	// each inline image is an object.
	objCount := 0
	for _, s := range segs {
		if s.checklist || s.image != nil {
			objCount++
		}
	}
	if objCount == 0 {
		objCount = 1
	}
	var objEntries [][]byte
	if rep == nil {
		// Fresh document: a single new replica clocked at the current content size.
		objEntries = [][]byte{objTableEntry(newNoteUUID(), uint64(textRunes), uint64(objCount))}
	} else {
		// In-place update: keep every other device's replica entry verbatim and add our
		// own advanced past its base (base + content size > base), so the vector strictly
		// dominates the device's last-seen and the update propagates.
		objEntries = append(objEntries, rep.foreign...)
		objEntries = append(objEntries,
			objTableEntry(rep.uuid, rep.baseC1+uint64(textRunes), rep.baseC2+uint64(objCount)))
	}
	note = appendObjectTable(note, objEntries)

	// Style table (f5): the title (first paragraph) carries the Apple "Title" style; then
	// default groups as bare entries, each checklist line as its own object entry, each
	// inline image as an attachment entry (U+FFFC) + a bare entry for its trailing newline,
	// then the explicit tail for the trailing empty paragraph.
	k := 0
	if len(segs) > 0 {
		note = appendParaStyle(note, uint64(segs[0].runes), titleStyleType, nil) // Title
		k = 1
	}
	for k < len(segs) {
		if segs[k].image != nil {
			note = appendAttachmentEntry(note, segs[k].image.attachmentID, segs[k].image.uti) // U+FFFC
			note = appendParaStyle(note, uint64(segs[k].runes-1), -1, nil)                    // its "\n"
			k++
			continue
		}
		if segs[k].checklist {
			note = appendParaStyle(note, uint64(segs[k].runes), checklistStyleType, newNoteUUID())
			k++
			continue
		}
		total := 0
		for k < len(segs) && !segs[k].checklist && segs[k].image == nil {
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
// images are spliced in at the @@IMG@@ markers in steps, in order.
func encodeMergeableNoteBody(title string, ingredientBlocks [][]string, steps string, images map[string]mImage, rep *replicaState) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(buildMergeableNoteProto(title, ingredientBlocks, steps, images, rep)); err != nil {
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

// parseObjectTable extracts the f4 object-table replica entries from a mergeable
// note body, each as a copied opaque byte slice (to be re-emitted verbatim on an
// in-place update). ok is false when the body can't be inflated/parsed or carries
// no object table — the caller then cannot build a dominating version vector and
// must fall back (soft-delete + recreate).
func parseObjectTable(blob []byte) (entries [][]byte, ok bool) {
	data, err := inflate(blob)
	if err != nil {
		return nil, false
	}
	note := pbBytes(pbBytes(data, 2), 3) // NoteStoreProto.document=2, Document.note=3
	tbl := pbBytes(note, 4)              // Note.object_table=4
	if tbl == nil {
		return nil, false
	}
	pbEachBytes(tbl, 1, func(e []byte) {
		entries = append(entries, append([]byte(nil), e...)) // copy: e aliases data
	})
	if len(entries) == 0 {
		return nil, false
	}
	return entries, true
}

// objEntryUUID returns the 16-byte replica UUID of an object-table entry, or nil.
func objEntryUUID(entry []byte) []byte { return pbBytes(entry, 1) }

// objEntryCounters returns the two version counters (f2 repeated twice, each {f1}).
func objEntryCounters(entry []byte) (c1, c2 uint64) {
	var cs []uint64
	pbEachBytes(entry, 2, func(b []byte) { cs = append(cs, pbVarint(b, 1)) })
	if len(cs) > 0 {
		c1 = cs[0]
	}
	if len(cs) > 1 {
		c2 = cs[1]
	}
	return
}
