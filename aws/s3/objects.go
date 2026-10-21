package s3

import (
	"cmp"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests; nothing here relies on them for security
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	nullVersion = "null"
	maxKeys     = 1000
)

// ObjectVersion is one version of an object, or a delete marker.
type ObjectVersion struct {
	Key string
	// VersionID is "null" for a version written while versioning was off or suspended.
	VersionID    string
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	// ETag is the quoted hex MD5 of the object's data; empty for a delete marker.
	ETag         string
	LastModified time.Time
}

type version struct {
	// seq orders a key's versions, and is what a version ID other than "null" encodes.
	seq          uint64
	id           string
	deleteMarker bool
	size         int64
	etag         string
	modified     time.Time
}

func (v *version) export(key string, latest bool) ObjectVersion {
	return ObjectVersion{Key: key, VersionID: v.id, IsLatest: latest, DeleteMarker: v.deleteMarker, Size: v.size, ETag: v.etag, LastModified: v.modified}
}

// newVersion returns a version written now, "null" unless versioning is on. The caller holds s.mu.
func (s *Server) newVersion(b *bucket) *version {
	s.nextVersion++
	v := &version{seq: s.nextVersion, id: nullVersion, modified: s.now().UTC()}
	if b.Versioning == versioningEnabled {
		v.id = fmt.Sprintf("%032x", v.seq)
	}
	return v
}

// validVersionID reports whether the server could have made id, and the sequence number it encodes.
func validVersionID(id string) (uint64, bool) {
	if id == nullVersion {
		return 0, true
	}
	if len(id) != 32 {
		return 0, false
	}
	n, err := strconv.ParseUint(id, 16, 64)
	return n, err == nil
}

func invalidVersion() *apiError { return invalidArgument("Invalid version id specified") }

// write adds v as key's newest version, replacing its "null" version unless v has an ID. The caller holds s.mu.
func (b *bucket) write(key string, v *version) {
	versions := b.objects[key]
	if v.id == nullVersion {
		versions = slices.DeleteFunc(versions, func(old *version) bool { return old.id == nullVersion })
	}
	b.objects[key] = append([]*version{v}, versions...)
}

// deleted is the result of deleting one object or version.
type deleted struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

// remove deletes version id of key, or with no id deletes key as the bucket's versioning says. The caller holds s.mu.
func (s *Server) remove(b *bucket, key, id string) deleted {
	if id != "" {
		out := deleted{Key: key, VersionID: id}
		versions := b.objects[key]
		i := slices.IndexFunc(versions, func(v *version) bool { return v.id == id })
		if i < 0 {
			return out
		}
		if versions[i].deleteMarker {
			out.DeleteMarker, out.DeleteMarkerVersionID = true, id
		}
		versions = slices.Delete(versions, i, i+1)
		if len(versions) == 0 {
			delete(b.objects, key)
		} else {
			b.objects[key] = versions
		}
		return out
	}
	if b.Versioning == "" {
		delete(b.objects, key)
		return deleted{Key: key}
	}
	marker := s.newVersion(b)
	marker.deleteMarker = true
	b.write(key, marker)
	return deleted{Key: key, DeleteMarker: true, DeleteMarkerVersionID: marker.id}
}

func (s *Server) putObject(c *call) (*reply, *apiError) {
	if c.r.Header.Get("x-amz-copy-source") != "" {
		return nil, &apiError{Status: http.StatusNotFound, Code: "NotImplemented", Message: "squall: CopyObject is not implemented"}
	}
	if acl := c.r.Header.Get("x-amz-acl"); acl != "" && acl != "private" && acl != "bucket-owner-full-control" {
		return nil, &apiError{Status: http.StatusBadRequest, Code: "AccessControlListNotSupported", Message: "The bucket does not allow ACLs"}
	}
	for h := range c.r.Header {
		if strings.HasPrefix(strings.ToLower(h), "x-amz-object-lock-") || strings.EqualFold(h, "x-amz-tagging") {
			return nil, invalidArgument("squall: the %s header is not supported", h)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	v := s.storeObject(b, c.key, c.body)
	header := map[string]string{"ETag": v.etag}
	if b.Versioning != "" {
		header["x-amz-version-id"] = v.id
	}
	return &reply{header: header}, nil
}

// storeObject writes data as key's newest version. The caller holds s.mu.
func (s *Server) storeObject(b *bucket, key string, data []byte) *version {
	sum := md5.Sum(data) //nolint:gosec // an ETag, not a security check
	v := s.newVersion(b)
	v.size, v.etag = int64(len(data)), `"`+hex.EncodeToString(sum[:])+`"`
	b.write(key, v)
	return v
}

func (s *Server) deleteObject(c *call) (*reply, *apiError) {
	id := c.query.Get("versionId")
	if c.query.Has("versionId") {
		if _, ok := validVersionID(id); !ok {
			return nil, invalidVersion()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	d := s.remove(b, c.key, id)
	header := map[string]string{}
	if d.DeleteMarker {
		header["x-amz-delete-marker"] = "true"
	}
	if v := cmp.Or(d.DeleteMarkerVersionID, d.VersionID); v != "" {
		header["x-amz-version-id"] = v
	}
	return &reply{status: http.StatusNoContent, header: header}, nil
}

type deleteXML struct {
	Quiet   bool `xml:"Quiet"`
	Objects []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}

type deleteResultXML struct {
	XMLName xml.Name         `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
	Deleted []deleted        `xml:"Deleted"`
	Errors  []deleteErrorXML `xml:"Error"`
}

type deleteErrorXML struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

func (s *Server) deleteObjects(c *call) (*reply, *apiError) {
	var in deleteXML
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if len(in.Objects) == 0 || len(in.Objects) > maxKeys {
		return nil, malformedXML()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	out := deleteResultXML{}
	for _, o := range in.Objects {
		if o.VersionID != "" {
			if _, ok := validVersionID(o.VersionID); !ok {
				out.Errors = append(out.Errors, deleteErrorXML{o.Key, o.VersionID, "NoSuchVersion", "The specified version does not exist."})
				continue
			}
		}
		d := s.remove(b, o.Key, o.VersionID)
		if !in.Quiet {
			out.Deleted = append(out.Deleted, d)
		}
	}
	return &reply{body: out}, nil
}

type listVersionsXML struct {
	XMLName             xml.Name     `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListVersionsResult"`
	Name                string       `xml:"Name"`
	Prefix              string       `xml:"Prefix"`
	KeyMarker           string       `xml:"KeyMarker"`
	VersionIDMarker     string       `xml:"VersionIdMarker"`
	NextKeyMarker       string       `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string       `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int          `xml:"MaxKeys"`
	IsTruncated         bool         `xml:"IsTruncated"`
	Entries             []versionXML `xml:""`
}

// versionXML is a Version or, by its XMLName, a DeleteMarker, so the two interleave in order.
type versionXML struct {
	XMLName      xml.Name
	Key          string   `xml:"Key"`
	VersionID    string   `xml:"VersionId"`
	IsLatest     bool     `xml:"IsLatest"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag,omitempty"`
	Size         *int64   `xml:"Size,omitempty"`
	StorageClass string   `xml:"StorageClass,omitempty"`
	Owner        ownerXML `xml:"Owner"`
}

func (s *Server) listObjectVersions(c *call) (*reply, *apiError) {
	limit := maxKeys
	if v := c.query.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalidArgument("Provided max-keys not an integer or within integer range")
		}
		limit = min(n, maxKeys)
	}
	prefix, keyMarker, idMarker := c.query.Get("prefix"), c.query.Get("key-marker"), c.query.Get("version-id-marker")
	var markerSeq uint64
	if idMarker != "" {
		if keyMarker == "" {
			return nil, invalidArgument("A version-id marker cannot be specified without a key marker.")
		}
		seq, ok := validVersionID(idMarker)
		if !ok {
			return nil, invalidArgument("Invalid version id specified")
		}
		markerSeq = seq
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if idMarker == nullVersion {
		markerSeq = 0
		for _, v := range b.objects[keyMarker] {
			if v.id == nullVersion {
				markerSeq = v.seq
			}
		}
	}
	out := listVersionsXML{Name: c.bucket, Prefix: prefix, KeyMarker: keyMarker, VersionIDMarker: idMarker, MaxKeys: limit}
	var last *versionXML
	for _, key := range slices.Sorted(maps.Keys(b.objects)) {
		if !strings.HasPrefix(key, prefix) || key < keyMarker || key == keyMarker && idMarker == "" {
			continue
		}
		for i, v := range b.objects[key] {
			// A key's versions are newest first, so those after the marker have lower sequence numbers.
			if key == keyMarker && v.seq >= markerSeq {
				continue
			}
			if len(out.Entries) == limit {
				out.IsTruncated = true
				if last != nil {
					out.NextKeyMarker, out.NextVersionIDMarker = last.Key, last.VersionID
				}
				return &reply{body: out}, nil
			}
			out.Entries = append(out.Entries, entryXML(v.export(key, i == 0)))
			last = &out.Entries[len(out.Entries)-1]
		}
	}
	return &reply{body: out}, nil
}

func entryXML(v ObjectVersion) versionXML {
	e := versionXML{
		XMLName: xml.Name{Local: "Version"}, Key: v.Key, VersionID: v.VersionID, IsLatest: v.IsLatest,
		LastModified: v.LastModified.Format(timeFormat), Owner: ownerXML{ID: ownerID},
	}
	if v.DeleteMarker {
		e.XMLName.Local = "DeleteMarker"
		return e
	}
	e.ETag, e.Size, e.StorageClass = v.ETag, &v.Size, "STANDARD"
	return e
}

// ObjectVersions returns the named bucket's versions and delete markers in ListObjectVersions order.
func (s *Server) ObjectVersions(bucketName string) []ObjectVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	var out []ObjectVersion
	for _, key := range slices.Sorted(maps.Keys(b.objects)) {
		for i, v := range b.objects[key] {
			out = append(out, v.export(key, i == 0))
		}
	}
	return out
}

// PutObject writes data to key in the named bucket, as PutObject does, and returns the version it made.
func (s *Server) PutObject(bucketName, key string, data []byte) (ObjectVersion, error) {
	if key == "" {
		return ObjectVersion{}, errors.New("squall: an object needs a key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return ObjectVersion{}, noSuchBucket(bucketName)
	}
	return s.storeObject(b, key, data).export(key, true), nil
}
