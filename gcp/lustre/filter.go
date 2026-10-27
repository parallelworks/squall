package lustre

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// expr is a parsed list filter: a comparison, the negation ('!') of its one
// term, or all ('&') or any ('|') of its terms. A nil *expr matches everything.
type expr struct {
	op    byte
	terms []expr
	cmp   comparison
}

// comparison is field = value, field != value, or field:* (has).
type comparison struct {
	field string
	op    string
	value string
}

func (c comparison) String() string {
	if c.op == ":" {
		return c.field + ":" + c.value
	}
	return c.field + " " + c.op + " " + c.value
}

func (e *expr) String() string {
	switch {
	case e == nil:
		return ""
	case e.op == 0:
		return e.cmp.String()
	case e.op == '!':
		return "NOT (" + e.terms[0].String() + ")"
	}
	parts := make([]string, len(e.terms))
	for i := range e.terms {
		parts[i] = "(" + e.terms[i].String() + ")"
	}
	if e.op == '|' {
		return strings.Join(parts, " OR ")
	}
	return strings.Join(parts, " AND ")
}

// filterFields are the instance fields a filter can compare, besides labels.
var filterFields = []string{"name", "state", "filesystem", "network", "description"}

func (in *Instance) filterValue(field string) (string, bool) {
	switch field {
	case "name":
		return in.Name, true
	case "state":
		return in.State, true
	case "filesystem":
		return in.Filesystem, true
	case "network":
		return in.Network, true
	case "description":
		return in.Description, true
	}
	v, ok := in.Labels[strings.TrimPrefix(field, "labels.")]
	return v, ok
}

func (e *expr) matches(in *Instance) bool {
	switch {
	case e == nil:
		return true
	case e.op == '!':
		return !e.terms[0].matches(in)
	case e.op == '&':
		for i := range e.terms {
			if !e.terms[i].matches(in) {
				return false
			}
		}
		return true
	case e.op == '|':
		return slices.ContainsFunc(e.terms, func(t expr) bool { return t.matches(in) })
	}
	if e.cmp.field == "labels" {
		_, ok := in.Labels[e.cmp.value]
		return ok
	}
	got, ok := in.filterValue(e.cmp.field)
	switch e.cmp.op {
	case ":":
		return ok && (got != "" || strings.HasPrefix(e.cmp.field, "labels."))
	case "=":
		return ok && got == e.cmp.value
	}
	return !ok || got != e.cmp.value
}

var errUnsupportedFilter = errors.New("unsupported filter")

// parseFilter parses the subset of the AIP-160 filter grammar the server
// supports:
//
//   - Comparisons field = value and field != value, on name, state,
//     filesystem, network, description and labels.KEY, with a value that has
//     spaces in double quotes. A "*" in a value is rejected, since AIP-160
//     reads it as a wildcard.
//   - labels.KEY:* and labels:KEY, which match an instance that has the label.
//   - Terms joined by AND or juxtaposition, and by OR, which binds tighter
//     than AND, negated with NOT or -, and grouped with parentheses.
func parseFilter(s string) (*expr, error) {
	p := filterParser{s: s}
	if p.skipSpace(); p.i == len(p.s) {
		return nil, nil
	}
	e, err := p.conjunction()
	if err != nil {
		return nil, err
	}
	if p.i < len(p.s) {
		return nil, p.errorf("unexpected %q", p.s[p.i:p.i+1])
	}
	return e, nil
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

// keyword consumes word when a space or parenthesis follows it.
func (p *filterParser) keyword(word string) bool {
	rest := p.s[p.i:]
	if !strings.HasPrefix(rest, word) || len(rest) > len(word) && !strings.ContainsRune(" \t(", rune(rest[len(word)])) {
		return false
	}
	p.i += len(word)
	return true
}

// until advances past bytes not in stop and returns them.
func (p *filterParser) until(stop string) string {
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(stop, rune(p.s[p.i])) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *filterParser) atEnd() bool {
	p.skipSpace()
	return p.i == len(p.s) || p.s[p.i] == ')'
}

// conjunction parses terms joined by AND or juxtaposition, up to a closing
// parenthesis or the end.
func (p *filterParser) conjunction() (*expr, error) {
	var terms []expr
	for {
		t, err := p.disjunction()
		if err != nil {
			return nil, err
		}
		terms = append(terms, *t)
		if p.atEnd() {
			break
		}
		if p.keyword("AND") && p.atEnd() {
			return nil, p.errorf("AND needs a term after it")
		}
	}
	if len(terms) == 1 {
		return &terms[0], nil
	}
	return &expr{op: '&', terms: terms}, nil
}

func (p *filterParser) disjunction() (*expr, error) {
	t, err := p.term()
	if err != nil {
		return nil, err
	}
	terms := []expr{*t}
	for {
		save := p.i
		if p.skipSpace(); !p.keyword("OR") {
			p.i = save
			break
		}
		if p.atEnd() {
			return nil, p.errorf("OR needs a term after it")
		}
		t, err := p.term()
		if err != nil {
			return nil, err
		}
		terms = append(terms, *t)
	}
	if len(terms) == 1 {
		return &terms[0], nil
	}
	return &expr{op: '|', terms: terms}, nil
}

func (p *filterParser) term() (*expr, error) {
	p.skipSpace()
	if p.keyword("NOT") || p.consume("-") {
		if p.atEnd() {
			return nil, p.errorf("NOT needs a term after it")
		}
		t, err := p.term()
		if err != nil {
			return nil, err
		}
		return &expr{op: '!', terms: []expr{*t}}, nil
	}
	if !p.consume("(") {
		c, err := p.comparison()
		if err != nil {
			return nil, err
		}
		return &expr{cmp: c}, nil
	}
	e, err := p.conjunction()
	if err != nil {
		return nil, err
	}
	if p.skipSpace(); !p.consume(")") {
		return nil, p.errorf("unclosed parenthesis")
	}
	return e, nil
}

func (p *filterParser) comparison() (comparison, error) {
	field := p.until(" \t=!:<>()\"")
	label, isLabel := strings.CutPrefix(field, "labels.")
	if !slices.Contains(filterFields, field) && field != "labels" && (!isLabel || label == "") {
		return comparison{}, p.errorf("cannot filter on %q", field)
	}
	p.skipSpace()
	c := comparison{field: field}
	switch {
	case p.consume("!="):
		c.op = "!="
	case p.consume("="):
		c.op = "="
	case p.consume(":"):
		c.op = ":"
	default:
		return comparison{}, p.errorf("expected =, != or : after %q", field)
	}
	if field == "labels" && c.op != ":" {
		return comparison{}, p.errorf("labels takes only : with a key")
	}
	p.skipSpace()
	if p.consume(`"`) {
		c.value = p.until(`"\`)
		if !p.consume(`"`) {
			return comparison{}, p.errorf("unclosed quote or escape")
		}
	} else if c.value = p.until(" \t()\""); c.value == "" {
		return comparison{}, p.errorf("expected a value for %q", field)
	}
	switch {
	case c.op == ":" && field == "labels":
		if strings.Contains(c.value, "*") {
			return comparison{}, p.errorf("wildcards are not supported")
		}
	case c.op == ":" && c.value != "*":
		return comparison{}, p.errorf("only %s:* is supported", field)
	case c.op != ":" && strings.Contains(c.value, "*"):
		return comparison{}, p.errorf("wildcards are not supported")
	}
	return c, nil
}
