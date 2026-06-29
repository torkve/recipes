package icloudadapter

import (
	"regexp"
	"strings"

	"github.com/torkve/icloud-notes/notes"

	"recipes/internal/notesync"
)

// imgTokenRE matches the inline-image marker the recipe engine uses in note body
// text. The id is the image's identity (a re-hosted filename on push, an
// attachment record id on pull).
var imgTokenRE = regexp.MustCompile(`@@IMG:[^@]*@@`)

func imgToken(id string) string { return "@@IMG:" + id + "@@" }

func markerID(marker string) string {
	return strings.TrimSuffix(strings.TrimPrefix(marker, "@@IMG:"), "@@")
}

// toNote projects an engine note into the library's structured model: ingredient
// checklists and step lines become body paragraphs, and inline-image markers
// become attachment paragraphs whose bytes travel in Images.
func toNote(n notesync.Note) notes.Note {
	out := notes.Note{
		ID:       notes.NoteID(n.ID),
		FolderID: notes.FolderID(n.FolderID),
		Etag:     notes.Etag(n.Etag),
		Title:    n.Title,
		Body:     notes.Body{Paragraphs: toParagraphs(n.Checklists, n.BodyHTML)},
		RawBody:  n.RawBody,
	}
	for _, im := range n.Images {
		out.Images = append(out.Images, notes.Image{
			ID: im.ID, Ref: im.Ref, ContentType: im.ContentType, Data: im.Data,
		})
	}
	return out
}

// toParagraphs builds body paragraphs from ingredient blocks and step text. A
// "# subtitle" ingredient becomes a plain "subtitle:" paragraph; other
// ingredients become checklist items; step lines split on inline-image markers
// into text paragraphs and attachment paragraphs.
func toParagraphs(checklists [][]string, steps string) []notes.Paragraph {
	var ps []notes.Paragraph
	for _, block := range checklists {
		for _, item := range block {
			if sub, ok := strings.CutPrefix(item, "# "); ok {
				ps = append(ps, notes.Paragraph{Text: sub + ":"})
				continue
			}
			ps = append(ps, notes.Paragraph{Text: item, Style: notes.StyleChecklist})
		}
	}
	if steps == "" {
		return ps
	}
	for _, line := range strings.Split(steps, "\n") {
		parts := imgTokenRE.Split(line, -1)
		markers := imgTokenRE.FindAllString(line, -1)
		for i, part := range parts {
			if strings.TrimSpace(part) != "" {
				ps = append(ps, notes.Paragraph{Text: part})
			}
			if i < len(markers) {
				ps = append(ps, notes.Paragraph{Attachment: &notes.Attachment{ImageID: markerID(markers[i])}})
			}
		}
	}
	return ps
}

// fromNote projects a library note back into the engine model: body paragraphs
// become ingredient blocks and step lines, and attachment paragraphs become
// inline-image markers.
func fromNote(n notes.Note) notesync.Note {
	blocks, steps := fromParagraphs(n.Body.Paragraphs)
	out := notesync.Note{
		ID:         notesync.NoteID(n.ID),
		FolderID:   notesync.FolderID(n.FolderID),
		Etag:       notesync.Etag(n.Etag),
		Title:      n.Title,
		Checklists: blocks,
		BodyHTML:   steps,
		RawBody:    n.RawBody,
	}
	for _, im := range n.Images {
		out.Images = append(out.Images, notesync.NoteImage{
			ID: im.ID, Ref: im.Ref, ContentType: im.ContentType, Data: im.Data,
		})
	}
	return out
}

// fromParagraphs groups body paragraphs into ingredient blocks (consecutive
// checklist items, with a preceding "subtitle:" line captured as a "# subtitle"
// header) and step lines (plain paragraphs and inline-image markers).
func fromParagraphs(paras []notes.Paragraph) (blocks [][]string, steps string) {
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
	for _, p := range paras {
		if p.Attachment != nil {
			flushBlock()
			flushSubtitle()
			stepLines = append(stepLines, imgToken(p.Attachment.ImageID))
			continue
		}
		if p.Text == "" {
			continue
		}
		if p.Style == notes.StyleChecklist {
			if cur == nil && pendingSubtitle != "" {
				cur = append(cur, "# "+pendingSubtitle)
				pendingSubtitle = ""
			}
			cur = append(cur, p.Text)
			continue
		}
		flushBlock()
		flushSubtitle()
		if strings.HasSuffix(p.Text, ":") {
			pendingSubtitle = strings.TrimSuffix(p.Text, ":")
		} else {
			stepLines = append(stepLines, p.Text)
		}
	}
	flushBlock()
	flushSubtitle()
	return blocks, strings.Join(stepLines, "\n")
}

// toFolder and fromFolder convert between the two folder models.
func fromFolder(f notes.Folder) notesync.Folder {
	return notesync.Folder{ID: notesync.FolderID(f.ID), ParentID: notesync.FolderID(f.ParentID), Name: f.Name}
}

func fromImage(im notes.Image) notesync.NoteImage {
	return notesync.NoteImage{ID: im.ID, Ref: im.Ref, ContentType: im.ContentType, Data: im.Data}
}

func toImage(im notesync.NoteImage) notes.Image {
	return notes.Image{ID: im.ID, Ref: im.Ref, ContentType: im.ContentType, Data: im.Data}
}
