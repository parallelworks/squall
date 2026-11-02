package bigquery

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/fault"
)

// apiError is an error in the shape the BigQuery API answers with.
type apiError struct {
	Status  int
	Reason  string
	Message string
	// rpc, when set, is the google.rpc.Code name the error carries in place of the one its status maps to.
	rpc string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Reason, e.Message) }

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "invalid", Message: fmt.Sprintf(format, args...)}
}

func notFound(project, dataset string) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: "Not found: Dataset " + project + ":" + dataset}
}

func duplicate(project, dataset string) *apiError {
	return &apiError{Status: http.StatusConflict, Reason: "duplicate", Message: "Already Exists: Dataset " + project + ":" + dataset}
}

func inUse(project, dataset string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "resourceInUse", Message: "Dataset " + project + ":" + dataset + " is still in use", rpc: "FAILED_PRECONDITION"}
}

func preconditionFailed() *apiError {
	return &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: "Precondition check failed.", rpc: "FAILED_PRECONDITION"}
}

func unimplemented(r *http.Request) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"}
}

// accessDenied is the refusal of a call that needs perm on the project or on a dataset in it.
func accessDenied(perm, project, dataset string) *apiError {
	perm, _, _ = strings.Cut(perm, "|")
	msg := fmt.Sprintf("Access Denied: Project %s: User does not have %s permission in project %s.", project, perm, project)
	if dataset != "" {
		msg = fmt.Sprintf("Access Denied: Dataset %s:%s: Permission %s denied on dataset %s:%s (or it may not exist).", project, dataset, perm, project, dataset)
	}
	return &apiError{Status: http.StatusForbidden, Reason: "accessDenied", Message: msg}
}

// notEnabled is the refusal of a call in a project where the BigQuery API is not enabled.
func notEnabled(project string) *apiError {
	return &apiError{
		Status: http.StatusForbidden, Reason: "accessNotConfigured",
		Message: fmt.Sprintf("BigQuery API has not been used in project %s before or it is disabled. Enable it by visiting "+
			"https://console.developers.google.com/apis/api/bigquery.googleapis.com/overview?project=%s then retry. "+
			"If you enabled this API recently, wait a few minutes for the action to propagate to our systems and retry.", project, project),
	}
}

func faultError(f fault.Fault) *apiError {
	return &apiError{Status: cmp.Or(f.Status, http.StatusServiceUnavailable), Reason: cmp.Or(f.Reason, "backendError"), Message: fmt.Sprintf("squall: injected fault (%s)", f)}
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

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Errors  []errorReason `json:"errors"`
	Status  string        `json:"status"`
}

type errorReason struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	domain := "global"
	if e.Reason == "accessNotConfigured" {
		domain = "usageLimits"
	}
	writeJSON(w, e.Status, errorBody{errorDetail{
		Code:    e.Status,
		Message: e.Message,
		Errors:  []errorReason{{Message: e.Message, Domain: domain, Reason: e.Reason}},
		Status:  cmp.Or(e.rpc, rpcName(e.Status)),
	}})
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
