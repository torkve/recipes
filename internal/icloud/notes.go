package icloud

import (
	"encoding/json"

	"recipes/internal/notesync"
)

// Record-type and field names used by the Notes zone. The exact on-the-wire
// schema for Apple Notes is undocumented; these are the conventional names and
// the mapping below is intentionally tolerant (it tries several candidates and
// degrades gracefully) so it is easy to correct against live data.
const (
	recordTypeNote   = "Note"
	recordTypeFolder = "Folder"
)

// referenceField decodes a CloudKit reference field to the referenced record
// name, or "" if absent.
func (r ckRecord) referenceField(names ...string) string {
	for _, n := range names {
		f, ok := r.Fields[n]
		if !ok {
			continue
		}
		var ref struct {
			RecordName string `json:"recordName"`
		}
		if err := json.Unmarshal(f.Value, &ref); err == nil && ref.RecordName != "" {
			return ref.RecordName
		}
	}
	return ""
}

// recordToFolder maps a Folder record to a notesync.Folder.
func recordToFolder(r ckRecord) notesync.Folder {
	return notesync.Folder{
		ID:       notesync.FolderID(r.RecordName),
		ParentID: notesync.FolderID(r.referenceField("parent", "Folder", "parentFolder")),
		Name:     r.stringField("title", "name", "TitleNote"),
	}
}

// recordToNote maps a Note record to a notesync.Note. Body and checklist
// extraction is best-effort: Apple stores the rich content in an encoded blob,
// so we read the plain-text fields Notes exposes (title, snippet/preview) and
// leave structured checklists to be refined against live data. The engine
// re-sanitizes the body regardless, so a permissive extraction is safe.
func recordToNote(r ckRecord) notesync.Note {
	folder := r.referenceField("Folders", "folder", "parent")
	return notesync.Note{
		ID:       notesync.NoteID(r.RecordName),
		FolderID: notesync.FolderID(folder),
		Etag:     notesync.Etag(r.RecordChangeTag),
		Title:    r.stringField("title", "TitleNote"),
		BodyHTML: r.stringField("snippet", "textPreview", "textContent", "body"),
	}
}

// noteToRecord builds a CloudKit record for pushing a note. The body is written
// as a plain-text field and the title as the title field; checklists are
// appended to the body as lines (the encoded checklist format is not written
// here). The operation type is chosen by the caller via the record name/etag.
func noteToRecord(n notesync.Note) ckRecord {
	fields := map[string]ckField{
		"title": stringValueField(n.Title),
		"body":  stringValueField(renderNoteBody(n)),
	}
	rec := ckRecord{
		RecordName:      string(n.ID),
		RecordType:      recordTypeNote,
		RecordChangeTag: string(n.Etag),
		Fields:          fields,
		ZoneID:          ckZoneID{ZoneName: notesZone},
	}
	if n.FolderID != "" {
		fields["Folders"] = referenceValueField(string(n.FolderID))
	}
	return rec
}

// renderNoteBody flattens a note's body and checklists into plain text for the
// push body field.
func renderNoteBody(n notesync.Note) string {
	body := n.BodyHTML
	for _, cl := range n.Checklists {
		for _, item := range cl {
			body += "\n- " + item
		}
	}
	return body
}

func stringValueField(s string) ckField {
	v, _ := json.Marshal(s)
	return ckField{Value: v, Type: "STRING"}
}

func referenceValueField(recordName string) ckField {
	v, _ := json.Marshal(map[string]any{
		"recordName": recordName,
		"action":     "DELETE_SELF",
	})
	return ckField{Value: v, Type: "REFERENCE"}
}
