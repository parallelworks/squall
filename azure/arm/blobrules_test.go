package arm_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const containerPath = accountPath + "/blobServices/default/containers/data"

func hold(t *testing.T, s *arm.Server) {
	t.Helper()
	if rec, body := do(t, s, http.MethodPost, containerPath+"/setLegalHold", `{"tags":["case1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("set legal hold = %d %v", rec.Code, body)
	}
}

func policy(t *testing.T, s *arm.Server) string {
	t.Helper()
	rec, body := do(t, s, http.MethodPut, containerPath+"/immutabilityPolicies/default", `{"properties":{"immutabilityPeriodSinceCreationInDays":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy = %d %v", rec.Code, body)
	}
	return body["etag"].(string)
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, body map[string]any, status int, code string) {
	t.Helper()
	if rec.Code != status || code != "" && errorCode(body) != code {
		t.Fatalf("answer = %d %v, want %d %s", rec.Code, body, status, code)
	}
}

func TestDeleteOnlySnapshotsOfABlobWithNone(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	rec := blobDo(t, s, http.MethodDelete, "/acct/data/a", "", "x-ms-delete-snapshots", "only")
	if rec.Code != http.StatusAccepted || rec.Header().Get("x-ms-delete-type-permanent") != "true" {
		t.Fatalf("delete only snapshots = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if got := s.Blobs(); len(got) != 1 || !got[0].Current {
		t.Fatalf("blobs = %+v", got)
	}
}

func TestVersionsOutliveVersioning(t *testing.T) {
	s := arm.New()
	newStorage(t, s, `{"isVersioningEnabled":true}`)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "2")
	blobDo(t, s, http.MethodPut, "/acct/data/b", "1")
	do(t, s, http.MethodPut, accountPath+"/blobServices/default", `{"properties":{"isVersioningEnabled":false}}`)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "3")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "4")
	blobDo(t, s, http.MethodDelete, "/acct/data/b", "")
	var aVersions, bVersions, current int
	for _, b := range s.Blobs() {
		switch {
		case b.Current:
			current++
		case b.Name == "a" && b.VersionID != "":
			aVersions++
		case b.Name == "b" && b.VersionID != "":
			bVersions++
		}
	}
	if aVersions != 2 || bVersions != 1 || current != 1 {
		t.Fatalf("after versioning is off: a's versions %d, b's %d, current %d: %+v", aVersions, bVersions, current, s.Blobs())
	}
}

func TestExpiredPolicyStillBlocksOverwrites(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := arm.New(arm.WithClock(func() time.Time { return now }))
	newStorage(t, s, "")
	policy(t, s)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	now = now.Add(48 * time.Hour)
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a", "2"), http.StatusConflict, "BlobImmutableDueToPolicy")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", ""), http.StatusConflict, "BlobImmutableDueToPolicy")
	if rec := blobDo(t, s, http.MethodDelete, "/acct/data/a", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("delete once the policy's days are over = %d %s", rec.Code, rec.Body)
	}
}

func TestSnapshotsRefusedUnderALegalHold(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	hold(t, s)
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", ""), http.StatusConflict, "BlobImmutableDueToLegalHold")
}

func TestVersionLevelImmutability(t *testing.T) {
	s := arm.New()
	newGroup(t, s)
	do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
	blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/b", "1", "x-ms-immutability-policy-mode", "unlocked"), http.StatusBadRequest, "BadRequest")
	rec, body := do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
	rec, body = do(t, s, http.MethodDelete, containerPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "BlobImmutableDueToPolicy")
	blobDo(t, s, http.MethodDelete, "/acct/data/a", "")
	rec, body = do(t, s, http.MethodDelete, containerPath, "")
	wantStatus(t, rec, body, http.StatusOK, "")
	rec, body = do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusOK, "")
}

func TestContainerVersionLevelImmutabilityProtectsTheAccount(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	do(t, s, http.MethodDelete, containerPath, "")
	do(t, s, http.MethodPut, containerPath, `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	rec, body := do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
}

func TestDeletesAProtectionAllows(t *testing.T) {
	t.Run("account with an unlocked policy and blobs", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		policy(t, s)
		blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
	t.Run("account with a legal hold on an empty container", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		hold(t, s)
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
	t.Run("account with a legal hold and only uncommitted blocks", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		hold(t, s)
		blobDo(t, s, http.MethodPut, "/acct/data/a?comp=block&blockid=QUFBQQ==", "zz")
		if got := s.Blobs(); len(got) != 0 {
			t.Fatalf("blobs = %+v", got)
		}
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
	t.Run("empty held container through ARM", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		hold(t, s)
		rec, body := do(t, s, http.MethodDelete, containerPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
	t.Run("empty container with a locked policy through ARM", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		etag := policy(t, s)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, containerPath+"/immutabilityPolicies/default/lock?api-version=1", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("If-Match", etag)
		lock := httptest.NewRecorder()
		s.ServeHTTP(lock, req)
		if lock.Code != http.StatusOK {
			t.Fatalf("lock = %d %s", lock.Code, lock.Body)
		}
		rec, body := do(t, s, http.MethodDelete, containerPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
}

func TestPutBlockOnASoftDeletedBlob(t *testing.T) {
	s := arm.New()
	newStorage(t, s, `{"deleteRetentionPolicy":{"enabled":true,"days":7}}`)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	blobDo(t, s, http.MethodDelete, "/acct/data/a", "")
	blobDo(t, s, http.MethodPut, "/acct/data/a?comp=block&blockid=QUFBQQ==", "zz")
	if got := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=uncommittedblobs", "").Body.String(); !strings.Contains(got, "<Name>a</Name>") {
		t.Fatalf("uncommitted = %s", got)
	}
	got := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=deleted,snapshots", "").Body.String()
	if !strings.Contains(got, "<Snapshot>") || !strings.Contains(got, "<Deleted>true</Deleted>") {
		t.Fatalf("the soft-deleted blob is not kept as a soft-deleted snapshot: %s", got)
	}
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a?comp=block&blockid=QkI=", "zz"), http.StatusBadRequest, "InvalidBlobOrBlock")
}

func TestSnapshotOfAVersionedBlobMakesAVersion(t *testing.T) {
	s := arm.New()
	newStorage(t, s, `{"isVersioningEnabled":true}`)
	first := blobDo(t, s, http.MethodPut, "/acct/data/a", "1").Header().Get("x-ms-version-id")
	rec := blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", "")
	if v := rec.Header().Get("x-ms-version-id"); v == "" || v == first || rec.Header().Get("x-ms-request-server-encrypted") != "true" {
		t.Fatalf("snapshot = %v", rec.Header())
	}
	if got := s.Blobs(); len(got) != 3 {
		t.Fatalf("blobs = %+v", got)
	}
}

func TestBlobDataActionsPerOperation(t *testing.T) {
	p := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/write",
		"Microsoft.Storage/storageAccounts/blobServices/containers/write", blobAction+"add/action", blobAction+"deleteBlobVersion/action")
	s := arm.New(arm.WithPolicy(p))
	newStorage(t, s, `{"isVersioningEnabled":true}`)
	old := blobDo(t, s, http.MethodPut, "/acct/data/a", "1").Header().Get("x-ms-version-id")
	if rec := blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", ""); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot with add/action = %d %s", rec.Code, rec.Body)
	}
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/b?comp=block&blockid=QUFBQQ==", "x"), http.StatusForbidden, "AuthorizationPermissionMismatch")
	if rec := blobDo(t, s, http.MethodDelete, "/acct/data/a?versionid="+old, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("deleting a version with deleteBlobVersion/action only = %d %s", rec.Code, rec.Body)
	}
	wantError(t, blobDo(t, s, http.MethodDelete, "/acct/data/a", ""), http.StatusForbidden, "AuthorizationPermissionMismatch")
}

const blobAction = "Microsoft.Storage/storageAccounts/blobServices/containers/blobs/"

// signalReader closes reading on its first Read.
type signalReader struct {
	once    sync.Once
	reading chan struct{}
	r       io.Reader
}

func (s *signalReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.reading) })
	return s.r.Read(p)
}

func TestWriteNeedsAreCheckedAgainWhenTheBlobAppears(t *testing.T) {
	p := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write",
		blobAction+"add/action")
	s := arm.New(arm.WithPolicy(p))
	newStorage(t, s, "")
	pr, pw := io.Pipe()
	body := &signalReader{reading: make(chan struct{}), r: pr}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/acct/data/a", body)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	slow := httptest.NewRecorder()
	var wg sync.WaitGroup
	wg.Go(func() { s.Blob().ServeHTTP(slow, req) })
	<-body.reading
	if rec := blobDo(t, s, http.MethodPut, "/acct/data/a", "fast"); rec.Code != http.StatusCreated {
		t.Fatalf("the create that wins = %d %s", rec.Code, rec.Body)
	}
	if _, err := pw.Write([]byte("slow")); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	wantError(t, slow, http.StatusForbidden, "AuthorizationPermissionMismatch")
}

func TestExpiredDeletedContainersArePurgedOnAccess(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := arm.New(arm.WithClock(func() time.Time { return now }))
	newStorage(t, s, `{"containerDeleteRetentionPolicy":{"enabled":true,"days":1}}`)
	blobDo(t, s, http.MethodDelete, "/acct/data?restype=container", "")
	now = now.Add(48 * time.Hour)
	do(t, s, http.MethodGet, accountPath+"/blobServices/default/containers/other", "")
	now = now.Add(-47 * time.Hour)
	if _, body := do(t, s, http.MethodGet, accountPath+"/blobServices/default/containers?$include=deleted&api-version=1", ""); len(body["value"].([]any)) != 0 {
		t.Fatalf("an expired deleted container outlived an access to its account: %v", body)
	}
}

func TestSnapshotOfABlobFromBeforeVersioning(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	do(t, s, http.MethodPut, accountPath+"/blobServices/default", `{"properties":{"isVersioningEnabled":true}}`)
	if rec := blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", ""); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body)
	}
	got := s.Blobs()
	if len(got) != 3 {
		t.Fatalf("blobs = %+v", got)
	}
	for _, b := range got {
		target := "/acct/data/a"
		switch {
		case b.Current:
			continue
		case b.Snapshot != "":
			target += "?snapshot=" + b.Snapshot
		case b.VersionID != "":
			target += "?versionid=" + b.VersionID
		default:
			t.Fatalf("a previous version without a version ID: %+v", b)
		}
		if rec := blobDo(t, s, http.MethodDelete, target, ""); rec.Code != http.StatusAccepted {
			t.Fatalf("DELETE %s = %d %s", target, rec.Code, rec.Body)
		}
	}
	if got := s.Blobs(); len(got) != 1 || !got[0].Current {
		t.Fatalf("blobs = %+v", got)
	}
}

func TestEmptyVersionLevelImmutableContainers(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		do(t, s, http.MethodPut, accountPath+"/blobServices/default/containers/worm", `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		wantError(t, blobDo(t, s, http.MethodDelete, "/acct/worm?restype=container", ""), http.StatusConflict, "BlobImmutableDueToPolicy")
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
		rec, body = do(t, s, http.MethodDelete, accountPath+"/blobServices/default/containers/worm", "")
		wantStatus(t, rec, body, http.StatusOK, "")
		rec, body = do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusOK, "")
	})
	t.Run("account", func(t *testing.T) {
		s := arm.New()
		newGroup(t, s)
		do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
		wantError(t, blobDo(t, s, http.MethodDelete, "/acct/data?restype=container", ""), http.StatusConflict, "BlobImmutableDueToPolicy")
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
	})
}

func TestBlockedGroupDeleteKeepsTheWholeAccount(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	hold(t, s)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	children := []string{"/fileServices/default/shares/files", "/queueServices/default/queues/jobs", "/tableServices/default/tables/Jobs"}
	for _, path := range children {
		if rec, body := do(t, s, http.MethodPut, accountPath+path, `{"properties":{}}`); rec.Code >= 300 {
			t.Fatalf("PUT %s = %d %v", path, rec.Code, body)
		}
	}
	rec, body := do(t, s, http.MethodDelete, groupPath, "")
	if rec.Code == http.StatusAccepted {
		u, _ := url.Parse(rec.Header().Get("Location"))
		s.Settle()
		rec, body = do(t, s, http.MethodGet, u.RequestURI(), "")
	}
	wantStatus(t, rec, body, http.StatusConflict, "ResourceGroupDeletionBlocked")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, "shares") || strings.Contains(msg, "queues") || !strings.Contains(msg, "storageAccounts/acct") {
		t.Fatalf("the error should name only the account: %s", msg)
	}
	for _, path := range append(children, "/blobServices/default/containers/data", "/blobServices/default", "/fileServices/default", "/queueServices/default", "/tableServices/default") {
		if rec, body := do(t, s, http.MethodGet, accountPath+path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s after a blocked group delete = %d %v", path, rec.Code, body)
		}
	}
	if got := s.Blobs(); len(got) != 1 {
		t.Fatalf("blobs = %+v", got)
	}
}
