package resourcemanager

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/reason"
)

type apiError struct {
	Status   int
	Message  string
	Reason   string
	Domain   string
	Metadata map[string]string
	// rpc, when set, is the google.rpc.Code name the error carries in place of the one its status maps to.
	rpc string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Message) }

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

func unimplemented(r *http.Request) *apiError {
	return &apiError{Status: http.StatusNotFound, Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"}
}

// aborted is the error setIamPolicy answers a stale etag with.
func aborted() *apiError {
	return &apiError{
		Status:  http.StatusConflict,
		Message: "There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.",
		rpc:     "ABORTED",
	}
}

func faultError(f fault.Fault) *apiError {
	return &apiError{
		Status:  cmp.Or(f.Status, http.StatusServiceUnavailable),
		Message: fmt.Sprintf("squall: injected fault (%s)", f),
		Reason:  reason.UpperSnake(f.Reason),
		Domain:  service,
	}
}

// permissionDenied is also the answer for a project the server does not hold,
// as the API gives no other for a project that does not exist.
func permissionDenied(perm, resource string) *apiError {
	if perm == "" {
		return &apiError{Status: http.StatusForbidden, Message: "The caller does not have permission"}
	}
	perm, _, _ = strings.Cut(perm, "|")
	full := "//" + service + "/" + resource
	return &apiError{
		Status:   http.StatusForbidden,
		Message:  fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", perm, full),
		Reason:   "IAM_PERMISSION_DENIED",
		Domain:   "iam.googleapis.com",
		Metadata: map[string]string{"permission": perm, "resource": full},
	}
}

// rpcNames follow Google's HTTP mapping of google.rpc.Code.
var rpcNames = map[int]string{
	http.StatusBadRequest:          "INVALID_ARGUMENT",
	http.StatusUnauthorized:        "UNAUTHENTICATED",
	http.StatusForbidden:           "PERMISSION_DENIED",
	http.StatusNotFound:            "NOT_FOUND",
	http.StatusConflict:            "ALREADY_EXISTS",
	http.StatusPreconditionFailed:  "FAILED_PRECONDITION",
	http.StatusTooManyRequests:     "RESOURCE_EXHAUSTED",
	499:                            "CANCELLED",
	http.StatusInternalServerError: "INTERNAL",
	http.StatusNotImplemented:      "UNIMPLEMENTED",
	http.StatusServiceUnavailable:  "UNAVAILABLE",
	http.StatusGatewayTimeout:      "DEADLINE_EXCEEDED",
}

func rpcName(status int) string {
	if name, ok := rpcNames[status]; ok {
		return name
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}

type rpcStatus struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Status  string       `json:"status"`
	Details []*errorInfo `json:"details,omitzero"`
}

type errorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitzero"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	var details []*errorInfo
	if e.Reason != "" {
		details = []*errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: e.Reason, Domain: e.Domain, Metadata: e.Metadata}}
	}
	writeJSON(w, e.Status, struct {
		Error rpcStatus `json:"error"`
	}{rpcStatus{Code: e.Status, Message: e.Message, Status: cmp.Or(e.rpc, rpcName(e.Status)), Details: details}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		panic("squall: encoding a response: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	// Once the status is sent, a client that went away is all a write error can mean.
	_, _ = w.Write(b)
}
