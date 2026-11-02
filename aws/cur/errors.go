package cur

import (
	"cmp"
	"fmt"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/fault"
)

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func validationError(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "ValidationException", Message: fmt.Sprintf(format, args...)}
}

// constraintErrors joins constraint violations in the "N validation errors detected" form AWS services use.
func constraintErrors(violations []string) *apiError {
	noun := "errors"
	if len(violations) == 1 {
		noun = "error"
	}
	return validationError("%d validation %s detected: %s", len(violations), noun, strings.Join(violations, "; "))
}

func serializationError(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "SerializationException", Message: fmt.Sprintf(format, args...)}
}

func unknownOperation(what string) *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "UnknownOperationException", Message: "squall: " + strings.TrimSpace(what) + " is not implemented"}
}

func duplicateReportName(name string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "DuplicateReportNameException", Message: "A report with the name " + name + " already exists in the account."}
}

func reportLimitReached(limit int) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Code: "ReportLimitReachedException",
		Message: fmt.Sprintf("This account already has %d reports defined. To define a new report, you must delete an existing report.", limit),
	}
}

func bucketPermission() *apiError {
	return validationError("Failed to verify customer bucket permission.")
}

// faultError maps the fault package's generic reasons to the API's codes; any other reason is the code itself.
func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = "InternalErrorException"
		if status == http.StatusServiceUnavailable {
			code = "ServiceUnavailable"
		}
	case "rateLimitExceeded":
		code, status = "ThrottlingException", http.StatusBadRequest
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorJSON struct {
	Type    string `json:"__type"`
	Message string `json:"Message"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("X-Amzn-ErrorType", e.Code)
	writeJSON(w, e.Status, errorJSON{Type: e.Code, Message: e.Message})
}
