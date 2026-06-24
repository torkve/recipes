package icloud

import (
	"encoding/json"
	"fmt"
)

// CloudKit container and zone for Apple Notes.
const (
	notesContainer = "com.apple.notes"
	notesZone      = "Notes"
	ckEnv          = "production"
	ckScope        = "private"
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

// queryBody builds a records/query request body for a record type (pure).
func queryBody(recordType string) ([]byte, error) {
	body := map[string]any{
		"zoneID": map[string]string{"zoneName": notesZone},
		"query":  map[string]any{"recordType": recordType},
	}
	return json.Marshal(body)
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
