package compute

import (
	"bytes"
	"fmt"
	"strconv"
)

// Int64 is a 64-bit integer field. It is written as a JSON string, as the
// API writes it, and read from a JSON string or number, as the API reads it.
type Int64 int64

// MarshalJSON writes n as a JSON string.
func (n Int64) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, strconv.FormatInt(int64(n), 10)), nil
}

// UnmarshalJSON reads n from a JSON string or number.
func (n *Int64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	digits := b
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		digits = b[1 : len(b)-1]
	}
	if bytes.ContainsAny(digits, `"\`) {
		return fmt.Errorf("invalid int64 value %s", b)
	}
	v, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid int64 value %s", b)
	}
	*n = Int64(v)
	return nil
}
