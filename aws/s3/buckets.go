package s3

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	versioningEnabled   = "Enabled"
	versioningSuspended = "Suspended"

	maxTags = 50
)

// Bucket is a general purpose S3 bucket.
type Bucket struct {
	Name string
	// Region is where the bucket is.
	Region       string
	CreationDate time.Time
	// Tags is nil while the bucket has no tag set.
	Tags map[string]string
	// Versioning is "" for a bucket never versioned, "Enabled" or "Suspended".
	Versioning string
	// Policy is the bucket policy JSON as sent, empty for none.
	Policy string
	// CORS is the CORSConfiguration XML as sent, empty for none.
	CORS string
}

type bucket struct {
	Bucket
	// objects holds each key's versions, newest first.
	objects map[string][]*version
	// nullSeqs holds each key's last "null" version's sequence number, kept after a delete so a marker naming it keeps its place.
	nullSeqs map[string]uint64
}

func (b *bucket) clone() Bucket {
	c := b.Bucket
	c.Tags = maps.Clone(b.Tags)
	return c
}

// lookup returns the bucket c names as the account sees it. The caller holds s.mu.
func (s *Server) lookup(c *call) (*bucket, *apiError) {
	if b, ok := s.buckets[c.bucket]; ok {
		if b.Region != c.region {
			return nil, redirect(c.bucket, b.Region)
		}
		return b, nil
	}
	if region, ok := s.foreign[c.bucket]; ok {
		if region != c.region {
			return nil, redirect(c.bucket, region)
		}
		denied := accessDenied(c.bucket)
		if c.r.Method == http.MethodHead {
			denied.Region = region
		}
		return nil, denied
	}
	return nil, noSuchBucket(c.bucket)
}

// validBucketName reports whether name follows the general purpose bucket naming rules.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 || strings.Contains(name, "..") {
		return false
	}
	for _, c := range []byte(name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	first, last := name[0], name[len(name)-1]
	if first == '.' || first == '-' || last == '.' || last == '-' {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for _, prefix := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	for _, suffix := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	return true
}

func invalidBucketName(name string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidBucketName", Message: "The specified bucket is not valid.", Bucket: name}
}

type createBucketConfiguration struct {
	LocationConstraint string    `xml:"LocationConstraint"`
	Location           *struct{} `xml:"Location"`
	Bucket             *struct{} `xml:"Bucket"`
	Tags               *struct{} `xml:"Tags"`
}

func (s *Server) createBucket(c *call) (*reply, *apiError) {
	if !validBucketName(c.bucket) {
		return nil, invalidBucketName(c.bucket)
	}
	if acl := c.r.Header.Get("x-amz-acl"); acl != "" && acl != "private" {
		return nil, &apiError{
			Status: http.StatusBadRequest, Code: "InvalidBucketAclWithObjectOwnership",
			Message: "Bucket cannot have ACLs set with ObjectOwnership's BucketOwnerEnforced setting",
		}
	}
	if o := c.r.Header.Get("x-amz-object-ownership"); o != "" && o != "BucketOwnerEnforced" {
		return nil, invalidArgument("squall: object ownership %s is not supported", o)
	}
	if strings.EqualFold(c.r.Header.Get("x-amz-bucket-object-lock-enabled"), "true") {
		return nil, invalidArgument("squall: object lock is not supported")
	}
	var cfg createBucketConfiguration
	if len(c.body) > 0 {
		if err := decodeXML(c.body, &cfg); err != nil {
			return nil, err
		}
		if cfg.Location != nil || cfg.Bucket != nil || cfg.Tags != nil {
			return nil, invalidArgument("squall: directory buckets and tags at creation are not supported")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.foreign[c.bucket]; ok {
		return nil, &apiError{
			Status: http.StatusConflict, Code: "BucketAlreadyExists",
			Message: "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again.",
			Bucket:  c.bucket,
		}
	}
	if b, ok := s.buckets[c.bucket]; ok {
		if b.Region == "us-east-1" && c.region == "us-east-1" {
			return &reply{header: map[string]string{"Location": "/" + c.bucket}}, nil
		}
		return nil, &apiError{
			Status: http.StatusConflict, Code: "BucketAlreadyOwnedByYou",
			Message: "Your previous request to create the named bucket succeeded and you already own it.", Bucket: c.bucket,
		}
	}
	switch constraint := cfg.LocationConstraint; {
	case constraint == "us-east-1":
		return nil, &apiError{Status: http.StatusBadRequest, Code: "InvalidLocationConstraint", Message: "The specified location-constraint is not valid"}
	case constraint == "" && c.region != "us-east-1":
		return nil, &apiError{
			Status: http.StatusBadRequest, Code: "IllegalLocationConstraintException",
			Message: "The unspecified location constraint is incompatible for the region specific endpoint this request was sent to.",
		}
	case constraint != "" && constraint != c.region:
		return nil, &apiError{
			Status: http.StatusBadRequest, Code: "IllegalLocationConstraintException",
			Message: fmt.Sprintf("The %s location constraint is incompatible for the region specific endpoint this request was sent to.", constraint),
		}
	}
	s.buckets[c.bucket] = &bucket{Name: c.bucket, Region: c.region, CreationDate: s.now().UTC(), objects: map[string][]*version{}}
	location := "/" + c.bucket
	if c.region != "us-east-1" {
		location = "http://" + c.bucket + ".s3.amazonaws.com/"
	}
	return &reply{header: map[string]string{"Location": location}}, nil
}

func (s *Server) headBucket(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	return &reply{header: map[string]string{"x-amz-bucket-region": b.Region, "x-amz-access-point-alias": "false"}}, nil
}

func (s *Server) deleteBucket(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if len(b.objects) > 0 {
		message := "The bucket you tried to delete is not empty"
		if b.Versioning != "" {
			message += ". You must delete all versions in the bucket."
		}
		return nil, &apiError{Status: http.StatusConflict, Code: "BucketNotEmpty", Message: message, Bucket: c.bucket}
	}
	delete(s.buckets, c.bucket)
	return &reply{status: http.StatusNoContent}, nil
}

type ownerXML struct {
	ID string `xml:"ID"`
}

type listBucketsXML struct {
	XMLName           xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Owner             ownerXML    `xml:"Owner"`
	Buckets           []bucketXML `xml:"Buckets>Bucket"`
	ContinuationToken string      `xml:"ContinuationToken,omitempty"`
	Prefix            string      `xml:"Prefix,omitempty"`
}

type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
	BucketRegion string `xml:"BucketRegion,omitempty"`
}

// listBucketsParams are the ListBuckets parameters that make it page and report regions.
var listBucketsParams = []string{"max-buckets", "continuation-token", "prefix", "bucket-region"}

func (s *Server) listBuckets(c *call) (*reply, *apiError) {
	paged := slices.ContainsFunc(listBucketsParams, c.query.Has)
	limit := 10_000
	if c.query.Has("max-buckets") {
		n, err := strconv.Atoi(c.query.Get("max-buckets"))
		if err != nil || n < 1 || n > 10_000 {
			return nil, invalidArgument("Argument max-buckets must be an integer between 1 and 10000")
		}
		limit = n
	}
	after := ""
	if token := c.query.Get("continuation-token"); token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(b) == 0 {
			return nil, invalidArgument("The continuation token provided is incorrect")
		}
		after = string(b)
	}
	prefix, region := c.query.Get("prefix"), c.query.Get("bucket-region")
	if region != "" && region != c.region {
		return nil, invalidArgument("squall: bucket-region %s differs from the request's region %s, which S3 does not support", region, c.region)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := listBucketsXML{Owner: ownerXML{ID: ownerID}, Prefix: prefix}
	for _, name := range slices.Sorted(maps.Keys(s.buckets)) {
		b := s.buckets[name]
		if name <= after || !strings.HasPrefix(name, prefix) || region != "" && b.Region != region {
			continue
		}
		if paged && len(out.Buckets) == limit {
			out.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(out.Buckets[limit-1].Name))
			break
		}
		entry := bucketXML{Name: name, CreationDate: b.CreationDate.Format(timeFormat)}
		if paged {
			entry.BucketRegion = b.Region
		}
		out.Buckets = append(out.Buckets, entry)
	}
	return &reply{body: out}, nil
}

type taggingXML struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  []tagXML `xml:"TagSet>Tag"`
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (s *Server) getBucketTagging(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if b.Tags == nil {
		return nil, &apiError{Status: http.StatusNotFound, Code: "NoSuchTagSet", Message: "The TagSet does not exist", Bucket: c.bucket}
	}
	out := taggingXML{XMLName: xml.Name{Space: namespace, Local: "Tagging"}}
	for _, k := range slices.Sorted(maps.Keys(b.Tags)) {
		out.TagSet = append(out.TagSet, tagXML{k, b.Tags[k]})
	}
	return &reply{body: out}, nil
}

func validTagText(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r) && !strings.ContainsRune("+-=._:/@", r) {
			return false
		}
	}
	return true
}

func parseTags(body []byte) (map[string]string, *apiError) {
	var in taggingXML
	if err := decodeXML(body, &in); err != nil {
		return nil, err
	}
	if len(in.TagSet) > maxTags {
		return nil, invalidTag(fmt.Sprintf("Bucket tag count cannot be greater than %d", maxTags))
	}
	tags := map[string]string{}
	for _, t := range in.TagSet {
		switch {
		case t.Key == "" || utf8.RuneCountInString(t.Key) > 128 || !validTagText(t.Key):
			return nil, invalidTag("The TagKey you have provided is invalid")
		case utf8.RuneCountInString(t.Value) > 256 || !validTagText(t.Value):
			return nil, invalidTag("The TagValue you have provided is invalid")
		case strings.HasPrefix(strings.ToLower(t.Key), "aws:"):
			return nil, invalidTag("System tags cannot be added/updated by requester")
		}
		if _, dup := tags[t.Key]; dup {
			return nil, invalidTag("Cannot provide multiple Tags with the same key")
		}
		tags[t.Key] = t.Value
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

func (s *Server) putBucketTagging(c *call) (*reply, *apiError) {
	tags, err := parseTags(c.body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	b.Tags = tags
	return &reply{status: http.StatusNoContent}, nil
}

func (s *Server) deleteBucketTagging(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	b.Tags = nil
	return &reply{status: http.StatusNoContent}, nil
}

type versioningXML struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Status    string   `xml:"Status,omitempty"`
	MfaDelete string   `xml:"MfaDelete,omitempty"`
}

func (s *Server) getBucketVersioning(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	return &reply{body: versioningXML{XMLName: xml.Name{Space: namespace, Local: "VersioningConfiguration"}, Status: b.Versioning}}, nil
}

func (s *Server) putBucketVersioning(c *call) (*reply, *apiError) {
	var in versioningXML
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if in.MfaDelete != "" {
		return nil, invalidArgument("squall: MFA delete is not supported")
	}
	if in.Status != versioningEnabled && in.Status != versioningSuspended {
		return nil, malformedXML()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	b.Versioning = in.Status
	return &reply{}, nil
}

func (s *Server) putBucketPolicy(c *call) (*reply, *apiError) {
	var policy map[string]any
	if len(c.body) == 0 || c.body[0] != '{' || json.Unmarshal(c.body, &policy) != nil {
		return nil, &apiError{Status: http.StatusBadRequest, Code: "MalformedPolicy", Message: "Policies must be valid JSON and the first byte must be '{'"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	b.Policy = string(c.body)
	return &reply{status: http.StatusNoContent}, nil
}

type corsXML struct {
	Rules []struct {
		AllowedMethods []string `xml:"AllowedMethod"`
		AllowedOrigins []string `xml:"AllowedOrigin"`
	} `xml:"CORSRule"`
}

func (s *Server) putBucketCors(c *call) (*reply, *apiError) {
	var in corsXML
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if len(in.Rules) == 0 || len(in.Rules) > 100 {
		return nil, malformedXML()
	}
	for _, rule := range in.Rules {
		if len(rule.AllowedMethods) == 0 || len(rule.AllowedOrigins) == 0 {
			return nil, malformedXML()
		}
		for _, m := range rule.AllowedMethods {
			if !slices.Contains([]string{"GET", "PUT", "HEAD", "POST", "DELETE"}, m) {
				return nil, &apiError{
					Status: http.StatusBadRequest, Code: "InvalidRequest",
					Message: "Found unsupported HTTP method in CORS config. Unsupported method is " + m,
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	b.CORS = string(c.body)
	return &reply{}, nil
}

// Buckets returns every bucket the account owns, ordered by name.
func (s *Server) Buckets() []Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Bucket, 0, len(s.buckets))
	for _, name := range slices.Sorted(maps.Keys(s.buckets)) {
		out = append(out, s.buckets[name].clone())
	}
	return out
}

// LookupBucket returns the named bucket, if the account owns it.
func (s *Server) LookupBucket(name string) (Bucket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return Bucket{}, false
	}
	return b.clone(), true
}

// PutBucket stores b, which needs a valid name and a region, keeping the objects of a bucket it replaces.
func (s *Server) PutBucket(b Bucket) (Bucket, error) {
	if !validBucketName(b.Name) {
		return Bucket{}, invalidBucketName(b.Name)
	}
	if b.Region == "" {
		return Bucket{}, errors.New("squall: a bucket needs a region")
	}
	if b.Versioning != "" && b.Versioning != versioningEnabled && b.Versioning != versioningSuspended {
		return Bucket{}, fmt.Errorf("squall: versioning %q is not one of Enabled and Suspended", b.Versioning)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.foreign[b.Name]; ok {
		return Bucket{}, fmt.Errorf("squall: bucket %s belongs to another account", b.Name)
	}
	if b.CreationDate.IsZero() {
		b.CreationDate = s.now()
	}
	b.CreationDate = b.CreationDate.UTC()
	b.Tags = maps.Clone(b.Tags)
	stored := &bucket{Bucket: b, objects: map[string][]*version{}}
	if old, ok := s.buckets[b.Name]; ok {
		stored.objects, stored.nullSeqs = old.objects, old.nullSeqs
	}
	s.buckets[b.Name] = stored
	return stored.clone(), nil
}

// PutForeignBucket holds name in region for another account, so creating it conflicts and calls on it are denied.
func (s *Server) PutForeignBucket(name, region string) error {
	if !validBucketName(name) {
		return invalidBucketName(name)
	}
	if region == "" {
		return errors.New("squall: a bucket needs a region")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return fmt.Errorf("squall: the account owns bucket %s", name)
	}
	s.foreign[name] = region
	return nil
}
