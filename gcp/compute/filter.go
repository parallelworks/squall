package compute

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// filterable is a resource a list filter can test.
type filterable interface {
	// filterValue is the value of field, such as "name" or "labels.owner",
	// or "" when the resource has none.
	filterValue(field string) string
}

// fieldSet names the fields a list can filter on.
type fieldSet struct {
	names  []string
	labels bool
}

func (fs fieldSet) has(field string) bool {
	if key, ok := strings.CutPrefix(field, "labels."); ok {
		return fs.labels && key != ""
	}
	return slices.Contains(fs.names, field)
}

// expr is a parsed list filter: a comparison, or all ('&') or any ('|') of
// its terms. A nil *expr matches everything.
type expr struct {
	op    byte
	terms []expr
	cmp   comparison
}

type comparison struct {
	field string
	value string
	equal bool
	// re is set for eq and ne, which match value as an RE2 expression against
	// the whole field.
	re *regexp.Regexp
}

func (c comparison) String() string {
	switch {
	case c.re != nil && c.equal:
		return c.field + " eq " + c.value
	case c.re != nil:
		return c.field + " ne " + c.value
	case c.equal:
		return c.field + " = " + c.value
	}
	return c.field + " != " + c.value
}

func (e *expr) String() string {
	if e == nil {
		return ""
	}
	if e.op == 0 {
		return e.cmp.String()
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

func (e *expr) matches(v filterable) bool {
	switch {
	case e == nil:
		return true
	case e.op == '&':
		for i := range e.terms {
			if !e.terms[i].matches(v) {
				return false
			}
		}
		return true
	case e.op == '|':
		for i := range e.terms {
			if e.terms[i].matches(v) {
				return true
			}
		}
		return false
	}
	got := v.filterValue(e.cmp.field)
	if e.cmp.re != nil {
		return e.cmp.re.MatchString(got) == e.cmp.equal
	}
	return (got == e.cmp.value) == e.cmp.equal
}

// comparisons calls f for every comparison in e.
func (e *expr) comparisons(f func(comparison)) {
	if e == nil {
		return
	}
	if e.op == 0 {
		f(e.cmp)
	}
	for i := range e.terms {
		e.terms[i].comparisons(f)
	}
}

var errUnsupportedFilter = errors.New("unsupported filter")

// parseFilter parses the subset of the Compute Engine filter syntax the server
// supports, for the fields in fields:
//
//   - Comparisons "field = value" and "field != value", with a value that has
//     spaces in double quotes, joined by AND, OR or juxtaposition (AND) and
//     grouped with parentheses. OR binds tighter than AND, as in the API's
//     filter grammar. A "*" in a value is rejected, since the API may read it
//     as a wildcard.
//   - Regular expressions "field eq re" and "field ne re", which must match
//     the whole field, quoted or not. As in the API, they are either one
//     unparenthesized comparison or several parenthesized ones, and they mix
//     with neither = nor AND and OR.
func parseFilter(s string, fields fieldSet) (*expr, error) {
	p := filterParser{s: s, fields: fields}
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
	if p.regexps > 0 && (p.plain > 0 || p.keywords > 0) {
		return nil, p.errorf("eq and ne cannot be combined with =, !=, AND or OR")
	}
	if p.regexps > 1 && p.bare > 0 {
		return nil, p.errorf("several eq and ne comparisons must each be in parentheses")
	}
	return e, nil
}

type filterParser struct {
	s      string
	i      int
	fields fieldSet
	depth  int

	plain, regexps, bare, keywords int
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
		if p.skipSpace(); p.i == len(p.s) || p.s[p.i] == ')' {
			break
		}
		if p.keyword("AND") {
			p.keywords++
			if p.skipSpace(); p.i == len(p.s) || p.s[p.i] == ')' {
				return nil, p.errorf("AND needs a comparison after it")
			}
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
		p.keywords++
		if p.skipSpace(); p.i == len(p.s) || p.s[p.i] == ')' {
			return nil, p.errorf("OR needs a comparison after it")
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
	if !p.consume("(") {
		c, err := p.comparison()
		if err != nil {
			return nil, err
		}
		return &expr{cmp: c}, nil
	}
	p.depth++
	e, err := p.conjunction()
	if err != nil {
		return nil, err
	}
	if p.skipSpace(); !p.consume(")") {
		return nil, p.errorf("unclosed parenthesis")
	}
	p.depth--
	return e, nil
}

func (p *filterParser) comparison() (comparison, error) {
	p.skipSpace()
	field := p.until(" \t=!()\"'")
	if !p.fields.has(field) {
		return comparison{}, p.errorf("cannot filter on %q", field)
	}
	p.skipSpace()
	c := comparison{field: field, equal: true}
	switch {
	case p.consume("!="):
		c.equal = false
	case p.consume("="):
	case p.keyword("eq"):
		return p.regexp(c)
	case p.keyword("ne"):
		c.equal = false
		return p.regexp(c)
	default:
		return comparison{}, p.errorf("expected =, !=, eq or ne after %q", field)
	}
	p.plain++
	p.skipSpace()
	if p.consume(`"`) {
		c.value = p.until(`"`)
		if !p.consume(`"`) {
			return comparison{}, p.errorf("unclosed quote")
		}
	} else if c.value = p.until(" \t()\"'"); c.value == "" {
		return comparison{}, p.errorf("expected a value for %q", field)
	}
	if strings.Contains(c.value, "*") {
		return comparison{}, p.errorf("wildcards are not supported")
	}
	return c, nil
}

// regexp parses the value of an eq or ne comparison.
func (p *filterParser) regexp(c comparison) (comparison, error) {
	p.regexps++
	p.skipSpace()
	switch {
	case p.consume(`"`):
		c.value = p.until(`"`)
		if !p.consume(`"`) {
			return comparison{}, p.errorf("unclosed quote")
		}
	case p.consume("'"):
		c.value = p.until("'")
		if !p.consume("'") {
			return comparison{}, p.errorf("unclosed quote")
		}
	case p.depth == 0:
		p.bare++
		c.value = p.until(" \t")
	default:
		c.value = p.balanced()
	}
	if c.value == "" {
		return comparison{}, p.errorf("expected a regular expression for %q", c.field)
	}
	re, err := regexp.Compile("^(?:" + c.value + ")$")
	if err != nil {
		return comparison{}, p.errorf("invalid regular expression %q: %v", c.value, err)
	}
	c.re = re
	return c, nil
}

// balanced returns the value up to the parenthesis that closes the enclosing
// group, so a regular expression may hold balanced parentheses of its own.
func (p *filterParser) balanced() string {
	start, depth := p.i, 0
	for ; p.i < len(p.s); p.i++ {
		switch p.s[p.i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return strings.TrimRight(p.s[start:p.i], " \t")
			}
			depth--
		}
	}
	return strings.TrimRight(p.s[start:], " \t")
}
