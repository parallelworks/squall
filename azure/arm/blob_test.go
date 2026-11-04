package arm_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const accountPath = groupPath + "/providers/Microsoft.Storage/storageAccounts/acct"

// newStorage makes the account acct, with blob service properties props when given, and its container data.
func newStorage(t *testing.T, s *arm.Server, props string) {
	t.Helper()
	newGroup(t, s)
	if rec, body := do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2"}`); rec.Code != http.StatusOK {
		t.Fatalf("create account = %d %v", rec.Code, body)
	}
	if props != "" {
		if rec, body := do(t, s, http.MethodPut, accountPath+"/blobServices/default", `{"properties":`+props+`}`); rec.Code != http.StatusOK {
			t.Fatalf("set blob service properties = %d %v", rec.Code, body)
		}
	}
	if rec := blobDo(t, s, http.MethodPut, "/acct/data?restype=container", ""); rec.Code != http.StatusCreated {
		t.Fatalf("create container = %d %s", rec.Code, rec.Body)
	}
}

// blobDo sends one call to s's Blob service with a bearer token.
func blobDo(t *testing.T, s *arm.Server, method, target, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("x-ms-version", "2026-02-06")
	if method == http.MethodPut && !strings.Contains(target, "restype=") && !strings.Contains(target, "comp=") {
		req.Header.Set("x-ms-blob-type", "BlockBlob")
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
		} else {
			req.Header.Set(header[i], header[i+1])
		}
	}
	rec := httptest.NewRecorder()
	s.Blob().ServeHTTP(rec, req)
	return rec
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || rec.Header().Get("x-ms-error-code") != code {
		t.Fatalf("answer = %d %s %s, want %d %s", rec.Code, rec.Header().Get("x-ms-error-code"), rec.Body, status, code)
	}
	if rec.Body.Len() > 0 && !strings.Contains(rec.Body.String(), "<Code>"+code+"</Code>") {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestBlobListShape(t *testing.T) {
	s := arm.New(arm.WithClock(func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }))
	newStorage(t, s, `{"isVersioningEnabled":true}`)
	blobDo(t, s, http.MethodPut, "/acct/data/dir%2Fa.txt", "hello", "Content-Type", "text/plain", "x-ms-meta-Owner", "me")
	rec := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=metadata&prefix=dir", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<EnumerationResults ServiceEndpoint="http://example.com/acct/" ContainerName="data"><Prefix>dir</Prefix><Blobs>` +
		`<Blob><Name>dir/a.txt</Name><VersionId>2026-01-02T03:04:05.0000000Z</VersionId><IsCurrentVersion>true</IsCurrentVersion>` +
		`<Properties><Creation-Time>Fri, 02 Jan 2026 03:04:05 GMT</Creation-Time><Last-Modified>Fri, 02 Jan 2026 03:04:05 GMT</Last-Modified>` +
		`<Etag>0x8D0000000000005</Etag><Content-Length>5</Content-Length><Content-Type>text/plain</Content-Type>` +
		`<Content-MD5>XUFAKrxLKna5cZ2REBfFkg==</Content-MD5><BlobType>BlockBlob</BlobType><AccessTier>Hot</AccessTier>` +
		`<AccessTierInferred>true</AccessTierInferred><LeaseStatus>unlocked</LeaseStatus><LeaseState>available</LeaseState>` +
		`<ServerEncrypted>true</ServerEncrypted></Properties><Metadata><owner>me</owner></Metadata></Blob></Blobs><NextMarker></NextMarker></EnumerationResults>`
	if got := rec.Body.String(); got != want {
		t.Fatalf("list body =\n%s\nwant\n%s", got, want)
	}
	for _, h := range []string{"x-ms-request-id", "x-ms-version", "Date"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("no %s header", h)
		}
	}
}

func TestBlobHostStyleAddressing(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "x")
	rec := blobDo(t, s, http.MethodGet, "https://acct.blob.core.windows.net/data?restype=container&comp=list", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `ServiceEndpoint="https://acct.blob.core.windows.net/"`) ||
		!strings.Contains(rec.Body.String(), "<Name>a</Name>") {
		t.Fatalf("host-style list = %d %s", rec.Code, rec.Body)
	}
}

func TestBlobRequestErrors(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "x")

	rec := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list", "", "Authorization", "")
	wantError(t, rec, http.StatusUnauthorized, "NoAuthenticationInformation")
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_id=https://storage.azure.com") {
		t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&sig=x", "", "Authorization", ""), http.StatusBadRequest, "BadRequest")
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data/a", ""), http.StatusNotFound, "NotImplemented")
	wantError(t, blobDo(t, s, http.MethodGet, "/nope/data?restype=container&comp=list", ""), http.StatusNotFound, "ResourceNotFound")
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&maxresults=0", ""), http.StatusBadRequest, "OutOfRangeQueryParameterValue")
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&maxresults=x", ""), http.StatusBadRequest, "InvalidQueryParameterValue")
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=bogus", ""), http.StatusBadRequest, "InvalidQueryParameterValue")
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&marker=%21", ""), http.StatusBadRequest, "InvalidQueryParameterValue")
	wantError(t, blobDo(t, s, http.MethodDelete, "/acct/data/a?deletetype=permanent", ""), http.StatusBadRequest, "BadRequest")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/b", "x", "x-ms-blob-type", ""), http.StatusBadRequest, "MissingRequiredHeader")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/b", "", "x-ms-blob-type", "PageBlob"), http.StatusBadRequest, "BadRequest")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/b", "x", "Content-MD5", "AAAAAAAAAAAAAAAAAAAAAA=="), http.StatusBadRequest, "Md5Mismatch")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a", "y", "If-None-Match", "*"), http.StatusConflict, "BlobAlreadyExists")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a", "y", "If-Match", `"0x1"`), http.StatusPreconditionFailed, "ConditionNotMet")
	wantError(t, blobDo(t, s, http.MethodPut, "/acct/data/a?comp=block&blockid=%21", "y"), http.StatusBadRequest, "InvalidBlockId")
	rec = blobDo(t, s, http.MethodHead, "/acct/data/missing", "")
	wantError(t, rec, http.StatusNotFound, "BlobNotFound")
	if rec.Body.Len() != 0 {
		t.Errorf("a HEAD error has a body: %s", rec.Body)
	}
}

func TestBlobListMarkersArePositions(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	for _, name := range []string{"a", "b", "c", "d"} {
		blobDo(t, s, http.MethodPut, "/acct/data/"+name, "x")
	}
	rec := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&maxresults=2", "")
	body := rec.Body.String()
	marker := body[strings.Index(body, "<NextMarker>")+len("<NextMarker>") : strings.Index(body, "</NextMarker>")]
	if marker == "" || !strings.Contains(body, "<MaxResults>2</MaxResults>") {
		t.Fatalf("first page = %s", body)
	}
	blobDo(t, s, http.MethodDelete, "/acct/data/b", "")
	blobDo(t, s, http.MethodDelete, "/acct/data/c", "")
	rec = blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&maxresults=2&marker="+marker, "")
	if body := rec.Body.String(); !strings.Contains(body, "<Name>d</Name>") || strings.Contains(body, "<Name>a</Name>") ||
		!strings.Contains(body, "<NextMarker></NextMarker>") {
		t.Fatalf("second page = %s", body)
	}
}

func TestBlobVersionsWithSoftDelete(t *testing.T) {
	s := arm.New()
	newStorage(t, s, `{"isVersioningEnabled":true,"deleteRetentionPolicy":{"enabled":true,"days":2}}`)
	old := blobDo(t, s, http.MethodPut, "/acct/data/a", "1").Header().Get("x-ms-version-id")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "2")
	if rec := blobDo(t, s, http.MethodDelete, "/acct/data/a?versionid="+old, ""); rec.Code != http.StatusAccepted ||
		rec.Header().Get("x-ms-delete-type-permanent") != "false" {
		t.Fatalf("delete a version = %d %v", rec.Code, rec.Header())
	}
	without := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=versions", "").Body.String()
	with := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=versions,deleted", "").Body.String()
	if strings.Count(without, "<Blob>") != 1 || strings.Count(with, "<Blob>") != 2 || !strings.Contains(with, "<Deleted>true</Deleted>") {
		t.Fatalf("versions = %s, with deleted = %s", without, with)
	}
	if rec := blobDo(t, s, http.MethodDelete, "/acct/data/a", ""); rec.Header().Get("x-ms-delete-type-permanent") != "false" {
		t.Fatalf("deleting a versioned blob = %v", rec.Header())
	}
}

func TestBlobDeleteOnlySnapshots(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	snap := blobDo(t, s, http.MethodPut, "/acct/data/a?comp=snapshot", "").Header().Get("x-ms-snapshot")
	if rec := blobDo(t, s, http.MethodHead, "/acct/data/a?snapshot="+snap, ""); rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "1" {
		t.Fatalf("snapshot properties = %d %v", rec.Code, rec.Header())
	}
	if rec := blobDo(t, s, http.MethodDelete, "/acct/data/a", "", "x-ms-delete-snapshots", "only"); rec.Code != http.StatusAccepted ||
		rec.Header().Get("x-ms-delete-type-permanent") != "true" {
		t.Fatalf("delete snapshots = %d %v", rec.Code, rec.Header())
	}
	if got := s.Blobs(); len(got) != 1 || !got[0].Current {
		t.Fatalf("blobs = %+v", got)
	}
}

func TestDeletedBlobsAndContainersExpire(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := arm.New(arm.WithClock(func() time.Time { return now }))
	newStorage(t, s, `{"deleteRetentionPolicy":{"enabled":true,"days":2},"containerDeleteRetentionPolicy":{"enabled":true,"days":1}}`)
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	blobDo(t, s, http.MethodDelete, "/acct/data/a", "")
	blobDo(t, s, http.MethodPut, "/acct/other?restype=container", "")
	blobDo(t, s, http.MethodDelete, "/acct/other?restype=container", "")
	now = now.Add(36 * time.Hour)
	list := blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list&include=deleted", "").Body.String()
	if !strings.Contains(list, "<RemainingRetentionDays>1</RemainingRetentionDays>") {
		t.Fatalf("a day and a half later = %s", list)
	}
	if got := blobDo(t, s, http.MethodGet, "/acct?comp=list&include=deleted", "").Body.String(); strings.Contains(got, "other") {
		t.Fatalf("a deleted container past its retention = %s", got)
	}
	now = now.Add(12 * time.Hour)
	if got := s.Blobs(); len(got) != 0 {
		t.Fatalf("blobs past their retention = %+v", got)
	}
}

func TestBlobAccountChecks(t *testing.T) {
	policy := access.Allow("Microsoft.Resources/*", "Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write")
	s := arm.New(arm.WithPolicy(policy))
	newStorage(t, s, "")
	const list = "/acct/data?restype=container&comp=list"
	wantError(t, blobDo(t, s, http.MethodGet, list, ""), http.StatusForbidden, "AuthorizationPermissionMismatch")
	if rec := blobDo(t, s, http.MethodGet, list, "", "Authorization", "SharedKey acct:sig"); rec.Code != http.StatusOK {
		t.Fatalf("a Shared Key call, which RBAC does not govern = %d %s", rec.Code, rec.Body)
	}
	do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"allowSharedKeyAccess":false}}`)
	wantError(t, blobDo(t, s, http.MethodGet, list, "", "Authorization", "SharedKey acct:sig"), http.StatusForbidden, "KeyBasedAuthenticationNotPermitted")
	do(t, s, http.MethodPut, accountPath, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{"publicNetworkAccess":"Disabled"}}`)
	wantError(t, blobDo(t, s, http.MethodGet, list, "", "Authorization", "SharedKey acct:sig"), http.StatusForbidden, "AuthorizationFailure")
}

func TestDeletingTheGroupDeletesBlobs(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	blobDo(t, s, http.MethodPut, "/acct/data/a", "1")
	if rec, body := do(t, s, http.MethodDelete, groupPath, ""); rec.Code >= 300 {
		t.Fatalf("delete group = %d %v", rec.Code, body)
	}
	if got := s.Blobs(); len(got) != 0 {
		t.Fatalf("blobs = %+v", got)
	}
	wantError(t, blobDo(t, s, http.MethodGet, "/acct/data?restype=container&comp=list", ""), http.StatusNotFound, "ResourceNotFound")
}

func TestStorageManagementRejectsWhatItDoesNotModel(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	containers := accountPath + "/blobServices/default/containers"
	for _, path := range []string{
		containers + "?api-version=1&$include=bogus", containers + "?api-version=1&$maxpagesize=1",
		accountPath + "/fileServices/default/shares?api-version=1&$filter=x&$expand=deleted", containers + "/data?api-version=1&$expand=stats",
	} {
		if rec, body := do(t, s, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s, which containers and shares take or ignore = %d %v", path, rec.Code, body)
		}
	}
	do(t, s, http.MethodPut, accountPath+"/queueServices/default/queues/jobs", `{}`)
	for _, path := range []string{
		accountPath + "/queueServices/default/queues?api-version=1&$filter=x",
		accountPath + "/queueServices/default/queues/jobs?api-version=1&$expand=x",
	} {
		if rec, body := do(t, s, http.MethodGet, path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d %v", path, rec.Code, body)
		}
	}
	if rec, body := do(t, s, http.MethodPost, containers+"/data/setLegalHold", `{"tags":["no"]}`); rec.Code != http.StatusBadRequest || errorCode(body) != "InvalidParameter" {
		t.Errorf("a short tag = %d %v", rec.Code, body)
	}
	if rec, body := do(t, s, http.MethodPost, containers+"/data/setLegalHold", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no tags = %d %v", rec.Code, body)
	}
	if rec, body := do(t, s, http.MethodPut, containers+"/data/immutabilityPolicies/other", `{"properties":{"immutabilityPeriodSinceCreationInDays":1}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a policy not named default = %d %v", rec.Code, body)
	}
	if rec, body := do(t, s, http.MethodPost, containers+"/data/immutabilityPolicies/default/lock", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a lock without If-Match = %d %v", rec.Code, body)
	}
	if rec, body := do(t, s, http.MethodPost, containers+"/missing/setLegalHold", `{"tags":["case1"]}`); rec.Code != http.StatusNotFound || errorCode(body) != "ContainerNotFound" {
		t.Errorf("a hold on a missing container = %d %v", rec.Code, body)
	}
}

func TestQueueAndTableShapes(t *testing.T) {
	s := arm.New()
	newStorage(t, s, "")
	rec, body := do(t, s, http.MethodPut, accountPath+"/queueServices/default/queues/jobs", `{"properties":{"metadata":{"a":"b"}}}`)
	props, _ := body["properties"].(map[string]any)
	if rec.Code != http.StatusOK || body["type"] != "Microsoft.Storage/storageAccounts/queueServices/queues" || props["approximateMessageCount"] != float64(0) {
		t.Fatalf("queue = %d %v", rec.Code, body)
	}
	rec, body = do(t, s, http.MethodPut, accountPath+"/tableServices/default/tables/Jobs", "")
	props, _ = body["properties"].(map[string]any)
	if rec.Code != http.StatusOK || props["tableName"] != "Jobs" || body["id"] != accountPath+"/tableServices/default/tables/Jobs" {
		t.Fatalf("table = %d %v", rec.Code, body)
	}
	for _, path := range []string{"/queueServices/default/queues/jobs", "/queueServices/default/queues/never", "/tableServices/default/tables/Jobs"} {
		if rec, _ := do(t, s, http.MethodDelete, accountPath+path, ""); rec.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d", path, rec.Code)
		}
	}
}
