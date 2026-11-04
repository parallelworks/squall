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
	"github.com/parallelworks/squall/fault"
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

func lock(t *testing.T, s *arm.Server, etag string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, containerPath+"/immutabilityPolicies/default/lock?api-version=1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lock = %d %s", rec.Code, rec.Body)
	}
}

// deleteGroup deletes the group at path and answers as its operation ends.
func deleteGroup(t *testing.T, s *arm.Server, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, body := do(t, s, http.MethodDelete, path, "")
	if rec.Code == http.StatusAccepted {
		u, _ := url.Parse(rec.Header().Get("Location"))
		s.Settle()
		rec, body = do(t, s, http.MethodGet, u.RequestURI(), "")
	}
	return rec, body
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
	wantStatus(t, rec, body, http.StatusConflict, "ContainerImmutableStorageWithVersioningEnabled")
	blobDo(t, s, http.MethodDelete, "/acct/data/a", "")
	left := s.Blobs()
	if len(left) != 1 || left[0].Current || left[0].VersionID == "" {
		t.Fatalf("a delete under version-level immutability should keep a previous version: %+v", left)
	}
	rec, body = do(t, s, http.MethodDelete, containerPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "ContainerImmutableStorageWithVersioningEnabled")
	blobDo(t, s, http.MethodDelete, "/acct/data/a?versionid="+left[0].VersionID, "")
	rec, body = do(t, s, http.MethodDelete, containerPath, "")
	wantStatus(t, rec, body, http.StatusOK, "")
	rec, body = do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusOK, "")
}

func TestContainerVersionLevelImmutabilityProtectsTheAccount(t *testing.T) {
	s := arm.New()
	newStorage(t, s, `{"isVersioningEnabled":true}`)
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
		lock(t, s, policy(t, s))
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
	select {
	case <-body.reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow Put Blob never read its body")
	}
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
		newStorage(t, s, `{"isVersioningEnabled":true}`)
		do(t, s, http.MethodPut, accountPath+"/blobServices/default/containers/worm", `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		wantError(t, blobDo(t, s, http.MethodDelete, "/acct/worm?restype=container", ""), http.StatusConflict, "ContainerImmutableStorageWithVersioningEnabled")
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
		wantError(t, blobDo(t, s, http.MethodDelete, "/acct/data?restype=container", ""), http.StatusConflict, "ContainerImmutableStorageWithVersioningEnabled")
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
	rec, body := deleteGroup(t, s, groupPath)
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

func TestStorageAccountsAsAGroupOrNetworkName(t *testing.T) {
	const vlwAccount = `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`
	t.Run("held container in a group named storageAccounts", func(t *testing.T) {
		s := arm.New()
		g := "/subscriptions/s/resourceGroups/storageAccounts"
		acct := g + "/providers/Microsoft.Storage/storageAccounts/acct"
		do(t, s, http.MethodPut, g, `{"location":"eastus"}`)
		do(t, s, http.MethodPut, acct, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2"}`)
		blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
		do(t, s, http.MethodPost, acct+"/blobServices/default/containers/data/setLegalHold", `{"tags":["case1"]}`)
		blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
		rec, body := deleteGroup(t, s, g)
		wantStatus(t, rec, body, http.StatusConflict, "ResourceGroupDeletionBlocked")
		if rec, body := do(t, s, http.MethodGet, acct+"/blobServices/default/containers/data", ""); rec.Code != http.StatusOK {
			t.Fatalf("held container after a blocked group delete = %d %v", rec.Code, body)
		}
	})
	t.Run("version-level immutable account in a group named storageaccounts", func(t *testing.T) {
		s := arm.New()
		g := "/subscriptions/s/resourceGroups/storageaccounts"
		do(t, s, http.MethodPut, g, `{"location":"eastus"}`)
		do(t, s, http.MethodPut, g+"/providers/Microsoft.Storage/storageAccounts/acct", vlwAccount)
		blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
		wantError(t, blobDo(t, s, http.MethodDelete, "/acct/data?restype=container", ""), http.StatusConflict, "ContainerImmutableStorageWithVersioningEnabled")
	})
	t.Run("virtual network named storageaccounts", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		hold(t, s)
		blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
		vnet := groupPath + "/providers/Microsoft.Network/virtualNetworks/storageaccounts"
		if rec, body := do(t, s, http.MethodPut, vnet, `{"location":"eastus","properties":{"addressSpace":{"addressPrefixes":["10.0.0.0/16"]},"subnets":[{"name":"acct","properties":{"addressPrefix":"10.0.0.0/24"}}]}}`); rec.Code >= 300 {
			t.Fatalf("vnet = %d %v", rec.Code, body)
		}
		rec, body := deleteGroup(t, s, groupPath)
		wantStatus(t, rec, body, http.StatusConflict, "ResourceGroupDeletionBlocked")
		if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, "virtualNetworks") {
			t.Fatalf("the virtual network should not be kept: %s", msg)
		}
		if rec, _ := do(t, s, http.MethodGet, vnet, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("vnet after the group delete = %d", rec.Code)
		}
	})
}

func TestBlockedGroupDeleteWithTwoAccounts(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	hold(t, s)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	other := groupPath + "/providers/Microsoft.Storage/storageAccounts/other"
	do(t, s, http.MethodPut, other, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2"}`)
	do(t, s, http.MethodPut, other+"/queueServices/default/queues/jobs", `{}`)
	blobDo(t, s, http.MethodPut, "/other/c1?restype=container", "")
	blobDo(t, s, http.MethodPut, "/other/c1/x", "1")
	rec, body := deleteGroup(t, s, groupPath)
	wantStatus(t, rec, body, http.StatusConflict, "ResourceGroupDeletionBlocked")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, "other") || !strings.Contains(msg, "storageAccounts/acct") {
		t.Fatalf("the error should name only the protected account: %s", msg)
	}
	if rec, _ := do(t, s, http.MethodGet, other, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the unprotected account after the group delete = %d", rec.Code)
	}
	if got := s.Blobs(); len(got) != 1 || got[0].Account != "acct" {
		t.Fatalf("blobs = %+v", got)
	}
}

func TestAccountVersionLevelImmutabilityIsFixedAtCreation(t *testing.T) {
	const plain = `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{}}`
	s := arm.New()
	newGroup(t, s)
	do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
	blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
	rec, body := do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":false}}}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "PropertyIsImmutable")
	rec, body = do(t, s, http.MethodPut, accountPath, plain)
	if props, _ := body["properties"].(map[string]any); rec.Code != http.StatusOK || props["immutableStorageWithVersioning"] == nil {
		t.Fatalf("an update that leaves it out = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")

	rec, body = do(t, s, http.MethodGet, accountPath+"/blobServices/default", "")
	if props, _ := body["properties"].(map[string]any); rec.Code != http.StatusOK || props["isVersioningEnabled"] != true {
		t.Fatalf("blob service of a version-level immutable account = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, http.MethodPut, accountPath+"/blobServices/default", `{"properties":{"isVersioningEnabled":false}}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "InvalidRequestPropertyValue")

	other := groupPath + "/providers/Microsoft.Storage/storageAccounts/other"
	do(t, s, http.MethodPut, other, plain)
	rec, body = do(t, s, http.MethodPut, other, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "PropertyIsImmutable")
}

func TestSoftDeletedContainersKeepTheAccountProtected(t *testing.T) {
	t.Run("locked policy", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"containerDeleteRetentionPolicy":{"enabled":true,"days":7}}`)
		lock(t, s, policy(t, s))
		if rec, body := do(t, s, http.MethodDelete, containerPath, ""); rec.Code != http.StatusOK {
			t.Fatalf("delete the empty container = %d %v", rec.Code, body)
		}
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
	})
	t.Run("version-level immutability", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"isVersioningEnabled":true,"containerDeleteRetentionPolicy":{"enabled":true,"days":7}}`)
		worm := accountPath + "/blobServices/default/containers/worm"
		do(t, s, http.MethodPut, worm, `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		do(t, s, http.MethodDelete, worm, "")
		rec, body := do(t, s, http.MethodDelete, accountPath, "")
		wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
	})
}

func TestVersionLevelImmutabilityNeedsVersioning(t *testing.T) {
	const worm = accountPath + "/blobServices/default/containers/worm"
	const vlw = `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`
	const off = `{"properties":{"isVersioningEnabled":false}}`
	t.Run("a container on an account without versioning", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		rec, body := do(t, s, http.MethodPut, worm, vlw)
		wantStatus(t, rec, body, http.StatusBadRequest, "InvalidRequestPropertyValue")
	})
	t.Run("a live container", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"isVersioningEnabled":true}`)
		do(t, s, http.MethodPut, worm, vlw)
		rec, body := do(t, s, http.MethodPut, accountPath+"/blobServices/default", off)
		wantStatus(t, rec, body, http.StatusBadRequest, "InvalidRequestPropertyValue")
	})
	t.Run("a soft-deleted container", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"isVersioningEnabled":true,"containerDeleteRetentionPolicy":{"enabled":true,"days":7}}`)
		do(t, s, http.MethodPut, worm, vlw)
		do(t, s, http.MethodDelete, worm, "")
		rec, body := do(t, s, http.MethodPut, accountPath+"/blobServices/default", off)
		wantStatus(t, rec, body, http.StatusBadRequest, "InvalidRequestPropertyValue")
	})
}

func TestContainerNamesAreLowerCase(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	rec, body := do(t, s, http.MethodPut, accountPath+"/blobServices/default/containers/Logs", "{}")
	wantStatus(t, rec, body, http.StatusBadRequest, "InvalidResourceName")
	rec, body = do(t, s, http.MethodPut, accountPath+"/blobServices/default/containers/ab", "{}")
	wantStatus(t, rec, body, http.StatusBadRequest, "OutOfRangeInput")
}

func TestBlobsNameTheAccountAsCreated(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	upper := groupPath + "/providers/Microsoft.Storage/storageAccounts/ACCT/blobServices/default/containers/logs"
	if rec, body := do(t, s, http.MethodPut, upper, "{}"); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create through a differently cased account path = %d %v", rec.Code, body)
	}
	blobDo(t, s, http.MethodPut, "/acct/logs/a", "1")
	if got := s.Blobs(); len(got) != 1 || got[0].Account != "acct" || got[0].Container != "logs" {
		t.Fatalf("blobs = %+v", got)
	}
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/Logs?restype=container&comp=list", ""), http.StatusNotFound, "ContainerNotFound")
}

func TestSoftDeletedContainerProtectionLapses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := arm.New(arm.WithClock(func() time.Time { return now }))
	newStorage(t, s, `{"containerDeleteRetentionPolicy":{"enabled":true,"days":1}}`)
	lock(t, s, policy(t, s))
	do(t, s, http.MethodDelete, containerPath, "")
	rec, body := do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusConflict, "AccountProtectedFromDeletion")
	now = now.Add(48 * time.Hour)
	rec, body = do(t, s, http.MethodDelete, accountPath, "")
	wantStatus(t, rec, body, http.StatusOK, "")
}

func TestBlobServiceEdges(t *testing.T) {
	t.Run("include takes tags in any case", func(t *testing.T) {
		p := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write", blobAction+"read")
		s := arm.New(arm.WithPolicy(p))
		newStorage(t, s, "")
		wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=metadata,%20Tags", ""), http.StatusForbidden, "AuthorizationPermissionMismatch")
	})
	t.Run("private link host names", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, "")
		blobDo(t, s, http.MethodPut, "/acct/data/a", "x")
		rec := blobDo(t, s, http.MethodGet, "https://acct.privatelink.blob.core.windows.net/data?restype=container&comp=list", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<Name>a</Name>") {
			t.Fatalf("private link list = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a versioned snapshot shares its time with the new version", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"isVersioningEnabled":true}`)
		blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
		rec := blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", "")
		if snap := rec.Header().Get("x-ms-snapshot"); snap == "" || snap != rec.Header().Get("x-ms-version-id") {
			t.Fatalf("snapshot = %v", rec.Header())
		}
	})
}

func TestVersionLevelPoliciesAreRefused(t *testing.T) {
	t.Run("a default policy on the account", func(t *testing.T) {
		s := arm.New()
		newGroup(t, s)
		rec, body := do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true,"immutabilityPolicy":{"immutabilityPeriodSinceCreationInDays":1,"state":"Unlocked"}}}}`)
		wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
	})
	t.Run("a policy on a container with version-level immutability", func(t *testing.T) {
		s := arm.New()
		newStorage(t, s, `{"isVersioningEnabled":true}`)
		worm := accountPath + "/blobServices/default/containers/worm"
		do(t, s, http.MethodPut, worm, `{"properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		rec, body := do(t, s, http.MethodPut, worm+"/immutabilityPolicies/default", `{"properties":{"immutabilityPeriodSinceCreationInDays":1}}`)
		wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
	})
	t.Run("a policy on a container of an account with version-level immutability", func(t *testing.T) {
		s := arm.New()
		newGroup(t, s)
		do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"immutableStorageWithVersioning":{"enabled":true}}}`)
		blobDo(t, s, http.MethodPut, "/acct/data?restype=container", "")
		rec, body := do(t, s, http.MethodPut, containerPath+"/immutabilityPolicies/default", `{"properties":{"immutabilityPeriodSinceCreationInDays":1}}`)
		wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
	})
}

func TestWormInputNotModeled(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	rec, body := do(t, s, http.MethodPut, containerPath+"/immutabilityPolicies/default", `{"properties":{"immutabilityPeriodSinceCreationInDays":1,"allowProtectedAppendWritesAll":true}}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
	rec, body = do(t, s, http.MethodPost, containerPath+"/setLegalHold", `{"tags":["case1"],"allowProtectedAppendWritesAll":true}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
	tags := make([]string, 11)
	for i := range tags {
		tags[i] = `"case` + string(rune('a'+i)) + `x"`
	}
	rec, body = do(t, s, http.MethodPost, containerPath+"/setLegalHold", `{"tags":[`+strings.Join(tags[:10], ",")+`]}`)
	wantStatus(t, rec, body, http.StatusOK, "")
	rec, body = do(t, s, http.MethodPost, containerPath+"/setLegalHold", `{"tags":[`+tags[10]+`]}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "InvalidParameter")
	do(t, s, http.MethodPost, containerPath+"/clearLegalHold", `{"tags":[`+strings.Join(tags[:10], ",")+`]}`)
	lock(t, s, policy(t, s))
	rec, body = do(t, s, http.MethodPut, containerPath+"/immutabilityPolicies/default", `{"properties":{"immutabilityPeriodSinceCreationInDays":2}}`)
	wantStatus(t, rec, body, http.StatusBadRequest, "BadRequest")
}

func TestSystemContainers(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	if rec, body := do(t, s, http.MethodPut, accountPath+"/blobServices/default/containers/$web", "{}"); rec.Code != http.StatusCreated {
		t.Fatalf("ARM $web = %d %v", rec.Code, body)
	}
	if rec := blobDo(t, s, http.MethodPut, "/acct/$root?restype=container", ""); rec.Code != http.StatusCreated {
		t.Fatalf("data-plane $root = %d %s", rec.Code, rec.Body)
	}
	if rec := blobDo(t, s, http.MethodPut, "/acct/$root/a", "1"); rec.Code != http.StatusCreated {
		t.Fatalf("a blob in $root = %d %s", rec.Code, rec.Body)
	}
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/$logs?restype=container", ""), http.StatusBadRequest, "InvalidResourceName")
	wantError(t, blobDo(t, s, http.MethodHead, "/acct/a", ""), http.StatusBadRequest, "BadRequest")
}

func TestBlobCallsUseCanonicalNames(t *testing.T) {
	t.Run("faults see the account in lower case", func(t *testing.T) {
		faults := fault.New().Add(fault.Rule{Method: "PutBlob", Resource: "acct/data/a", Fault: fault.Unavailable()})
		s := arm.New(arm.WithFaults(faults))
		newStorage(t, s, "")
		if rec := blobDo(t, s, http.MethodPut, "/ACCT/data/a", "1"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("a fault on acct/data/a through /ACCT = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("write needs look the container up by its exact name", func(t *testing.T) {
		p := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write",
			blobAction+"add/action")
		s := arm.New(arm.WithPolicy(p))
		newStorage(t, s, "")
		blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
		wantError(t, blobDo(t, s, http.MethodPut, "/acct/Data/a", "2"), http.StatusNotFound, "ContainerNotFound")
	})
}
