package compute

import (
	"errors"
	"fmt"
	"strings"
)

// filter is a parsed list filter: comparisons that all must hold.
type filter []comparison

type comparison struct {
	field string
	value string
	equal bool
}

var errUnsupportedFilter = errors.New("unsupported filter")

// parseFilter parses the subset of the Compute Engine filter syntax the server
// supports: comparisons "field = value" and "field != value", optionally in
// parentheses, joined by AND or by juxtaposition. A value with spaces is quoted.
// It rejects a "*" in a value, which the API may read as a wildcard.
func parseFilter(s string) (filter, error) {
	p := filterParser{s: s}
	var f filter
	for p.skipSpace(); p.i < len(p.s); p.skipSpace() {
		paren := p.consume("(")
		c, err := p.comparison()
		if err != nil {
			return nil, err
		}
		if paren {
			p.skipSpace()
			if !p.consume(")") {
				return nil, p.errorf("unclosed parenthesis")
			}
		}
		f = append(f, c)
		p.skipSpace()
		if p.consume("AND") {
			if p.i < len(p.s) && p.s[p.i] != ' ' && p.s[p.i] != '\t' && p.s[p.i] != '(' {
				return nil, p.errorf("AND needs a space after it")
			}
			if p.skipSpace(); p.i == len(p.s) {
				return nil, p.errorf("AND needs a comparison after it")
			}
		}
	}
	return f, nil
}

type filterParser struct {
	s string
	i int
}

func (p *filterParser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s at offset %d of %q", errUnsupportedFilter, fmt.Sprintf(format, args...), p.i, p.s)
}

func (p *filterParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *filterParser) consume(token string) bool {
	if strings.HasPrefix(p.s[p.i:], token) {
		p.i += len(token)
		return true
	}
	return false
}

// until advances past bytes not in stop and returns them.
func (p *filterParser) until(stop string) string {
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(stop, rune(p.s[p.i])) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *filterParser) comparison() (comparison, error) {
	p.skipSpace()
	field := p.until(" \t=!()\"")
	if !filterable(field) {
		return comparison{}, p.errorf("cannot filter on %q", field)
	}
	p.skipSpace()
	equal := true
	switch {
	case p.consume("!="):
		equal = false
	case p.consume("="):
	default:
		return comparison{}, p.errorf("expected = or != after %q", field)
	}
	p.skipSpace()
	var value string
	if p.consume(`"`) {
		value = p.until(`"`)
		if !p.consume(`"`) {
			return comparison{}, p.errorf("unclosed quote")
		}
	} else if value = p.until(" \t()\""); value == "" {
		return comparison{}, p.errorf("expected a value for %q", field)
	}
	if strings.Contains(value, "*") {
		return comparison{}, p.errorf("wildcards are not supported")
	}
	return comparison{field: field, value: value, equal: equal}, nil
}

func filterable(field string) bool {
	switch field {
	case "name", "description", "status", "address", "addressType":
		return true
	}
	key, ok := strings.CutPrefix(field, "labels.")
	return ok && key != ""
}

func (f filter) matches(a *Address) bool {
	for _, c := range f {
		if (fieldValue(a, c.field) == c.value) != c.equal {
			return false
		}
	}
	return true
}

func fieldValue(a *Address, field string) string {
	switch field {
	case "name":
		return a.Name
	case "description":
		return a.Description
	case "status":
		return a.Status
	case "address":
		return a.Address
	case "addressType":
		return a.AddressType
	}
	key, _ := strings.CutPrefix(field, "labels.")
	return a.Labels[key]
}
