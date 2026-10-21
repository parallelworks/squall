// Package stub runs a stand-in's calls under the faults the injector decides.
package stub

import (
	"net/http"
	"time"

	"github.com/parallelworks/squall/fault"
)

// Serve runs one call under f. It waits out f's delay, and then either
// rejects the call with fail or runs it. run applies the call and returns how
// to answer it, and whether it succeeded; a lost response or a failure after
// the call took effect replaces a successful answer. contentType is what a
// lost response starts with.
func Serve(w http.ResponseWriter, r *http.Request, f fault.Fault, reject bool, contentType string, fail func(), run func() (answer func(), ok bool)) {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-r.Context().Done():
			return
		}
	}
	if reject {
		fail()
		return
	}
	answer, ok := run()
	switch {
	case f.Mode == fault.ModeLoseResponse && r.Method == http.MethodHead:
		loseHeadResponse(w)
	case f.Mode == fault.ModeLoseResponse:
		loseResponse(w, contentType)
	case !ok:
		answer()
	case f.Mode == fault.ModeFailAfter:
		fail()
	default:
		answer()
	}
}

// loseResponse ends the response partway through, as a connection lost after
// the server committed would. Without the header on the wire, the client's
// transport would silently retry an idempotent request on a reused connection.
func loseResponse(w http.ResponseWriter, contentType string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

// loseHeadResponse breaks off inside the status line, since a sent HEAD header would read as an answer.
func loseHeadResponse(w http.ResponseWriter) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 2")
	_ = buf.Flush()
}
