package storage

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// softDeletedGeneration reads the softDeleted and generation parameters of a get, each of which needs the other.
func softDeletedGeneration(r *http.Request) (bool, int64, *apiError) {
	soft, err := boolParam(r, "softDeleted")
	if err != nil {
		return false, 0, err
	}
	gen, ok, err := int64Param(r, "generation")
	if err != nil {
		return false, 0, err
	}
	switch {
	case soft && !ok:
		return false, 0, required("generation")
	case ok && !soft && r.PathValue("object") == "":
		return false, 0, required("softDeleted")
	}
	return soft, gen, nil
}

// trashed returns soft-deleted generation gen of name in b, or nil.
func (b *bucket) trashed(name string, gen int64) *Object {
	for _, o := range b.trash {
		if o.Name == name && o.Generation == gen {
			return o
		}
	}
	return nil
}

// purgeTrash drops the soft-deleted generations in b whose retention has ended. The caller holds s.mu.
func (s *Server) purgeTrash(b *bucket) {
	now := s.now()
	if len(b.trash) == 0 || now.Before(b.nextPurge) {
		return
	}
	b.trash = slices.DeleteFunc(b.trash, func(o *Object) bool { return !now.Before(o.hardDelete) })
	b.nextPurge = time.Time{}
	for _, o := range b.trash {
		b.keepUntil(o.hardDelete)
	}
}

// keepUntil records that b holds a soft-deleted generation until t.
func (b *bucket) keepUntil(t time.Time) {
	if b.nextPurge.IsZero() || t.Before(b.nextPurge) {
		b.nextPurge = t
	}
}

// softDeleteBucket keeps b, which was just deleted, if it keeps deletions. The caller holds s.mu.
func (s *Server) softDeleteBucket(b *bucket) {
	keep := b.softDeleteDuration()
	if keep == 0 {
		return
	}
	t, now := s.timestamp()
	b.SoftDeleteTime, b.hardDelete = now, t.Add(keep)
	b.HardDeleteTime = b.hardDelete.Format(timeFormat)
	s.softBuckets[b.Name] = append(s.softBuckets[b.Name], b)
}

// purgeBuckets drops the soft-deleted buckets whose retention has ended. The caller holds s.mu.
func (s *Server) purgeBuckets() {
	now := s.now()
	for name, gens := range s.softBuckets {
		gens = slices.DeleteFunc(gens, func(b *bucket) bool { return !now.Before(b.hardDelete) })
		if len(gens) == 0 {
			delete(s.softBuckets, name)
		} else {
			s.softBuckets[name] = gens
		}
	}
}

// softDeletedBucket returns soft-deleted generation gen of the named bucket, or nil. The caller holds s.mu.
func (s *Server) softDeletedBucket(name string, gen int64) *bucket {
	s.purgeBuckets()
	for _, b := range s.softBuckets[name] {
		if b.Generation == gen {
			return b
		}
	}
	return nil
}

// softDeletedBuckets returns every soft-deleted bucket, ordered by name and generation. The caller holds s.mu.
func (s *Server) softDeletedBuckets() []Bucket {
	var out []Bucket
	for _, name := range slices.Sorted(maps.Keys(s.softBuckets)) {
		gens := slices.SortedFunc(slices.Values(s.softBuckets[name]), func(a, b *bucket) int { return int(a.Generation - b.Generation) })
		for _, b := range gens {
			out = append(out, b.clone())
		}
	}
	return out
}

func (s *Server) restoreBucket(r *http.Request) (any, *apiError) {
	gen, ok, err := int64Param(r, "generation")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, required("generation")
	}
	name := r.PathValue("bucket")
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.softDeletedBucket(name, gen)
	if b == nil {
		if s.foreign[name] {
			return nil, forbidden("storage.buckets.restore")
		}
		return nil, notFoundBucket()
	}
	if _, ok := s.buckets[name]; ok || s.foreign[name] {
		return nil, &apiError{Status: http.StatusConflict, Reason: "conflict", Message: "The requested bucket name is not available."}
	}
	s.softBuckets[name] = slices.DeleteFunc(s.softBuckets[name], func(g *bucket) bool { return g == b })
	if len(s.softBuckets[name]) == 0 {
		delete(s.softBuckets, name)
	}
	b.SoftDeleteTime, b.HardDeleteTime = "", ""
	s.buckets[name] = b
	s.purgeTrash(b)
	return b.clone(), nil
}

func (s *Server) restoreObject(r *http.Request) (any, *apiError) {
	gen, ok, err := int64Param(r, "generation")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, required("generation")
	}
	if _, err := boolParam(r, "copySourceAcl"); err != nil {
		return nil, err
	}
	bucketName, name := r.PathValue("bucket"), r.PathValue("object")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.lookup(bucketName, "storage.objects.restore")
	if err != nil {
		return nil, err
	}
	if b.softDeleteDuration() == 0 {
		return nil, &apiError{Status: http.StatusBadRequest, Reason: "SoftDeletePolicyRequired", Message: "Soft delete policy is required to restore soft-deleted objects."}
	}
	src := b.trashed(name, gen)
	if src == nil {
		if b.generation(name, gen) != nil {
			return nil, &apiError{Status: http.StatusPreconditionFailed, Reason: "objectNotSoftDeleted", Message: fmt.Sprintf("Object '%s/%s' is not soft-deleted.", bucketName, name)}
		}
		return nil, noSuchObject(bucketName, name)
	}
	var liveGen, liveMeta int64
	live := b.liveObject(name)
	if live != nil {
		liveGen, liveMeta = live.Generation, live.Metageneration
	}
	if err := checkMatch(r, "ifGenerationMatch", "ifGenerationNotMatch", liveGen); err != nil {
		return nil, err
	}
	if live == nil && (r.URL.Query().Has("ifMetagenerationMatch") || r.URL.Query().Has("ifMetagenerationNotMatch")) {
		return nil, conditionNotMet()
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", liveMeta); err != nil {
		return nil, err
	}
	if err := s.replaceLive(b, name); err != nil {
		return nil, err
	}
	o := s.newGeneration(b, name, "STANDARD")
	o.ContentType, o.ContentEncoding, o.Metadata = src.ContentType, src.ContentEncoding, maps.Clone(src.Metadata)
	o.Size, o.MD5Hash, o.CRC32C = src.Size, src.MD5Hash, src.CRC32C
	if b.DefaultEventBasedHold {
		o.EventBasedHold = new(true)
	}
	return s.view(b, o), nil
}

// SoftDeletedObjects returns the soft-deleted generations in the named bucket whose retention has not ended,
// ordered by name and generation.
func (s *Server) SoftDeletedObjects(bucketName string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	s.purgeTrash(b)
	out := make([]Object, 0, len(b.trash))
	for _, o := range b.trash {
		out = append(out, o.clone())
	}
	slices.SortFunc(out, func(a, b Object) int { return strings.Compare(versionKey(a), versionKey(b)) })
	return out
}

// SoftDeletedBuckets returns every soft-deleted bucket whose retention has not ended, ordered by name and generation.
func (s *Server) SoftDeletedBuckets() []Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeBuckets()
	return s.softDeletedBuckets()
}

// PutSoftDeletedObject writes data to name in the named bucket as a soft-deleted generation, as if a live one had
// just been deleted, and returns it. The bucket must keep deletions.
func (s *Server) PutSoftDeletedObject(bucketName, name string, data []byte) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return Object{}, notFoundBucket()
	}
	if b.softDeleteDuration() == 0 {
		return Object{}, errors.New("squall: bucket " + bucketName + " keeps no soft-deleted objects")
	}
	if !validObjectName(name) {
		return Object{}, invalid("The specified object name is not valid.")
	}
	o := s.newGeneration(b, name, b.StorageClass)
	o.ContentType, o.Size = "application/octet-stream", uint64(len(data))
	o.MD5Hash, o.CRC32C = checksums(data)
	s.discard(b, o)
	return o.clone(), nil
}
