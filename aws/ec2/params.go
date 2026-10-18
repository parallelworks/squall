package ec2

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxTags           = 50
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// params reads a call's query parameters and remembers which it read, so a
// parameter the server does not understand is rejected rather than ignored.
type params struct {
	values url.Values
	used   map[string]bool
}

func newParams(values url.Values) *params {
	p := &params{values: values, used: map[string]bool{"Action": true, "Version": true, "DryRun": true}}
	for k := range values {
		if strings.HasPrefix(k, "X-Amz-") {
			p.used[k] = true
		}
	}
	return p
}

func (p *params) get(key string) string {
	p.used[key] = true
	return p.values.Get(key)
}

func (p *params) required(key string) (string, *apiError) {
	v := p.get(key)
	if v == "" {
		return "", missing(key)
	}
	return v, nil
}

// bool reads key, or returns absent when the call leaves it out.
func (p *params) bool(key string, absent bool) (bool, *apiError) {
	switch v := p.get(key); v {
	case "":
		return absent, nil
	case "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, invalidValue("Value (%s) for parameter %s is invalid. Expected a boolean.", v, key)
	}
}

// enum reads key, which must be one of allowed, or returns absent when the
// call leaves it out.
func (p *params) enum(key, absent string, allowed ...string) (string, *apiError) {
	v := p.get(key)
	switch {
	case v == "":
		return absent, nil
	case !slices.Contains(allowed, v):
		return "", invalidValue("Value (%s) for parameter %s is invalid. Valid values are: %s.", v, key, strings.Join(allowed, ", "))
	}
	return v, nil
}

// indexes returns, in order, the N of every parameter named prefix.N or
// prefix.N.something.
func (p *params) indexes(prefix string) []string {
	seen := map[int]string{}
	for k := range p.values {
		rest, ok := strings.CutPrefix(k, prefix+".")
		if !ok {
			continue
		}
		n, _, _ := strings.Cut(rest, ".")
		if i, err := strconv.Atoi(n); err == nil {
			seen[i] = n
		}
	}
	out := make([]string, 0, len(seen))
	for _, i := range slices.Sorted(maps.Keys(seen)) {
		out = append(out, seen[i])
	}
	return out
}

// list returns the values of prefix.1, prefix.2 and so on, in order.
func (p *params) list(prefix string) []string {
	var out []string
	for _, n := range p.indexes(prefix) {
		out = append(out, p.get(prefix+"."+n))
	}
	return out
}

// done rejects a parameter nothing read, then a dry run. Handlers call it
// once they have read their parameters and before they look at any state.
func (p *params) done() *apiError {
	for _, k := range slices.Sorted(maps.Keys(p.values)) {
		if !p.used[k] {
			return clientError("UnknownParameter", "The parameter %s is not recognized", k)
		}
	}
	dry, err := p.bool("DryRun", false)
	if err != nil {
		return err
	}
	if dry {
		return &apiError{http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set."}
	}
	return nil
}

// fingerprint identifies a call's parameters apart from its client token,
// so a repeated token can be checked against the call that first used it.
func (p *params) fingerprint() string {
	v := maps.Clone(p.values)
	for k := range v {
		if k == "ClientToken" || k == "DryRun" || strings.HasPrefix(k, "X-Amz-") {
			delete(v, k)
		}
		// Sending a parameter's documented default is the same request as leaving it out.
		if strings.HasSuffix(k, ".NetworkCardIndex") && v.Get(k) == "0" {
			delete(v, k)
		}
	}
	return v.Encode()
}

// tags reads TagSpecification.N for resourceType.
func (p *params) tags(resourceType string) (map[string]string, *apiError) {
	specs, err := p.tagSpecs(resourceType)
	return specs[resourceType], err
}

// tagSpecs reads TagSpecification.N, keyed by resource type, for the resource
// types a call can tag.
func (p *params) tagSpecs(resourceTypes ...string) (map[string]map[string]string, *apiError) {
	out := map[string]map[string]string{}
	for _, n := range p.indexes("TagSpecification") {
		spec := "TagSpecification." + n
		t := p.get(spec + ".ResourceType")
		if !slices.Contains(resourceTypes, t) {
			return nil, invalidValue("'%s' is not a valid taggable resource type for this operation.", t)
		}
		tags := out[t]
		for _, m := range p.indexes(spec + ".Tag") {
			key, value := p.get(spec+".Tag."+m+".Key"), p.get(spec+".Tag."+m+".Value")
			if _, dup := tags[key]; dup {
				return nil, invalidValue("Duplicate tag key '%s' specified.", key)
			}
			if tags == nil {
				tags = map[string]string{}
			}
			tags[key] = value
		}
		if err := validateTags(tags); err != nil {
			return nil, err
		}
		out[t] = tags
	}
	return out, nil
}

// int reads key as an integer, or returns 0 when the call leaves it out.
func (p *params) int(key string) (int, *apiError) {
	v := p.get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, invalidValue("Value (%s) for parameter %s is invalid. Expected an integer.", v, key)
	}
	return n, nil
}

// page reads MaxResults, up to most, and NextToken, which is an ID with
// prefix when prefix is set. A call that names IDs in idsParam cannot page.
func (p *params) page(idsParam string, named bool, most int, prefix string) (limit int, after string, err *apiError) {
	if v := p.get("MaxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < minResults || n > most {
			return 0, "", invalidValue("Value ( %s ) for parameter maxResults is invalid. Expecting a value between %d and %d.", v, minResults, most)
		}
		if named {
			return 0, "", clientError("InvalidParameterCombination", "The parameter %s cannot be used with the parameter MaxResults", idsParam)
		}
		limit = n
	}
	after = p.get("NextToken")
	if after != "" && prefix != "" && !strings.HasPrefix(after, prefix+"-") {
		return 0, "", clientError("InvalidPaginationToken", "Invalid pagination token %s", after)
	}
	return limit, after, nil
}

// paginate returns the items, ordered by key, whose key sorts after after, up
// to limit when it is set, and the key to continue after when more remain.
func paginate[T any](items []T, key func(T) string, after string, limit int) ([]T, string) {
	var out []T
	for _, item := range items {
		if key(item) <= after {
			continue
		}
		if limit > 0 && len(out) == limit {
			return out, key(out[limit-1])
		}
		out = append(out, item)
	}
	return out, ""
}

func validateTags(tags map[string]string) *apiError {
	if len(tags) > maxTags {
		return clientError("TagLimitExceeded", "The maximum number of tags (%d) has been exceeded.", maxTags)
	}
	for k, v := range tags {
		switch {
		case k == "" || utf8.RuneCountInString(k) > maxTagKeyLength:
			return invalidValue("Tag key '%s' must be between 1 and %d characters.", k, maxTagKeyLength)
		case strings.HasPrefix(strings.ToLower(k), "aws:"):
			return invalidValue("Tag keys starting with 'aws:' are reserved for internal use.")
		case utf8.RuneCountInString(v) > maxTagValueLength:
			return invalidValue("Tag value for key '%s' must be at most %d characters.", k, maxTagValueLength)
		}
	}
	return nil
}

// filter is one Filter.N: a resource matches when any of its values for name
// matches any of the filter's values.
type filter struct {
	name   string
	values []string
}

// filters reads Filter.N, rejecting a filter name fields does not know.
// fields returns a resource's values for a filter name, and false for a name
// it does not support.
func (p *params) filters(fields func(name string) ([]string, bool)) ([]filter, *apiError) {
	var out []filter
	for _, n := range p.indexes("Filter") {
		f := filter{name: p.get("Filter." + n + ".Name"), values: p.list("Filter." + n + ".Value")}
		if _, ok := fields(f.name); !ok {
			return nil, invalidValue("The filter '%s' is invalid", f.name)
		}
		if len(f.values) == 0 {
			return nil, invalidValue("The filter '%s' requires at least one value", f.name)
		}
		out = append(out, f)
	}
	return out, nil
}

// matchFilters reports whether a resource whose filter values fields returns
// matches every filter.
func matchFilters(filters []filter, fields func(name string) ([]string, bool)) bool {
	for _, f := range filters {
		have, _ := fields(f.name)
		if !slices.ContainsFunc(have, func(v string) bool {
			return slices.ContainsFunc(f.values, func(pattern string) bool { return wildcard(pattern, v) })
		}) {
			return false
		}
	}
	return true
}

// tagFilter answers the filters on tags: tag:<key> and tag-key.
func tagFilter(tags map[string]string, name string) ([]string, bool) {
	if key, ok := strings.CutPrefix(name, "tag:"); ok {
		if v, ok := tags[key]; ok {
			return []string{v}, true
		}
		return nil, true
	}
	if name == "tag-key" {
		return slices.Sorted(maps.Keys(tags)), true
	}
	return nil, false
}

// wildcard matches s against an EC2 filter value, in which * is any run of
// characters, ? is any one character, and a backslash escapes either.
// A mismatch backtracks only to the last *, which bounds a match by
// len(pattern)*len(s) steps however many * the pattern has.
func wildcard(pattern, s string) bool {
	p, i := 0, 0
	star, retry := -1, 0
	for i < len(s) {
		if p < len(pattern) {
			c, size := utf8.DecodeRuneInString(pattern[p:])
			_, n := utf8.DecodeRuneInString(s[i:])
			switch c {
			case '*':
				p += size
				star, retry = p, i
				continue
			case '?':
				p, i = p+size, i+n
				continue
			case '\\':
				if next, m := utf8.DecodeRuneInString(pattern[p+size:]); m > 0 && (next == '*' || next == '?' || next == '\\') {
					c, size = next, size+m
				}
			}
			if got, _ := utf8.DecodeRuneInString(s[i:]); got == c {
				p, i = p+size, i+n
				continue
			}
		}
		if star < 0 {
			return false
		}
		_, n := utf8.DecodeRuneInString(s[retry:])
		retry += n
		p, i = star, retry
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
