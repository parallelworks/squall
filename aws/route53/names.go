package route53

import (
	"fmt"
	"strings"
)

// canonicalName lowercases name, adds the trailing dot and writes each byte
// other than a letter, digit, hyphen or underscore as a \ooo escape, as Route
// 53 returns names. It reports false for a name that is not a domain name.
func canonicalName(name string) (string, bool) {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 255 {
		return "", false
	}
	var b strings.Builder
	for label := range strings.SplitSeq(name, ".") {
		raw, ok := unescape(label)
		if !ok || len(raw) == 0 || len(raw) > 63 {
			return "", false
		}
		for _, c := range []byte(strings.ToLower(raw)) {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "\\%03o", c)
			}
		}
		b.WriteByte('.')
	}
	return b.String(), true
}

// unescape decodes a label's \ooo octal escapes and \c escapes.
func unescape(label string) (string, bool) {
	if !strings.Contains(label, `\`) {
		return label, true
	}
	var b strings.Builder
	for i := 0; i < len(label); i++ {
		if label[i] != '\\' {
			b.WriteByte(label[i])
			continue
		}
		if i+3 < len(label) && label[i+1] >= '0' && label[i+1] <= '3' && isOctal(label[i+2]) && isOctal(label[i+3]) {
			b.WriteByte((label[i+1]-'0')<<6 | (label[i+2]-'0')<<3 | (label[i+3] - '0'))
			i += 3
			continue
		}
		if i+1 >= len(label) {
			return "", false
		}
		b.WriteByte(label[i+1])
		i++
	}
	return b.String(), true
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// compareNames orders canonical names as Route 53 lists them: by their labels
// from the last to the first.
func compareNames(a, b string) int {
	a, b = strings.TrimSuffix(a, "."), strings.TrimSuffix(b, ".")
	for a != "" && b != "" {
		var la, lb string
		a, la = lastLabel(a)
		b, lb = lastLabel(b)
		if c := strings.Compare(la, lb); c != 0 {
			return c
		}
	}
	return strings.Compare(a, b)
}

// lastLabel splits name into the labels before its last one and its last one.
func lastLabel(name string) (rest, label string) {
	rest, label, ok := strings.CutLast(name, ".")
	if !ok {
		return "", name
	}
	return rest, label
}

// inZone reports whether the canonical name is the zone's or below it.
func inZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}
