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

// recordToFolder maps an Apple Notes Folder record to a notesync.Folder. The
// folder name is a server-decrypted, base64 ENCRYPTED_BYTES field.
func recordToFolder(r ckRecord) notesync.Folder {
	return notesync.Folder{
		ID:       notesync.FolderID(r.RecordName),
		ParentID: notesync.FolderID(r.referenceField("ParentFolder", "Folder", "parent")),
		Name:     r.decodedField("TitleEncrypted", "title", "name"),
	}
}

// recordToNote maps an Apple Notes Note record to a notesync.Note. Title and the
// snippet are server-decrypted base64 plaintext. The full rich body lives in
// TextDataEncrypted (gzip+protobuf, fetched via records/lookup) — handled
// separately; here the snippet is used as the body. The folder is a reference.
func recordToNote(r ckRecord) notesync.Note {
	n := notesync.Note{
		ID:       notesync.NoteID(r.RecordName),
		FolderID: notesync.FolderID(r.referenceField("Folders", "Folder", "parent")),
		Etag:     notesync.Etag(r.RecordChangeTag),
		Title:    r.decodedField("TitleEncrypted", "title"),
	}
	// Prefer the full body (checklist ingredients + steps) from the note blob;
	// fall back to the plain-text snippet preview when it can't be parsed.
	if td := r.decodedField("TextDataEncrypted"); td != "" {
		if blocks, steps, imageIDs, ok := parseNoteBody([]byte(td)); ok {
			n.Checklists = blocks
			n.BodyHTML = steps
			// Inline images are referenced by @@IMG:id@@ markers in BodyHTML; the
			// provider resolves each id to a download URL (FetchZone) and the engine
			// fetches the bytes only for notes it imports.
			for _, id := range imageIDs {
				n.Images = append(n.Images, notesync.NoteImage{ID: id})
			}
			return n
		}
	}
	n.BodyHTML = r.decodedField("SnippetEncrypted", "Snippet")
	return n
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
