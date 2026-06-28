package icloud

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"testing"
)

func TestRecordRefShape(t *testing.T) {
	var ref map[string]any
	if err := json.Unmarshal(recordRef("MEDIA-1", "_owner").Value, &ref); err != nil {
		t.Fatal(err)
	}
	if ref["recordName"] != "MEDIA-1" || ref["action"] != "VALIDATE" {
		t.Fatalf("bad ref: %+v", ref)
	}
	z := ref["zoneID"].(map[string]any)
	if z["zoneName"] != "Notes" || z["ownerRecordName"] != "_owner" || z["zoneType"] != "REGULAR_CUSTOM_ZONE" {
		t.Fatalf("bad zoneID: %+v", z)
	}
	// Without an owner, the zoneID carries only the zone name.
	var noOwner map[string]any
	_ = json.Unmarshal(recordRef("M", "").Value, &noOwner)
	z2 := noOwner["zoneID"].(map[string]any)
	if _, ok := z2["ownerRecordName"]; ok {
		t.Fatalf("ownerRecordName should be omitted when empty: %+v", z2)
	}
}

func TestMediaToRecord(t *testing.T) {
	asset := json.RawMessage(`{"receipt":"R","fileChecksum":"C","size":5}`)
	rec := mediaToRecord("MEDIA-1", "NOTE-1", "pic.jpg", asset)
	if rec.RecordType != recordTypeMedia || rec.RecordName != "MEDIA-1" || !rec.CreateShortGUID {
		t.Fatalf("bad media envelope: %+v", rec)
	}
	if rec.Parent == nil || rec.Parent.RecordName != "NOTE-1" {
		t.Fatalf("media parent not the note: %+v", rec.Parent)
	}
	if string(rec.Fields["Asset"].Value) != string(asset) {
		t.Fatalf("Asset not passed through verbatim: %s", rec.Fields["Asset"].Value)
	}
	if rec.decodedField("FilenameEncrypted") != "pic.jpg" {
		t.Fatalf("filename = %q", rec.decodedField("FilenameEncrypted"))
	}
}

func TestAttachmentToRecord(t *testing.T) {
	rec := attachmentToRecord("ATT-1", "NOTE-1", "MEDIA-1", "_owner", "public.jpeg", 800, 600, 1234)
	if rec.RecordType != recordTypeAttachment || rec.RecordName != "ATT-1" {
		t.Fatalf("bad attachment envelope: %+v", rec)
	}
	if rec.Parent == nil || rec.Parent.RecordName != "NOTE-1" {
		t.Fatalf("attachment parent not the note: %+v", rec.Parent)
	}
	if rec.referenceField("Media") != "MEDIA-1" || rec.referenceField("Note") != "NOTE-1" {
		t.Fatalf("bad refs: Media=%q Note=%q", rec.referenceField("Media"), rec.referenceField("Note"))
	}
	if rec.decodedField("UTIEncrypted") != "public.jpeg" {
		t.Fatalf("UTIEncrypted = %q", rec.decodedField("UTIEncrypted"))
	}
	if _, ok := rec.Fields["Width"]; !ok {
		t.Fatal("Width should be set when > 0")
	}
	// Width/Height omitted when unknown (0).
	noDim := attachmentToRecord("ATT-2", "NOTE-1", "MEDIA-1", "_owner", "public.png", 0, 0, 10)
	if _, ok := noDim.Fields["Width"]; ok {
		t.Fatal("Width should be omitted when 0")
	}
}

func TestBodyImageNames(t *testing.T) {
	body := "Step one\n@@IMG:a.jpg@@\nStep two @@IMG:b.png@@ end\n@@IMG:a.jpg@@"
	got := bodyImageNames(body)
	if !reflect.DeepEqual(got, []string{"a.jpg", "b.png"}) {
		t.Fatalf("bodyImageNames = %v, want [a.jpg b.png]", got)
	}
}

func TestImageDims(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 7, 11))); err != nil {
		t.Fatal(err)
	}
	w, h := imageDims(buf.Bytes())
	if w != 7 || h != 11 {
		t.Fatalf("imageDims = %d×%d, want 7×11", w, h)
	}
	if w, h := imageDims([]byte("not an image")); w != 0 || h != 0 {
		t.Fatalf("imageDims(garbage) = %d×%d, want 0×0", w, h)
	}
}

func TestUtiForContentType(t *testing.T) {
	for ct, want := range map[string]string{
		"image/png": "public.png", "image/jpeg": "public.jpeg", "image/heic": "public.heic", "": "public.jpeg",
	} {
		if got := utiForContentType(ct); got != want {
			t.Errorf("utiForContentType(%q) = %q, want %q", ct, got, want)
		}
	}
}
