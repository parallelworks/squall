package arm

import (
	"bytes"
	"cmp"
	"crypto/md5" //nolint:gosec // Content-MD5 is what the Blob service computes; it guards integrity, not secrets.
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parallelworks/squall/access"
)

// blobName is everything a container holds under one blob name.
type blobName struct {
	// current is the blob, or nil when it was deleted or only has blocks
	// uploaded; deleted is the blob soft-deleted, when there is no current one.
	current, deleted *blobEntry
	// versions are the blob's previous versions, and snapshots its
	// snapshots, each in the order they were made.
	versions, snapshots []*blobEntry
	// uncommitted are the sizes of blocks uploaded but not committed, by block ID.
	uncommitted map[string]int64
}

func (b *blobName) empty() bool {
	return b.current == nil && b.deleted == nil && len(b.versions) == 0 && len(b.snapshots) == 0 && len(b.uncommitted) == 0
}

// blobEntry is one state of a blob: the blob, a version or a snapshot.
type blobEntry struct {
	created, modified time.Time
	etag              string
	size              int64
	md5               []byte
	headers           blobHeaders
	metadata          map[string]string
	versionID         string
	snapshot          string
	// blocks are the committed blocks of a blob made by Put Block List.
	blocks []block
	// deleted is when the entry was soft-deleted, and keep how many days it is kept.
	deleted time.Time
	keep    int
}

type block struct {
	id   string
	size int64
}

// blobHeaders are the HTTP properties a blob keeps.
type blobHeaders struct {
	contentType, contentEncoding, contentLanguage, cacheControl, contentDisposition string
}

func (e *blobEntry) expired(now time.Time) bool {
	return !e.deleted.IsZero() && daysLeft(e.deleted, now, e.keep) == 0
}

// daysLeft is how many days, counting a part of one, something deleted at
// deleted and kept for keep days has left at now; 0 means it is gone.
func daysLeft(deleted, now time.Time, keep int) int {
	left := deleted.AddDate(0, 0, keep).Sub(now)
	if left <= 0 {
		return 0
	}
	return int((left + 24*time.Hour - 1) / (24 * time.Hour))
}

// blobSettings are the blob service properties that decide what writes and deletes keep.
type blobSettings struct {
	versioning bool
	// keep and keepContainers are how many days deleted blobs and containers are kept, or 0 when they are not.
	keep, keepContainers int
}

func settingsOf(acct *resource) blobSettings {
	props := acct.account.services[blobService.path]
	versioning, _ := boolean(props, "isVersioningEnabled")
	return blobSettings{
		versioning:     versioning,
		keep:           retentionDays(props, "deleteRetentionPolicy"),
		keepContainers: retentionDays(props, "containerDeleteRetentionPolicy"),
	}
}

// retentionDays reads a delete retention policy, whose days default to 7 when it is enabled without them.
func retentionDays(props map[string]any, name string) int {
	p, _ := props[name].(map[string]any)
	if on, _ := boolean(p, "enabled"); !on {
		return 0
	}
	if n, ok := p["days"].(json.Number); ok {
		if d, err := n.Int64(); err == nil && d > 0 {
			return int(d)
		}
	}
	return 7
}

// deletedContainer is a soft-deleted blob container.
type deletedContainer struct {
	res     *resource
	version string
	deleted time.Time
	keep    int
}

func (d *deletedContainer) expired(now time.Time) bool { return daysLeft(d.deleted, now, d.keep) == 0 }

// deleteContainer deletes the container res of the account acct, keeping it
// with its blobs as deleted when the blob service keeps deleted containers.
// The caller holds s.mu.
func (s *Server) deleteContainer(acct, res *resource) {
	delete(s.resources, key(res.id))
	if keep := settingsOf(acct).keepContainers; keep > 0 {
		s.seq++
		acct.account.deleted = append(acct.account.deleted, &deletedContainer{
			res: res, version: fmt.Sprintf("%016X", s.seq), deleted: s.now(), keep: keep,
		})
	}
}

// deletedContainers returns the account's soft-deleted containers that are
// still kept, dropping the rest. The caller holds s.mu.
func (s *Server) deletedContainers(acct *resource) []*deletedContainer {
	now := s.now()
	acct.account.deleted = slices.DeleteFunc(acct.account.deleted, func(d *deletedContainer) bool { return d.expired(now) })
	return acct.account.deleted
}

func containerID(acct *resource, name string) string {
	return acct.id + "/" + blobService.path + "/default/" + containerKind.plural + "/" + name
}

// containerOf returns the call's account and container, or the error the
// Blob service answers. The caller holds s.mu.
func (s *Server) containerOf(c *blobCall) (*resource, *resource, *apiError) {
	acct := s.accountNamed(c.account)
	if acct == nil {
		return nil, nil, newError(http.StatusNotFound, "ResourceNotFound", "The specified resource does not exist.")
	}
	s.deletedContainers(acct)
	res := s.resources[key(containerID(acct, c.container))]
	if res == nil {
		return acct, nil, containers.notFound()
	}
	if c.blobName != "" {
		purgeBlob(res, c.blobName, s.now())
	}
	return acct, res, nil
}

// purgeBlobs drops what container res kept deleted for longer than it keeps
// it. The caller holds s.mu.
func (s *Server) purgeBlobs(res *resource) {
	now := s.now()
	for name := range res.blobs {
		purgeBlob(res, name, now)
	}
}

// purgeBlob drops what container res kept deleted of the blob name for
// longer than it keeps it, and reports whether anything is left.
func purgeBlob(res *resource, name string, now time.Time) bool {
	b := res.blobs[name]
	if b == nil {
		return false
	}
	gone := func(e *blobEntry) bool { return e.expired(now) }
	if b.deleted != nil && b.deleted.expired(now) {
		b.deleted = nil
	}
	b.versions = slices.DeleteFunc(b.versions, gone)
	b.snapshots = slices.DeleteFunc(b.snapshots, gone)
	if b.empty() {
		delete(res.blobs, name)
		return false
	}
	return true
}

// holdsBlobs reports whether container res holds a blob, soft-deleted
// blobs, versions and snapshots included; blocks never committed do not
// count. The caller holds s.mu.
func (s *Server) holdsBlobs(res *resource) bool {
	s.purgeBlobs(res)
	for _, b := range res.blobs {
		if b.current != nil || b.deleted != nil || len(b.versions) > 0 || len(b.snapshots) > 0 {
			return true
		}
	}
	return false
}

func blobNotFound() *apiError {
	return newError(http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
}

func invalidQueryValue(name, value string) *apiError {
	return newError(http.StatusBadRequest, "InvalidQueryParameterValue",
		"Value for one of the query parameters specified in the request URI is invalid. Parameter %s, value %q.", name, value)
}

func invalidHeaderValue(name, value string) *apiError {
	return newError(http.StatusBadRequest, "InvalidHeaderValue",
		"The value for one of the HTTP headers is not in the correct format. Header %s, value %q.", name, value)
}

// tick returns the time of a write, later than any before it so version and
// snapshot IDs are unique. The caller holds s.mu.
func (s *Server) tick() time.Time {
	t := s.now().UTC().Round(0).Truncate(100 * time.Nanosecond)
	if !t.After(s.lastTick) {
		t = s.lastTick.Add(100 * time.Nanosecond)
	}
	s.lastTick = t
	return t
}

// stamp formats a version or snapshot ID, as the Blob service writes them.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.0000000Z") }

// newETag returns a fresh blob ETag. The caller holds s.mu.
func (s *Server) newETag() string {
	s.seq++
	return fmt.Sprintf("0x8D%013X", s.seq)
}

func quote(etag string) string { return `"` + strings.Trim(etag, `"`) + `"` }

func writeNeeds(s *Server, c *blobCall, acct *resource) []string {
	if res := s.resources[key(containerID(acct, c.container))]; res != nil {
		if b := res.blobs[c.blobName]; b != nil && b.current != nil {
			return []string{blobAction + "write"}
		}
	}
	return []string{blobAction + "write|" + blobAction + "add/action"}
}

func deleteNeeds(_ *Server, c *blobCall, _ *resource) []string {
	if c.query.Has("versionid") {
		return []string{blobAction + "deleteBlobVersion/action"}
	}
	return []string{blobAction + "delete"}
}

// recheck checks a write's needs again under s.mu, as the blob may have been
// made since the call was admitted, which turns a create into an overwrite.
// The caller holds s.mu.
func (s *Server) recheck(c *blobCall, acct *resource) *apiError {
	if c.sharedKey || s.policy == nil || c.op == nil {
		return nil
	}
	needs := c.op.needs(s, c, acct)
	if slices.Equal(needs, c.needs) || s.policy.Check(access.Call{Method: c.op.name, Resource: c.resource(), Needs: needs}) == nil {
		return nil
	}
	return permissionMismatch()
}

func listBlobsNeeds(_ *Server, c *blobCall, _ *resource) []string {
	needs := []string{blobAction + "read"}
	if slices.Contains(strings.Split(c.query.Get("include"), ","), "tags") {
		needs = append(needs, blobAction+"tags/read")
	}
	return needs
}

// readBody reads the request body, up to the largest blob the server keeps.
func readBody(r *http.Request) ([]byte, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBlob+1))
	if err != nil {
		return nil, badRequest("reading the request body: %v", err)
	}
	if len(body) > maxBlob {
		return nil, newError(http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
			"The request body is too large and exceeds the maximum permissible limit. squall keeps blobs of up to %d bytes.", maxBlob)
	}
	return body, nil
}

// checkMD5 compares the MD5 the request gives for body, if any, with body's.
func checkMD5(r *http.Request, header string, sum []byte) *apiError {
	given := r.Header.Get(header)
	if given == "" {
		return nil
	}
	if want, err := base64.StdEncoding.DecodeString(given); err != nil || !bytes.Equal(want, sum) {
		return newError(http.StatusBadRequest, "Md5Mismatch", "The MD5 value specified in the request did not match with the MD5 value calculated by the server.")
	}
	return nil
}

// headersOf reads the blob properties a write sets: the x-ms-blob-* header,
// or else the standard one.
func headersOf(r *http.Request, standard bool) blobHeaders {
	get := func(custom, std string) string {
		if v := r.Header.Get(custom); v != "" || !standard {
			return v
		}
		return r.Header.Get(std)
	}
	return blobHeaders{
		contentType:        cmp.Or(get("x-ms-blob-content-type", "Content-Type"), "application/octet-stream"),
		contentEncoding:    get("x-ms-blob-content-encoding", "Content-Encoding"),
		contentLanguage:    get("x-ms-blob-content-language", "Content-Language"),
		cacheControl:       get("x-ms-blob-cache-control", "Cache-Control"),
		contentDisposition: r.Header.Get("x-ms-blob-content-disposition"),
	}
}

func metadataOf(r *http.Request) map[string]string {
	var m map[string]string
	for name, values := range r.Header {
		if k, ok := strings.CutPrefix(strings.ToLower(name), "x-ms-meta-"); ok && len(values) > 0 {
			if m == nil {
				m = map[string]string{}
			}
			m[k] = values[0]
		}
	}
	return m
}

// checkConditions applies a write's If-Match and If-None-Match headers to the blob's current version, if any.
func checkConditions(r *http.Request, current *blobEntry) *apiError {
	if r.Header.Get("If-None-Match") == "*" && current != nil {
		return newError(http.StatusConflict, "BlobAlreadyExists", "The specified blob already exists.")
	}
	if m := r.Header.Get("If-Match"); m != "" && (current == nil || m != "*" && quote(m) != quote(current.etag)) {
		return newError(http.StatusPreconditionFailed, "ConditionNotMet", "The condition specified using HTTP conditional header(s) is not met.")
	}
	return nil
}

func (s *Server) putBlob(c *blobCall) (*blobReply, *apiError) {
	switch t := c.r.Header.Get("x-ms-blob-type"); t {
	case "BlockBlob":
	case "":
		return nil, newError(http.StatusBadRequest, "MissingRequiredHeader", "An HTTP header that's mandatory for this request is not specified. Header x-ms-blob-type.")
	default:
		return nil, unsupported("only block blobs are supported, not %s", t)
	}
	body, err := readBody(c.r)
	if err != nil {
		return nil, err
	}
	sum := md5.Sum(body) //nolint:gosec // Content-MD5, as the Blob service computes it.
	if err := checkMD5(c.r, "Content-MD5", sum[:]); err != nil {
		return nil, err
	}
	stored := sum[:]
	if given := c.r.Header.Get("x-ms-blob-content-md5"); given != "" {
		if stored, _ = base64.StdEncoding.DecodeString(given); stored == nil {
			return nil, invalidHeaderValue("x-ms-blob-content-md5", given)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	if err := s.recheck(c, acct); err != nil {
		return nil, err
	}
	if err := s.checkOverwrite(c, cont); err != nil {
		return nil, err
	}
	e := &blobEntry{size: int64(len(body)), md5: stored, headers: headersOf(c.r, true), metadata: metadataOf(c.r)}
	s.write(acct, cont, c.blobName, e)
	h := writeHeaders(e)
	h.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	return &blobReply{status: http.StatusCreated, header: h}, nil
}

func writeHeaders(e *blobEntry) http.Header {
	h := http.Header{}
	h.Set("ETag", quote(e.etag))
	h.Set("Last-Modified", e.modified.Format(http.TimeFormat))
	h.Set("x-ms-request-server-encrypted", "true")
	if e.versionID != "" {
		h.Set("x-ms-version-id", e.versionID)
	}
	return h
}

// checkOverwrite checks a write's conditions, and that the blob it replaces,
// if any, is not immutable. The caller holds s.mu.
func (s *Server) checkOverwrite(c *blobCall, cont *resource) *apiError {
	if err := blobLevelWORM(c.r); err != nil {
		return err
	}
	cur := current(cont, c.blobName)
	if err := checkConditions(c.r, cur); err != nil {
		return err
	}
	if cur != nil {
		return blobImmutable(cont, cur.created, s.now(), true)
	}
	return nil
}

// blobLevelWORM rejects the headers that set a blob's own immutability
// policy or legal hold, which version-level immutability takes and the
// server does not model.
func blobLevelWORM(r *http.Request) *apiError {
	for _, h := range []string{"x-ms-immutability-policy-until-date", "x-ms-immutability-policy-mode", "x-ms-legal-hold"} {
		if r.Header.Get(h) != "" {
			return unsupported("blob-level immutability (%s) is not supported", h)
		}
	}
	return nil
}

func current(cont *resource, name string) *blobEntry {
	if b := cont.blobs[name]; b != nil {
		return b.current
	}
	return nil
}

// write makes e the current version of the blob name in cont, keeping the
// version it replaces as the account's blob service says: as a previous
// version with versioning, else as a soft-deleted snapshot with soft delete.
// The caller holds s.mu.
func (s *Server) write(acct, cont *resource, name string, e *blobEntry) {
	set := settingsOf(acct)
	if cont.blobs == nil {
		cont.blobs = map[string]*blobName{}
	}
	b := cont.blobs[name]
	if b == nil {
		b = &blobName{}
		cont.blobs[name] = b
	}
	e.modified = s.tick()
	e.created = e.modified
	e.etag = s.newETag()
	if set.versioning {
		e.versionID = stamp(e.modified)
	}
	if old := b.current; old != nil {
		e.created = old.created
		switch {
		case set.versioning || old.versionID != "":
			if old.versionID == "" {
				old.versionID = stamp(old.modified)
			}
			b.versions = append(b.versions, old)
		case set.keep > 0:
			old.deleted, old.keep = e.modified, set.keep
			b.snapshots = append(b.snapshots, old)
			old.snapshot = stamp(s.tick())
		}
	}
	s.retireDeleted(b)
	b.current, b.uncommitted = e, nil
}

// retireDeleted keeps b's soft-deleted blob, if any, as a soft-deleted
// snapshot, as a new blob of its name does. The caller holds s.mu.
func (s *Server) retireDeleted(b *blobName) {
	if old := b.deleted; old != nil {
		old.snapshot = stamp(s.tick())
		b.snapshots = append(b.snapshots, old)
		b.deleted = nil
	}
}

var blockIDs = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

func (s *Server) putBlock(c *blobCall) (*blobReply, *apiError) {
	id := c.query.Get("blockid")
	if raw, err := base64.StdEncoding.DecodeString(id); id == "" || err != nil || len(raw) > 64 || !blockIDs.MatchString(id) {
		return nil, newError(http.StatusBadRequest, "InvalidBlockId", "The specified block ID is invalid. The block ID must be Base64-encoded.")
	}
	body, err := readBody(c.r)
	if err != nil {
		return nil, err
	}
	sum := md5.Sum(body) //nolint:gosec // Content-MD5, as the Blob service computes it.
	if err := checkMD5(c.r, "Content-MD5", sum[:]); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	if cont.blobs == nil {
		cont.blobs = map[string]*blobName{}
	}
	b := cont.blobs[c.blobName]
	if b == nil {
		b = &blobName{}
		cont.blobs[c.blobName] = b
	}
	if !sameLength(b, id) {
		return nil, newError(http.StatusBadRequest, "InvalidBlobOrBlock", "The specified blob or block content is invalid.")
	}
	if b.uncommitted == nil {
		b.uncommitted = map[string]int64{}
	}
	s.retireDeleted(b)
	b.uncommitted[id] = int64(len(body))
	h := http.Header{}
	h.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	h.Set("x-ms-request-server-encrypted", "true")
	return &blobReply{status: http.StatusCreated, header: h}, nil
}

// sameLength reports whether id is as long as the IDs of b's blocks, as the
// Blob service requires of one blob's blocks.
func sameLength(b *blobName, id string) bool {
	for other := range b.uncommitted {
		return len(other) == len(id)
	}
	if b.current != nil && len(b.current.blocks) > 0 {
		return len(b.current.blocks[0].id) == len(id)
	}
	return true
}

type blockList struct {
	Items []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

func (s *Server) putBlockList(c *blobCall) (*blobReply, *apiError) {
	body, err := readBody(c.r)
	if err != nil {
		return nil, err
	}
	var list blockList
	if xml.Unmarshal(body, &list) != nil {
		return nil, newError(http.StatusBadRequest, "InvalidXmlDocument", "XML specified is not syntactically valid.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	b := cont.blobs[c.blobName]
	var committed map[string]int64
	var uncommitted map[string]int64
	if b != nil {
		uncommitted = b.uncommitted
		if b.current != nil {
			committed = map[string]int64{}
			for _, bl := range b.current.blocks {
				committed[bl.id] = bl.size
			}
		}
	}
	if err := s.recheck(c, acct); err != nil {
		return nil, err
	}
	if err := s.checkOverwrite(c, cont); err != nil {
		return nil, err
	}
	e := &blobEntry{headers: headersOf(c.r, false), metadata: metadataOf(c.r)}
	for _, item := range list.Items {
		id := strings.TrimSpace(item.ID)
		size, ok := int64(0), false
		switch item.XMLName.Local {
		case "Committed":
			size, ok = committed[id]
		case "Uncommitted":
			size, ok = uncommitted[id]
		case "Latest":
			if size, ok = uncommitted[id]; !ok {
				size, ok = committed[id]
			}
		}
		if !ok {
			return nil, newError(http.StatusBadRequest, "InvalidBlockList", "The specified block list is invalid.")
		}
		e.blocks = append(e.blocks, block{id: id, size: size})
		e.size += size
	}
	if given := c.r.Header.Get("x-ms-blob-content-md5"); given != "" {
		if e.md5, _ = base64.StdEncoding.DecodeString(given); e.md5 == nil {
			return nil, invalidHeaderValue("x-ms-blob-content-md5", given)
		}
	}
	s.write(acct, cont, c.blobName, e)
	return &blobReply{status: http.StatusCreated, header: writeHeaders(e)}, nil
}

func (s *Server) snapshotBlob(c *blobCall) (*blobReply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	cur := current(cont, c.blobName)
	if cur == nil {
		return nil, blobNotFound()
	}
	if err := blobImmutable(cont, cur.created, s.now(), true); err != nil {
		return nil, err
	}
	snap := *cur
	snap.snapshot, snap.versionID = stamp(s.tick()), ""
	if m := metadataOf(c.r); m != nil {
		snap.metadata = m
	}
	b := cont.blobs[c.blobName]
	b.snapshots = append(b.snapshots, &snap)
	h := http.Header{}
	h.Set("x-ms-snapshot", snap.snapshot)
	h.Set("ETag", quote(snap.etag))
	h.Set("Last-Modified", snap.modified.Format(http.TimeFormat))
	h.Set("x-ms-request-server-encrypted", "true")
	if settingsOf(acct).versioning {
		next := *cur
		next.versionID = stamp(s.tick())
		b.versions = append(b.versions, cur)
		b.current = &next
		h.Set("x-ms-version-id", next.versionID)
	}
	return &blobReply{status: http.StatusCreated, header: h}, nil
}

// target finds the entry a call names with its snapshot or versionid
// parameter, or the current blob, and the slice holding it.
func target(b *blobName, q map[string][]string) (*blobEntry, *[]*blobEntry) {
	if b == nil {
		return nil, nil
	}
	find := func(list *[]*blobEntry, id func(*blobEntry) string, want string) (*blobEntry, *[]*blobEntry) {
		for _, e := range *list {
			if id(e) == want {
				return e, list
			}
		}
		return nil, nil
	}
	if v := first(q, "snapshot"); v != "" {
		return find(&b.snapshots, func(e *blobEntry) string { return e.snapshot }, v)
	}
	if v := first(q, "versionid"); v != "" {
		if b.current != nil && b.current.versionID == v {
			return b.current, nil
		}
		return find(&b.versions, func(e *blobEntry) string { return e.versionID }, v)
	}
	return b.current, nil
}

func first(q map[string][]string, name string) string {
	if v := q[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (s *Server) blobProperties(c *blobCall) (*blobReply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	b := cont.blobs[c.blobName]
	e, _ := target(b, c.query)
	if e == nil || !e.deleted.IsZero() {
		return nil, blobNotFound()
	}
	h := http.Header{}
	h.Set("Last-Modified", e.modified.Format(http.TimeFormat))
	h.Set("x-ms-creation-time", e.created.Format(http.TimeFormat))
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("Content-Length", strconv.FormatInt(e.size, 10))
	h.Set("Content-Type", e.headers.contentType)
	setIf(h, "Content-Encoding", e.headers.contentEncoding)
	setIf(h, "Content-Language", e.headers.contentLanguage)
	setIf(h, "Cache-Control", e.headers.cacheControl)
	setIf(h, "Content-Disposition", e.headers.contentDisposition)
	if e.md5 != nil {
		h.Set("Content-MD5", base64.StdEncoding.EncodeToString(e.md5))
	}
	h.Set("ETag", quote(e.etag))
	h.Set("Accept-Ranges", "bytes")
	h.Set("x-ms-server-encrypted", "true")
	h.Set("x-ms-access-tier", "Hot")
	h.Set("x-ms-access-tier-inferred", "true")
	if e.snapshot == "" {
		h.Set("x-ms-lease-state", "available")
		h.Set("x-ms-lease-status", "unlocked")
	}
	if e.versionID != "" {
		h.Set("x-ms-version-id", e.versionID)
		h.Set("x-ms-is-current-version", strconv.FormatBool(e == b.current))
	}
	for k, v := range e.metadata {
		h.Set("x-ms-meta-"+k, v)
	}
	return &blobReply{status: http.StatusOK, header: h}, nil
}

func setIf(h http.Header, name, value string) {
	if value != "" {
		h.Set(name, value)
	}
}

func (s *Server) deleteBlob(c *blobCall) (*blobReply, *apiError) {
	if v := c.query.Get("deletetype"); v != "" {
		return nil, unsupported("permanent deletion (deletetype=%s) is not supported", v)
	}
	if c.query.Has("snapshot") && c.query.Has("versionid") {
		return nil, invalidQueryValue("versionid", c.query.Get("versionid"))
	}
	which := c.r.Header.Get("x-ms-delete-snapshots")
	if which != "" && which != "include" && which != "only" {
		return nil, invalidHeaderValue("x-ms-delete-snapshots", which)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	set := settingsOf(acct)
	now := s.now()
	b := cont.blobs[c.blobName]
	permanent := true
	discard := func(e *blobEntry, list *[]*blobEntry) {
		if set.keep > 0 {
			e.deleted, e.keep = now, set.keep
			permanent = false
			return
		}
		*list = slices.DeleteFunc(*list, func(x *blobEntry) bool { return x == e })
	}
	if c.query.Has("snapshot") || c.query.Has("versionid") {
		if which != "" {
			return nil, invalidHeaderValue("x-ms-delete-snapshots", which)
		}
		e, list := target(b, c.query)
		switch {
		case e == nil || !e.deleted.IsZero():
			return nil, blobNotFound()
		case list == nil:
			return nil, unsupported("deleting the current version by its versionid is not supported")
		}
		if err := blobImmutable(cont, e.created, now, false); err != nil {
			return nil, err
		}
		discard(e, list)
	} else {
		if b == nil || b.current == nil && len(b.uncommitted) == 0 {
			return nil, blobNotFound()
		}
		live := slices.DeleteFunc(slices.Clone(b.snapshots), func(e *blobEntry) bool { return !e.deleted.IsZero() })
		if len(live) > 0 && which == "" {
			return nil, newError(http.StatusConflict, "SnapshotsPresent", "This operation is not permitted while the blob has snapshots.")
		}
		doomed := live
		if which != "only" && b.current != nil {
			doomed = append(doomed, b.current)
		}
		if len(doomed) > 0 {
			if err := blobImmutable(cont, doomed[0].created, now, false); err != nil {
				return nil, err
			}
		}
		for _, e := range live {
			discard(e, &b.snapshots)
		}
		if which != "only" {
			if cur := b.current; cur != nil && (set.versioning || set.keep > 0 || cur.versionID != "") {
				permanent = false
			}
			s.deleteCurrent(b, set, now)
		}
	}
	if b.empty() {
		delete(cont.blobs, c.blobName)
	}
	h := http.Header{}
	h.Set("x-ms-delete-type-permanent", strconv.FormatBool(permanent))
	return &blobReply{status: http.StatusAccepted, header: h}, nil
}

// deleteCurrent deletes the current version of b: with versioning, or when it
// is a version made while versioning was on, it becomes a previous version;
// with soft delete it is kept as deleted; and otherwise it is gone. The
// caller holds s.mu.
func (s *Server) deleteCurrent(b *blobName, set blobSettings, now time.Time) {
	cur := b.current
	b.current, b.uncommitted = nil, nil
	switch {
	case cur == nil:
	case set.versioning || cur.versionID != "":
		if cur.versionID == "" {
			cur.versionID = stamp(cur.modified)
		}
		b.versions = append(b.versions, cur)
	case set.keep > 0:
		cur.deleted, cur.keep = now, set.keep
		b.deleted = cur
	}
}

// Blob is a blob, a previous version or a snapshot of one, that the server holds.
type Blob struct {
	// Account, Container and Name name the blob.
	Account, Container, Name string
	// VersionID is set when the blob was written with versioning, and
	// Snapshot for a snapshot.
	VersionID, Snapshot string
	// Current is whether this is the blob rather than a previous version or a snapshot.
	Current bool
	// Deleted is whether it is soft-deleted.
	Deleted bool
	Size    int64
}

// Blobs returns what the blob containers hold, soft-deleted blobs, previous
// versions and snapshots included, ordered by account, container and name.
// Blocks not yet committed and soft-deleted containers are left out.
func (s *Server) Blobs() []Blob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Blob
	for _, res := range s.resources {
		if res.kind != containerKind {
			continue
		}
		s.purgeBlobs(res)
		account := strings.Split(res.id, "/")[8]
		for name, b := range res.blobs {
			add := func(e *blobEntry) {
				out = append(out, Blob{
					Account: account, Container: res.name, Name: name, VersionID: e.versionID, Snapshot: e.snapshot,
					Current: e == b.current, Deleted: !e.deleted.IsZero(), Size: e.size,
				})
			}
			for _, e := range slices.Concat(b.snapshots, b.versions) {
				add(e)
			}
			for _, e := range []*blobEntry{b.deleted, b.current} {
				if e != nil {
					add(e)
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Blob) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.Container, b.Container), cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Snapshot, b.Snapshot), cmp.Compare(a.VersionID, b.VersionID))
	})
	return out
}
