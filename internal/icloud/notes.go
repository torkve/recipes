package icloud

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

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

// noteToRecord builds a CloudKit Note record for pushing, matching the schema the
// iCloud Notes web app uses (verified against a captured save): server-encrypted
// ENCRYPTED_BYTES carrying base64 plaintext for the title/snippet, the body as
// base64(zlib(NoteStoreProto)), and folder membership as a REFERENCE_LIST (plus a
// singular Folder + a top-level parent). Fields are sent value-only (no type). The
// caller picks create vs update via the note id/etag: an empty id is a create
// (recordName/recordChangeTag are omitted; the server mints the id via
// createShortGUID).
func noteToRecord(n notesync.Note) (ckRecord, error) {
	body, err := encodeMergeableNoteBody(n.Title, n.Checklists, n.BodyHTML)
	if err != nil {
		return ckRecord{}, err
	}
	now := time.Now().UnixMilli()
	fields := map[string]ckField{
		"TitleEncrypted":              encryptedBytesField([]byte(n.Title)),
		"SnippetEncrypted":            encryptedBytesField([]byte(noteSnippet(n))),
		"TextDataEncrypted":           encryptedBytesField(body),
		"CreationDate":                int64Field(now),
		"ModificationDate":            int64Field(now),
		"TextDataAsset":               {},
		"FirstAttachmentThumbnail":    {},
		"FirstAttachmentUTIEncrypted": {},
	}
	// A note is identified by a client-chosen UUID recordName (matching the web app);
	// for a create (no id yet) we mint one. Updates are done as delete+create, so the
	// create path always mints a fresh id.
	recordName := string(n.ID)
	if recordName == "" {
		recordName = uuid.NewString()
	}
	rec := ckRecord{
		RecordName:      recordName,
		RecordType:      recordTypeNote,
		RecordChangeTag: string(n.Etag),
		Fields:          fields,
		ZoneID:          ckZoneID{ZoneName: notesZone},
		CreateShortGUID: true,
	}
	if n.FolderID != "" {
		fields["Folders"] = folderRefListField(string(n.FolderID))
		fields["Folder"] = folderRefField(string(n.FolderID))
		rec.Parent = &ckRef{RecordName: string(n.FolderID)}
	}
	return rec, nil
}

// int64Field sends a numeric value (CloudKit INT64/TIMESTAMP), value-only.
func int64Field(v int64) ckField {
	b, _ := json.Marshal(v)
	return ckField{Value: b}
}

// noteSnippet builds the short list-preview text for SnippetEncrypted: the title
// and the first body line, capped to a sane length.
func noteSnippet(n notesync.Note) string {
	s := strings.TrimSpace(n.Title)
	if first := strings.TrimSpace(strings.SplitN(n.BodyHTML, "\n", 2)[0]); first != "" {
		s = strings.TrimSpace(s + " " + first)
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return s
}

// encryptedBytesField sends an ENCRYPTED_BYTES value: base64 of the plaintext/blob.
// iCloud encrypts it server-side, so no client-side crypto is needed.
func encryptedBytesField(b []byte) ckField {
	v, _ := json.Marshal(base64.StdEncoding.EncodeToString(b))
	return ckField{Value: v}
}

// folderRef builds a folder-membership reference. ownerRecordName is left to the
// server to infer (private DB); action VALIDATE matches the web app.
func folderRef(recordName string) map[string]any {
	return map[string]any{
		"recordName": recordName,
		"action":     "VALIDATE",
		"zoneID":     map[string]string{"zoneName": notesZone},
	}
}

func folderRefField(recordName string) ckField {
	v, _ := json.Marshal(folderRef(recordName))
	return ckField{Value: v}
}

func folderRefListField(recordName string) ckField {
	v, _ := json.Marshal([]any{folderRef(recordName)})
	return ckField{Value: v}
}
