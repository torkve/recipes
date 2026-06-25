package icloud

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"regexp"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// Apple Notes "NoteStoreProto" field numbers (reverse-engineered, verified
// against live notes): NoteStoreProto.document=2, Document.note=3,
// Note.note_text=2, Note.attribute_run=5 (repeated),
// AttributeRun.length=1, AttributeRun.paragraph_style=2,
// ParagraphStyle.style_type=1, AttributeRun.attachment_info=12,
// AttachmentInfo.attachment_identifier=1, AttachmentInfo.type_uti=2.
// A style_type of 103 marks a checklist paragraph.
const checklistStyleType = 103

// galleryUTI is the type_uti of a scanned-document attachment. It contains child
// page attachments (each a raster image) rather than a single image; the provider
// expands it to its pages.
const galleryUTI = "com.apple.notes.gallery"

// isInlineImageUTI reports whether a note attachment should be imported as an
// image: a raster image (public.*) or a scanned-document gallery.
func isInlineImageUTI(uti string) bool {
	return strings.HasPrefix(uti, "public.") || uti == galleryUTI
}

// imgToken marks an inline image in the steps text; the sync engine substitutes
// it for the re-hosted <img> tag (and the hash projection strips it). The id is
// the image Attachment record's name, resolved to a download URL by the provider.
var imgTokenRE = regexp.MustCompile(`@@IMG:[^@]*@@`)

func imgToken(id string) string { return "@@IMG:" + id + "@@" }

// stripImgTokens removes any inline-image markers and trims the result, for
// paragraphs (titles, ingredients, subtitles) where a marker must not survive.
func stripImgTokens(s string) string {
	return strings.TrimSpace(imgTokenRE.ReplaceAllString(s, ""))
}

// inflate decompresses an Apple Notes blob (gzip, or raw zlib as a fallback).
func inflate(blob []byte) ([]byte, error) {
	if gr, err := gzip.NewReader(bytes.NewReader(blob)); err == nil {
		defer gr.Close()
		if out, err := io.ReadAll(gr); err == nil {
			return out, nil
		}
	}
	zr, err := zlib.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// pbBytes returns the first bytes-typed field with the given number.
func pbBytes(msg []byte, num protowire.Number) []byte {
	var out []byte
	pbWalk(msg, func(n protowire.Number, t protowire.Type, b []byte, _ uint64) {
		if n == num && t == protowire.BytesType {
			out = b
		}
	})
	return out
}

// pbVarint returns the first varint field with the given number.
func pbVarint(msg []byte, num protowire.Number) uint64 {
	var out uint64
	pbWalk(msg, func(n protowire.Number, t protowire.Type, _ []byte, v uint64) {
		if n == num && t == protowire.VarintType {
			out = v
		}
	})
	return out
}

// pbEachBytes calls fn for every bytes-typed field with the given number.
func pbEachBytes(msg []byte, num protowire.Number, fn func([]byte)) {
	pbWalk(msg, func(n protowire.Number, t protowire.Type, b []byte, _ uint64) {
		if n == num && t == protowire.BytesType {
			fn(b)
		}
	})
}

// pbWalk iterates the fields of a protobuf message, tolerating malformed input.
func pbWalk(msg []byte, fn func(num protowire.Number, typ protowire.Type, b []byte, v uint64)) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return
		}
		msg = msg[n:]
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(msg)
			if m < 0 {
				return
			}
			fn(num, typ, nil, v)
			msg = msg[m:]
		case protowire.BytesType:
			b, m := protowire.ConsumeBytes(msg)
			if m < 0 {
				return
			}
			fn(num, typ, b, 0)
			msg = msg[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, msg)
			if m < 0 {
				return
			}
			msg = msg[m:]
		}
	}
}

// parseNoteBody decodes an Apple Notes TextDataEncrypted blob into ingredient
// blocks (checklist lines, with an optional "# subtitle" leading item), the
// remaining body as plain-text steps (with @@IMG:id@@ markers where inline images
// appear), and the ordered ids of those images. ok is false when the blob can't
// be parsed (caller falls back to the snippet).
func parseNoteBody(blob []byte) (ingredientBlocks [][]string, steps string, imageIDs []string, ok bool) {
	data, err := inflate(blob)
	if err != nil {
		return nil, "", nil, false
	}
	note := pbBytes(pbBytes(data, 2), 3) // NoteStoreProto.document.note
	if note == nil {
		return nil, "", nil, false
	}
	text := string(pbBytes(note, 2)) // note_text
	if text == "" {
		return nil, "", nil, false
	}

	// Style and any inline-image attachment per character, from the ordered
	// attribute runs (lengths are ~rune counts for the text we handle).
	runes := []rune(text)
	styleAt := make([]int, len(runes))
	for i := range styleAt {
		styleAt[i] = -1
	}
	attachAt := map[int]string{}
	pos := 0
	pbEachBytes(note, 5, func(run []byte) {
		length := int(pbVarint(run, 1))
		style := -1
		if ps := pbBytes(run, 2); ps != nil {
			style = int(pbVarint(ps, 1))
		}
		// attachment_info (field 12): only image attachments are inlined
		// (raster images and scanned-document galleries); tables/hashtags/
		// drawings are left to be stripped out.
		var attachID string
		if ai := pbBytes(run, 12); ai != nil {
			if isInlineImageUTI(string(pbBytes(ai, 2))) {
				attachID = string(pbBytes(ai, 1))
			}
		}
		for j := 0; j < length && pos < len(styleAt); j++ {
			styleAt[pos] = style
			if attachID != "" {
				attachAt[pos] = attachID
			}
			pos++
		}
	})

	blocks, steps, ok := groupParagraphs(runes, styleAt, attachAt)
	if !ok {
		return nil, "", nil, false
	}

	// Every inline image must survive into the steps body. groupParagraphs keeps
	// markers that fall in step paragraphs but drops those in the title (first
	// paragraph) or ingredient lines — common for a "hero" image at the top of a
	// note. Collect all image ids in document order (deduped) and prepend any
	// whose marker didn't make it into steps, so none are lost.
	seen := map[string]bool{}
	var missing []string
	for pos := 0; pos < len(runes); pos++ {
		id := attachAt[pos]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		imageIDs = append(imageIDs, id)
		if !strings.Contains(steps, imgToken(id)) {
			missing = append(missing, imgToken(id))
		}
	}
	if len(missing) > 0 {
		steps = strings.TrimSpace(strings.Join(missing, "\n") + "\n" + steps)
	}
	return blocks, steps, imageIDs, true
}

// groupParagraphs splits the styled text into paragraphs and maps them to
// ingredient blocks (checklist paragraphs) and steps (the rest), skipping the
// title (first paragraph).
func groupParagraphs(runes []rune, styleAt []int, attachAt map[int]string) (blocks [][]string, steps string, ok bool) {
	type para struct {
		text  string
		style int
	}
	var paras []para
	start := 0
	emit := func(end int) {
		var sb strings.Builder
		for p := start; p < end; p++ {
			r := runes[p]
			if r == '￼' { // attachment placeholder
				if id := attachAt[p]; id != "" {
					sb.WriteString(imgToken(id)) // inline image -> marker
				}
				continue // non-image attachments are dropped
			}
			sb.WriteRune(r)
		}
		style := -1
		if start < len(styleAt) {
			style = styleAt[start]
		}
		paras = append(paras, para{strings.TrimSpace(sb.String()), style})
	}
	for i, r := range runes {
		if r == '\n' {
			emit(i)
			start = i + 1
		}
	}
	emit(len(runes))

	var stepLines []string
	var cur []string
	var pendingSubtitle string
	flushSubtitle := func() {
		if pendingSubtitle != "" {
			stepLines = append(stepLines, pendingSubtitle+":")
			pendingSubtitle = ""
		}
	}
	flushBlock := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = nil
		}
	}

	for i, p := range paras {
		if i == 0 || p.text == "" { // skip title and blank lines
			continue
		}
		if p.style == checklistStyleType {
			if cur == nil && pendingSubtitle != "" {
				cur = append(cur, "# "+pendingSubtitle)
				pendingSubtitle = ""
			}
			cur = append(cur, stripImgTokens(p.text)) // ingredients are plain text
			continue
		}
		flushBlock()
		flushSubtitle()
		if clean := stripImgTokens(p.text); strings.HasSuffix(clean, ":") {
			pendingSubtitle = strings.TrimSuffix(clean, ":")
		} else {
			stepLines = append(stepLines, p.text) // keep image markers in steps
		}
	}
	flushBlock()
	flushSubtitle()

	if len(blocks) == 0 && len(stepLines) == 0 {
		return nil, "", false
	}
	return blocks, strings.Join(stepLines, "\n"), true
}
