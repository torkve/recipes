package icloud

import (
	"bytes"
	"compress/gzip"
	"reflect"
	"testing"
)

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(b)
	_ = gw.Close()
	return buf.Bytes()
}

func TestGalleryPageIDs(t *testing.T) {
	page1 := "5A02150F-D4A4-4C83-955E-546618BD8004"
	page2 := "AABBCCDD-1122-3344-5566-778899AABBCC"
	// A blob with two page ids (page1 appearing twice) plus surrounding noise.
	raw := []byte("crdt\x00" + page1 + "\x01junk" + page2 + "\x02" + page1)

	// Works on a gzipped blob (Apple compresses MergeableDataEncrypted).
	got := galleryPageIDs(gzipBytes(raw))
	if !reflect.DeepEqual(got, []string{page1, page2}) {
		t.Fatalf("gzipped: got %v, want [page1 page2] (deduped, in order)", got)
	}
	// Works on a raw (uncompressed) blob too.
	if got := galleryPageIDs(raw); !reflect.DeepEqual(got, []string{page1, page2}) {
		t.Fatalf("raw: got %v, want [page1 page2]", got)
	}
	// Empty / no-uuid input yields nothing.
	if got := galleryPageIDs(nil); got != nil {
		t.Fatalf("nil blob: got %v, want nil", got)
	}
	if got := galleryPageIDs([]byte("no uuids here")); got != nil {
		t.Fatalf("no-uuid blob: got %v, want nil", got)
	}
}
