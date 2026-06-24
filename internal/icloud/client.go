package icloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

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

	var rawFolders []notesync.Folder
	for _, r := range recs {
		if r.RecordType == recordTypeFolder {
			rawFolders = append(rawFolders, recordToFolder(r))
		}
	}
	folders := descendantsOf(rawFolders, root)

	attURL := imageURLsByAttachment(recs)

	inScope := map[notesync.FolderID]bool{root: true}
	for _, f := range folders {
		inScope[f.ID] = true
	}
	var notes []notesync.Note
	for _, r := range recs {
		if r.RecordType != recordTypeNote || r.intField("Deleted") == 1 {
			continue
		}
		n := recordToNote(r)
		if inScope[n.FolderID] || n.FolderID == "" {
			n.Images = resolveImageRefs(n.Images, attURL)
			notes = append(notes, n)
		}
	}
	return folders, notes, next, nil
}

// imageURLsByAttachment maps each image Attachment record name to its download
// URL by following Attachment.Media -> Media.Asset.downloadURL.
func imageURLsByAttachment(recs []ckRecord) map[string]string {
	mediaURL := map[string]string{}
	for _, r := range recs {
		if r.RecordType == recordTypeMedia {
			if url, _ := r.assetField("Asset"); url != "" {
				mediaURL[r.RecordName] = url
			}
		}
	}
	attURL := map[string]string{}
	for _, r := range recs {
		if r.RecordType != recordTypeAttachment {
			continue
		}
		if url, ok := mediaURL[r.referenceField("Media")]; ok {
			attURL[r.RecordName] = url
		}
	}
	return attURL
}

// resolveImageRefs stamps each image's download URL from the attachment map,
// dropping images whose asset could not be resolved (their @@IMG marker is then
// stripped from the body on import).
func resolveImageRefs(imgs []notesync.NoteImage, attURL map[string]string) []notesync.NoteImage {
	var out []notesync.NoteImage
	for _, img := range imgs {
		if url, ok := attURL[img.ID]; ok {
			img.Ref = url
			out = append(out, img)
		}
	}
	return out
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

// PushNote creates or updates a note, translating CloudKit conflicts.
func (p *Provider) PushNote(ctx context.Context, sess notesync.Session, n notesync.Note, expectedEtag notesync.Etag) (notesync.Note, error) {
	s, ok := sess.(*Session)
	if !ok {
		return notesync.Note{}, errBadSession
	}
	op := "create"
	if n.ID != "" {
		op = "update"
		n.Etag = expectedEtag
	}
	rec := noteToRecord(n)
	body, err := modifyBody([]modifyOp{{OperationType: op, Record: rec}})
	if err != nil {
		return notesync.Note{}, err
	}
	respBody, err := p.ckPost(ctx, s, "records/modify", body)
	if err != nil {
		return notesync.Note{}, err
	}
	recs, err := parseRecords(respBody)
	if errors.Is(err, errEtagConflict) {
		return notesync.Note{}, notesync.ErrEtagConflict
	}
	if err != nil {
		return notesync.Note{}, err
	}
	if len(recs) == 0 {
		return notesync.Note{}, fmt.Errorf("icloud: modify returned no records")
	}
	return recordToNote(recs[0]), nil
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
			"title": stringValueField(name),
		},
		ZoneID: ckZoneID{ZoneName: notesZone},
	}
	if parent != "" {
		rec.Fields["parent"] = referenceValueField(string(parent))
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

func (p *Provider) ckPost(ctx context.Context, s *Session, op string, body []byte) ([]byte, error) {
	base := s.ckDatabaseURL()
	if base == "" {
		return nil, errors.New("icloud: no ckdatabasews URL in session")
	}
	endpoint := fmt.Sprintf("%s/database/1/%s/%s/%s/%s", base, notesContainer, ckEnv, ckScope, op)
	q := url.Values{}
	q.Set("ckjsBuildVersion", ckjsBuildVersion)
	q.Set("ckjsVersion", ckjsVersion)
	q.Set("clientId", s.ClientID)
	q.Set("clientBuildNumber", ckClientBuildNumber)
	q.Set("clientMasteringNumber", ckClientMasteringNumber)
	q.Set("dsid", s.DSID)

	// CloudKit web auth is cookie-based; it expects text/plain and an icloud.com
	// origin. Cookies (incl. X-APPLE-WEBAUTH-VALIDATE / PCS) are replayed by do().
	headers := map[string]string{
		"Content-Type": "text/plain",
		"Origin":       oauthRedir,
		"Referer":      oauthRedir + "/",
	}
	respBody, _, err := p.do(ctx, http.MethodPost, endpoint+"?"+q.Encode(), headers, body, s)
	return respBody, err
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
