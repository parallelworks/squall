package ec2

import (
	"cmp"
	"encoding/xml"
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/fault"
)

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func clientError(code, format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, code, fmt.Sprintf(format, args...)}
}

func invalidValue(format string, args ...any) *apiError {
	return clientError("InvalidParameterValue", format, args...)
}

func missing(param string) *apiError {
	return clientError("MissingParameter", "The request must contain the parameter %s", param)
}

func allocationNotFound(id string) *apiError {
	return clientError("InvalidAllocationID.NotFound", "The allocation ID '%s' does not exist", id)
}

func interfaceNotFound(id string) *apiError {
	return clientError("InvalidNetworkInterfaceID.NotFound", "The networkInterface ID '%s' does not exist", id)
}

// faultError is the error a fault answers with. The fault package's generic
// reasons become the EC2 codes for the same condition; any other reason is
// the code itself, so a rule can answer with any EC2 error.
func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = "InternalError"
		if status == http.StatusServiceUnavailable {
			code = "Unavailable"
		}
	case "rateLimitExceeded":
		code = "RequestLimitExceeded"
	}
	return &apiError{status, code, fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorResponse struct {
	XMLName   xml.Name   `xml:"Response"`
	Errors    []errorXML `xml:"Errors>Error"`
	RequestID string     `xml:"RequestID"`
}

type errorXML struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func writeError(w http.ResponseWriter, requestID string, e *apiError) {
	writeXML(w, e.Status, xml.StartElement{Name: xml.Name{Local: "Response"}},
		errorResponse{Errors: []errorXML{{e.Code, e.Message}}, RequestID: requestID})
}

func writeXML(w http.ResponseWriter, status int, start xml.StartElement, body any) {
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = xml.NewEncoder(w).EncodeElement(body, start)
}
