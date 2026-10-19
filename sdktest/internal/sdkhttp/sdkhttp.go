// Package sdkhttp holds HTTP plumbing shared by the client library tests.
package sdkhttp

import (
	"bytes"
	"io"
	"net/http"
)

// Doer sends a request, as the AWS and Azure clients' HTTP clients do.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// CopyBodies sends each request through next with its body copied into memory,
// since an SDK may close its body (AWS) or seek it to retry (Azure) while the transport still reads it.
func CopyBodies(next Doer) Doer { return copier{next} }

type copier struct{ next Doer }

func (c copier) Do(req *http.Request) (*http.Response, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return c.next.Do(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		_ = req.Body.Close()
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return c.next.Do(req)
}
