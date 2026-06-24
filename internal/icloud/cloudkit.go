package icloud

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// CloudKit container and zone for Apple Notes.
const (
	notesContainer = "com.apple.notes"
	notesZone      = "Notes"
	ckEnv          = "production"
	ckScope        = "private"

	// CloudKit-JS client identifiers the iCloud web app sends on every
	// ckdatabasews request (observed values; they drift across Apple releases).
	ckjsBuildVersion        = "2310ProjectDev27"
	ckjsVersion             = "2.6.4"
	ckClientBuildNumber     = "2622Build18"
	ckClientMasteringNumber = "2622Build18"
)

// ckRecord is a generic CloudKit record.
type ckRecord struct {
	RecordName      string             `json:"recordName"`
	RecordType      string             `json:"recordType"`
	RecordChangeTag string             `json:"recordChangeTag"`
	Fields          map[string]ckField `json:"fields"`
	ZoneID          ckZoneID           `json:"zoneID,omitempty"`
}

type ckField struct {
	Value json.RawMessage `json:"value"`
	Type  string          `json:"type"`
}

type ckZoneID struct {
	ZoneName string `json:"zoneName"`
}

// stringField returns the first present field (by candidate name) decoded as a
// string, or "" if none decode.
func (r ckRecord) stringField(names ...string) string {
	for _, n := range names {
		f, ok := r.Fields[n]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(f.Value, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// intField returns the first present field decoded as an int64 (CloudKit INT64),
// or 0.
func (r ckRecord) intField(names ...string) int64 {
	for _, n := range names {
		f, ok := r.Fields[n]
		if !ok {
			continue
		}
		var i int64
		if err := json.Unmarshal(f.Value, &i); err == nil {
			return i
		}
	}
	return 0
}

// assetField returns the downloadURL and decrypted size of an ASSETID field
// (e.g. Media.Asset), or "",0 if absent. CloudKit serves the asset plaintext at
// downloadURL (server-side decrypted, like the ENCRYPTED_BYTES text fields).
func (r ckRecord) assetField(names ...string) (url string, size int64) {
	for _, n := range names {
		f, ok := r.Fields[n]
		if !ok {
			continue
		}
		var a struct {
			DownloadURL string `json:"downloadURL"`
			Size        int64  `json:"size"`
		}
		if err := json.Unmarshal(f.Value, &a); err == nil && a.DownloadURL != "" {
			return a.DownloadURL, a.Size
		}
	}
	return "", 0
}

// decodedField returns an ENCRYPTED_BYTES field decoded to its plaintext.
// CloudKit Web Services decrypts these server-side (via the account's PCS
// cookies) and returns base64 of the plaintext, so we just base64-decode. Falls
// back to the raw string if it is not valid base64.
func (r ckRecord) decodedField(names ...string) string {
	s := r.stringField(names...)
	if s == "" {
		return ""
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}

// notesDesiredKeys / notesDesiredRecordTypes scope the zone-changes scan to the
// records and fields we map. Folder is not indexable for records/query, so the
// whole Notes zone is enumerated via changes/zone instead.
var (
	notesDesiredKeys        = []string{"TitleEncrypted", "SnippetEncrypted", "TextDataEncrypted", "Folders", "Folder", "ParentFolder", "Deleted", "ModificationDate", "Media", "Asset"}
	notesDesiredRecordTypes = []string{"Note", "Folder", "Attachment", "Media"}

	// Folder-only scan for the picker / push (cheap — no note bodies).
	folderDesiredKeys        = []string{"TitleEncrypted", "ParentFolder"}
	folderDesiredRecordTypes = []string{"Folder"}
)

// zoneChangesBody builds a changes/zone request for the Notes zone scoped to the
// given keys and record types. syncToken is included only when resuming (pure).
func zoneChangesBody(syncToken string, desiredKeys, recordTypes []string) ([]byte, error) {
	zone := map[string]any{
		"zoneID":             map[string]string{"zoneName": notesZone},
		"desiredKeys":        desiredKeys,
		"desiredRecordTypes": recordTypes,
		"reverse":            true,
	}
	if syncToken != "" {
		zone["syncToken"] = syncToken
	}
	return json.Marshal(map[string]any{"zones": []any{zone}})
}

// zoneChangesResponse is the changes/zone envelope.
type zoneChangesResponse struct {
	Zones []struct {
		Records    []ckRecord `json:"records"`
		SyncToken  string     `json:"syncToken"`
		MoreComing bool       `json:"moreComing"`
	} `json:"zones"`
}

// parseZoneChanges extracts one page of records plus the next sync token and the
// moreComing flag (pure).
func parseZoneChanges(body []byte) (records []ckRecord, syncToken string, moreComing bool, err error) {
	var r zoneChangesResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, "", false, fmt.Errorf("icloud: parse changes/zone: %w", err)
	}
	if len(r.Zones) == 0 {
		return nil, "", false, nil
	}
	z := r.Zones[0]
	return z.Records, z.SyncToken, z.MoreComing, nil
}

// queryResponse is the records/query / records/modify response envelope.
type queryResponse struct {
	Records []struct {
		ckRecord
		ServerErrorCode string `json:"serverErrorCode"`
		Reason          string `json:"reason"`
	} `json:"records"`
}

// parseRecords extracts records from a query/modify response, returning an error
// if the envelope reports a record-level server error (e.g. etag conflict).
func parseRecords(body []byte) ([]ckRecord, error) {
	var r queryResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("icloud: parse records: %w", err)
	}
	out := make([]ckRecord, 0, len(r.Records))
	for _, rec := range r.Records {
		if rec.ServerErrorCode != "" {
			if rec.ServerErrorCode == "CONFLICT" {
				return nil, errEtagConflict
			}
			return nil, fmt.Errorf("icloud: record error %s: %s", rec.ServerErrorCode, rec.Reason)
		}
		out = append(out, rec.ckRecord)
	}
	return out, nil
}

// modifyOp is a single record operation for records/modify.
type modifyOp struct {
	OperationType string   `json:"operationType"` // create | update | delete
	Record        ckRecord `json:"record"`
}

// modifyBody builds a records/modify request body (pure).
func modifyBody(ops []modifyOp) ([]byte, error) {
	body := map[string]any{
		"zoneID":     map[string]string{"zoneName": notesZone},
		"operations": ops,
	}
	return json.Marshal(body)
}

// errEtagConflict signals a CloudKit optimistic-concurrency failure; the client
// translates it to notesync.ErrEtagConflict.
var errEtagConflict = fmt.Errorf("icloud: record etag conflict")
