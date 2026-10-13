package compute

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/fault"
)

// apiError is an error in the shape Google APIs answer with.
type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Reason, e.Message) }

func notFound(resource string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("The resource '%s' was not found", resource)}
}

func alreadyExists(resource string) *apiError {
	return &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource '%s' already exists", resource)}
}

func invalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...)}
}

func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	return &apiError{status, cmp.Or(f.Reason, "backendError"), fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Errors  []errorReason `json:"errors"`
}

type errorReason struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, errorBody{errorDetail{
		Code:    e.Status,
		Message: e.Message,
		Errors:  []errorReason{{Message: e.Message, Domain: "global", Reason: e.Reason}},
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = json.NewEncoder(w).Encode(body)
}
