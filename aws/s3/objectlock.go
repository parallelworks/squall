package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/parallelworks/squall/access"
)

const (
	lockGovernance = "GOVERNANCE"
	lockCompliance = "COMPLIANCE"

	legalHoldOn  = "ON"
	legalHoldOff = "OFF"

	governanceOverride = "s3:BypassGovernanceRetention"

	maxRetentionDays  = 36500
	maxRetentionYears = 100
)

// DefaultRetention is the retention an Object Lock bucket gives each new version: a mode and Days or Years.
type DefaultRetention struct {
	// Mode is GOVERNANCE or COMPLIANCE.
	Mode  string
	Days  int
	Years int
}

func (d *DefaultRetention) valid() error {
	if d.Mode != lockGovernance && d.Mode != lockCompliance {
		return fmt.Errorf("squall: retention mode %q is not one of GOVERNANCE and COMPLIANCE", d.Mode)
	}
	if (d.Days == 0) == (d.Years == 0) {
		return errors.New("squall: a default retention needs one of Days and Years")
	}
	if d.Days < 0 || d.Years < 0 || d.Days > maxRetentionDays || d.Years > maxRetentionYears {
		return errors.New("squall: a default retention runs from 1 to 36500 days or 1 to 100 years")
	}
	return nil
}

// objectLock is a version's retention and legal hold.
type objectLock struct {
	mode  string
	until time.Time
	// hold is "" for a version never given a legal hold, "ON" or "OFF".
	hold string
}

// retains reports whether the lock's retention period is still running at now.
func (l objectLock) retains(now time.Time) bool {
	return l.mode != "" && l.until.After(now)
}

func objectLocked() *apiError {
	return &apiError{Status: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied because object protected by object lock."}
}

func missingLockConfiguration() *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: "Bucket is missing Object Lock Configuration"}
}

func noSuchObjectLock() *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "NoSuchObjectLockConfiguration", Message: "The specified object does not have a ObjectLock configuration"}
}

// retainUntil keeps a retain-until date to the millisecond, the precision S3's timestamps carry.
func retainUntil(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// lockChecksumMissing refuses a write with Object Lock parameters and no checksum; what is "Put Object" or "Put Part".
func lockChecksumMissing(what string) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Code: "InvalidRequest",
		Message: "Content-MD5 OR x-amz-checksum- HTTP header is required for " + what + " requests with Object Lock parameters",
	}
}

func checksumMissing() *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: "Missing required header for this request: Content-MD5"}
}

// decodeRoot reads an XML request body into v, which must be the root element named root.
func decodeRoot(c *call, root string, v any) *apiError {
	if !hasChecksum(c.r.Header) {
		return checksumMissing()
	}
	return decodeRootXML(c.body, root, v)
}

func retainInPast() *apiError {
	return invalidArgument("The retain until date must be in the future!")
}

// protects returns why v may not be deleted now, or nil. The caller holds s.mu.
func (s *Server) protects(v *version, bypass bool, method, resource string) *apiError {
	if !v.deleteMarker && v.lock.hold == legalHoldOn {
		return objectLocked()
	}
	return s.retentionProtects(v, bypass, method, resource)
}

// retentionProtects is protects without the legal hold. The caller holds s.mu.
func (s *Server) retentionProtects(v *version, bypass bool, method, resource string) *apiError {
	if v.deleteMarker || !v.lock.retains(s.now()) {
		return nil
	}
	if v.lock.mode == lockCompliance || !bypass {
		return objectLocked()
	}
	if s.policy.Check(access.Call{Method: method, Resource: resource, Needs: []string{governanceOverride}}) != nil {
		return notAuthorized(governanceOverride, resource)
	}
	return nil
}

// lockRequest is the Object Lock a PutObject or CreateMultipartUpload asks for in its headers.
type lockRequest struct {
	mode  string
	until time.Time
	hold  string
}

func (l lockRequest) any() bool { return l.mode != "" || l.hold != "" }

// lockHeaders reads the x-amz-object-lock headers of a request that writes an object.
func (s *Server) lockHeaders(h http.Header) (lockRequest, *apiError) {
	for name := range h {
		known := func(n string) bool { return strings.EqualFold(n, name) }
		if hasPrefixFold(name, "X-Amz-Object-Lock-") &&
			!slices.ContainsFunc([]string{"X-Amz-Object-Lock-Mode", "X-Amz-Object-Lock-Retain-Until-Date", "X-Amz-Object-Lock-Legal-Hold"}, known) {
			return lockRequest{}, invalidArgument("squall: the %s header is not supported", name)
		}
	}
	var l lockRequest
	mode, until := h.Get("X-Amz-Object-Lock-Mode"), h.Get("X-Amz-Object-Lock-Retain-Until-Date")
	if (mode == "") != (until == "") {
		return lockRequest{}, invalidArgument("x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
	}
	if mode != "" {
		if mode != lockGovernance && mode != lockCompliance {
			return lockRequest{}, invalidArgument("Unknown wormMode directive.")
		}
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return lockRequest{}, invalidArgument("The retain until date is not a valid date.")
		}
		t = retainUntil(t)
		if !t.After(s.now()) {
			return lockRequest{}, retainInPast()
		}
		l.mode, l.until = mode, t
	}
	switch hold := h.Get("X-Amz-Object-Lock-Legal-Hold"); hold {
	case "", legalHoldOn, legalHoldOff:
		l.hold = hold
	default:
		return lockRequest{}, invalidArgument("Legal Hold must be either of 'ON' or 'OFF'")
	}
	return l, nil
}

// checkLockable refuses a lock on a bucket without Object Lock.
func checkLockable(b *bucket, l lockRequest) *apiError {
	if l.any() && !b.ObjectLock {
		return missingLockConfiguration()
	}
	return nil
}

// applyLock gives a new version l, or the bucket's default retention when l names none. The caller holds s.mu.
func (s *Server) applyLock(b *bucket, v *version, l lockRequest) {
	switch {
	case l.mode != "":
		v.lock.mode, v.lock.until = l.mode, l.until
	case b.DefaultRetention != nil:
		d := b.DefaultRetention
		v.lock.mode, v.lock.until = d.Mode, retainUntil(v.modified.AddDate(d.Years, 0, d.Days))
	}
	v.lock.hold = l.hold
}

// hasChecksum reports whether a request carries Content-MD5 or a checksum, in a header or a trailer.
func hasChecksum(h http.Header) bool {
	if h.Get("Content-MD5") != "" {
		return true
	}
	trailer := strings.ToLower(h.Get("X-Amz-Trailer"))
	for _, name := range checksumHeaders {
		if h.Get(name) != "" || strings.Contains(trailer, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// checksumHeaders are the headers that carry a checksum's value.
var checksumHeaders = []string{
	"X-Amz-Checksum-Crc32", "X-Amz-Checksum-Crc32c", "X-Amz-Checksum-Crc64nvme", "X-Amz-Checksum-Sha1", "X-Amz-Checksum-Sha256",
	"X-Amz-Checksum-Sha512", "X-Amz-Checksum-Md5", "X-Amz-Checksum-Xxhash64", "X-Amz-Checksum-Xxhash3", "X-Amz-Checksum-Xxhash128",
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

type objectLockConfigurationXML struct {
	XMLName           xml.Name
	ObjectLockEnabled string             `xml:"ObjectLockEnabled,omitempty"`
	Rule              *objectLockRuleXML `xml:"Rule,omitempty"`
}

type objectLockRuleXML struct {
	DefaultRetention *defaultRetentionXML `xml:"DefaultRetention"`
}

type defaultRetentionXML struct {
	Mode             string    `xml:"Mode,omitempty"`
	Days             *int      `xml:"Days,omitempty"`
	Years            *int      `xml:"Years,omitempty"`
	DefaultEventHold *struct{} `xml:"DefaultEventHold,omitempty"`
}

func (s *Server) getObjectLockConfiguration(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if !b.ObjectLock {
		return nil, &apiError{
			Status: http.StatusNotFound, Code: "ObjectLockConfigurationNotFoundError",
			Message: "Object Lock configuration does not exist for this bucket", Bucket: c.bucket,
		}
	}
	out := objectLockConfigurationXML{XMLName: xml.Name{Space: namespace, Local: "ObjectLockConfiguration"}, ObjectLockEnabled: "Enabled"}
	if d := b.DefaultRetention; d != nil {
		r := &defaultRetentionXML{Mode: d.Mode}
		if d.Days > 0 {
			r.Days = &d.Days
		} else {
			r.Years = &d.Years
		}
		out.Rule = &objectLockRuleXML{DefaultRetention: r}
	}
	return &reply{body: out}, nil
}

func (s *Server) putObjectLockConfiguration(c *call) (*reply, *apiError) {
	var in objectLockConfigurationXML
	if err := decodeRoot(c, "ObjectLockConfiguration", &in); err != nil {
		return nil, err
	}
	if in.ObjectLockEnabled != "Enabled" {
		return nil, malformedXML()
	}
	var retention *DefaultRetention
	if in.Rule != nil {
		r := in.Rule.DefaultRetention
		if r == nil || r.Mode != lockGovernance && r.Mode != lockCompliance || (r.Days == nil) == (r.Years == nil) {
			return nil, malformedXML()
		}
		if r.DefaultEventHold != nil {
			return nil, invalidArgument("squall: default event holds are not supported")
		}
		retention = &DefaultRetention{Mode: r.Mode}
		if r.Days != nil {
			retention.Days = *r.Days
		} else {
			retention.Years = *r.Years
		}
		if retention.Days < 0 || retention.Years < 0 || retention.Days+retention.Years == 0 {
			return nil, invalidArgument("Default retention period must be a positive integer value.")
		}
		if retention.Days > maxRetentionDays || retention.Years > maxRetentionYears {
			return nil, invalidArgument("Default retention period is too large.")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if !b.ObjectLock && b.Versioning != versioningEnabled {
		return nil, &apiError{
			Status: http.StatusConflict, Code: "InvalidBucketState",
			Message: "Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration", Bucket: c.bucket,
		}
	}
	b.ObjectLock, b.DefaultRetention = true, retention
	return &reply{}, nil
}

// lockedVersion returns the version a retention or legal hold call names, in a bucket with Object Lock. The caller holds s.mu.
func (s *Server) lockedVersion(c *call) (*version, *apiError) {
	id := c.query.Get("versionId")
	if c.query.Has("versionId") {
		if _, ok := validVersionID(id); !ok {
			return nil, invalidVersion()
		}
	}
	b, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if !b.ObjectLock {
		return nil, missingLockConfiguration()
	}
	return findVersion(b, c.key, id)
}

// findVersion returns version id of key, or its newest with no id. The caller holds s.mu.
func findVersion(b *bucket, key, id string) (*version, *apiError) {
	versions := b.objects[key]
	if id == "" {
		if len(versions) == 0 || versions[0].deleteMarker {
			return nil, &apiError{Status: http.StatusNotFound, Code: "NoSuchKey", Message: "The specified key does not exist."}
		}
		return versions[0], nil
	}
	i := slices.IndexFunc(versions, func(v *version) bool { return v.id == id })
	if i < 0 {
		return nil, &apiError{Status: http.StatusNotFound, Code: "NoSuchVersion", Message: "The specified version does not exist."}
	}
	if versions[i].deleteMarker {
		return nil, &apiError{Status: http.StatusMethodNotAllowed, Code: "MethodNotAllowed", Message: "The specified method is not allowed against this resource."}
	}
	return versions[i], nil
}

type retentionXML struct {
	XMLName           xml.Name
	Mode              string    `xml:"Mode,omitempty"`
	RetainUntilDate   string    `xml:"RetainUntilDate,omitempty"`
	EventHold         *string   `xml:"EventHold,omitempty"`
	EventHoldDuration *struct{} `xml:"EventHoldDuration,omitempty"`
}

func (s *Server) getObjectRetention(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lockedVersion(c)
	if err != nil {
		return nil, err
	}
	if v.lock.mode == "" {
		return nil, noSuchObjectLock()
	}
	return &reply{body: retentionXML{
		XMLName: xml.Name{Space: namespace, Local: "Retention"}, Mode: v.lock.mode, RetainUntilDate: v.lock.until.Format(timeFormat),
	}}, nil
}

func (s *Server) putObjectRetention(c *call) (*reply, *apiError) {
	var in retentionXML
	if err := decodeRoot(c, "Retention", &in); err != nil {
		return nil, err
	}
	if in.EventHold != nil || in.EventHoldDuration != nil {
		return nil, invalidArgument("squall: event holds are not supported")
	}
	if (in.Mode == "") != (in.RetainUntilDate == "") || in.Mode != "" && in.Mode != lockGovernance && in.Mode != lockCompliance {
		return nil, malformedXML()
	}
	var until time.Time
	if in.RetainUntilDate != "" {
		t, err := time.Parse(time.RFC3339, in.RetainUntilDate)
		if err != nil {
			return nil, malformedXML()
		}
		until = retainUntil(t)
		if !until.After(s.now()) {
			return nil, retainInPast()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lockedVersion(c)
	if err != nil {
		return nil, err
	}
	if in.Mode != v.lock.mode || until.Before(v.lock.until) {
		if err := s.retentionProtects(v, bypassing(c), "s3.PutObjectRetention", c.resource()); err != nil {
			return nil, err
		}
	}
	v.lock.mode, v.lock.until = in.Mode, until
	return &reply{}, nil
}

type legalHoldXML struct {
	XMLName xml.Name
	Status  string `xml:"Status"`
}

func (s *Server) getObjectLegalHold(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lockedVersion(c)
	if err != nil {
		return nil, err
	}
	if v.lock.hold == "" {
		return nil, noSuchObjectLock()
	}
	return &reply{body: legalHoldXML{XMLName: xml.Name{Space: namespace, Local: "LegalHold"}, Status: v.lock.hold}}, nil
}

func (s *Server) putObjectLegalHold(c *call) (*reply, *apiError) {
	var in legalHoldXML
	if err := decodeRoot(c, "LegalHold", &in); err != nil {
		return nil, err
	}
	if in.Status != legalHoldOn && in.Status != legalHoldOff {
		return nil, malformedXML()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lockedVersion(c)
	if err != nil {
		return nil, err
	}
	v.lock.hold = in.Status
	return &reply{}, nil
}

// testVersion returns the version a test helper names, in a bucket with Object Lock. The caller holds s.mu.
func (s *Server) testVersion(bucketName, key, versionID string) (*version, error) {
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil, noSuchBucket(bucketName)
	}
	if !b.ObjectLock {
		return nil, missingLockConfiguration()
	}
	v, err := findVersion(b, key, versionID)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SetRetention gives a version of key in the named bucket a retention period in mode, GOVERNANCE or
// COMPLIANCE, until until, or with mode "" removes it, whatever retention the version has. An empty
// versionID names the newest version. The bucket needs Object Lock.
func (s *Server) SetRetention(bucketName, key, versionID, mode string, until time.Time) error {
	if mode != "" && mode != lockGovernance && mode != lockCompliance {
		return fmt.Errorf("squall: retention mode %q is not one of GOVERNANCE and COMPLIANCE", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.testVersion(bucketName, key, versionID)
	if err != nil {
		return err
	}
	v.lock.mode, v.lock.until = mode, time.Time{}
	if mode != "" {
		v.lock.until = retainUntil(until)
	}
	return nil
}

// SetLegalHold places or removes a legal hold on a version of key in the named bucket. An empty versionID
// names the newest version. The bucket needs Object Lock.
func (s *Server) SetLegalHold(bucketName, key, versionID string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.testVersion(bucketName, key, versionID)
	if err != nil {
		return err
	}
	v.lock.hold = legalHoldOff
	if on {
		v.lock.hold = legalHoldOn
	}
	return nil
}
