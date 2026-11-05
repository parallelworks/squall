package s3

import (
	"cmp"
	"crypto/md5" //nolint:gosec // multipart ETags are MD5 digests; nothing here relies on them for security
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxParts    = 10_000
	minPartSize = 5 << 20
	maxListed   = 1000
)

// MultipartUpload is an upload that was created and not yet completed or aborted.
type MultipartUpload struct {
	Bucket    string
	Key       string
	UploadID  string
	Initiated time.Time
	// Parts are the parts uploaded so far, by part number.
	Parts []Part
}

// Part is one uploaded part of a multipart upload.
type Part struct {
	Number int
	Size   int64
	// ETag is the quoted hex MD5 of the part's data.
	ETag         string
	LastModified time.Time
}

type upload struct {
	key       string
	id        string
	initiated time.Time
	parts     map[int]Part
	lock      lockRequest
}

func (u *upload) export(bucketName string) MultipartUpload {
	out := MultipartUpload{Bucket: bucketName, Key: u.key, UploadID: u.id, Initiated: u.initiated}
	for _, n := range slices.Sorted(maps.Keys(u.parts)) {
		out.Parts = append(out.Parts, u.parts[n])
	}
	return out
}

// sortedUploads returns b's uploads by key and then upload ID, which as a fixed-width counter orders them by initiation.
func sortedUploads(b *bucket) []*upload {
	return slices.SortedFunc(maps.Values(b.uploads), func(x, y *upload) int {
		return cmp.Or(strings.Compare(x.key, y.key), strings.Compare(x.id, y.id))
	})
}

func noSuchUpload() *apiError {
	return &apiError{
		Status: http.StatusNotFound, Code: "NoSuchUpload",
		Message: "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
	}
}

// startUpload creates an upload of key. The caller holds s.mu.
func (s *Server) startUpload(b *bucket, key string, lock lockRequest) *upload {
	s.nextUpload++
	u := &upload{key: key, id: fmt.Sprintf("%032x", s.nextUpload), initiated: s.now().UTC(), parts: map[int]Part{}, lock: lock}
	if b.uploads == nil {
		b.uploads = map[string]*upload{}
	}
	b.uploads[u.id] = u
	return u
}

// findUpload returns c's upload, which must be of c's key. The caller holds s.mu.
func (s *Server) findUpload(c *call) (*bucket, *upload, *apiError) {
	b, err := s.lookup(c)
	if err != nil {
		return nil, nil, err
	}
	u, ok := b.uploads[c.query.Get("uploadId")]
	if !ok || u.key != c.key {
		return nil, nil, noSuchUpload()
	}
	return b, u, nil
}

type initiateXML struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) createMultipartUpload(c *call) (*reply, *apiError) {
	if len(c.key) > maxKeyBytes {
		return nil, keyTooLong()
	}
	if acl := c.r.Header.Get("x-amz-acl"); acl != "" && acl != "private" && acl != "bucket-owner-full-control" {
		return nil, &apiError{Status: http.StatusBadRequest, Code: "AccessControlListNotSupported", Message: "The bucket does not allow ACLs"}
	}
	if c.r.Header.Get("X-Amz-Tagging") != "" {
		return nil, invalidArgument("squall: the x-amz-tagging header is not supported")
	}
	lock, err := s.lockHeaders(c.r.Header)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if err := checkLockable(b, lock); err != nil {
		return nil, err
	}
	u := s.startUpload(b, c.key, lock)
	return &reply{body: initiateXML{Bucket: c.bucket, Key: c.key, UploadID: u.id}}, nil
}

// partNumber reads a part number, from 1 to 10,000.
func partNumber(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 1 && n <= maxParts
}

func (s *Server) uploadPart(c *call) (*reply, *apiError) {
	if c.r.Header.Get("x-amz-copy-source") != "" {
		return nil, &apiError{Status: http.StatusNotFound, Code: "NotImplemented", Message: "squall: UploadPartCopy is not implemented"}
	}
	if !c.query.Has("uploadId") {
		return nil, invalidArgument("squall: UploadPart needs an uploadId")
	}
	n, ok := partNumber(c.query.Get("partNumber"))
	if !ok {
		return nil, invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, u, err := s.findUpload(c)
	if err != nil {
		return nil, err
	}
	if (u.lock.mode != "" || b.DefaultRetention != nil) && !hasChecksum(c.r.Header) {
		return nil, lockChecksumMissing("Put Part")
	}
	p := Part{Number: n, Size: int64(len(c.body)), ETag: etagOf(c.body), LastModified: s.now().UTC()}
	u.parts[n] = p
	return &reply{header: map[string]string{"ETag": p.ETag}}, nil
}

type completeXML struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeResultXML struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (s *Server) completeMultipartUpload(c *call) (*reply, *apiError) {
	for _, h := range []string{"If-Match", "If-None-Match"} {
		if c.r.Header.Get(h) != "" {
			return nil, invalidArgument("squall: the %s header is not supported", h)
		}
	}
	var in completeXML
	if err := decodeRootXML(c.body, "CompleteMultipartUpload", &in); err != nil {
		return nil, err
	}
	if len(in.Parts) == 0 || len(in.Parts) > maxParts {
		return nil, malformedXML()
	}
	for i, p := range in.Parts {
		if i > 0 && p.PartNumber <= in.Parts[i-1].PartNumber {
			return nil, &apiError{
				Status: http.StatusBadRequest, Code: "InvalidPartOrder",
				Message: "The list of parts was not in ascending order. The parts list must be specified in order by part number.",
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, u, err := s.findUpload(c)
	if err != nil {
		return nil, err
	}
	parts := make([]Part, len(in.Parts))
	for i, want := range in.Parts {
		p, ok := u.parts[want.PartNumber]
		if !ok || strings.Trim(want.ETag, `"`) != strings.Trim(p.ETag, `"`) {
			return nil, &apiError{
				Status: http.StatusBadRequest, Code: "InvalidPart",
				Message: "One or more of the specified parts could not be found.  The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.",
			}
		}
		parts[i] = p
	}
	digests := make([]byte, 0, md5.Size*len(parts))
	var size int64
	for i, p := range parts {
		if i < len(parts)-1 && p.Size < minPartSize {
			return nil, &apiError{Status: http.StatusBadRequest, Code: "EntityTooSmall", Message: "Your proposed upload is smaller than the minimum allowed object size."}
		}
		sum, _ := hex.DecodeString(strings.Trim(p.ETag, `"`))
		digests = append(digests, sum...)
		size += p.Size
	}
	sum := md5.Sum(digests) //nolint:gosec // an ETag, not a security check
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), len(in.Parts))
	v := s.storeVersion(b, c.key, size, etag)
	s.applyLock(b, v, u.lock)
	delete(b.uploads, u.id)
	header := map[string]string{}
	if b.Versioning != "" {
		header["x-amz-version-id"] = v.id
	}
	location := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", c.bucket, b.Region, (&url.URL{Path: c.key}).EscapedPath())
	return &reply{header: header, body: completeResultXML{Location: location, Bucket: c.bucket, Key: c.key, ETag: etag}}, nil
}

func (s *Server) abortMultipartUpload(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, u, err := s.findUpload(c)
	if err != nil {
		return nil, err
	}
	delete(b.uploads, u.id)
	return &reply{status: http.StatusNoContent}, nil
}

// listLimit reads a listing's page size parameter, at least 1, which defaults to and is capped at 1000.
func listLimit(c *call, name string) (int, *apiError) {
	if !c.query.Has(name) {
		return maxListed, nil
	}
	n, err := strconv.Atoi(c.query.Get(name))
	if err != nil || n < 1 {
		return 0, invalidArgument("Provided %s not an integer or within integer range", name)
	}
	return min(n, maxListed), nil
}

var listUploadsParams = []string{"prefix", "delimiter", "key-marker", "upload-id-marker", "max-uploads", "encoding-type"}

type listUploadsXML struct {
	XMLName            xml.Name          `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
	Bucket             string            `xml:"Bucket"`
	KeyMarker          string            `xml:"KeyMarker"`
	UploadIDMarker     string            `xml:"UploadIdMarker"`
	NextKeyMarker      string            `xml:"NextKeyMarker"`
	NextUploadIDMarker string            `xml:"NextUploadIdMarker"`
	Prefix             string            `xml:"Prefix"`
	Delimiter          string            `xml:"Delimiter,omitempty"`
	MaxUploads         int               `xml:"MaxUploads"`
	EncodingType       string            `xml:"EncodingType,omitempty"`
	IsTruncated        bool              `xml:"IsTruncated"`
	Uploads            []uploadXML       `xml:"Upload"`
	CommonPrefixes     []commonPrefixXML `xml:"CommonPrefixes"`
}

type uploadXML struct {
	Key          string   `xml:"Key"`
	UploadID     string   `xml:"UploadId"`
	Initiator    ownerXML `xml:"Initiator"`
	Owner        ownerXML `xml:"Owner"`
	StorageClass string   `xml:"StorageClass"`
	Initiated    string   `xml:"Initiated"`
}

func (s *Server) listMultipartUploads(c *call) (*reply, *apiError) {
	limit, err := listLimit(c, "max-uploads")
	if err != nil {
		return nil, err
	}
	encode, err := encodingOf(c)
	if err != nil {
		return nil, err
	}
	prefix, delimiter := c.query.Get("prefix"), c.query.Get("delimiter")
	keyMarker, idMarker := c.query.Get("key-marker"), c.query.Get("upload-id-marker")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	out := listUploadsXML{
		Bucket: c.bucket, KeyMarker: encode(keyMarker), UploadIDMarker: idMarker, Prefix: encode(prefix),
		Delimiter: encode(delimiter), MaxUploads: limit,
	}
	if c.query.Get("encoding-type") != "" {
		out.EncodingType = "url"
	}
	uploads := sortedUploads(b)
	var lastEntry, lastID string
	count := 0
	for _, u := range uploads {
		if !strings.HasPrefix(u.key, prefix) ||
			keyMarker != "" && (u.key < keyMarker || u.key == keyMarker && (idMarker == "" || u.id <= idMarker)) {
			continue
		}
		entry, rolled := u.key, false
		if delimiter != "" {
			if i := strings.Index(u.key[len(prefix):], delimiter); i >= 0 {
				entry, rolled = u.key[:len(prefix)+i+len(delimiter)], true
			}
		}
		if rolled && (entry <= keyMarker || count > 0 && lastID == "" && entry == lastEntry) {
			continue
		}
		if count == limit {
			out.IsTruncated = true
			if lastID == "" {
				out.NextKeyMarker, out.NextUploadIDMarker = encode(lastEntry), ""
			}
			break
		}
		if rolled {
			out.CommonPrefixes = append(out.CommonPrefixes, commonPrefixXML{Prefix: encode(entry)})
			lastEntry, lastID = entry, ""
		} else {
			out.Uploads = append(out.Uploads, uploadXML{
				Key: encode(u.key), UploadID: u.id, Initiator: ownerXML{ID: ownerID}, Owner: ownerXML{ID: ownerID},
				StorageClass: "STANDARD", Initiated: u.initiated.Format(timeFormat),
			})
			lastEntry, lastID = u.key, u.id
			out.NextKeyMarker, out.NextUploadIDMarker = encode(u.key), u.id
		}
		count++
	}
	return &reply{body: out}, nil
}

type listPartsXML struct {
	XMLName              xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
	Bucket               string    `xml:"Bucket"`
	Key                  string    `xml:"Key"`
	UploadID             string    `xml:"UploadId"`
	Initiator            ownerXML  `xml:"Initiator"`
	Owner                ownerXML  `xml:"Owner"`
	StorageClass         string    `xml:"StorageClass"`
	PartNumberMarker     int       `xml:"PartNumberMarker"`
	NextPartNumberMarker int       `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int       `xml:"MaxParts"`
	IsTruncated          bool      `xml:"IsTruncated"`
	Parts                []partXML `xml:"Part"`
}

type partXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

func (s *Server) listParts(c *call) (*reply, *apiError) {
	limit, err := listLimit(c, "max-parts")
	if err != nil {
		return nil, err
	}
	marker := 0
	if c.query.Has("part-number-marker") {
		n, err := strconv.Atoi(c.query.Get("part-number-marker"))
		if err != nil || n < 0 {
			return nil, invalidArgument("Provided part-number-marker not an integer or within integer range")
		}
		marker = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, u, err := s.findUpload(c)
	if err != nil {
		return nil, err
	}
	out := listPartsXML{
		Bucket: c.bucket, Key: c.key, UploadID: u.id, Initiator: ownerXML{ID: ownerID}, Owner: ownerXML{ID: ownerID},
		StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: limit,
	}
	for _, n := range slices.Sorted(maps.Keys(u.parts)) {
		if n <= marker {
			continue
		}
		if len(out.Parts) == limit {
			out.IsTruncated = true
			out.NextPartNumberMarker = out.Parts[limit-1].PartNumber
			break
		}
		p := u.parts[n]
		out.Parts = append(out.Parts, partXML{PartNumber: n, LastModified: p.LastModified.Format(timeFormat), ETag: p.ETag, Size: p.Size})
	}
	return &reply{body: out}, nil
}

// CreateMultipartUpload starts an upload of key in the named bucket, as CreateMultipartUpload does without
// Object Lock headers, and returns it.
func (s *Server) CreateMultipartUpload(bucketName, key string) (MultipartUpload, error) {
	if key == "" {
		return MultipartUpload{}, errors.New("squall: an upload needs a key")
	}
	if len(key) > maxKeyBytes {
		return MultipartUpload{}, keyTooLong()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return MultipartUpload{}, noSuchBucket(bucketName)
	}
	return s.startUpload(b, key, lockRequest{}).export(bucketName), nil
}

// MultipartUploads returns the named bucket's in-progress uploads in ListMultipartUploads order.
func (s *Server) MultipartUploads(bucketName string) []MultipartUpload {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	uploads := sortedUploads(b)
	out := make([]MultipartUpload, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, u.export(bucketName))
	}
	return out
}
