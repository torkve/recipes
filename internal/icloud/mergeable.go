package icloud

import "regexp"

// uuidRE matches an Apple CloudKit record name (a canonical UUID).
var uuidRE = regexp.MustCompile(`[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`)

// maxGalleryPages caps how many candidate page ids we extract from one gallery,
// so a malformed blob can't balloon a lookup batch.
const maxGalleryPages = 64

// galleryPageIDs extracts candidate page-attachment record names from a scanned
// document ("gallery") Attachment's MergeableDataEncrypted blob. The blob is an
// Apple CRDT we do not fully parse; instead we scan it (decompressed if it is
// gzip/zlib, else raw) for UUID-shaped substrings. This is an over-approximation
// — it may include non-page ids (the gallery's own id, device/replica ids) — so
// callers MUST filter the result by resolvability (keep only ids that look up to
// an Attachment with a Media asset). Results are deduped, in first-seen order,
// and capped.
func galleryPageIDs(blob []byte) []string {
	if len(blob) == 0 {
		return nil
	}
	data := blob
	if inflated, err := inflate(blob); err == nil {
		data = inflated
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range uuidRE.FindAll(data, -1) {
		id := string(m)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxGalleryPages {
			break
		}
	}
	return out
}
