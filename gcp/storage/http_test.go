package storage_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/storage"
)

func do(t *testing.T, s *storage.Server, method, target, contentType, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestResponsesUseTheAPIShape(t *testing.T) {
	s := storage.New()
	code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p&alt=json&prettyPrint=false", "application/json", `{"name":"shape","location":"us-east1"}`)
	for _, want := range []string{
		`"kind":"storage#bucket"`, `"id":"shape"`, `"selfLink":"https://www.googleapis.com/storage/v1/b/shape"`,
		`"metageneration":"1"`, `"location":"US-EAST1"`, `"locationType":"region"`, `"projectNumber":"`,
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("insert = %d %s, want %s", code, body, want)
		}
	}
	code, body = do(t, s, http.MethodPost, "/upload/storage/v1/b/shape/o?uploadType=media&name=a%2Fb", "text/plain", "hello")
	for _, want := range []string{`"kind":"storage#object"`, `"name":"a/b"`, `"size":"5"`, `"md5Hash":"XUFAKrxLKna5cZ2REBfFkg=="`, `"crc32c":"mnG7TA=="`, `"contentType":"text/plain"`} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("upload = %d %s, want %s", code, body, want)
		}
	}
	code, body = do(t, s, http.MethodDelete, "/storage/v1/b/shape", "", "")
	if code != http.StatusConflict || body != `{"error":{"code":409,"message":"The bucket you tried to delete is not empty.","errors":[{"message":"The bucket you tried to delete is not empty.","domain":"global","reason":"conflict"}]}}` {
		t.Fatalf("delete = %d %s", code, body)
	}
	if code, body = do(t, s, http.MethodDelete, "/storage/v1/b/shape/o/a%2Fb", "", ""); code != http.StatusNoContent || body != "" {
		t.Fatalf("delete object = %d %s", code, body)
	}
}

func TestRequestsTheServerCannotRead(t *testing.T) {
	s := storage.New()
	if code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"known"}`); code != http.StatusOK {
		t.Fatalf("insert = %d %s", code, body)
	}
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
		reason                     string
	}{
		{"no project", http.MethodPost, "/storage/v1/b", `{"name":"x-bucket"}`, http.StatusBadRequest, "required"},
		{"an unknown field", http.MethodPost, "/storage/v1/b?project=p", `{"name":"x-bucket","website":{}}`, http.StatusBadRequest, "invalid"},
		{"lifecycle rules", http.MethodPost, "/storage/v1/b?project=p", `{"name":"x-bucket","lifecycle":{"rule":[{}]}}`, http.StatusBadRequest, "invalid"},
		{"malformed JSON", http.MethodPost, "/storage/v1/b?project=p", `{`, http.StatusBadRequest, "invalid"},
		{"an unknown parameter", http.MethodGet, "/storage/v1/b?project=p&bogus=1", "", http.StatusBadRequest, "invalid"},
		{"another alt", http.MethodGet, "/storage/v1/b/known?alt=media", "", http.StatusBadRequest, "invalid"},
		{"a public ACL", http.MethodPost, "/storage/v1/b?project=p&predefinedAcl=publicRead", `{"name":"x-bucket"}`, http.StatusBadRequest, "invalid"},
		{"a bad page token", http.MethodGet, "/storage/v1/b?project=p&pageToken=%25", "", http.StatusBadRequest, "invalid"},
		{"a resumable upload", http.MethodPost, "/upload/storage/v1/b/known/o?uploadType=resumable", "", http.StatusBadRequest, "invalid"},
		{"an upload without a name", http.MethodPost, "/upload/storage/v1/b/known/o?uploadType=media", "x", http.StatusBadRequest, "required"},
		{"a patch of the location", http.MethodPatch, "/storage/v1/b/known", `{"location":"EU"}`, http.StatusBadRequest, "invalid"},
		{"an unimplemented method", http.MethodGet, "/storage/v1/b/known/iam", "", http.StatusNotFound, "notFound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, s, tc.method, tc.target, "application/json", tc.body)
			if code != tc.status || !strings.Contains(body, `"reason":"`+tc.reason+`"`) {
				t.Fatalf("= %d %s, want %d %s", code, body, tc.status, tc.reason)
			}
		})
	}
}
