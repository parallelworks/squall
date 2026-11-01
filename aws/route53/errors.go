package route53

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
	// Messages are an InvalidChangeBatch's messages, one per failed change.
	Messages []string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func clientError(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalidInput(format string, args ...any) *apiError {
	return clientError(http.StatusBadRequest, "InvalidInput", format, args...)
}

func notImplemented(what string) *apiError {
	return clientError(http.StatusNotFound, "NotImplemented", "squall: %s is not implemented", what)
}

func noSuchHostedZone(id string) *apiError {
	return clientError(http.StatusNotFound, "NoSuchHostedZone", "No hosted zone found with ID: %s", id)
}

func invalidDomainName(name string) *apiError {
	return clientError(http.StatusBadRequest, "InvalidDomainName", "%s is not a valid domain name", name)
}

func invalidVPCID(id string) *apiError {
	return clientError(http.StatusBadRequest, "InvalidVPCId",
		"The VPC ID %s either isn't a valid ID or the current account is not authorized to access this VPC.", id)
}

func conflictingDomain(v VPC, zoneID string) *apiError {
	return clientError(http.StatusBadRequest, "ConflictingDomainExists",
		"The VPC %s in %s region has already been associated with the hosted zone %s with the same domain name.", v.ID, v.Region, zoneID)
}

func invalidChangeBatch(messages []string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidChangeBatch", Message: "ChangeBatch errors occurred", Messages: messages}
}

// faultError maps the fault package's generic reasons to Route 53 codes; any other reason is the code itself.
func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = "InternalFailure"
		if status == http.StatusServiceUnavailable {
			code = "ServiceUnavailable"
		}
	case "rateLimitExceeded":
		code, status = "Throttling", http.StatusBadRequest
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorResponse struct {
	XMLName   xml.Name `xml:"ErrorResponse"`
	NS        string   `xml:"xmlns,attr"`
	Error     errorXML `xml:"Error"`
	RequestID string   `xml:"RequestId"`
}

type errorXML struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type invalidChangeBatchXML struct {
	XMLName   xml.Name `xml:"InvalidChangeBatch"`
	NS        string   `xml:"xmlns,attr"`
	Messages  []string `xml:"Messages>Message"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, requestID string, e *apiError) {
	if e.Code == "InvalidChangeBatch" && len(e.Messages) > 0 {
		writeXML(w, e.Status, invalidChangeBatchXML{NS: namespace, Messages: e.Messages, RequestID: requestID})
		return
	}
	kind := "Sender"
	if e.Status >= http.StatusInternalServerError {
		kind = "Receiver"
	}
	writeXML(w, e.Status, errorResponse{NS: namespace, Error: errorXML{Type: kind, Code: e.Code, Message: e.Message}, RequestID: requestID})
}
