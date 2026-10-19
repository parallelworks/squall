package arm

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/fault"
)

// apiError is an error in the shape Azure Resource Manager answers with.
type apiError struct {
	Status  int
	Code    string
	Message string
	Target  string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func newError(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func groupNotFound(name string) *apiError {
	return newError(http.StatusNotFound, "ResourceGroupNotFound", "Resource group '%s' could not be found.", name)
}

func groupBeingDeleted(name string) *apiError {
	return newError(http.StatusConflict, "ResourceGroupBeingDeleted",
		"The resource group '%s' is in deprovisioning state and cannot perform this operation.", name)
}

func resourceNotFound(typ, name, group string) *apiError {
	return newError(http.StatusNotFound, "ResourceNotFound",
		"The Resource '%s/%s' under resource group '%s' was not found. For more details please go to https://aka.ms/ARMResourceNotFoundFix", typ, name, group)
}

func parentNotFound(name string) *apiError {
	return newError(http.StatusNotFound, "ParentResourceNotFound",
		"Can not perform requested operation on nested resource. Parent resource '%s' not found.", name)
}

func wrongLocation(name, have, want, group string) *apiError {
	return newError(http.StatusConflict, "InvalidResourceLocation",
		"The resource '%s' already exists in location '%s' in resource group '%s'. A resource with the same name cannot be created in location '%s'. Please select a new resource name.",
		name, have, group, want)
}

func locationRequired() *apiError {
	return newError(http.StatusBadRequest, "LocationRequired", "The location property is required for this definition.")
}

func badRequest(format string, args ...any) *apiError {
	return newError(http.StatusBadRequest, "BadRequest", format, args...)
}

// unsupported rejects input the stand-in does not model.
func unsupported(format string, args ...any) *apiError {
	return newError(http.StatusBadRequest, "BadRequest", "squall: "+format, args...)
}

func invalidParameter(target, format string, args ...any) *apiError {
	e := newError(http.StatusBadRequest, "InvalidParameter", format, args...)
	e.Target = target
	return e
}

func requiredParameter(target string) *apiError {
	return invalidParameter(target, "Required parameter '%s' is missing (null).", target)
}

func changeNotAllowed(property string) *apiError {
	e := newError(http.StatusConflict, "PropertyChangeNotAllowed", "Changing property '%s' is not allowed.", property)
	e.Target = property
	return e
}

func invalidReference(ref, by string) *apiError {
	return newError(http.StatusBadRequest, "InvalidResourceReference",
		"Resource %s referenced by resource %s was not found. Please make sure that the referenced resource exists, and that both resources are in the same region.", ref, by)
}

func unknownMember(member, typeName, path string) *apiError {
	return newError(http.StatusBadRequest, "InvalidRequestContent",
		"The request content was invalid and could not be deserialized: 'Could not find member '%s' on object of type '%s'. Path '%s'.'.", member, typeName, path)
}

func invalidContent(path string, err error) *apiError {
	return newError(http.StatusBadRequest, "InvalidRequestContent",
		"The request content was invalid and could not be deserialized: '%s at '%s''.", err, path)
}

func preconditionFailed() *apiError {
	return newError(http.StatusPreconditionFailed, "PreconditionFailed", "The condition specified in the If-Match or If-None-Match header was not met.")
}

// faultError is the error a fault answers with. The fault package's generic
// reasons become the code ARM uses for the status, such as
// ServiceUnavailable or TooManyRequests; any other reason is the code itself.
func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = cmp.Or(strings.ReplaceAll(http.StatusText(status), " ", ""), "InternalServerError")
	case "rateLimitExceeded":
		code = "TooManyRequests"
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Target  string `json:"target,omitzero"`
}

func (e *apiError) body() errorBody {
	return errorBody{errorDetail{Code: e.Code, Message: e.Message, Target: e.Target}}
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, nil, encode(e.body()))
}

// writeJSON answers with status, header and body, which may be empty.
func writeJSON(w http.ResponseWriter, status int, header http.Header, body json.RawMessage) {
	maps.Copy(w.Header(), header)
	if len(body) == 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Once the status is sent, a client that went away is all a write error can mean.
	_ = json.NewEncoder(w).Encode(body)
}

func encode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("squall: encoding a response: %v", err))
	}
	return b
}
