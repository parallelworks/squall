// Package reason spells error reasons as Google APIs do.
package reason

import (
	"strings"
	"unicode"
)

// UpperSnake spells a reason as AIP-193 requires: "backendError" becomes "BACKEND_ERROR".
func UpperSnake(s string) string {
	var b strings.Builder
	prev := ' '
	for _, c := range s {
		switch {
		case c == ' ' || c == '-':
			c = '_'
		case unicode.IsUpper(c) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(c))
		prev = c
	}
	return b.String()
}
