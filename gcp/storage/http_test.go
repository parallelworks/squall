package storage_test

import (
	"bytes"
	"encoding/json"
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
		{"data after the JSON", http.MethodPost, "/storage/v1/b?project=p", `{"name":"x-bucket"} {}`, http.StatusBadRequest, "invalid"},
		{"cors that is not a list", http.MethodPost, "/storage/v1/b?project=p", `{"name":"x-bucket","cors":5}`, http.StatusBadRequest, "invalid"},
		{"a patch of cors that is not a list", http.MethodPatch, "/storage/v1/b/known", `{"cors":[null]}`, http.StatusBadRequest, "invalid"},
		{"an ACME challenge name", http.MethodPost, "/upload/storage/v1/b/known/o?uploadType=media&name=.well-known%2Facme-challenge%2Fx", "x", http.StatusBadRequest, "invalid"},
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

func TestOversizedMultipartUploads(t *testing.T) {
	s := storage.New()
	if code, body := do(t, s, http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"big"}`); code != http.StatusOK {
		t.Fatalf("insert = %d %s", code, body)
	}
	body := "--x\r\nContent-Type: application/json\r\n\r\n{\"name\":\"o\"}\r\n--x\r\nContent-Type: text/plain\r\n\r\n" +
		strings.Repeat("x", 18<<20) + "\r\n--x--\r\n"
	code, resp := do(t, s, http.MethodPost, "/upload/storage/v1/b/big/o?uploadType=multipart", "multipart/related; boundary=x", body)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(resp, `"reason":"uploadTooLarge"`) {
		t.Fatalf("upload = %d %.200s", code, resp)
	}
}

func TestPatchClearsNullFields(t *testing.T) {
	s := storage.New()
	if _, err := s.PutBucket(storage.Bucket{
		Name: "cleared", Project: "p", Labels: map[string]string{"a": "1", "b": "2"},
		Versioning: &storage.Versioning{Enabled: true}, Encryption: &storage.Encryption{DefaultKmsKeyName: "k"},
		CORS: json.RawMessage(`[{"origin":["*"]}]`),
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, s, http.MethodPatch, "/storage/v1/b/cleared", "application/json", `{"labels":{"a":null}}`); code != http.StatusOK {
		t.Fatalf("patch a label = %d %s", code, body)
	}
	if b, _ := s.LookupBucket("cleared"); len(b.Labels) != 1 || b.Labels["b"] != "2" {
		t.Fatalf("labels = %v", b.Labels)
	}
	code, body := do(t, s, http.MethodPatch, "/storage/v1/b/cleared", "application/json", `{"labels":null,"versioning":null,"encryption":null,"cors":null}`)
	if code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	if b, _ := s.LookupBucket("cleared"); b.Labels != nil || b.Versioning != nil || b.Encryption != nil || b.CORS != nil || b.Metageneration != 3 {
		t.Fatalf("bucket = %+v", b)
	}
}

func FuzzServeHTTP(f *testing.F) {
	multipartBody := "--x\r\nContent-Type: application/json\r\n\r\n{\"name\":\"o\"}\r\n--x\r\nContent-Type: text/plain\r\n\r\nhi\r\n--x--\r\n"
	for _, seed := range []struct{ method, target, contentType, body string }{
		{http.MethodPost, "/storage/v1/b?project=p", "application/json", `{"name":"other","labels":{"a":"b"},"cors":[{"origin":["*"]}]}`},
		{http.MethodPatch, "/storage/v1/b/fuzz?ifMetagenerationMatch=1", "application/json", `{"labels":{"a":null},"versioning":null}`},
		{http.MethodGet, "/storage/v1/b/fuzz/o?versions=true&maxResults=1&pageToken=YQ&startOffset=a&endOffset=z", "", ""},
		{http.MethodPost, "/upload/storage/v1/b/fuzz/o?uploadType=multipart", "multipart/related; boundary=x", multipartBody},
		{http.MethodPost, "/upload/storage/v1/b/fuzz/o?uploadType=media&name=a%2F..%2Fb&ifGenerationMatch=0", "text/plain", "x"},
		{http.MethodDelete, "/storage/v1/b/fuzz/o/k?generation=1", "", ""},
	} {
		f.Add(seed.method, seed.target, seed.contentType, []byte(seed.body))
	}
	f.Fuzz(func(t *testing.T, method, target, contentType string, body []byte) {
		req, err := http.NewRequestWithContext(t.Context(), method, "http://storage.test"+target, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", contentType)
		s := storage.New()
		if _, err := s.PutBucket(storage.Bucket{Name: "fuzz", Project: "p", Versioning: &storage.Versioning{Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutObject("fuzz", "k", []byte("v")); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code < 200 || rec.Code > 599 {
			t.Fatalf("%s %s = %d", method, target, rec.Code)
		}
		if rec.Code != http.StatusNoContent && method != http.MethodHead && !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s %s = %d %q, not JSON", method, target, rec.Code, rec.Body)
		}
	})
}
