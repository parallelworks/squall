package servicenetworking

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
	// code, when set, is the google.rpc.Code the error carries in place of the one its status maps to.
	code int
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Message) }

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

func notFound(what string) *apiError {
	return &apiError{Status: http.StatusNotFound, Message: what + " not found."}
}

// failedPrecondition is the error an operation ends in when the state of the
// network or its ranges rules out the change.
func failedPrecondition(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...), code: codeFailedPrecondition}
}

func unimplemented(r *http.Request) *apiError {
	return &apiError{Status: http.StatusNotFound, Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"}
}

func faultError(f fault.Fault) *apiError {
	return &apiError{
		Status:  cmp.Or(f.Status, http.StatusServiceUnavailable),
		Message: fmt.Sprintf("squall: injected fault (%s)", f),
		Reason:  reason.UpperSnake(f.Reason),
		Domain:  service,
	}
}

func permissionDenied(missing []string) *apiError {
	perm, _, _ := strings.Cut(missing[0], "|")
	full := "//" + service + "/" + parent
	return &apiError{
		Status:   http.StatusForbidden,
		Message:  fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", perm, full),
		Reason:   "IAM_PERMISSION_DENIED",
		Domain:   "iam.googleapis.com",
		Metadata: map[string]string{"permission": perm, "resource": full},
	}
}

const codeFailedPrecondition = 9

// rpcCodes follow Google's HTTP mapping of google.rpc.Code.
var rpcCodes = map[int]struct {
	code int
	name string
}{
	http.StatusBadRequest:          {3, "INVALID_ARGUMENT"},
	http.StatusUnauthorized:        {16, "UNAUTHENTICATED"},
	http.StatusForbidden:           {7, "PERMISSION_DENIED"},
	http.StatusNotFound:            {5, "NOT_FOUND"},
	http.StatusConflict:            {6, "ALREADY_EXISTS"},
	http.StatusPreconditionFailed:  {9, "FAILED_PRECONDITION"},
	http.StatusTooManyRequests:     {8, "RESOURCE_EXHAUSTED"},
	499:                            {1, "CANCELLED"},
	http.StatusInternalServerError: {13, "INTERNAL"},
	http.StatusNotImplemented:      {12, "UNIMPLEMENTED"},
	http.StatusServiceUnavailable:  {14, "UNAVAILABLE"},
	http.StatusGatewayTimeout:      {4, "DEADLINE_EXCEEDED"},
}

func rpcCode(status int) (int, string) {
	if c, ok := rpcCodes[status]; ok {
		return c.code, c.name
	}
	if status >= 500 {
		return 13, "INTERNAL"
	}
	return 2, "UNKNOWN"
}

// rpcStatus holds the HTTP status in an error response but the rpc code in an operation.
type rpcStatus struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Status  string       `json:"status,omitzero"`
	Details []*errorInfo `json:"details,omitzero"`
}

type errorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitzero"`
}

func (e *apiError) details() []*errorInfo {
	if e.Reason == "" {
		return nil
	}
	return []*errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: e.Reason, Domain: e.Domain, Metadata: e.Metadata}}
}

func (e *apiError) operationStatus() *rpcStatus {
	code, _ := rpcCode(e.Status)
	return &rpcStatus{Code: cmp.Or(e.code, code), Message: e.Message, Details: e.details()}
}

func writeError(w http.ResponseWriter, e *apiError) {
	_, name := rpcCode(e.Status)
	writeJSON(w, e.Status, struct {
		Error rpcStatus `json:"error"`
	}{rpcStatus{Code: e.Status, Message: e.Message, Status: name, Details: e.details()}})
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
