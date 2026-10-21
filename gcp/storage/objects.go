package storage

import (
	"bytes"
	"cmp"
	"crypto/md5" //nolint:gosec // the API reports MD5 digests; nothing here relies on them for security
	"encoding/base64"
	"encoding/binary"
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
	"unicode/utf8"
)

// Object is one generation of an object, in the API's JSON shape; a noncurrent one has TimeDeleted.
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
}

func (o *Object) clone() Object {
	c := *o
	c.Metadata = maps.Clone(o.Metadata)
	return c
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
	b.drop(name, live.Generation)
}

// drop removes generation gen of name. The caller holds s.mu.
func (b *bucket) drop(name string, gen int64) {
	gens := slices.DeleteFunc(b.objects[name], func(o *Object) bool { return o.Generation == gen })
	if len(gens) == 0 {
		delete(b.objects, name)
		return
	}
	b.objects[name] = gens
}

func noSuchObject(bucket, name string) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: fmt.Sprintf("No such object: %s/%s", bucket, name)}
}

func validObjectName(name string) bool {
	return len(name) <= 1024 && utf8.ValidString(name) && !strings.ContainsAny(name, "\r\n") && name != "." && name != ".."
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
	o, err := s.writeObject(bucketName, in, data, func(live int64) *apiError {
		return checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", live)
	})
	if err != nil {
		return nil, err
	}
	return o.clone(), nil
}

// writeObject stores data as in.Name's newest generation once precondition passes. The caller holds s.mu.
func (s *Server) writeObject(bucketName string, in objectInput, data []byte, precondition func(live int64) *apiError) (*Object, *apiError) {
	if in.Name == "" {
		return nil, required("name")
	}
	if !validObjectName(in.Name) {
		return nil, invalid("The specified object name is not valid.")
	}
	sum := md5.Sum(data) //nolint:gosec // a reported digest, not a security check
	md5Hash := base64.StdEncoding.EncodeToString(sum[:])
	crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))))
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
	s.retire(b, in.Name)
	t, now := s.timestamp()
	s.lastGeneration = max(s.lastGeneration+1, t.UnixMicro())
	gen := s.lastGeneration
	escaped := url.PathEscape(in.Name)
	o := &Object{
		Kind: "storage#object", ID: fmt.Sprintf("%s/%s/%d", bucketName, in.Name, gen),
		SelfLink:  apiBase + "b/" + bucketName + "/o/" + escaped,
		MediaLink: downloadBase + "b/" + bucketName + "/o/" + escaped + "?generation=" + strconv.FormatInt(gen, 10) + "&alt=media",
		Name:      in.Name, Bucket: bucketName, Generation: gen, Metageneration: 1,
		ContentType: cmp.Or(in.ContentType, "application/octet-stream"), ContentEncoding: in.ContentEncoding,
		StorageClass: b.StorageClass, Size: uint64(len(data)), MD5Hash: md5Hash, CRC32C: crc, Etag: etag(gen, 1),
		TimeCreated: now, Updated: now, TimeStorageClassUpdated: now, Metadata: maps.Clone(in.Metadata),
	}
	b.objects[in.Name] = append(b.objects[in.Name], o)
	return o, nil
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
	var target *Object
	if byGeneration {
		for _, o := range b.objects[name] {
			if o.Generation == gen {
				target = o
			}
		}
	} else {
		target = b.liveObject(name)
	}
	if target == nil {
		return nil, noSuchObject(bucketName, name)
	}
	if err := checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", target.Generation); err != nil {
		return nil, err
	}
	if byGeneration {
		b.drop(name, gen)
	} else {
		s.retire(b, name)
	}
	return nil, nil
}

type objectList struct {
	Kind          string   `json:"kind"`
	Items         []Object `json:"items,omitzero"`
	NextPageToken string   `json:"nextPageToken,omitzero"`
}

// versionKey orders generations by name and then generation.
func versionKey(o Object) string { return fmt.Sprintf("%s\x00%020d", o.Name, o.Generation) }

func (s *Server) listObjects(r *http.Request) (any, *apiError) {
	q := r.URL.Query()
	for _, p := range []string{"delimiter", "matchGlob", "filter"} {
		if q.Get(p) != "" {
			return nil, invalid("squall: objects.list does not support %s", p)
		}
	}
	for _, p := range []string{"includeTrailingDelimiter", "includeFoldersAsPrefixes", "softDeleted"} {
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
	var all []Object
	for _, name := range slices.Sorted(maps.Keys(b.objects)) {
		if !strings.HasPrefix(name, prefix) || name < start || end != "" && name >= end {
			continue
		}
		for _, o := range b.objects[name] {
			if versions || o.live() {
				all = append(all, o.clone())
			}
		}
	}
	items, next, err := page(r, all, versionKey)
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
			out = append(out, o.clone())
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
	return o.clone(), nil
}
