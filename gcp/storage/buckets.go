package storage

import (
	"cmp"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxLabels      = 64
	maxLabelLength = 63
)

// Bucket is a bucket, in the API's JSON shape.
type Bucket struct {
	Kind                  string                 `json:"kind"`
	ID                    string                 `json:"id"`
	SelfLink              string                 `json:"selfLink"`
	ProjectNumber         uint64                 `json:"projectNumber,string"`
	Name                  string                 `json:"name"`
	TimeCreated           string                 `json:"timeCreated"`
	Updated               string                 `json:"updated"`
	Metageneration        int64                  `json:"metageneration,string"`
	Location              string                 `json:"location"`
	LocationType          string                 `json:"locationType"`
	StorageClass          string                 `json:"storageClass"`
	Etag                  string                 `json:"etag"`
	Labels                map[string]string      `json:"labels,omitzero"`
	Versioning            *Versioning            `json:"versioning,omitzero"`
	CORS                  json.RawMessage        `json:"cors,omitzero"`
	CustomPlacementConfig *CustomPlacementConfig `json:"customPlacementConfig,omitzero"`
	Encryption            *Encryption            `json:"encryption,omitzero"`
	RPO                   string                 `json:"rpo,omitzero"`

	// Project is the ID of the project the bucket is in; it is not in the JSON.
	Project string `json:"-"`
}

// Versioning is a bucket's versioning setting.
type Versioning struct {
	Enabled bool `json:"enabled"`
}

// CustomPlacementConfig names the regions of a configurable dual-region.
type CustomPlacementConfig struct {
	DataLocations []string `json:"dataLocations"`
}

// Encryption is a bucket's default encryption.
type Encryption struct {
	DefaultKmsKeyName string `json:"defaultKmsKeyName,omitzero"`
}

type bucket struct {
	Bucket
	// objects holds each name's generations, oldest first; the last is live unless it has TimeDeleted.
	objects map[string][]*Object
}

func (b *bucket) clone() Bucket {
	c := b.Bucket
	c.Labels = maps.Clone(b.Labels)
	if b.Versioning != nil {
		c.Versioning = new(*b.Versioning)
	}
	c.CORS = slices.Clone(b.CORS)
	if b.CustomPlacementConfig != nil {
		c.CustomPlacementConfig = &CustomPlacementConfig{DataLocations: slices.Clone(b.CustomPlacementConfig.DataLocations)}
	}
	if b.Encryption != nil {
		c.Encryption = new(*b.Encryption)
	}
	return c
}

func (b *bucket) versioned() bool { return b.Versioning != nil && b.Versioning.Enabled }

func (b *bucket) empty() bool { return len(b.objects) == 0 }

// touch records a change to b's metadata. The caller holds s.mu.
func (s *Server) touch(b *bucket) {
	b.Metageneration++
	b.Updated = s.now().UTC().Format(timeFormat)
	b.Etag = etag(b.Metageneration)
}

// etag encodes its numbers as tagged varints, so it changes whenever they do.
func etag(numbers ...int64) string {
	var buf []byte
	for i, n := range numbers {
		buf = append(buf, byte(i+1)<<3)
		buf = binary.AppendVarint(buf, n)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// projectNumber derives a 12-digit project number from a project ID.
func projectNumber(project string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(project))
	return 100_000_000_000 + h.Sum64()%900_000_000_000
}

// validBucketName reports whether name follows the bucket naming rules.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 222 || strings.HasPrefix(name, "goog") || strings.Contains(name, "google") || strings.Contains(name, "g00gle") {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for part := range strings.SplitSeq(name, ".") {
		if part == "" || len(part) > 63 {
			return false
		}
	}
	if len(name) > 63 && !strings.Contains(name, ".") {
		return false
	}
	for _, c := range []byte(name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	return alnum(name[0]) && alnum(name[len(name)-1])
}

// validLabel reports whether s is a valid label key or, with key false, value.
func validLabel(s string, key bool) bool {
	if utf8.RuneCountInString(s) > maxLabelLength || key && s == "" {
		return false
	}
	for i, c := range s {
		letter := unicode.IsLetter(c) && !unicode.IsUpper(c)
		if !letter && (i == 0 && key || !unicode.IsDigit(c) && c != '_' && c != '-') {
			return false
		}
	}
	return true
}

func validateLabels(labels map[string]string) *apiError {
	if len(labels) > maxLabels {
		return invalid("Too many labels: %d, more than %d.", len(labels), maxLabels)
	}
	for k, v := range labels {
		if !validLabel(k, true) || !validLabel(v, false) {
			return invalid("Invalid argument: label '%s: %s' violates format constraints.", k, v)
		}
	}
	return nil
}

var storageClasses = []string{"STANDARD", "NEARLINE", "COLDLINE", "ARCHIVE", "MULTI_REGIONAL", "REGIONAL", "DURABLE_REDUCED_AVAILABILITY"}

func locationType(location string, custom *CustomPlacementConfig) string {
	switch {
	case custom != nil || slices.Contains([]string{"NAM4", "EUR4", "EUR5", "EUR7", "EUR8", "ASIA1"}, location):
		return "dual-region"
	case slices.Contains([]string{"US", "EU", "ASIA"}, location):
		return "multi-region"
	}
	return "region"
}

func notFoundBucket() *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: "The specified bucket does not exist."}
}

func forbidden(permission string) *apiError {
	return &apiError{
		Status: http.StatusForbidden, Reason: "forbidden",
		Message: fmt.Sprintf("The caller does not have %s access to the Google Cloud Storage bucket. Permission '%s' denied on resource (or it may not exist).", permission, permission),
	}
}

// lookup returns the named bucket, or the error a call needing permission fails with. The caller holds s.mu.
func (s *Server) lookup(name, permission string) (*bucket, *apiError) {
	if b, ok := s.buckets[name]; ok {
		return b, nil
	}
	if s.foreign[name] {
		return nil, forbidden(permission)
	}
	return nil, notFoundBucket()
}

// bucketInput is what buckets.insert and buckets.patch take.
type bucketInput struct {
	Name                  string                 `json:"name"`
	Location              string                 `json:"location"`
	StorageClass          string                 `json:"storageClass"`
	Labels                map[string]*string     `json:"labels"`
	Versioning            *Versioning            `json:"versioning"`
	CORS                  json.RawMessage        `json:"cors"`
	CustomPlacementConfig *CustomPlacementConfig `json:"customPlacementConfig"`
	Encryption            *Encryption            `json:"encryption"`
	RPO                   string                 `json:"rpo"`
	// Lifecycle is accepted without rules, since the client sends it empty.
	Lifecycle *struct {
		Rule []json.RawMessage `json:"rule"`
	} `json:"lifecycle"`
}

// check rejects what the server does not model.
func (in *bucketInput) check() *apiError {
	if in.Lifecycle != nil && len(in.Lifecycle.Rule) > 0 {
		return invalid("squall: lifecycle rules are not supported")
	}
	return nil
}

func checkACLs(r *http.Request) *apiError {
	for _, p := range []string{"predefinedAcl", "predefinedDefaultObjectAcl"} {
		if v := r.URL.Query().Get(p); v != "" && v != "private" && v != "projectPrivate" {
			return invalid("squall: %s=%s is not supported", p, v)
		}
	}
	return nil
}

func (s *Server) insertBucket(r *http.Request) (any, *apiError) {
	project := r.URL.Query().Get("project")
	if project == "" {
		return nil, required("project")
	}
	if err := checkACLs(r); err != nil {
		return nil, err
	}
	if retention, err := boolParam(r, "enableObjectRetention"); err != nil {
		return nil, err
	} else if retention {
		return nil, invalid("squall: object retention is not supported")
	}
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	var in bucketInput
	if err := unmarshalStrict(body, &in); err != nil {
		return nil, err
	}
	if err := in.check(); err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for k, v := range in.Labels {
		if v != nil {
			labels[k] = *v
		}
	}
	b := Bucket{
		Name: in.Name, Location: in.Location, StorageClass: in.StorageClass, Labels: labels, Versioning: in.Versioning,
		CORS: in.CORS, CustomPlacementConfig: in.CustomPlacementConfig, Encryption: in.Encryption, RPO: in.RPO, Project: project,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.store(b)
	if err != nil {
		return nil, err
	}
	return stored.clone(), nil
}

// store validates b, fills in what it leaves empty and stores it as a new bucket. The caller holds s.mu.
func (s *Server) store(b Bucket) (*bucket, *apiError) {
	if b.Name == "" {
		return nil, required("resource.name")
	}
	if !validBucketName(b.Name) {
		return nil, invalid("Invalid bucket name: '%s'", b.Name)
	}
	if err := validateLabels(b.Labels); err != nil {
		return nil, err
	}
	b.StorageClass = strings.ToUpper(cmp.Or(b.StorageClass, "STANDARD"))
	if !slices.Contains(storageClasses, b.StorageClass) {
		return nil, invalid("Invalid argument: storage class '%s' is not valid.", b.StorageClass)
	}
	if c := b.CustomPlacementConfig; c != nil && len(c.DataLocations) != 2 {
		return nil, invalid("Invalid argument: a custom dual-region takes exactly two data locations.")
	}
	if s.foreign[b.Name] {
		return nil, &apiError{
			Status: http.StatusConflict, Reason: "conflict",
			Message: "Sorry, that name is not available. Please try a different one.",
		}
	}
	if _, ok := s.buckets[b.Name]; ok {
		return nil, &apiError{
			Status: http.StatusConflict, Reason: "conflict",
			Message: "You already own this bucket. Please select another name.",
		}
	}
	now := s.now().UTC().Format(timeFormat)
	b.Kind, b.ID, b.SelfLink = "storage#bucket", b.Name, apiBase+"b/"+b.Name
	b.ProjectNumber = projectNumber(b.Project)
	b.TimeCreated, b.Updated, b.Metageneration, b.Etag = now, now, 1, etag(1)
	b.Location = strings.ToUpper(cmp.Or(b.Location, "US"))
	b.LocationType = locationType(b.Location, b.CustomPlacementConfig)
	if len(b.Labels) == 0 {
		b.Labels = nil
	}
	stored := &bucket{Bucket: b, objects: map[string][]*Object{}}
	stored.Bucket = stored.clone()
	s.buckets[b.Name] = stored
	return stored, nil
}

func (s *Server) getBucket(r *http.Request) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(r.PathValue("bucket"), "storage.buckets.get")
	if err != nil {
		return nil, err
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", b.Metageneration); err != nil {
		return nil, err
	}
	return b.clone(), nil
}

func (s *Server) patchBucket(r *http.Request) (any, *apiError) {
	if err := checkACLs(r); err != nil {
		return nil, err
	}
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	var in bucketInput
	if err := unmarshalStrict(body, &in); err != nil {
		return nil, err
	}
	if err := in.check(); err != nil {
		return nil, err
	}
	if in.Name != "" || in.Location != "" || in.CustomPlacementConfig != nil || in.RPO != "" {
		return nil, invalid("squall: a patch may change only labels, versioning, cors, storageClass and encryption")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, invalid("Invalid JSON payload received: %v", err)
	}
	cleared := func(field string) bool { return string(fields[field]) == "null" }
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(r.PathValue("bucket"), "storage.buckets.update")
	if err != nil {
		return nil, err
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", b.Metageneration); err != nil {
		return nil, err
	}
	labels := maps.Clone(b.Labels)
	if labels == nil || cleared("labels") {
		labels = map[string]string{}
	}
	for k, v := range in.Labels {
		if v == nil {
			delete(labels, k)
		} else {
			labels[k] = *v
		}
	}
	if err := validateLabels(labels); err != nil {
		return nil, err
	}
	class := b.StorageClass
	if in.StorageClass != "" {
		class = strings.ToUpper(in.StorageClass)
		if !slices.Contains(storageClasses, class) {
			return nil, invalid("Invalid argument: storage class '%s' is not valid.", class)
		}
	}
	if len(labels) == 0 {
		labels = nil
	}
	b.Labels, b.StorageClass = labels, class
	if in.Versioning != nil {
		b.Versioning = new(*in.Versioning)
	} else if cleared("versioning") {
		b.Versioning = nil
	}
	if in.CORS != nil {
		b.CORS = slices.Clone(in.CORS)
		if string(in.CORS) == "null" || string(in.CORS) == "[]" {
			b.CORS = nil
		}
	}
	if in.Encryption != nil {
		b.Encryption = new(*in.Encryption)
	} else if cleared("encryption") {
		b.Encryption = nil
	}
	s.touch(b)
	return b.clone(), nil
}

func (s *Server) deleteBucket(r *http.Request) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("bucket")
	b, err := s.lookup(name, "storage.buckets.delete")
	if err != nil {
		return nil, err
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", b.Metageneration); err != nil {
		return nil, err
	}
	if !b.empty() {
		return nil, &apiError{Status: http.StatusConflict, Reason: "conflict", Message: "The bucket you tried to delete is not empty."}
	}
	delete(s.buckets, name)
	return nil, nil
}

type bucketList struct {
	Kind          string   `json:"kind"`
	Items         []Bucket `json:"items,omitzero"`
	NextPageToken string   `json:"nextPageToken,omitzero"`
}

func (s *Server) listBuckets(r *http.Request) (any, *apiError) {
	project := r.URL.Query().Get("project")
	if project == "" {
		return nil, required("project")
	}
	if _, err := boolParam(r, "returnPartialSuccess"); err != nil {
		return nil, err
	}
	prefix := r.URL.Query().Get("prefix")
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Bucket
	for _, name := range slices.Sorted(maps.Keys(s.buckets)) {
		if b := s.buckets[name]; b.Project == project && strings.HasPrefix(name, prefix) {
			all = append(all, b.clone())
		}
	}
	items, next, err := page(r, all, func(b Bucket) string { return b.Name })
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Project = ""
	}
	return bucketList{Kind: "storage#buckets", Items: items, NextPageToken: next}, nil
}

// Buckets returns every bucket the caller has, ordered by name.
func (s *Server) Buckets() []Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Bucket, 0, len(s.buckets))
	for _, name := range slices.Sorted(maps.Keys(s.buckets)) {
		out = append(out, s.buckets[name].clone())
	}
	return out
}

// LookupBucket returns the named bucket, if the caller has it.
func (s *Server) LookupBucket(name string) (Bucket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return Bucket{}, false
	}
	return b.clone(), true
}

// PutBucket creates b in its Project from the fields buckets.insert takes, and returns it as stored.
func (s *Server) PutBucket(b Bucket) (Bucket, error) {
	if b.Project == "" {
		return Bucket{}, errors.New("squall: a bucket needs a project")
	}
	in := Bucket{
		Name: b.Name, Location: b.Location, StorageClass: b.StorageClass, Labels: b.Labels, Versioning: b.Versioning,
		CORS: b.CORS, CustomPlacementConfig: b.CustomPlacementConfig, Encryption: b.Encryption, RPO: b.RPO, Project: b.Project,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.store((&bucket{Bucket: in}).clone())
	if err != nil {
		return Bucket{}, err
	}
	return stored.clone(), nil
}

// PutForeignBucket holds name for someone the caller cannot reach, so inserting it conflicts and calls on it are forbidden.
func (s *Server) PutForeignBucket(name string) error {
	if !validBucketName(name) {
		return invalid("Invalid bucket name: '%s'", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return fmt.Errorf("squall: the caller has bucket %s", name)
	}
	s.foreign[name] = true
	return nil
}

// timestamp reads the server's clock, and formats it as the API does.
func (s *Server) timestamp() (time.Time, string) {
	t := s.now().UTC()
	return t, t.Format(timeFormat)
}
