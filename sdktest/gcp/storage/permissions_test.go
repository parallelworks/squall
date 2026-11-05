package storage_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	rawstorage "google.golang.org/api/storage/v1"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/storage"
)

// The client asks for projection=full on every get and list; Cloud Storage leaves out the ACLs rather than refusing.
func TestPolicyLetsAReaderWithoutIamPolicyGetAndList(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.objects.create", "storage.buckets.get", "storage.objects.list")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	if err := c.Bucket("data").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "data", "a.txt", "one")

	if _, err := c.Bucket("data").Attrs(t.Context()); err != nil {
		t.Errorf("get bucket: %v", err)
	}
	if _, err := c.Bucket("data").Objects(t.Context(), nil).Next(); err != nil {
		t.Errorf("list objects: %v", err)
	}
	if denials := policy.Denials(); len(denials) != 0 {
		t.Errorf("Denials() = %+v, want none", denials)
	}
}

func TestPolicyNeedsDeleteToOverwriteAnObject(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.objects.create", "storage.objects.list")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	if err := c.Bucket("data").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "data", "a.txt", "one")

	w := c.Bucket("data").Object("a.txt").NewWriter(t.Context())
	if _, err := w.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	err := w.Close()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("overwrite = %v, want 403", err)
	}
	if !strings.Contains(gerr.Message, "Permission 'storage.objects.delete' denied") {
		t.Errorf("message = %q, want it to name storage.objects.delete", gerr.Message)
	}
	if got := policy.Unused(); !slices.Equal(got, []string{"storage.objects.list"}) {
		t.Errorf("Unused() = %v, want [storage.objects.list]", got)
	}
}

func TestPolicyRefusesAMethodItDoesNotGrant(t *testing.T) {
	policy := access.Allow("storage.buckets.create")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	if err := c.Bucket("data").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Bucket("data").Delete(t.Context())
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("delete = %v, want 403", err)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "storage.buckets.delete" || denials[0].Resource != "b/data" {
		t.Errorf("Denials() = %+v", denials)
	}
}

func TestPolicyNeedsRetentionAndRestorePermissions(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.buckets.enableObjectRetention", "storage.objects.create", "storage.objects.update", "storage.objects.delete")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	b := c.Bucket("data")
	if err := b.SetObjectRetention(true).Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	first := write(t, c, "data", "a.txt", "one")
	write(t, c, "data", "a.txt", "two")

	_, err := b.Object("a.txt").Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: time.Now().Add(time.Hour)}})
	wantErr(t, "set retention", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.setRetention' denied")
	_, err = b.Object("a.txt").OverrideUnlockedRetention(true).Update(t.Context(), gcs.ObjectAttrsToUpdate{ContentType: "text/csv"})
	wantErr(t, "override retention", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.overrideUnlockedRetention' denied")
	_, err = b.Object("a.txt").Generation(first.Generation).Restore(t.Context(), &gcs.RestoreOptions{})
	wantErr(t, "restore", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.restore' denied")
	if _, err := b.Object("a.txt").Update(t.Context(), gcs.ObjectAttrsToUpdate{ContentType: "text/csv"}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}
}

func TestPolicyNeedsOverrideToLockUnlockedRetention(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.buckets.enableObjectRetention", "storage.objects.create",
		"storage.objects.update", "storage.objects.setRetention")
	c := newClient(t, storage.New(storage.WithPolicy(policy)))
	b := c.Bucket("data")
	if err := b.SetObjectRetention(true).Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if _, err := writeWith(t, c, "data", "a.txt", "one", gcs.ObjectAttrs{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until}}); err != nil {
		t.Fatal(err)
	}
	lock := gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Locked", RetainUntil: until}}
	_, err := b.Object("a.txt").OverrideUnlockedRetention(true).Update(t.Context(), lock)
	wantErr(t, "lock with override", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.overrideUnlockedRetention' denied")
	_, err = b.Object("a.txt").Update(t.Context(), lock)
	wantErr(t, "lock without override", err, http.StatusForbidden, "forbidden", "without overriding unlocked retention intent")
}

func TestPolicyNeedsSetRetentionToRemoveRetention(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.buckets.enableObjectRetention", "storage.objects.create",
		"storage.objects.update", "storage.objects.overrideUnlockedRetention")
	s := storage.New(storage.WithPolicy(policy))
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("data").SetObjectRetention(true).Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	write(t, c, "data", "a.txt", "one")
	if _, err := s.SetObjectRetention("data", "a.txt", &storage.Retention{Mode: "Unlocked", RetainUntilTime: time.Now().Add(time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Bucket("data").Object("a.txt").OverrideUnlockedRetention(true).Update(t.Context(), gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{}})
	wantErr(t, "remove retention with null", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.setRetention' denied")
	_, err = svc.Objects.Update("data", "a.txt", &rawstorage.Object{}).OverrideUnlockedRetention(true).Context(t.Context()).Do()
	wantErr(t, "remove retention by leaving it out", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.setRetention' denied")
}

func TestPolicyChecksTheGenerationAnUpdateNames(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.buckets.enableObjectRetention", "storage.objects.create",
		"storage.objects.update", "storage.objects.overrideUnlockedRetention")
	s := storage.New(storage.WithPolicy(policy))
	url := serve(t, s)
	c, svc := clientAt(t, url), rawService(t, url)
	if err := c.Bucket("versions").SetObjectRetention(true).Create(t.Context(), project, &gcs.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	put := func(name, data string) storage.Object {
		t.Helper()
		o, err := s.PutObject("versions", name, []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	first := put("a.txt", "one")
	if _, err := s.SetObjectRetention("versions", "a.txt", &storage.Retention{Mode: "Unlocked", RetainUntilTime: time.Now().Add(time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	put("a.txt", "two")
	_, err := svc.Objects.Update("versions", "a.txt", &rawstorage.Object{}).Generation(first.Generation).OverrideUnlockedRetention(true).Context(t.Context()).Do()
	wantErr(t, "remove a noncurrent generation's retention", err, http.StatusForbidden, "forbidden", "Permission 'storage.objects.setRetention' denied")
	if _, err := svc.Objects.Update("versions", "a.txt", &rawstorage.Object{}).Context(t.Context()).Do(); err != nil {
		t.Fatalf("update the live generation, which has no retention: %v", err)
	}

	older := put("b.txt", "one")
	put("b.txt", "two")
	if _, err := s.SetObjectRetention("versions", "b.txt", &storage.Retention{Mode: "Unlocked", RetainUntilTime: time.Now().Add(time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Objects.Update("versions", "b.txt", &rawstorage.Object{}).Generation(older.Generation).Context(t.Context()).Do(); err != nil {
		t.Fatalf("update a noncurrent generation without retention while the live one has it: %v", err)
	}
}
