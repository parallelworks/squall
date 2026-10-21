package s3

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
	// Bucket names the bucket the error is about, when there is one.
	Bucket string
	// Region, for a redirect, is the region the bucket is in.
	Region string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func invalidArgument(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: fmt.Sprintf(format, args...)}
}

func malformedXML() *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Code: "MalformedXML",
		Message: "The XML you provided was not well-formed or did not validate against our published schema",
	}
}

func noSuchBucket(name string) *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "NoSuchBucket", Message: "The specified bucket does not exist", Bucket: name}
}

func accessDenied(name string) *apiError {
	return &apiError{Status: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied", Bucket: name}
}

func invalidTag(message string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "InvalidTag", Message: message}
}

func redirect(name, region string) *apiError {
	return &apiError{
		Status: http.StatusMovedPermanently, Code: "PermanentRedirect",
		Message: "The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.",
		Bucket:  name, Region: region,
	}
}

// faultError maps the fault package's generic reasons to S3 codes; any other reason is the code itself.
func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = "InternalError"
		if status == http.StatusServiceUnavailable {
			code = "ServiceUnavailable"
		}
	case "rateLimitExceeded":
		code, status = "SlowDown", http.StatusServiceUnavailable
	}
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorXML struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	BucketName string   `xml:"BucketName,omitempty"`
	Bucket     string   `xml:"Bucket,omitempty"`
	Endpoint   string   `xml:"Endpoint,omitempty"`
	RequestID  string   `xml:"RequestId"`
	HostID     string   `xml:"HostId"`
}

// writeError answers with e, with no body for a HEAD, as S3 answers one.
func writeError(w http.ResponseWriter, r *http.Request, requestID string, e *apiError) {
	if e.Region != "" {
		w.Header().Set("x-amz-bucket-region", e.Region)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	body := errorXML{Code: e.Code, Message: e.Message, BucketName: e.Bucket, RequestID: requestID, HostID: "squall/" + requestID}
	if e.Region != "" {
		body.BucketName, body.Bucket, body.Endpoint = "", e.Bucket, "s3."+e.Region+".amazonaws.com"
	}
	writeXML(w, e.Status, body)
}
