// Package sdkerr reads the codes the client libraries' errors carry.
package sdkerr

import (
	"errors"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	"google.golang.org/api/googleapi"
)

// AWSCode returns the error code of an AWS API error, or "".
func AWSCode(err error) string {
	if e, ok := errors.AsType[smithy.APIError](err); ok {
		return e.ErrorCode()
	}
	return ""
}

// AWSStatus returns the HTTP status of an AWS response error, or 0.
func AWSStatus(err error) int {
	if e, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		return e.HTTPStatusCode()
	}
	return 0
}

// GoogleCode returns the HTTP status of a Google API error, or 0.
func GoogleCode(err error) int {
	if e, ok := errors.AsType[*googleapi.Error](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[interface {
		error
		HTTPCode() int
	}](err); ok {
		return e.HTTPCode()
	}
	return 0
}
