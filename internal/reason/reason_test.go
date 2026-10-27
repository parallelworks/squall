package reason

import "testing"

func TestUpperSnake(t *testing.T) {
	for in, want := range map[string]string{
		"backendError":          "BACKEND_ERROR",
		"Internal Server Error": "INTERNAL_SERVER_ERROR",
		"rateLimitExceeded":     "RATE_LIMIT_EXCEEDED",
		"Forbidden":             "FORBIDDEN",
		"IAM_PERMISSION_DENIED": "IAM_PERMISSION_DENIED",
	} {
		if got := UpperSnake(in); got != want {
			t.Errorf("UpperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}
