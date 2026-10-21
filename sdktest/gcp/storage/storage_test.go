package storage_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/storage"
)

const project = "my-project"

func serve(t *testing.T, s *storage.Server) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

func clientAt(t *testing.T, url string) *gcs.Client {
	t.Helper()
	c, err := gcs.NewClient(t.Context(), option.WithEndpoint(url+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// The client retries a lost or failed call; a short backoff keeps the tests quick.
	c.SetRetry(gcs.WithBackoff(gax.Backoff{Initial: time.Millisecond, Max: 10 * time.Millisecond}))
	return c
}

func newClient(t *testing.T, s *storage.Server) *gcs.Client {
	t.Helper()
	return clientAt(t, serve(t, s))
}

func apiCode(err error) int {
	if e, ok := errors.AsType[*googleapi.Error](err); ok {
		return e.Code
	}
	return 0
}

func write(t *testing.T, c *gcs.Client, bucket, name, data string) *gcs.ObjectAttrs {
	t.Helper()
	w := c.Bucket(bucket).Object(name).NewWriter(t.Context())
	w.ContentType = "text/plain"
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return w.Attrs()
}

func names(t *testing.T, it *gcs.BucketIterator) []string {
	t.Helper()
	var out []string
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list buckets: %v", err)
		}
		out = append(out, b.Name)
	}
}

func objects(t *testing.T, it *gcs.ObjectIterator) []*gcs.ObjectAttrs {
	t.Helper()
	var out []*gcs.ObjectAttrs
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list objects: %v", err)
		}
		out = append(out, o)
	}
}

func TestCreateReadUpdateDelete(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	b := c.Bucket("data-bucket")
	err := b.Create(t.Context(), project, &gcs.BucketAttrs{
		Location: "us-east1",
		Labels:   map[string]string{"owner-id": "abc", "owner": "me"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	attrs, err := b.Attrs(t.Context())
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.Name != "data-bucket" || attrs.Location != "US-EAST1" || attrs.LocationType != "region" || attrs.StorageClass != "STANDARD" ||
		attrs.Labels["owner-id"] != "abc" || attrs.Created.IsZero() || attrs.ProjectNumber == 0 || attrs.MetaGeneration != 1 || attrs.VersioningEnabled {
		t.Fatalf("attrs = %+v", attrs)
	}

	ua := gcs.BucketAttrsToUpdate{CORS: []gcs.CORS{{Methods: []string{"GET"}, Origins: []string{"*"}, MaxAge: time.Hour}}}
	ua.SetLabel("team", "data")
	ua.DeleteLabel("owner")
	updated, err := b.Update(t.Context(), ua)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(updated.Labels) != 2 || updated.Labels["team"] != "data" || updated.Labels["owner-id"] != "abc" ||
		len(updated.CORS) != 1 || updated.CORS[0].MaxAge != time.Hour || updated.MetaGeneration != 2 {
		t.Fatalf("updated = %+v", updated)
	}
	if got, ok := s.LookupBucket("data-bucket"); !ok || got.Project != project || !strings.Contains(string(got.CORS), `"maxAgeSeconds":3600`) {
		t.Fatalf("lookup = %+v, %v", got, ok)
	}
	if _, err := b.If(gcs.BucketConditions{MetagenerationMatch: 1}).Update(t.Context(), ua); apiCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("a stale update = %v", err)
	}

	if err := b.Delete(t.Context()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.Attrs(t.Context()); !errors.Is(err, gcs.ErrBucketNotExist) {
		t.Fatalf("attrs after delete = %v", err)
	}
	if err := b.Delete(t.Context()); apiCode(err) != http.StatusNotFound {
		t.Fatalf("delete again = %v", err)
	}
}

func TestPlacements(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	for name, tc := range map[string]struct {
		attrs        gcs.BucketAttrs
		location, lt string
	}{
		"default":      {gcs.BucketAttrs{}, "US", "multi-region"},
		"multi-region": {gcs.BucketAttrs{Location: "eu"}, "EU", "multi-region"},
		"dual-region":  {gcs.BucketAttrs{Location: "nam4"}, "NAM4", "dual-region"},
		"custom": {gcs.BucketAttrs{
			Location: "US", CustomPlacementConfig: &gcs.CustomPlacementConfig{DataLocations: []string{"US-EAST1", "US-WEST1"}},
		}, "US", "dual-region"},
	} {
		if err := c.Bucket("placed-"+name).Create(t.Context(), project, &tc.attrs); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		attrs, err := c.Bucket("placed-" + name).Attrs(t.Context())
		if err != nil || attrs.Location != tc.location || attrs.LocationType != tc.lt {
			t.Errorf("%s = %+v, %v", name, attrs, err)
		}
	}
	attrs, _ := c.Bucket("placed-custom").Attrs(t.Context())
	if attrs.CustomPlacementConfig == nil || !slices.Equal(attrs.CustomPlacementConfig.DataLocations, []string{"US-EAST1", "US-WEST1"}) {
		t.Fatalf("custom placement = %+v", attrs.CustomPlacementConfig)
	}
}

func TestBucketErrors(t *testing.T) {
	s := storage.New()
	if err := s.PutForeignBucket("taken-name"); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s)
	if err := c.Bucket("mine").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		err   error
		code  int
		exact error
	}{
		{"a name the caller has", c.Bucket("mine").Create(t.Context(), "other-project", nil), http.StatusConflict, nil},
		{"a name someone else has", c.Bucket("taken-name").Create(t.Context(), project, nil), http.StatusConflict, nil},
		{"an invalid name", c.Bucket("Bad_Name").Create(t.Context(), project, nil), http.StatusBadRequest, nil},
		{"a name with google in it", c.Bucket("my-google-bucket").Create(t.Context(), project, nil), http.StatusBadRequest, nil},
		{"an invalid label", c.Bucket("labeled").Create(t.Context(), project, &gcs.BucketAttrs{Labels: map[string]string{"Upper": "x"}}), http.StatusBadRequest, nil},
		{"reading someone else's bucket", func() error { _, err := c.Bucket("taken-name").Attrs(t.Context()); return err }(), http.StatusForbidden, nil},
		{"deleting someone else's bucket", c.Bucket("taken-name").Delete(t.Context()), http.StatusForbidden, nil},
		{"reading a missing bucket", func() error { _, err := c.Bucket("nobody").Attrs(t.Context()); return err }(), 0, gcs.ErrBucketNotExist},
		{"listing someone else's objects", func() error { _, err := c.Bucket("taken-name").Objects(t.Context(), nil).Next(); return err }(), http.StatusForbidden, nil},
		{"an unimplemented method", func() error { _, err := c.Bucket("mine").IAM().Policy(t.Context()); return err }(), http.StatusNotFound, nil},
	} {
		if tc.exact != nil && !errors.Is(tc.err, tc.exact) || tc.exact == nil && apiCode(tc.err) != tc.code {
			t.Errorf("%s = %v, want %d %v", tc.name, tc.err, tc.code, tc.exact)
		}
	}
	if e, ok := errors.AsType[*googleapi.Error](c.Bucket("taken-name").Create(t.Context(), project, nil)); !ok ||
		len(e.Errors) != 1 || e.Errors[0].Reason != "conflict" || !strings.Contains(e.Message, "not available") {
		t.Fatalf("conflict = %+v", e)
	}
	if got := s.Buckets(); len(got) != 1 || got[0].Project != project {
		t.Fatalf("buckets = %+v", got)
	}
}

func TestListBuckets(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	for _, name := range []string{"app-c", "app-a", "logs", "app-b"} {
		if err := c.Bucket(name).Create(t.Context(), project, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Bucket("elsewhere").Create(t.Context(), "other-project", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutForeignBucket("app-foreign"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, c.Buckets(t.Context(), project)); !slices.Equal(got, []string{"app-a", "app-b", "app-c", "logs"}) {
		t.Fatalf("all = %v", got)
	}
	it := c.Buckets(t.Context(), project)
	it.Prefix = "app-"
	it.PageInfo().MaxSize = 1
	if got := names(t, it); !slices.Equal(got, []string{"app-a", "app-b", "app-c"}) {
		t.Fatalf("paged = %v", got)
	}
	attrs, err := c.Buckets(t.Context(), "other-project").Next()
	if err != nil || attrs.Name != "elsewhere" || attrs.Location != "US" {
		t.Fatalf("other project = %+v, %v", attrs, err)
	}
}

func TestEmptyThenDelete(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	b := c.Bucket("full")
	if err := b.Create(t.Context(), project, &gcs.BucketAttrs{Labels: map[string]string{"owner-id": "abc"}}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		write(t, c, "full", fmt.Sprintf("dir/file-%d.txt", i), strings.Repeat("x", i))
	}
	if err := b.Delete(t.Context()); apiCode(err) != http.StatusConflict {
		t.Fatalf("delete a full bucket = %v", err)
	}
	it := b.Objects(t.Context(), nil)
	it.PageInfo().MaxSize = 2
	deleted := 0
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Object(o.Name).Delete(t.Context()); err != nil {
			t.Fatalf("delete %s: %v", o.Name, err)
		}
		deleted++
	}
	if deleted != 5 {
		t.Fatalf("deleted %d objects while paging, want 5", deleted)
	}
	if err := b.Object("dir/file-0.txt").Delete(t.Context()); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("delete a deleted object = %v", err)
	}
	if err := b.Delete(t.Context()); err != nil {
		t.Fatalf("delete the empty bucket: %v", err)
	}
}

func TestVersionedBucketsKeepNoncurrentGenerations(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	b := c.Bucket("versioned")
	if err := b.Create(t.Context(), project, &gcs.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	first := write(t, c, "versioned", "k", "one")
	second := write(t, c, "versioned", "k", "two")
	if second.Generation <= first.Generation || first.Size != 3 || first.ContentType != "text/plain" || len(first.MD5) != 16 || first.CRC32C == 0 {
		t.Fatalf("generations = %+v then %+v", first, second)
	}
	if err := b.Object("k").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if live := objects(t, b.Objects(t.Context(), nil)); len(live) != 0 {
		t.Fatalf("live = %+v", live)
	}
	all := objects(t, b.Objects(t.Context(), &gcs.Query{Versions: true}))
	if len(all) != 2 || all[0].Generation != first.Generation || all[0].Deleted.IsZero() || all[1].Deleted.IsZero() {
		t.Fatalf("versions = %+v", all)
	}
	if err := b.Delete(t.Context()); apiCode(err) != http.StatusConflict {
		t.Fatalf("delete with noncurrent generations = %v", err)
	}
	for _, o := range all {
		if err := b.Object(o.Name).Generation(o.Generation).Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Delete(t.Context()); err != nil {
		t.Fatalf("delete the emptied bucket: %v", err)
	}
}

func TestUploadPreconditionsAndChecksums(t *testing.T) {
	s := storage.New()
	c := newClient(t, s)
	if err := c.Bucket("uploads").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "uploads", "a/b c.txt", "hello")
	w := c.Bucket("uploads").Object("a/b c.txt").If(gcs.Conditions{DoesNotExist: true}).NewWriter(t.Context())
	_, _ = w.Write([]byte("again"))
	if err := w.Close(); apiCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("a create-only write over an object = %v", err)
	}
	w = c.Bucket("uploads").Object("checked").NewWriter(t.Context())
	w.SendCRC32C, w.CRC32C = true, 12345
	_, _ = w.Write([]byte("data"))
	if err := w.Close(); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("a write with the wrong CRC32C = %v", err)
	}
	got := s.Objects("uploads")
	if len(got) != 1 || got[0].Name != "a/b c.txt" || got[0].Size != 5 {
		t.Fatalf("objects = %+v", got)
	}
	if err := c.Bucket("uploads").Object("a/b c.txt").Delete(t.Context()); err != nil {
		t.Fatalf("delete a name with a slash and a space: %v", err)
	}
	if _, err := s.PutObject("uploads", "seeded", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if listed := objects(t, c.Bucket("uploads").Objects(t.Context(), &gcs.Query{Prefix: "see"})); len(listed) != 1 || listed[0].Name != "seeded" {
		t.Fatalf("listed = %+v", listed)
	}
	if _, err := c.Bucket("uploads").Objects(t.Context(), &gcs.Query{Delimiter: "/"}).Next(); apiCode(err) != http.StatusBadRequest {
		t.Fatalf("a delimiter = %v", err)
	}
}

func TestEmulatorHost(t *testing.T) {
	s := storage.New()
	t.Setenv("STORAGE_EMULATOR_HOST", serve(t, s))
	c, err := gcs.NewClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Bucket("emulated").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "emulated", "k", "v")
	if got := s.Objects("emulated"); len(got) != 1 {
		t.Fatalf("objects = %+v", got)
	}
}

func TestFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "storage.buckets.insert", Resource: "b/lost", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "storage.buckets.list", Resource: "projects/locked/b", Fault: fault.Reject(http.StatusForbidden, "forbidden")},
		fault.Rule{Method: "storage.buckets.delete", Nth: 1, Fault: fault.FailAfter(http.StatusInternalServerError)},
		fault.Rule{Method: "storage.objects.insert", Resource: "b/lost/o/dir/denied", Fault: fault.Reject(http.StatusForbidden, "forbidden")},
	)
	s := storage.New(storage.WithFaults(faults))
	c := newClient(t, s)
	err := c.Bucket("lost").Create(t.Context(), project, nil)
	if apiCode(err) != http.StatusConflict {
		t.Fatalf("a create whose response was lost = %v, want the retry's conflict", err)
	}
	if _, ok := s.LookupBucket("lost"); !ok {
		t.Fatal("the lost create did not take effect")
	}
	if _, err := c.Buckets(t.Context(), "locked").Next(); apiCode(err) != http.StatusForbidden {
		t.Fatalf("a denied list = %v", err)
	}
	w := c.Bucket("lost").Object("dir/denied").NewWriter(t.Context())
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); apiCode(err) != http.StatusForbidden {
		t.Fatalf("a denied upload = %v", err)
	}
	if err := c.Bucket("lost").Delete(t.Context()); apiCode(err) != http.StatusNotFound {
		t.Fatalf("a delete failing after it took effect = %v, want the retry's 404", err)
	}
	if got := s.Buckets(); len(got) != 0 {
		t.Fatalf("buckets = %+v", got)
	}
	if got := faults.Injected(); len(got) != 4 || got[0].Call.Resource != "b/lost" {
		t.Fatalf("injected = %v", got)
	}
}

func TestSweepsConvergeThroughFaults(t *testing.T) {
	for seed := range uint64(20) {
		faults := fault.New(fault.Seed(seed)).Random(0.3, fault.LoseResponse(), fault.Unavailable(), fault.FailAfter(http.StatusInternalServerError))
		s := storage.New(storage.WithFaults(faults))
		c := newClient(t, s)
		for _, name := range []string{"sweep-a", "sweep-b"} {
			if _, err := s.PutBucket(storage.Bucket{Name: name, Project: project, Labels: map[string]string{"owner-id": "s1"}}); err != nil {
				t.Fatal(err)
			}
			for _, o := range []string{"x", "y"} {
				if _, err := s.PutObject(name, o, []byte(o)); err != nil {
					t.Fatal(err)
				}
			}
		}
		for range 100 {
			if sweep(t, c) == nil {
				break
			}
		}
		if got := s.Buckets(); len(got) != 0 {
			t.Fatalf("seed %d left %+v after %v", seed, got, faults.Injected())
		}
	}
}

// sweep empties and deletes every bucket labeled for the owner, stopping at the first error.
func sweep(t *testing.T, c *gcs.Client) error {
	t.Helper()
	it := c.Buckets(t.Context(), project)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		if attrs.Labels["owner-id"] != "s1" {
			continue
		}
		b := c.Bucket(attrs.Name)
		objs := b.Objects(t.Context(), nil)
		for {
			o, err := objs.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return err
			}
			if err := b.Object(o.Name).Delete(t.Context()); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
				return err
			}
		}
		if err := b.Delete(t.Context()); err != nil && apiCode(err) != http.StatusNotFound {
			return err
		}
	}
}
