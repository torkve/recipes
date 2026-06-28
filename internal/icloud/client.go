package icloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"recipes/internal/notesync"
)

// Provider implements notesync.SyncProvider and notesync.Binder against the
// private iCloud web services.
type Provider struct {
	http       *http.Client
	srpVariant int // index into srpVariants for the SRP byte convention
}

// New returns a Provider using the given HTTP client (which must have a cookie
// jar). Pass nil for a default client with a jar. srpVariant selects the SRP
// convention (see srpVariants); out-of-range values fall back to 0.
func New(client *http.Client, srpVariant int) *Provider {
	if client == nil {
		client = newJarClient()
	}
	if srpVariant < 0 || srpVariant >= len(srpVariants) {
		srpVariant = 0
	}
	return &Provider{http: client, srpVariant: srpVariant}
}

var _ notesync.SyncProvider = (*Provider)(nil)
var _ notesync.Binder = (*Provider)(nil)

// Restore rebuilds a Session from the persisted blob. If the CloudKit token has
// expired but a trust token is present, it refreshes via accountLogin; if that
// is impossible it returns notesync.ErrReauthRequired.
func (p *Provider) Restore(ctx context.Context, blob []byte) (notesync.Session, error) {
	s, err := parseSession(blob)
	if err != nil {
		return nil, err
	}
	if s.Expired() {
		if s.TrustToken == "" {
			return nil, notesync.ErrReauthRequired
		}
		if err := p.refresh(ctx, s); err != nil {
			return nil, notesync.ErrReauthRequired
		}
	}
	return s, nil
}

// refresh re-runs accountLogin using the stored trust token (no password/2FA).
func (p *Provider) refresh(ctx context.Context, s *Session) error {
	body, err := buildAccountLoginBody(s.SessionToken, s.TrustToken, s.AccountCountry)
	if err != nil {
		return err
	}
	respBody, _, err := p.do(ctx, http.MethodPost, setupBase+"/accountLogin", nil, body, s)
	if err != nil {
		return err
	}
	dsid, services, err := parseAccountLogin(respBody)
	if err != nil {
		return err
	}
	s.DSID = dsid
	s.WebServices = services
	return nil
}

// zoneChanges enumerates the Notes zone via changes/zone (scoped to the given
// keys/record types), paginating on moreComing, returning all records + token.
func (p *Provider) zoneChanges(ctx context.Context, s *Session, since string, desiredKeys, recordTypes []string) ([]ckRecord, string, error) {
	const maxPages = 200
	var all []ckRecord
	token := since
	for page := 0; page < maxPages; page++ {
		body, err := zoneChangesBody(token, desiredKeys, recordTypes)
		if err != nil {
			return nil, "", err
		}
		respBody, err := p.ckPost(ctx, s, "changes/zone", body)
		if err != nil {
			return nil, "", err
		}
		recs, next, more, err := parseZoneChanges(respBody)
		if err != nil {
			return nil, "", err
		}
		all = append(all, recs...)
		token = next
		if !more {
			return all, token, nil
		}
	}
	// Hitting the cap while more pages remain would silently drop records; fail
	// loudly rather than return a partial result with a non-final token.
	return nil, "", fmt.Errorf("icloud: changes/zone exceeded %d pages", maxPages)
}

// ListFolders returns folders under root (descendants only), with parents
// relative to root. It uses a cheap folder-only zone scan (no note bodies).
func (p *Provider) ListFolders(ctx context.Context, sess notesync.Session, root notesync.FolderID) ([]notesync.Folder, error) {
	s, ok := sess.(*Session)
	if !ok {
		return nil, errBadSession
	}
	recs, _, err := p.zoneChanges(ctx, s, "", folderDesiredKeys, folderDesiredRecordTypes)
	if err != nil {
		return nil, err
	}
	var all []notesync.Folder
	for _, r := range recs {
		if r.RecordType == recordTypeFolder {
			all = append(all, recordToFolder(r))
		}
	}
	return descendantsOf(all, root), nil
}

// FetchZone enumerates the whole Notes zone in a single scan, returning both the
// folders (under root) and the notes in scope, plus the next change token.
func (p *Provider) FetchZone(ctx context.Context, sess notesync.Session, root notesync.FolderID, since string) ([]notesync.Folder, []notesync.Note, string, error) {
	s, ok := sess.(*Session)
	if !ok {
		return nil, nil, "", errBadSession
	}
	recs, next, err := p.zoneChanges(ctx, s, since, notesDesiredKeys, notesDesiredRecordTypes)
	if err != nil {
		return nil, nil, "", err
	}

	// Capture the zone owner id (needed in cross-record reference zoneIDs when pushing
	// images) from any record's creation metadata.
	if s.OwnerRecordName == "" {
		for _, r := range recs {
			if r.Created != nil && r.Created.UserRecordName != "" {
				s.OwnerRecordName = r.Created.UserRecordName
				break
			}
		}
	}

	var rawFolders []notesync.Folder
	for _, r := range recs {
		if r.RecordType == recordTypeFolder {
			rawFolders = append(rawFolders, recordToFolder(r))
		}
	}
	folders := descendantsOf(rawFolders, root)

	inScope := map[notesync.FolderID]bool{root: true}
	for _, f := range folders {
		inScope[f.ID] = true
	}
	var notes []notesync.Note
	var attIDs []string
	for _, r := range recs {
		if r.RecordType != recordTypeNote || r.intField("Deleted") == 1 {
			continue
		}
		n := recordToNote(r)
		if inScope[n.FolderID] || n.FolderID == "" {
			notes = append(notes, n)
			for _, img := range n.Images {
				attIDs = append(attIDs, img.ID)
			}
		}
	}

	// Attachment/Media records are not enumerated by changes/zone; resolve their
	// download URLs via records/lookup, expanding scanned-document galleries into
	// their page images, and rewrite each note's markers/images accordingly.
	pages, pageURL := p.resolveAttachmentURLs(ctx, s, attIDs)
	for i := range notes {
		notes[i].BodyHTML, notes[i].Images = resolveImageRefs(notes[i].BodyHTML, notes[i].Images, pages, pageURL)
	}
	return folders, notes, next, nil
}

// resolveAttachmentURLs resolves each body image marker to its downloadable page
// image(s) via records/lookup. A raster image (public.*) resolves to itself; a
// scanned-document gallery (com.apple.notes.gallery) fans out to its page images,
// whose ids come from the gallery's MergeableDataEncrypted. It returns two maps:
// pages[markerID] -> ordered page record names (== [markerID] for a direct image),
// and pageURL[pageID] -> asset download URL. Resolution is best-effort: lookup
// errors are logged and unresolved images dropped (their @@IMG markers are
// stripped on import), never aborting the pull.
//
// The page ids extracted from a gallery blob are an over-approximation, so they
// are filtered by resolvability: only ids that look up to an Attachment with a
// Media asset survive (the gallery's own id, device/replica ids fall out).
func (p *Provider) resolveAttachmentURLs(ctx context.Context, s *Session, markerIDs []string) (pages map[string][]string, pageURL map[string]string) {
	pages = map[string][]string{}
	pageURL = map[string]string{}
	if len(markerIDs) == 0 {
		return pages, pageURL
	}
	atts, err := p.lookupRecords(ctx, s, markerIDs)
	if err != nil {
		log.Printf("icloud: attachment lookup failed, skipping %d image(s): %v", len(markerIDs), err)
		return pages, pageURL
	}

	mediaOf := map[string]string{} // page/image recordName -> media recordName
	galleryCand := map[string][]string{}
	var candPageIDs []string
	for _, a := range atts {
		if attachmentUTI(a) == galleryUTI {
			cand := galleryPageIDs([]byte(a.decodedField("MergeableDataEncrypted")))
			galleryCand[a.RecordName] = cand
			candPageIDs = append(candPageIDs, cand...)
			continue
		}
		// Direct raster image: it is its own single "page".
		if m := a.referenceField("Media"); m != "" {
			pages[a.RecordName] = []string{a.RecordName}
			mediaOf[a.RecordName] = m
		}
	}

	// Resolve gallery pages: keep only candidates that are real page Attachments
	// (have a Media asset), preserving order.
	if len(candPageIDs) > 0 {
		pageAtts, perr := p.lookupRecords(ctx, s, candPageIDs)
		if perr != nil {
			log.Printf("icloud: gallery page lookup failed: %v", perr)
		} else {
			pageMedia := map[string]string{}
			for _, pa := range pageAtts {
				if m := pa.referenceField("Media"); m != "" {
					pageMedia[pa.RecordName] = m
				}
			}
			for gal, cand := range galleryCand {
				var kept []string
				for _, pid := range cand {
					if m, ok := pageMedia[pid]; ok {
						kept = append(kept, pid)
						mediaOf[pid] = m
					}
				}
				if len(kept) > 0 {
					pages[gal] = kept
				}
			}
		}
	}

	// Resolve media -> asset download URL (deduped).
	var mediaIDs []string
	seen := map[string]bool{}
	for _, m := range mediaOf {
		if !seen[m] {
			seen[m] = true
			mediaIDs = append(mediaIDs, m)
		}
	}
	if len(mediaIDs) == 0 {
		return pages, pageURL
	}
	medias, err := p.lookupRecords(ctx, s, mediaIDs)
	if err != nil {
		log.Printf("icloud: media lookup failed, skipping %d image(s): %v", len(mediaIDs), err)
		return pages, pageURL
	}
	mediaURL := map[string]string{}
	for _, md := range medias {
		if url, _ := md.assetField("Asset"); url != "" {
			mediaURL[md.RecordName] = url
		}
	}
	for page, media := range mediaOf {
		if url, ok := mediaURL[media]; ok {
			pageURL[page] = url
		}
	}
	return pages, pageURL
}

// attachmentUTI returns an Attachment's type identifier (plaintext UTI field, or
// the decrypted UTIEncrypted fallback).
func attachmentUTI(a ckRecord) string {
	if u := a.stringField("UTI"); u != "" {
		return u
	}
	return a.decodedField("UTIEncrypted")
}

// lookupRecords fetches records by name via records/lookup, chunked to stay under
// CloudKit's per-request batch limit, tolerating per-record errors.
func (p *Provider) lookupRecords(ctx context.Context, s *Session, names []string) ([]ckRecord, error) {
	const chunk = 50
	var out []ckRecord
	for i := 0; i < len(names); i += chunk {
		end := i + chunk
		if end > len(names) {
			end = len(names)
		}
		body, err := lookupBody(names[i:end])
		if err != nil {
			return nil, err
		}
		respBody, err := p.ckPost(ctx, s, "records/lookup", body)
		if err != nil {
			return nil, err
		}
		recs, err := parseLookup(respBody)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

// resolveImageRefs rewrites a note's body markers and image list from the
// resolved pages. Each marker @@IMG:markerID@@ is replaced by the newline-joined
// per-page markers @@IMG:pageID@@ (unchanged for a direct image, fanned out for a
// gallery), and the returned images carry each page's download Ref. A marker with
// no resolvable page is left in place, to be stripped on import.
func resolveImageRefs(body string, imgs []notesync.NoteImage, pages map[string][]string, pageURL map[string]string) (string, []notesync.NoteImage) {
	var out []notesync.NoteImage
	for _, img := range imgs {
		var repl []string
		for _, pid := range pages[img.ID] {
			url := pageURL[pid]
			if url == "" {
				continue
			}
			repl = append(repl, imgToken(pid))
			out = append(out, notesync.NoteImage{ID: pid, Ref: url})
		}
		if len(repl) == 0 {
			continue // unresolved: leave the original marker for the engine to strip
		}
		body = strings.ReplaceAll(body, imgToken(img.ID), strings.Join(repl, "\n"))
	}
	return body, out
}

// FetchImage downloads one image's bytes from its resolved Ref (a CloudKit asset
// download URL), sniffing the content type from the payload.
func (p *Provider) FetchImage(ctx context.Context, sess notesync.Session, img notesync.NoteImage) (notesync.NoteImage, error) {
	s, ok := sess.(*Session)
	if !ok {
		return notesync.NoteImage{}, errBadSession
	}
	if img.Ref == "" {
		return notesync.NoteImage{}, fmt.Errorf("icloud: image %s has no download ref", img.ID)
	}
	data, resp, err := p.do(ctx, http.MethodGet, img.Ref, nil, nil, s)
	if err != nil {
		return notesync.NoteImage{}, err
	}
	// do() caps the body at 32 MB; if the server declared more, we got a truncated
	// (corrupt) image — drop it rather than store garbage.
	if resp != nil && resp.ContentLength > int64(len(data)) {
		return notesync.NoteImage{}, fmt.Errorf("icloud: image %s truncated (%d of %d bytes)", img.ID, len(data), resp.ContentLength)
	}
	img.Data = data
	img.ContentType = http.DetectContentType(data)
	return img, nil
}

// PushNote creates, updates-in-place, or replaces a note, translating CloudKit conflicts.
//
// A new note (n.ID == "") is a plain create. A linked note (n.ID set) is UPDATED in
// place when we can read its mergeable CRDT version vector (prev.RawBody): we keep every
// device's replica entry verbatim and advance our own so the update strictly dominates
// and propagates — with no delete, so no validating-reference error and no trash. When
// the vector is unreadable we fall back to soft-deleting the old note (Deleted=1) and
// creating a fresh one (a hard delete would be rejected by the note's child attachments).
// Inline images are uploaded and their Media/Attachment records created in the same atomic
// batch, referencing the surviving note id. An expectedEtag mismatch maps to ErrEtagConflict.
func (p *Provider) PushNote(ctx context.Context, sess notesync.Session, n notesync.Note, expectedEtag notesync.Etag, prev notesync.PrevNote) (notesync.Note, error) {
	s, ok := sess.(*Session)
	if !ok {
		return notesync.Note{}, errBadSession
	}

	var rep *replicaState
	inPlace := false
	if n.ID != "" && len(prev.ReplicaUUID) == 16 {
		if entries, ok := parseObjectTable(prev.RawBody); ok {
			rep = replicaStateFor(entries, prev.ReplicaUUID)
			inPlace = true
		}
	}

	// The note id images attach to: the surviving note for an update, else a fresh one.
	noteID := string(n.ID)
	if !inPlace {
		noteID = uuid.NewString()
	}
	images, attachOps := p.buildImageOps(ctx, s, n, noteID)

	var ops []modifyOp
	if inPlace {
		// Update the existing record in place: same recordName, etag-guarded, with the
		// preserved+advanced version vector spliced into the body via rep.
		rec, err := noteToRecord(n, images, rep)
		if err != nil {
			return notesync.Note{}, err
		}
		rec.RecordName = noteID
		rec.RecordChangeTag = string(expectedEtag)
		rec.CreateShortGUID = false
		ops = append(ops, modifyOp{OperationType: "update", Record: rec})
		// The new body references freshly-uploaded image attachments, so the note's
		// previous Attachment/Media records are now orphaned. Delete them in the same
		// batch (the note is updated, not deleted, so this trips no validating reference).
		if len(prev.AttachmentIDs) > 0 {
			delOps, err := p.oldChildDeleteOps(ctx, s, prev.AttachmentIDs)
			if err != nil {
				return notesync.Note{}, err
			}
			ops = append(ops, delOps...)
		}
	} else {
		if n.ID != "" {
			// Linked note with no readable version vector: an in-place update wouldn't
			// propagate and a hard delete is blocked by its child attachments, so trash
			// the old note and recreate. Logged so we can measure how often this fires.
			log.Printf("icloud: note %s has no readable version vector; soft-deleting and recreating", n.ID)
			ops = append(ops, softDeleteOp(string(n.ID), expectedEtag))
		}
		n.ID, n.Etag = "", ""
		rec, err := noteToRecord(n, images, nil)
		if err != nil {
			return notesync.Note{}, err
		}
		rec.RecordName = noteID
		ops = append(ops, modifyOp{OperationType: "create", Record: rec})
	}
	ops = append(ops, attachOps...)

	body, err := modifyBody(ops)
	if err != nil {
		return notesync.Note{}, err
	}
	respBody, postErr := p.ckPost(ctx, s, "records/modify", body)
	// An atomic batch that fails comes back as HTTP 400 (ckPost errors) with the
	// culprit record's CONFLICT in the body, so check the body for a conflict before
	// surfacing the transport error.
	recs, err := parseRecords(respBody)
	if errors.Is(err, errEtagConflict) {
		return notesync.Note{}, notesync.ErrEtagConflict
	}
	if postErr != nil {
		return notesync.Note{}, postErr
	}
	if err != nil {
		return notesync.Note{}, err
	}
	// Return the saved note (id/etag) so the engine re-links the recipe; the response
	// may echo other ops' records, so match by our note id.
	for _, r := range recs {
		if r.RecordName == noteID {
			return recordToNote(r), nil
		}
	}
	return notesync.Note{}, fmt.Errorf("icloud: modify did not return the note")
}

// buildImageOps uploads each inline image referenced by the note body markers and
// returns the filename->mImage map the body encoder splices in plus the create ops for
// their Media/Attachment records (referencing noteID). An image that fails to upload is
// skipped (its marker then drops out of the body).
func (p *Provider) buildImageOps(ctx context.Context, s *Session, n notesync.Note, noteID string) (map[string]mImage, []modifyOp) {
	imgByName := make(map[string]notesync.NoteImage, len(n.Images))
	for _, img := range n.Images {
		imgByName[img.ID] = img
	}
	images := map[string]mImage{}
	var ops []modifyOp
	for _, fn := range bodyImageNames(n.BodyHTML) {
		img, ok := imgByName[fn]
		if !ok || len(img.Data) == 0 {
			continue
		}
		mediaID := uuid.NewString()
		attachmentID := uuid.NewString()
		asset, err := p.uploadAsset(ctx, s, recordTypeMedia, "Asset", mediaID, img.Data)
		if err != nil {
			log.Printf("icloud: skipping image %q: %v", fn, err) // keep the rest of the note
			continue
		}
		uti := utiForContentType(img.ContentType)
		w, h := imageDims(img.Data)
		images[fn] = mImage{attachmentID: attachmentID, uti: uti}
		ops = append(ops,
			modifyOp{OperationType: "create", Record: mediaToRecord(mediaID, noteID, fn, asset)},
			modifyOp{OperationType: "create", Record: attachmentToRecord(attachmentID, noteID, mediaID, s.OwnerRecordName, uti, w, h, int64(len(img.Data)))},
		)
	}
	return images, ops
}

// oldChildDeleteOps builds delete ops for the given old Attachment records and their
// backing Media, so an in-place update that re-creates inline images doesn't orphan the
// previous ones. Media ids aren't in the note body, so they're recovered by looking the
// Attachments up (each carries a Media reference), deduped. A lookup error aborts —
// better than silently leaking storage. Deleting these alongside a note UPDATE (not a
// delete) trips no validating reference: the note survives and each attachment's only
// validating referrer (nothing) and each media's (its attachment) go in the same batch.
//
// The ops use forceDelete: a plain "delete" requires the record's current recordChangeTag
// (CloudKit rejects it otherwise), but we don't track child etags and don't want
// optimistic concurrency on records we're discarding. The note update is still etag-guarded,
// so a concurrent edit still fails the whole atomic batch (taking these deletes with it).
func (p *Provider) oldChildDeleteOps(ctx context.Context, s *Session, attachmentIDs []string) ([]modifyOp, error) {
	atts, err := p.lookupRecords(ctx, s, attachmentIDs)
	if err != nil {
		return nil, fmt.Errorf("icloud: lookup old attachments for cleanup: %w", err)
	}
	ops := make([]modifyOp, 0, len(attachmentIDs))
	for _, id := range attachmentIDs {
		ops = append(ops, modifyOp{OperationType: "forceDelete", Record: ckRecord{
			RecordName: id, RecordType: recordTypeAttachment, ZoneID: ckZoneID{ZoneName: notesZone},
		}})
	}
	seenMedia := map[string]bool{}
	for _, a := range atts {
		m := a.referenceField("Media")
		if m == "" || seenMedia[m] {
			continue
		}
		seenMedia[m] = true
		ops = append(ops, modifyOp{OperationType: "forceDelete", Record: ckRecord{
			RecordName: m, RecordType: recordTypeMedia, ZoneID: ckZoneID{ZoneName: notesZone},
		}})
	}
	return ops, nil
}

// softDeleteOp updates a note to Deleted=1 (Apple's "Recently Deleted"), etag-guarded.
// Used as the replace fallback when an in-place update isn't possible.
func softDeleteOp(noteID string, etag notesync.Etag) modifyOp {
	return modifyOp{OperationType: "update", Record: ckRecord{
		RecordName:      noteID,
		RecordType:      recordTypeNote,
		RecordChangeTag: string(etag),
		Fields:          map[string]ckField{"Deleted": int64Field(1)},
		ZoneID:          ckZoneID{ZoneName: notesZone},
	}}
}

// replicaStateFor splits a note's existing object-table entries into the foreign
// replicas to preserve verbatim and our own replica's base counters to advance from
// (zero if our replica isn't in the note yet).
func replicaStateFor(entries [][]byte, ourUUID []byte) *replicaState {
	rep := &replicaState{uuid: ourUUID}
	for _, e := range entries {
		if bytes.Equal(objEntryUUID(e), ourUUID) {
			rep.baseC1, rep.baseC2 = objEntryCounters(e)
			continue // our entry is re-emitted advanced, not as a foreign passthrough
		}
		rep.foreign = append(rep.foreign, e)
	}
	return rep
}

// EnsureFolder finds a folder by name under parent, creating it if absent.
func (p *Provider) EnsureFolder(ctx context.Context, sess notesync.Session, parent notesync.FolderID, name string) (notesync.Folder, error) {
	s, ok := sess.(*Session)
	if !ok {
		return notesync.Folder{}, errBadSession
	}
	// ListFolders returns descendants of parent with direct children reparented
	// to "" (the synced scope). EnsureFolder only ever creates direct children
	// of parent, so an existing match is a direct child: name + ParentID == "".
	existing, err := p.ListFolders(ctx, s, parent)
	if err == nil {
		for _, f := range existing {
			if f.Name == name && f.ParentID == "" {
				return f, nil
			}
		}
	}
	rec := ckRecord{
		RecordType: recordTypeFolder,
		Fields: map[string]ckField{
			"TitleEncrypted": encryptedBytesField([]byte(name)),
		},
		ZoneID:          ckZoneID{ZoneName: notesZone},
		CreateShortGUID: true,
	}
	if parent != "" {
		rec.Fields["ParentFolder"] = folderRefField(string(parent))
		rec.Parent = &ckRef{RecordName: string(parent)}
	}
	body, err := modifyBody([]modifyOp{{OperationType: "create", Record: rec}})
	if err != nil {
		return notesync.Folder{}, err
	}
	respBody, err := p.ckPost(ctx, s, "records/modify", body)
	if err != nil {
		return notesync.Folder{}, err
	}
	recs, err := parseRecords(respBody)
	if err != nil || len(recs) == 0 {
		return notesync.Folder{}, fmt.Errorf("icloud: create folder failed: %w", err)
	}
	return recordToFolder(recs[0]), nil
}

// --- CloudKit HTTP helpers --------------------------------------------------

// ckURL builds the ckdatabasews endpoint URL (with the CloudKit-JS query params) for
// an operation, e.g. "records/modify" or "assets/upload".
func (p *Provider) ckURL(s *Session, op string) (string, error) {
	base := s.ckDatabaseURL()
	if base == "" {
		return "", errors.New("icloud: no ckdatabasews URL in session")
	}
	endpoint := fmt.Sprintf("%s/database/1/%s/%s/%s/%s", base, notesContainer, ckEnv, ckScope, op)
	q := url.Values{}
	q.Set("ckjsBuildVersion", ckjsBuildVersion)
	q.Set("ckjsVersion", ckjsVersion)
	q.Set("clientId", s.ClientID)
	q.Set("clientBuildNumber", ckClientBuildNumber)
	q.Set("clientMasteringNumber", ckClientMasteringNumber)
	q.Set("dsid", s.DSID)
	return endpoint + "?" + q.Encode(), nil
}

func (p *Provider) ckPost(ctx context.Context, s *Session, op string, body []byte) ([]byte, error) {
	u, err := p.ckURL(s, op)
	if err != nil {
		return nil, err
	}
	// CloudKit web auth is cookie-based; it expects text/plain and an icloud.com
	// origin. Cookies (incl. X-APPLE-WEBAUTH-VALIDATE / PCS) are replayed by do().
	headers := map[string]string{
		"Content-Type": "text/plain",
		"Origin":       oauthRedir,
		"Referer":      oauthRedir + "/",
	}
	respBody, _, err := p.do(ctx, http.MethodPost, u, headers, body, s)
	return respBody, err
}

// maxAssetBytes is CloudKit Web Services' single-asset upload cap.
const maxAssetBytes = 15 << 20

// uploadAsset uploads one asset's bytes for (recordType, fieldName, recordName) via the
// two-step CloudKit flow (request an upload URL, then POST the bytes) and returns the
// singleFile dict to drop verbatim into that ASSET field on the subsequent create.
func (p *Provider) uploadAsset(ctx context.Context, s *Session, recordType, fieldName, recordName string, data []byte) (json.RawMessage, error) {
	if len(data) > maxAssetBytes {
		return nil, fmt.Errorf("icloud: asset too large (%d bytes; max %d)", len(data), maxAssetBytes)
	}
	reqBody, _ := json.Marshal(map[string]any{"tokens": []any{map[string]string{
		"recordType": recordType, "fieldName": fieldName, "recordName": recordName,
	}}})
	respBody, err := p.ckPost(ctx, s, "assets/upload", reqBody)
	if err != nil {
		return nil, err
	}
	var tr struct {
		Tokens []struct {
			URL string `json:"url"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return nil, fmt.Errorf("icloud: parse asset upload tokens: %w", err)
	}
	if len(tr.Tokens) == 0 || tr.Tokens[0].URL == "" {
		return nil, fmt.Errorf("icloud: no asset upload URL returned")
	}
	upBody, _, err := p.do(ctx, http.MethodPost, tr.Tokens[0].URL,
		map[string]string{"Content-Type": "application/octet-stream", "Origin": oauthRedir, "Referer": oauthRedir + "/"},
		data, s)
	if err != nil {
		return nil, err
	}
	var up struct {
		SingleFile json.RawMessage `json:"singleFile"`
	}
	if err := json.Unmarshal(upBody, &up); err != nil {
		return nil, fmt.Errorf("icloud: parse asset upload result: %w", err)
	}
	if len(up.SingleFile) == 0 {
		return nil, fmt.Errorf("icloud: asset upload returned no singleFile")
	}
	return up.SingleFile, nil
}

// do performs an HTTP request, replaying the session cookies and returning the
// body and response.
func (p *Provider) do(ctx context.Context, method, urlStr string, headers map[string]string, body []byte, s *Session) ([]byte, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	setBrowserHeaders(req)
	if s != nil {
		for _, c := range s.Cookies {
			req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
		}
	}
	logRequest(method, urlStr)
	resp, err := p.http.Do(req)
	if err != nil {
		log.Printf("icloud: ✗ %s %s: %v", method, stripQuery(urlStr), err)
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp, err
	}
	logResponse(method, urlStr, resp.StatusCode, respBody)
	if resp.StatusCode >= 400 {
		return respBody, resp, fmt.Errorf("icloud: %s %s: status %d", method, stripQuery(urlStr), resp.StatusCode)
	}
	return respBody, resp, nil
}

var (
	errBadSession = errors.New("icloud: session is not an *icloud.Session")
)

// descendantsOf returns folders that are descendants of root, with direct
// children of root reparented to "" so the engine treats them as top-level
// categories under the synced scope.
func descendantsOf(all []notesync.Folder, root notesync.FolderID) []notesync.Folder {
	byID := map[notesync.FolderID]notesync.Folder{}
	for _, f := range all {
		byID[f.ID] = f
	}
	isDescendant := func(id notesync.FolderID) bool {
		for cur := id; cur != ""; {
			f, ok := byID[cur]
			if !ok {
				return false
			}
			if f.ParentID == root {
				return true
			}
			cur = f.ParentID
		}
		return false
	}
	var out []notesync.Folder
	for _, f := range all {
		if f.ID == root {
			continue
		}
		if f.ParentID == root {
			f.ParentID = ""
			out = append(out, f)
		} else if isDescendant(f.ID) {
			out = append(out, f)
		}
	}
	return out
}
