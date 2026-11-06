package storage_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/gcp/storage"
)

const uniformBucket = `{"name":"%s","iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`

func uniform(t *testing.T, s *storage.Server, name string) {
	t.Helper()
	if code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", strings.Replace(uniformBucket, "%s", name, 1)); code != http.StatusOK {
		t.Fatalf("insert bucket = %d %s", code, body)
	}
}

func TestManagedFolderShape(t *testing.T) {
	s := storage.New(storage.WithClock(func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }))
	uniform(t, s, "mfb")
	code, body := do(t, s, http.MethodPost, "/storage/v1/b/mfb/managedFolders", "application/json", `{"name":"logs/app"}`)
	want := `{"kind":"storage#managedFolder","id":"mfb/logs/app/","selfLink":"https://www.googleapis.com/storage/v1/b/mfb/managedFolders/logs%2Fapp%2F",` +
		`"name":"logs/app/","bucket":"mfb","metageneration":"1","createTime":"2026-01-02T03:04:05.000Z","updateTime":"2026-01-02T03:04:05.000Z"}`
	if code != http.StatusOK || body != want {
		t.Fatalf("insert = %d %s, want %s", code, body, want)
	}
	for _, path := range []string{"logs%2Fapp%2F", "logs%2Fapp"} {
		if code, got := do(t, s, http.MethodGet, "/storage/v1/b/mfb/managedFolders/"+path, "", ""); code != http.StatusOK || got != want {
			t.Errorf("get %s = %d %s", path, code, got)
		}
	}
	code, body = do(t, s, http.MethodGet, "/storage/v1/b/mfb", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"iamConfiguration":{"bucketPolicyOnly":{"enabled":true,"lockedTime":"2026-04-02T03:04:05.000Z"},"uniformBucketLevelAccess":{"enabled":true,"lockedTime":"2026-04-02T03:04:05.000Z"},"publicAccessPrevention":"inherited"}`) {
		t.Errorf("get bucket = %d %s", code, body)
	}
}

func TestManagedFolderErrors(t *testing.T) {
	s := storage.New()
	uniform(t, s, "mfb")
	if code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"fine-grained"}`); code != http.StatusOK {
		t.Fatalf("insert bucket = %d %s", code, body)
	}
	if _, err := s.PutManagedFolder("mfb", "a/"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
		reason                     string
	}{
		{"an existing folder", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":"a/"}`, http.StatusConflict, "conflict"},
		{"no name", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{}`, http.StatusBadRequest, "required"},
		{"a dot", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":"./"}`, http.StatusBadRequest, "invalid"},
		{"a line feed", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":"a\nb/"}`, http.StatusBadRequest, "invalid"},
		{"16 levels", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":"` + strings.Repeat("d/", 16) + `"}`, http.StatusBadRequest, "invalid"},
		{"an ACME challenge", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":".well-known/acme-challenge/x/"}`, http.StatusBadRequest, "invalid"},
		{"rapid cache", http.MethodPost, "/storage/v1/b/mfb/managedFolders", `{"name":"c/","rapidCacheConfig":{}}`, http.StatusBadRequest, "invalid"},
		{"a bucket without uniform access", http.MethodPost, "/storage/v1/b/fine-grained/managedFolders", `{"name":"c/"}`, http.StatusConflict, "conflict"},
		{"a list without uniform access", http.MethodGet, "/storage/v1/b/fine-grained/managedFolders", "", http.StatusConflict, "conflict"},
		{"a missing bucket", http.MethodGet, "/storage/v1/b/nowhere/managedFolders", "", http.StatusNotFound, "notFound"},
		{"a missing folder", http.MethodGet, "/storage/v1/b/mfb/managedFolders/b%2F", "", http.StatusNotFound, "notFound"},
		{"a delete of a missing folder", http.MethodDelete, "/storage/v1/b/mfb/managedFolders/b%2F", "", http.StatusNotFound, "notFound"},
		{"a metageneration mismatch", http.MethodGet, "/storage/v1/b/mfb/managedFolders/a%2F?ifMetagenerationMatch=2", "", http.StatusPreconditionFailed, "conditionNotMet"},
		{"a delete on a metageneration match", http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F?ifMetagenerationNotMatch=1", "", http.StatusPreconditionFailed, "conditionNotMet"},
		{"page sizes that disagree", http.MethodGet, "/storage/v1/b/mfb/managedFolders?pageSize=1&maxResults=2", "", http.StatusBadRequest, "invalid"},
		{"a page size of 0 beside another", http.MethodGet, "/storage/v1/b/mfb/managedFolders?pageSize=0&maxResults=2", "", http.StatusOK, ""},
		{"a negative page size", http.MethodGet, "/storage/v1/b/mfb/managedFolders?pageSize=-1", "", http.StatusBadRequest, "invalid"},
		{"a bad allowNonEmpty", http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F?allowNonEmpty=yes", "", http.StatusBadRequest, "invalid"},
		{"an IAM policy", http.MethodGet, "/storage/v1/b/mfb/managedFolders/a%2F/iam", "", http.StatusNotFound, "notFound"},
		{"a patch", http.MethodPatch, "/storage/v1/b/mfb/managedFolders/a%2F", `{}`, http.StatusNotFound, "notFound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, s, tc.method, tc.target, "application/json", tc.body)
			if code != tc.status || tc.reason != "" && !strings.Contains(body, `"reason":"`+tc.reason+`"`) {
				t.Errorf("%s %s = %d %s, want %d %s", tc.method, tc.target, code, body, tc.status, tc.reason)
			}
		})
	}
}

func TestManagedFolderList(t *testing.T) {
	s := storage.New()
	uniform(t, s, "mfb")
	for _, name := range []string{"b/", "a/", "a/x/", "c/"} {
		if _, err := s.PutManagedFolder("mfb", name); err != nil {
			t.Fatal(err)
		}
	}
	code, body := do(t, s, http.MethodGet, "/storage/v1/b/mfb/managedFolders?prefix=a", "", "")
	if code != http.StatusOK || strings.Count(body, `"kind":"storage#managedFolder"`) != 2 || !strings.Contains(body, `"kind":"storage#managedFolders"`) {
		t.Fatalf("list a = %d %s", code, body)
	}
	var got []string
	token := ""
	for {
		code, body := do(t, s, http.MethodGet, "/storage/v1/b/mfb/managedFolders?maxResults=3&pageToken="+token, "", "")
		if code != http.StatusOK {
			t.Fatalf("list = %d %s", code, body)
		}
		var page struct {
			Items         []storage.ManagedFolder
			NextPageToken string
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		for _, f := range page.Items {
			got = append(got, f.Name)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if strings.Join(got, ",") != "a/,a/x/,b/,c/" {
		t.Errorf("listed %v", got)
	}
}

func TestManagedFolderDeleteNeedsItEmpty(t *testing.T) {
	s := storage.New()
	uniform(t, s, "mfb")
	for _, name := range []string{"a/", "a/b/", "c/"} {
		if _, err := s.PutManagedFolder("mfb", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutObject("mfb", "c/file", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"a%2F", "c%2F"} {
		code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/"+folder, "", "")
		if code != http.StatusConflict || !strings.Contains(body, "is not empty") {
			t.Errorf("delete %s = %d %s", folder, code, body)
		}
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2Fb%2F", "", ""); code != http.StatusNoContent {
		t.Errorf("delete a/b/ = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F", "", ""); code != http.StatusNoContent {
		t.Errorf("delete emptied a/ = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/c%2F?allowNonEmpty=true", "", ""); code != http.StatusNoContent {
		t.Errorf("delete c/ allowing non-empty = %d %s", code, body)
	}
	if objs := s.Objects("mfb"); len(objs) != 1 || objs[0].Name != "c/file" {
		t.Errorf("objects after the folder went = %+v, want c/file kept", objs)
	}
	if got := s.ManagedFolders("mfb"); len(got) != 0 {
		t.Errorf("ManagedFolders() = %+v, want none", got)
	}
}

func TestBucketWithManagedFoldersIsNotEmpty(t *testing.T) {
	s := storage.New()
	uniform(t, s, "mfb")
	if _, err := s.PutManagedFolder("mfb", "a"); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb", "", "")
	if code != http.StatusConflict || !strings.Contains(body, `"message":"The bucket you tried to delete is not empty."`) || !strings.Contains(body, `"reason":"conflict"`) {
		t.Fatalf("delete bucket = %d %s", code, body)
	}
	code, body = do(t, s, http.MethodPatch, "/storage/v1/b/mfb", "application/json", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":false}}}`)
	if code != http.StatusConflict {
		t.Errorf("turning uniform access off = %d %s, want 409", code, body)
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F", "", ""); code != http.StatusNoContent {
		t.Fatalf("delete folder = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb", "", ""); code != http.StatusNoContent {
		t.Errorf("delete emptied bucket = %d %s", code, body)
	}
}

func TestUniformAccessLocksAfter90Days(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := storage.New(storage.WithClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return now }))
	uniform(t, s, "locks")
	off := `{"iamConfiguration":{"bucketPolicyOnly":{"enabled":false},"uniformBucketLevelAccess":{"enabled":false}}}`
	if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/locks", "application/json", off); code != http.StatusOK || !strings.Contains(body, `"uniformBucketLevelAccess":{"enabled":false}`) {
		t.Fatalf("turning it off within 90 days = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/locks", "application/json", `{"iamConfiguration":{"bucketPolicyOnly":{"enabled":true}}}`); code != http.StatusOK {
		t.Fatalf("turning it on = %d %s", code, body)
	}
	mu.Lock()
	now = now.Add(90 * 24 * time.Hour)
	mu.Unlock()
	if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/locks", "application/json", off); code != http.StatusBadRequest {
		t.Errorf("turning it off after 90 days = %d %s, want 400", code, body)
	}
	for _, in := range []string{
		`{"iamConfiguration":{"publicAccessPrevention":"open"}}`,
		`{"iamConfiguration":null}`,
	} {
		if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/locks", "application/json", in); code != http.StatusBadRequest {
			t.Errorf("patch %s = %d %s, want 400", in, code, body)
		}
	}
}

func TestPolicyNeedsSetIamPolicyToDeleteANonEmptyManagedFolder(t *testing.T) {
	policy := access.Allow("storage.managedFolders.delete")
	s := storage.New(storage.WithPolicy(policy))
	if _, err := s.PutBucket(storage.Bucket{Name: "mfb", Project: "p", IamConfiguration: &storage.IamConfiguration{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutManagedFolder("mfb", "a/"); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F?allowNonEmpty=true", "", "")
	if code != http.StatusForbidden || !strings.Contains(body, "Permission 'storage.managedFolders.setIamPolicy' denied on resource") {
		t.Fatalf("delete allowing non-empty = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodDelete, "/storage/v1/b/mfb/managedFolders/a%2F", "", ""); code != http.StatusNoContent {
		t.Errorf("delete = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodGet, "/storage/v1/b/mfb/managedFolders", "", ""); code != http.StatusForbidden || !strings.Contains(body, "storage.managedFolders.list") {
		t.Errorf("list = %d %s", code, body)
	}
}

func TestBucketsReportIamConfiguration(t *testing.T) {
	s := storage.New()
	code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"plain"}`)
	if code != http.StatusOK || !strings.Contains(body, `"iamConfiguration":{"bucketPolicyOnly":{"enabled":false},"uniformBucketLevelAccess":{"enabled":false},"publicAccessPrevention":"inherited"}`) {
		t.Fatalf("insert = %d %s", code, body)
	}
	code, body = do(t, s, http.MethodPatch, "/storage/v1/b/plain", "application/json", `{"iamConfiguration":{"publicAccessPrevention":"enforced"}}`)
	if code != http.StatusOK || !strings.Contains(body, `"uniformBucketLevelAccess":{"enabled":false},"publicAccessPrevention":"enforced"`) {
		t.Fatalf("patch = %d %s", code, body)
	}
	code, body = do(t, s, http.MethodPatch, "/storage/v1/b/plain", "application/json", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`)
	if code != http.StatusOK || !strings.Contains(body, `"publicAccessPrevention":"enforced"`) {
		t.Errorf("patch keeping public access prevention = %d %s", code, body)
	}
}

func TestUniformAccessRefusesACLs(t *testing.T) {
	s := storage.New()
	uniform(t, s, "uniform")
	if code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"fine-grained"}`); code != http.StatusOK {
		t.Fatalf("insert bucket = %d %s", code, body)
	}
	if _, err := s.PutObject("uniform", "k", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSoftDeletedObject("uniform", "gone", []byte("x")); err != nil {
		t.Fatal(err)
	}
	gone := s.SoftDeletedObjects("uniform")[0].Generation
	const legacy = "Cannot insert legacy ACL for an object when uniform bucket-level access is enabled."
	for _, tc := range []struct{ name, method, target, body, reason string }{
		{"a bucket insert", http.MethodPost, "/storage/v1/b?project=p&predefinedAcl=private", strings.Replace(uniformBucket, "%s", "acl-bucket", 1), "badRequest"},
		{"a bucket patch", http.MethodPatch, "/storage/v1/b/uniform?predefinedDefaultObjectAcl=projectPrivate", `{}`, "badRequest"},
		{"a patch turning it on", http.MethodPatch, "/storage/v1/b/fine-grained?predefinedAcl=private", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`, "badRequest"},
		{"an upload", http.MethodPost, "/upload/storage/v1/b/uniform/o?uploadType=media&name=n&predefinedAcl=private", "x", "invalid"},
		{"an object patch", http.MethodPatch, "/storage/v1/b/uniform/o/k?predefinedAcl=projectPrivate", `{}`, "invalid"},
		{"an object update", http.MethodPut, "/storage/v1/b/uniform/o/k?predefinedAcl=private", `{}`, "invalid"},
		{"a restore copying the ACL", http.MethodPost, fmt.Sprintf("/storage/v1/b/uniform/o/gone/restore?generation=%d&copySourceAcl=true", gone), "", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, s, tc.method, tc.target, "application/json", tc.body)
			if code != http.StatusBadRequest || !strings.Contains(body, `"reason":"`+tc.reason+`"`) || tc.reason == "invalid" && !strings.Contains(body, legacy) {
				t.Errorf("%s %s = %d %s, want 400 %s", tc.method, tc.target, code, body, tc.reason)
			}
		})
	}
	if code, body := do(t, s, http.MethodPost, fmt.Sprintf("/storage/v1/b/uniform/o/gone/restore?generation=%d", gone), "", ""); code != http.StatusOK {
		t.Errorf("restore without copySourceAcl = %d %s", code, body)
	}
	if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/fine-grained?predefinedAcl=private", "application/json", `{}`); code != http.StatusOK {
		t.Errorf("an ACL without uniform access = %d %s", code, body)
	}
}

func TestUniformAccessNameWinsOverItsFormerName(t *testing.T) {
	s := storage.New()
	uniform(t, s, "names")
	code, body := do(t, s, http.MethodPatch, "/storage/v1/b/names", "application/json", `{"iamConfiguration":{"bucketPolicyOnly":{"enabled":true},"uniformBucketLevelAccess":{"enabled":false}}}`)
	if code != http.StatusOK || !strings.Contains(body, `"bucketPolicyOnly":{"enabled":false}`) {
		t.Errorf("patch = %d %s", code, body)
	}
}

func TestPutBucketChecksPublicAccessPrevention(t *testing.T) {
	s := storage.New()
	if _, err := s.PutBucket(storage.Bucket{Name: "pap", Project: "p", IamConfiguration: &storage.IamConfiguration{PublicAccessPrevention: "bogus"}}); err == nil {
		t.Error("PutBucket with publicAccessPrevention bogus succeeded")
	}
	b, err := s.PutBucket(storage.Bucket{Name: "pap", Project: "p", IamConfiguration: &storage.IamConfiguration{PublicAccessPrevention: "enforced"}})
	if err != nil || b.IamConfiguration.PublicAccessPrevention != "enforced" {
		t.Errorf("PutBucket = %+v, %v", b.IamConfiguration, err)
	}
}

func TestPolicyNeedsSetIamPolicyToCopyARestoredACL(t *testing.T) {
	policy := access.Allow("storage.objects.restore", "storage.objects.create")
	s := storage.New(storage.WithPolicy(policy))
	if _, err := s.PutBucket(storage.Bucket{Name: "acls", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSoftDeletedObject("acls", "gone", []byte("x")); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("/storage/v1/b/acls/o/gone/restore?generation=%d&copySourceAcl=true", s.SoftDeletedObjects("acls")[0].Generation)
	if code, body := do(t, s, http.MethodPost, target, "", ""); code != http.StatusForbidden || !strings.Contains(body, "storage.objects.setIamPolicy") {
		t.Errorf("restore = %d %s, want 403 naming storage.objects.setIamPolicy", code, body)
	}
}
