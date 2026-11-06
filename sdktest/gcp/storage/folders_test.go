package storage_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	rawstorage "google.golang.org/api/storage/v1"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/storage"
)

func uniformBucket(t *testing.T, c *gcs.Client, name string) {
	t.Helper()
	attrs := &gcs.BucketAttrs{UniformBucketLevelAccess: gcs.UniformBucketLevelAccess{Enabled: true}}
	if err := c.Bucket(name).Create(t.Context(), project, attrs); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func wantCode(t *testing.T, what string, err error, code int) {
	t.Helper()
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || gerr.Code != code {
		t.Errorf("%s = %v, want %d", what, err, code)
	}
}

func TestManagedFolders(t *testing.T) {
	url := serve(t, storage.New())
	c := clientAt(t, url)
	svc := rawService(t, url)
	uniformBucket(t, c, "folders")

	attrs, err := c.Bucket("folders").Attrs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !attrs.UniformBucketLevelAccess.Enabled || !attrs.BucketPolicyOnly.Enabled || attrs.UniformBucketLevelAccess.LockedTime.Sub(attrs.Created) != 90*24*time.Hour {
		t.Errorf("uniform access = %+v, created %v", attrs.UniformBucketLevelAccess, attrs.Created)
	}

	created, err := svc.ManagedFolders.Insert("folders", &rawstorage.ManagedFolder{Name: "team/reports/"}).Context(t.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != "storage#managedFolder" || created.Bucket != "folders" || created.Metageneration != 1 || created.CreateTime == "" || created.UpdateTime != created.CreateTime {
		t.Errorf("created = %+v", created)
	}
	_, err = svc.ManagedFolders.Insert("folders", &rawstorage.ManagedFolder{Name: "team/reports/"}).Context(t.Context()).Do()
	wantCode(t, "insert again", err, http.StatusConflict)
	if _, err := svc.ManagedFolders.Insert("folders", &rawstorage.ManagedFolder{Name: "team/"}).Context(t.Context()).Do(); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ManagedFolders.Get("folders", "team/reports/").IfMetagenerationMatch(1).Context(t.Context()).Do()
	if err != nil || got.Name != "team/reports/" || got.Id != created.Id {
		t.Errorf("get = %+v, %v", got, err)
	}
	_, err = svc.ManagedFolders.Get("folders", "team/other/").Context(t.Context()).Do()
	wantCode(t, "get a missing folder", err, http.StatusNotFound)

	var listed []string
	err = svc.ManagedFolders.List("folders").Prefix("team/").PageSize(1).Pages(t.Context(), func(page *rawstorage.ManagedFolders) error {
		for _, f := range page.Items {
			listed = append(listed, f.Name)
		}
		return nil
	})
	if err != nil || !slices.Equal(listed, []string{"team/", "team/reports/"}) {
		t.Errorf("list = %v, %v", listed, err)
	}

	write(t, c, "folders", "team/reports/q1.csv", "1,2")
	wantCode(t, "delete a folder holding an object", svc.ManagedFolders.Delete("folders", "team/reports/").Context(t.Context()).Do(), http.StatusConflict)
	wantCode(t, "delete a folder holding a folder", svc.ManagedFolders.Delete("folders", "team/").Context(t.Context()).Do(), http.StatusConflict)
	wantCode(t, "delete on a stale metageneration", svc.ManagedFolders.Delete("folders", "team/").IfMetagenerationMatch(2).AllowNonEmpty(true).Context(t.Context()).Do(), http.StatusPreconditionFailed)
	wantCode(t, "delete the bucket", c.Bucket("folders").Delete(t.Context()), http.StatusConflict)
	_, err = c.Bucket("folders").Update(t.Context(), gcs.BucketAttrsToUpdate{UniformBucketLevelAccess: &gcs.UniformBucketLevelAccess{Enabled: false}})
	wantCode(t, "turn off uniform access", err, http.StatusConflict)

	if err := svc.ManagedFolders.Delete("folders", "team/").AllowNonEmpty(true).Context(t.Context()).Do(); err != nil {
		t.Fatalf("delete allowing non-empty: %v", err)
	}
	if err := svc.ManagedFolders.Delete("folders", "team/reports/").AllowNonEmpty(true).IfMetagenerationNotMatch(2).Context(t.Context()).Do(); err != nil {
		t.Fatalf("delete allowing non-empty: %v", err)
	}
	if _, err := c.Bucket("folders").Object("team/reports/q1.csv").Attrs(t.Context()); err != nil {
		t.Errorf("the object after its folder went: %v", err)
	}
	if err := c.Bucket("folders").Object("team/reports/q1.csv").Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Bucket("folders").Delete(t.Context()); err != nil {
		t.Errorf("delete the emptied bucket: %v", err)
	}
}

func TestManagedFoldersNeedUniformAccess(t *testing.T) {
	url := serve(t, storage.New())
	c := clientAt(t, url)
	svc := rawService(t, url)
	if err := c.Bucket("acls").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ManagedFolders.Insert("acls", &rawstorage.ManagedFolder{Name: "a/"}).Context(t.Context()).Do()
	wantCode(t, "insert", err, http.StatusConflict)

	if _, err := c.Bucket("acls").Update(t.Context(), gcs.BucketAttrsToUpdate{UniformBucketLevelAccess: &gcs.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ManagedFolders.Insert("acls", &rawstorage.ManagedFolder{Name: "a/"}).Context(t.Context()).Do(); err != nil {
		t.Errorf("insert once uniform access is on: %v", err)
	}
}

func TestPolicyChecksManagedFolderPermissions(t *testing.T) {
	policy := access.Allow("storage.buckets.create", "storage.managedFolders.create", "storage.managedFolders.get", "storage.managedFolders.delete")
	url := serve(t, storage.New(storage.WithPolicy(policy)))
	c := clientAt(t, url)
	svc := rawService(t, url)
	uniformBucket(t, c, "guarded")
	if _, err := svc.ManagedFolders.Insert("guarded", &rawstorage.ManagedFolder{Name: "a/"}).Context(t.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ManagedFolders.Get("guarded", "a/").Context(t.Context()).Do(); err != nil {
		t.Error(err)
	}
	wantCode(t, "list", svc.ManagedFolders.List("guarded").Pages(t.Context(), func(*rawstorage.ManagedFolders) error { return nil }), http.StatusForbidden)
	wantCode(t, "delete allowing non-empty", svc.ManagedFolders.Delete("guarded", "a/").AllowNonEmpty(true).Context(t.Context()).Do(), http.StatusForbidden)
	if err := svc.ManagedFolders.Delete("guarded", "a/").Context(t.Context()).Do(); err != nil {
		t.Error(err)
	}
	if got := policy.Unused(); len(got) != 0 {
		t.Errorf("Unused() = %v, want none", got)
	}
}

func TestPublicAccessPreventionIsRecorded(t *testing.T) {
	c := newClient(t, storage.New())
	if err := c.Bucket("plain").Create(t.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	attrs, err := c.Bucket("plain").Attrs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if attrs.PublicAccessPrevention != gcs.PublicAccessPreventionInherited || attrs.UniformBucketLevelAccess.Enabled {
		t.Errorf("default = %v, %+v", attrs.PublicAccessPrevention, attrs.UniformBucketLevelAccess)
	}

	secure := &gcs.BucketAttrs{UniformBucketLevelAccess: gcs.UniformBucketLevelAccess{Enabled: true}, PublicAccessPrevention: gcs.PublicAccessPreventionEnforced}
	if err := c.Bucket("secure").Create(t.Context(), project, secure); err != nil {
		t.Fatal(err)
	}
	attrs, err = c.Bucket("secure").Attrs(t.Context())
	if err != nil || attrs.PublicAccessPrevention != gcs.PublicAccessPreventionEnforced || !attrs.UniformBucketLevelAccess.Enabled {
		t.Fatalf("secure = %+v, %v", attrs, err)
	}
	attrs, err = c.Bucket("secure").Update(t.Context(), gcs.BucketAttrsToUpdate{PublicAccessPrevention: gcs.PublicAccessPreventionInherited})
	if err != nil || attrs.PublicAccessPrevention != gcs.PublicAccessPreventionInherited || !attrs.UniformBucketLevelAccess.Enabled {
		t.Errorf("update = %+v, %v", attrs, err)
	}
}

func TestUniformAccessRefusesPredefinedACLs(t *testing.T) {
	c := newClient(t, storage.New())
	uniformBucket(t, c, "uniform")
	w := c.Bucket("uniform").Object("a.txt").NewWriter(t.Context())
	w.PredefinedACL = "private"
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "upload with a predefined ACL", w.Close(), http.StatusBadRequest)
	write(t, c, "uniform", "a.txt", "x")
	_, err := c.Bucket("uniform").Update(t.Context(), gcs.BucketAttrsToUpdate{PredefinedDefaultObjectACL: "projectPrivate"})
	wantCode(t, "bucket update with a predefined default object ACL", err, http.StatusBadRequest)
}
