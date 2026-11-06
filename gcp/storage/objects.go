package storage

import (
	"bytes"
	"cmp"
	"crypto/md5" //nolint:gosec // the API reports MD5 digests; nothing here relies on them for security
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Object is one generation of an object in the API's JSON shape; noncurrent has TimeDeleted, soft-deleted SoftDeleteTime.
type Object struct {
	Kind                    string            `json:"kind"`
	ID                      string            `json:"id"`
	SelfLink                string            `json:"selfLink"`
	MediaLink               string            `json:"mediaLink"`
	Name                    string            `json:"name"`
	Bucket                  string            `json:"bucket"`
	Generation              int64             `json:"generation,string"`
	Metageneration          int64             `json:"metageneration,string"`
	ContentType             string            `json:"contentType,omitzero"`
	ContentEncoding         string            `json:"contentEncoding,omitzero"`
	StorageClass            string            `json:"storageClass"`
	Size                    uint64            `json:"size,string"`
	MD5Hash                 string            `json:"md5Hash"`
	CRC32C                  string            `json:"crc32c"`
	Etag                    string            `json:"etag"`
	TimeCreated             string            `json:"timeCreated"`
	Updated                 string            `json:"updated"`
	TimeStorageClassUpdated string            `json:"timeStorageClassUpdated"`
	TimeDeleted             string            `json:"timeDeleted,omitzero"`
	Metadata                map[string]string `json:"metadata,omitzero"`
	TemporaryHold           *bool             `json:"temporaryHold,omitzero"`
	EventBasedHold          *bool             `json:"eventBasedHold,omitzero"`
	RetentionExpirationTime string            `json:"retentionExpirationTime,omitzero"`
	Retention               *Retention        `json:"retention,omitzero"`
	SoftDeleteTime          string            `json:"softDeleteTime,omitzero"`
	HardDeleteTime          string            `json:"hardDeleteTime,omitzero"`

	// retentionStart is when the object's time in its bucket began, for a retention policy.
	retentionStart time.Time
	hardDelete     time.Time
}

func (o *Object) clone() Object {
	c := *o
	c.Metadata = maps.Clone(o.Metadata)
	if o.TemporaryHold != nil {
		c.TemporaryHold = new(*o.TemporaryHold)
	}
	if o.EventBasedHold != nil {
		c.EventBasedHold = new(*o.EventBasedHold)
	}
	if o.Retention != nil {
		c.Retention = new(*o.Retention)
	}
	return c
}

// view is o as the API shows it in b. The caller holds s.mu.
func (s *Server) view(b *bucket, o *Object) Object {
	c := o.clone()
	if p := b.RetentionPolicy; p != nil && o.SoftDeleteTime == "" && !isSet(o.EventBasedHold) {
		c.RetentionExpirationTime = o.retentionStart.Add(time.Duration(p.RetentionPeriod) * time.Second).UTC().Format(timeFormat)
	}
	return c
}

// setHolds sets o's holds, starting its retention period again when an event-based hold is released. The caller holds s.mu.
func (s *Server) setHolds(o *Object, temporary, eventBased *bool) {
	if isSet(o.EventBasedHold) && !isSet(eventBased) {
		o.retentionStart = s.now()
	}
	o.TemporaryHold, o.EventBasedHold = temporary, eventBased
}

// touchObject records a change to o's metadata. The caller holds s.mu.
func (s *Server) touchObject(o *Object) {
	o.Metageneration++
	_, o.Updated = s.timestamp()
	o.Etag = etag(o.Generation, o.Metageneration)
}

func (o *Object) live() bool { return o.TimeDeleted == "" }

// liveObject returns name's live generation in b, or nil. The caller holds s.mu.
func (b *bucket) liveObject(name string) *Object {
	gens := b.objects[name]
	if len(gens) == 0 || !gens[len(gens)-1].live() {
		return nil
	}
	return gens[len(gens)-1]
}

// generation returns generation gen of name in b, live or noncurrent, or nil.
func (b *bucket) generation(name string, gen int64) *Object {
	for _, o := range b.objects[name] {
		if o.Generation == gen {
			return o
		}
	}
	return nil
}

// retire makes name's live generation noncurrent if b is versioned, or removes it. The caller holds s.mu.
func (s *Server) retire(b *bucket, name string) {
	live := b.liveObject(name)
	if live == nil {
		return
	}
	if b.versioned() {
		_, live.TimeDeleted = s.timestamp()
		live.Updated = live.TimeDeleted
		return
	}
	s.discard(b, live)
}

// discard removes generation o from b, keeping it soft-deleted if b keeps deletions. The caller holds s.mu.
func (s *Server) discard(b *bucket, o *Object) {
	gens := slices.DeleteFunc(b.objects[o.Name], func(g *Object) bool { return g == o })
	if len(gens) == 0 {
		delete(b.objects, o.Name)
	} else {
		b.objects[o.Name] = gens
	}
	keep := b.softDeleteDuration()
	if keep == 0 {
		return
	}
	t, now := s.timestamp()
	o.SoftDeleteTime, o.hardDelete = now, t.Add(keep)
	o.HardDeleteTime = o.hardDelete.Format(timeFormat)
	b.trash = append(b.trash, o)
	b.keepUntil(o.hardDelete)
}

func noSuchObject(bucket, name string) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: fmt.Sprintf("No such object: %s/%s", bucket, name)}
}

func validObjectName(name string) bool {
	return len(name) <= 1024 && utf8.ValidString(name) && !strings.ContainsAny(name, "\r\n") && name != "." && name != ".." &&
		!strings.HasPrefix(name, ".well-known/acme-challenge/")
}

// objectInput is the metadata an upload takes.
type objectInput struct {
	Bucket          string            `json:"bucket"`
	Name            string            `json:"name"`
	ContentType     string            `json:"contentType"`
	ContentEncoding string            `json:"contentEncoding"`
	Metadata        map[string]string `json:"metadata"`
	CRC32C          string            `json:"crc32c"`
	MD5Hash         string            `json:"md5Hash"`
	TemporaryHold   *bool             `json:"temporaryHold"`
	EventBasedHold  *bool             `json:"eventBasedHold"`
	Retention       *Retention        `json:"retention"`
}

// splitMultipart returns the metadata and media of a multipart/related upload.
func splitMultipart(contentType string, body []byte) (meta, media []byte, err error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/related" || params["boundary"] == "" {
		return nil, nil, errors.New("a multipart upload needs a multipart/related body with a boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts [][]byte
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, b)
	}
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("a multipart upload has 2 parts, not %d", len(parts))
	}
	return parts[0], parts[1], nil
}

func (s *Server) insertObject(r *http.Request) (any, *apiError) {
	if err := checkACLs(r); err != nil {
		return nil, err
	}
	body, err := readBody(r, maxObject+maxBody)
	if err != nil {
		return nil, err
	}
	var in objectInput
	var data []byte
	switch t := r.URL.Query().Get("uploadType"); t {
	case "multipart":
		meta, media, err := splitMultipart(r.Header.Get("Content-Type"), body)
		if err != nil {
			return nil, invalid("%v", err)
		}
		if err := unmarshalStrict(meta, &in); err != nil {
			return nil, err
		}
		data = media
		if in.Name == "" {
			in.Name = r.URL.Query().Get("name")
		}
	case "media":
		in.Name, in.ContentType, data = r.URL.Query().Get("name"), r.Header.Get("Content-Type"), body
		in.ContentEncoding = r.URL.Query().Get("contentEncoding")
	case "":
		return nil, required("uploadType")
	default:
		return nil, invalid("squall: uploadType=%s is not supported", t)
	}
	if len(data) > maxObject {
		return nil, &apiError{Status: http.StatusRequestEntityTooLarge, Reason: "uploadTooLarge", Message: fmt.Sprintf("squall: objects are limited to %d bytes", maxObject)}
	}
	bucketName := r.PathValue("bucket")
	if in.Bucket != "" && in.Bucket != bucketName {
		return nil, invalid("The bucket in the metadata, %s, is not the bucket of the request, %s.", in.Bucket, bucketName)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[bucketName]; b != nil && b.uniformAccess() && aclRequested(r) {
		return nil, uniformAccessACL()
	}
	o, err := s.writeObject(bucketName, in, data, func(live int64) *apiError {
		if live == 0 && r.URL.Query().Has("ifGenerationNotMatch") {
			return conditionNotMet()
		}
		return checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", live)
	})
	if err != nil {
		return nil, err
	}
	return s.view(s.buckets[bucketName], o), nil
}

// checksums returns data's MD5 and CRC32C, as the API encodes them.
func checksums(data []byte) (md5Hash, crc string) {
	sum := md5.Sum(data) //nolint:gosec // a reported digest, not a security check
	crc32c := binary.BigEndian.AppendUint32(nil, crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	return base64.StdEncoding.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(crc32c)
}

// writeObject stores data as in.Name's newest generation once precondition passes. The caller holds s.mu.
func (s *Server) writeObject(bucketName string, in objectInput, data []byte, precondition func(live int64) *apiError) (*Object, *apiError) {
	if in.Name == "" {
		return nil, required("name")
	}
	if !validObjectName(in.Name) {
		return nil, invalid("The specified object name is not valid.")
	}
	md5Hash, crc := checksums(data)
	if in.CRC32C != "" && in.CRC32C != crc {
		return nil, invalid("Provided CRC32C \"%s\" doesn't match calculated CRC32C \"%s\".", in.CRC32C, crc)
	}
	if in.MD5Hash != "" && in.MD5Hash != md5Hash {
		return nil, invalid("Provided MD5 hash \"%s\" doesn't match calculated MD5 hash \"%s\".", in.MD5Hash, md5Hash)
	}
	b, err := s.lookup(bucketName, "storage.objects.create")
	if err != nil {
		return nil, err
	}
	var liveGen int64
	if live := b.liveObject(in.Name); live != nil {
		liveGen = live.Generation
	}
	if precondition != nil {
		if err := precondition(liveGen); err != nil {
			return nil, err
		}
	}
	eventHold := in.EventBasedHold
	if eventHold == nil && b.DefaultEventBasedHold {
		eventHold = new(true)
	}
	retention, err := s.checkRetention(b, nil, in.Retention, false, isSet(eventHold))
	if err != nil {
		return nil, err
	}
	if err := s.replaceLive(b, in.Name); err != nil {
		return nil, err
	}
	o := s.newGeneration(b, in.Name, b.StorageClass)
	o.ContentType, o.ContentEncoding = cmp.Or(in.ContentType, "application/octet-stream"), in.ContentEncoding
	o.Size, o.MD5Hash, o.CRC32C, o.Metadata = uint64(len(data)), md5Hash, crc, maps.Clone(in.Metadata)
	o.TemporaryHold, o.EventBasedHold, o.Retention = in.TemporaryHold, eventHold, retention
	return o, nil
}

// replaceLive retires name's live generation in b, unless what protects it refuses. The caller holds s.mu.
func (s *Server) replaceLive(b *bucket, name string) *apiError {
	if live := b.liveObject(name); live != nil {
		if err := s.guard(b, live, b.versioned()); err != nil {
			return err
		}
	}
	s.retire(b, name)
	return nil
}

// newGeneration adds a live generation of name to b, to be filled in. The caller holds s.mu.
func (s *Server) newGeneration(b *bucket, name, storageClass string) *Object {
	t, now := s.timestamp()
	gen := s.nextGeneration(t)
	escaped := url.PathEscape(name)
	o := &Object{
		Kind: "storage#object", ID: fmt.Sprintf("%s/%s/%d", b.Name, name, gen),
		SelfLink:  apiBase + "b/" + b.Name + "/o/" + escaped,
		MediaLink: downloadBase + "b/" + b.Name + "/o/" + escaped + "?generation=" + strconv.FormatInt(gen, 10) + "&alt=media",
		Name:      name, Bucket: b.Name, Generation: gen, Metageneration: 1, StorageClass: storageClass, Etag: etag(gen, 1),
		TimeCreated: now, Updated: now, TimeStorageClassUpdated: now, retentionStart: t,
	}
	b.objects[name] = append(b.objects[name], o)
	return o
}

func (s *Server) deleteObject(r *http.Request) (any, *apiError) {
	gen, byGeneration, err := int64Param(r, "generation")
	if err != nil {
		return nil, err
	}
	bucketName, name := r.PathValue("bucket"), r.PathValue("object")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(bucketName, "storage.objects.delete")
	if err != nil {
		return nil, err
	}
	target := b.liveObject(name)
	if byGeneration {
		target = b.generation(name, gen)
	}
	if target == nil {
		return nil, noSuchObject(bucketName, name)
	}
	if err := checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", target.Generation); err != nil {
		return nil, err
	}
	if err := s.guard(b, target, !byGeneration && b.versioned()); err != nil {
		return nil, err
	}
	if byGeneration {
		s.discard(b, target)
	} else {
		s.retire(b, name)
	}
	return nil, nil
}

// target returns the generation a call on an object names, live unless it has a generation parameter.
func (s *Server) target(r *http.Request, permission string) (*bucket, *Object, *apiError) {
	gen, byGeneration, err := int64Param(r, "generation")
	if err != nil {
		return nil, nil, err
	}
	bucketName, name := r.PathValue("bucket"), r.PathValue("object")
	b, err := s.lookup(bucketName, permission)
	if err != nil {
		return nil, nil, err
	}
	o := b.liveObject(name)
	if byGeneration {
		o = b.generation(name, gen)
	}
	if o == nil {
		return nil, nil, noSuchObject(bucketName, name)
	}
	if err := checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", o.Generation); err != nil {
		return nil, nil, err
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", o.Metageneration); err != nil {
		return nil, nil, err
	}
	return b, o, nil
}

func (s *Server) getObject(r *http.Request) (any, *apiError) {
	soft, gen, _, err := softDeletedGeneration(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !soft {
		b, o, err := s.target(r, "storage.objects.get")
		if err != nil {
			return nil, err
		}
		return s.view(b, o), nil
	}
	bucketName, name := r.PathValue("bucket"), r.PathValue("object")
	b, err := s.lookup(bucketName, "storage.objects.get")
	if err != nil {
		return nil, err
	}
	o := b.trashed(name, gen)
	if o == nil {
		return nil, noSuchObject(bucketName, name)
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", o.Metageneration); err != nil {
		return nil, err
	}
	return o.clone(), nil
}

// objectPatch is what objects.patch and objects.update take.
type objectPatch struct {
	Bucket          string             `json:"bucket"`
	Name            string             `json:"name"`
	ContentType     *string            `json:"contentType"`
	ContentEncoding *string            `json:"contentEncoding"`
	Metadata        map[string]*string `json:"metadata"`
	TemporaryHold   *bool              `json:"temporaryHold"`
	EventBasedHold  *bool              `json:"eventBasedHold"`
	Retention       *Retention         `json:"retention"`
	StorageClass    string             `json:"storageClass"`
	outputOnly
}

// outputOnly holds the Object fields the server sets, which a read-modify-write sends back and which are ignored.
type outputOnly struct {
	Kind                    json.RawMessage `json:"kind"`
	ID                      json.RawMessage `json:"id"`
	SelfLink                json.RawMessage `json:"selfLink"`
	MediaLink               json.RawMessage `json:"mediaLink"`
	Generation              json.RawMessage `json:"generation"`
	Metageneration          json.RawMessage `json:"metageneration"`
	Size                    json.RawMessage `json:"size"`
	MD5Hash                 json.RawMessage `json:"md5Hash"`
	CRC32C                  json.RawMessage `json:"crc32c"`
	Etag                    json.RawMessage `json:"etag"`
	TimeCreated             json.RawMessage `json:"timeCreated"`
	Updated                 json.RawMessage `json:"updated"`
	TimeStorageClassUpdated json.RawMessage `json:"timeStorageClassUpdated"`
	TimeFinalized           json.RawMessage `json:"timeFinalized"`
	TimeDeleted             json.RawMessage `json:"timeDeleted"`
	SoftDeleteTime          json.RawMessage `json:"softDeleteTime"`
	HardDeleteTime          json.RawMessage `json:"hardDeleteTime"`
	RetentionExpirationTime json.RawMessage `json:"retentionExpirationTime"`
	ComponentCount          json.RawMessage `json:"componentCount"`
	Owner                   json.RawMessage `json:"owner"`
	RestoreToken            json.RawMessage `json:"restoreToken"`
}

func (s *Server) patchObject(r *http.Request) (any, *apiError) { return s.changeObject(r, false) }

func (s *Server) updateObject(r *http.Request) (any, *apiError) { return s.changeObject(r, true) }

// changeObject applies objects.patch, or with replace objects.update, which sets every writable field.
func (s *Server) changeObject(r *http.Request, replace bool) (any, *apiError) {
	if err := checkACLs(r); err != nil {
		return nil, err
	}
	override, err := boolParam(r, "overrideUnlockedRetention")
	if err != nil {
		return nil, err
	}
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	var in objectPatch
	if err := unmarshalStrict(body, &in); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, invalid("Invalid JSON payload received: %v", err)
	}
	cleared := func(field string) bool { return string(fields[field]) == "null" }
	s.mu.Lock()
	defer s.mu.Unlock()
	b, o, err := s.target(r, "storage.objects.update")
	if err != nil {
		return nil, err
	}
	if b.uniformAccess() && aclRequested(r) {
		return nil, uniformAccessACL()
	}
	if in.Bucket != "" && in.Bucket != b.Name || in.Name != "" && in.Name != o.Name {
		return nil, invalid("squall: an object's bucket and name cannot be changed")
	}
	if in.StorageClass != "" && in.StorageClass != o.StorageClass {
		return nil, invalid("squall: changing an object's storage class needs objects.rewrite, which is not supported")
	}
	temporary, eventBased, metadata := o.TemporaryHold, o.EventBasedHold, maps.Clone(o.Metadata)
	contentType, contentEncoding := o.ContentType, o.ContentEncoding
	if replace {
		temporary, eventBased, metadata = in.TemporaryHold, in.EventBasedHold, nil
		contentType, contentEncoding = "application/octet-stream", ""
	}
	if in.TemporaryHold != nil || cleared("temporaryHold") {
		temporary = in.TemporaryHold
	}
	if in.EventBasedHold != nil || cleared("eventBasedHold") {
		eventBased = in.EventBasedHold
	}
	if in.ContentType != nil {
		contentType = cmp.Or(*in.ContentType, "application/octet-stream")
	} else if cleared("contentType") {
		contentType = "application/octet-stream"
	}
	if in.ContentEncoding != nil {
		contentEncoding = *in.ContentEncoding
	} else if cleared("contentEncoding") {
		contentEncoding = ""
	}
	if metadata == nil || cleared("metadata") {
		metadata = map[string]string{}
	}
	for k, v := range in.Metadata {
		if v == nil {
			delete(metadata, k)
		} else {
			metadata[k] = *v
		}
	}
	retention := o.Retention
	if replace || in.Retention != nil || cleared("retention") {
		if retention, err = s.checkRetention(b, o.Retention, in.Retention, override, isSet(eventBased)); err != nil {
			return nil, err
		}
	} else if retention != nil && isSet(eventBased) {
		return nil, retentionConflict()
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	s.setHolds(o, temporary, eventBased)
	o.Retention, o.Metadata, o.ContentType, o.ContentEncoding = retention, metadata, contentType, contentEncoding
	s.touchObject(o)
	return s.view(b, o), nil
}

type objectList struct {
	Kind          string   `json:"kind"`
	Items         []Object `json:"items,omitzero"`
	NextPageToken string   `json:"nextPageToken,omitzero"`
}

// versionKey orders generations by name and then generation, escaping NUL so a name that extends another sorts after it.
func versionKey(o Object) string {
	return strings.ReplaceAll(o.Name, "\x00", "\x00\xff") + fmt.Sprintf("\x00\x00%020d", o.Generation)
}

func (s *Server) listObjects(r *http.Request) (any, *apiError) {
	q := r.URL.Query()
	for _, p := range []string{"delimiter", "matchGlob", "filter"} {
		if q.Get(p) != "" {
			return nil, invalid("squall: objects.list does not support %s", p)
		}
	}
	soft, err := boolParam(r, "softDeleted")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{"includeTrailingDelimiter", "includeFoldersAsPrefixes"} {
		if on, err := boolParam(r, p); err != nil {
			return nil, err
		} else if on {
			return nil, invalid("squall: objects.list does not support %s", p)
		}
	}
	versions, err := boolParam(r, "versions")
	if err != nil {
		return nil, err
	}
	prefix, start, end := q.Get("prefix"), q.Get("startOffset"), q.Get("endOffset")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(r.PathValue("bucket"), "storage.objects.list")
	if err != nil {
		return nil, err
	}
	listed := func(name string) bool {
		return strings.HasPrefix(name, prefix) && name >= start && (end == "" || name < end)
	}
	var all []Object
	if soft {
		if b.softDeleteDuration() == 0 && len(b.trash) == 0 {
			return nil, invalid("Soft delete policy is required to list soft-deleted versions")
		}
		for _, o := range b.trash {
			if listed(o.Name) {
				all = append(all, o.clone())
			}
		}
		slices.SortFunc(all, func(a, b Object) int { return strings.Compare(versionKey(a), versionKey(b)) })
	} else {
		for _, name := range slices.Sorted(maps.Keys(b.objects)) {
			if !listed(name) {
				continue
			}
			for _, o := range b.objects[name] {
				if versions || o.live() {
					all = append(all, s.view(b, o))
				}
			}
		}
	}
	items, next, err := page(r, all, versionKey, "maxResults")
	if err != nil {
		return nil, err
	}
	return objectList{Kind: "storage#objects", Items: items, NextPageToken: next}, nil
}

// Objects returns every live and noncurrent generation in the named bucket, ordered by name and generation.
func (s *Server) Objects(bucketName string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	var out []Object
	for _, name := range slices.Sorted(maps.Keys(b.objects)) {
		for _, o := range b.objects[name] {
			out = append(out, s.view(b, o))
		}
	}
	return out
}

// PutObject writes data to name in the named bucket, as an upload does, and returns the generation it made.
func (s *Server) PutObject(bucketName, name string, data []byte) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.writeObject(bucketName, objectInput{Name: name}, data, nil)
	if err != nil {
		return Object{}, err
	}
	return s.view(s.buckets[bucketName], o), nil
}
