package compute

import (
	"bytes"
	"encoding/json"
	"io"
	"iter"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// collection holds one scope's resources of a kind, with names kept sorted for listing.
type collection[T any] struct {
	names  []string
	byName map[string]T
}

func (c *collection[T]) put(name string, v T) {
	if _, ok := c.byName[name]; !ok {
		i, _ := slices.BinarySearch(c.names, name)
		c.names = slices.Insert(c.names, i, name)
	}
	c.byName[name] = v
}

func (c *collection[T]) has(name string) bool {
	_, ok := c.byName[name]
	return ok
}

func (c *collection[T]) remove(name string) {
	if _, ok := c.byName[name]; !ok {
		return
	}
	delete(c.byName, name)
	if i, found := slices.BinarySearch(c.names, name); found {
		c.names = slices.Delete(c.names, i, i+1)
	}
}

// scoped returns the collection of scope in m, creating it.
func scoped[T any](m map[string]*collection[T], scope string) *collection[T] {
	c, ok := m[scope]
	if !ok {
		c = &collection[T]{byName: map[string]T{}}
		m[scope] = c
	}
	return c
}

// lookupIn returns the named resource of scope in m.
func lookupIn[T any](m map[string]*collection[T], scope, name string) (T, bool) {
	c, ok := m[scope]
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := c.byName[name]
	return v, ok
}

// each calls f for every resource in m, ordered by scope and then name.
func each[T any](m map[string]*collection[T], f func(T)) {
	for _, scope := range slices.Sorted(maps.Keys(m)) {
		c := m[scope]
		for _, name := range c.names {
			f(c.byName[name])
		}
	}
}

// jsonClone deep-copies v through its JSON shape, dropping unexported state.
func jsonClone[T any](v *T) T {
	var out T
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		panic("squall: cloning a resource: " + err.Error())
	}
	return out
}

// encoded returns v's JSON, so a handler can answer with it after releasing s.mu.
func encoded(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("squall: encoding a resource: " + err.Error())
	}
	return b
}

// listQuery is a list call's filter, page size and page token.
type listQuery struct {
	filter *expr
	limit  int
	after  string
}

func parseListQuery(r *http.Request, fields fieldSet) (listQuery, *apiError) {
	q := r.URL.Query()
	f, err := parseFilter(q.Get("filter"), fields)
	if err != nil {
		return listQuery{}, invalid("Invalid value for field 'filter': %v", err)
	}
	if o := q.Get("orderBy"); o != "" && o != "name" {
		return listQuery{}, invalid("Invalid value for field 'orderBy': '%s'. Only ordering by name is supported.", o)
	}
	limit := maxListResults
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxListResults {
			return listQuery{}, invalid("Invalid value for field 'maxResults': '%s'. Must be between 0 and %d.", v, maxListResults)
		}
		if n > 0 {
			limit = n
		}
	}
	return listQuery{filter: f, limit: limit, after: q.Get("pageToken")}, nil
}

// resourceList is a list response in the API's JSON shape.
type resourceList struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	SelfLink      string `json:"selfLink"`
	NextPageToken string `json:"nextPageToken,omitzero"`
	// items is the JSON of the page's items, encoded under s.mu, or nil.
	items []byte
}

// WriteTo writes l's JSON, with the items it already holds as JSON.
func (l *resourceList) WriteTo(w io.Writer) (int64, error) {
	type head resourceList
	return writeWithItems(w, (*head)(l), l.items)
}

// aggregatedList is an aggregatedList response: items grouped by scope, such
// as "zones/us-east1-b", under a key naming the resource, such as "instances".
type aggregatedList struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	SelfLink      string `json:"selfLink"`
	NextPageToken string `json:"nextPageToken,omitzero"`
	// items is the JSON of the page's items object, encoded under s.mu, or nil.
	items []byte
}

// WriteTo writes l's JSON, with the items it already holds as JSON.
func (l *aggregatedList) WriteTo(w io.Writer) (int64, error) {
	type head aggregatedList
	return writeWithItems(w, (*head)(l), l.items)
}

// writeWithItems writes head's JSON with an "items" member holding items,
// which is already JSON, without copying the items.
func writeWithItems(w io.Writer, head any, items []byte) (int64, error) {
	b, err := json.Marshal(head)
	if err != nil {
		return 0, err
	}
	parts := net.Buffers{b}
	if items != nil {
		parts = net.Buffers{b[:len(b)-1], []byte(`,"items":`), items, []byte("}")}
	}
	return parts.WriteTo(w)
}

// itemWriter encodes a list's items into one buffer, so a list costs no
// allocation per item.
type itemWriter struct {
	buf bytes.Buffer
	enc *json.Encoder
}

// add appends v's JSON.
func (w *itemWriter) add(v any) {
	if w.enc == nil {
		w.enc = json.NewEncoder(&w.buf)
	}
	if err := w.enc.Encode(v); err != nil {
		panic("squall: encoding a resource: " + err.Error())
	}
	w.buf.Truncate(w.buf.Len() - 1)
}

// scan yields the resources of c from index start on that filter matches.
// seen, when not nil, is called for each before the filter applies, and a
// resource it finishes deleting is skipped. The caller holds s.mu.
func scan[T filterable](c *collection[T], start int, filter *expr, seen func(T)) iter.Seq2[string, T] {
	return func(yield func(string, T) bool) {
		for i := start; i < len(c.names); i++ {
			name := c.names[i]
			v := c.byName[name]
			if seen != nil {
				if seen(v); !c.has(name) {
					i = afterName(c.names, name) - 1
					continue
				}
			}
			if filter.matches(v) && !yield(name, v) {
				return
			}
		}
	}
}

// listScope answers a list of c, the collection at path. seen, when not nil,
// is called for each resource the page scans, as scan calls it. The caller
// holds s.mu.
func listScope[T filterable](c *collection[T], q listQuery, kind, path string, seen func(T)) *resourceList {
	list := &resourceList{Kind: kind, ID: path, SelfLink: selfLinkBase + path}
	if c == nil {
		return list
	}
	var items itemWriter
	n, last := 0, ""
	for name, v := range scan(c, afterName(c.names, q.after), q.filter, seen) {
		if n == q.limit {
			list.NextPageToken = last
			break
		}
		if n == 0 {
			items.buf.WriteByte('[')
		} else {
			items.buf.WriteByte(',')
		}
		items.add(v)
		n, last = n+1, name
	}
	if n > 0 {
		items.buf.WriteByte(']')
		list.items = items.buf.Bytes()
	}
	return list
}

// afterName is the index of the first of the sorted names after token.
func afterName(names []string, token string) int {
	i, found := slices.BinarySearch(names, token)
	if found {
		i++
	}
	return i
}

// listAggregated answers an aggregatedList of the collections in m whose
// scope is in project, with each scope's items under key. A page token is the
// last scope and name returned, such as "zones/us-east1-b/vm-1". The caller
// holds s.mu.
func listAggregated[T filterable](m map[string]*collection[T], project string, q listQuery, kind, key string, seen func(T)) *aggregatedList {
	path := "projects/" + project + "/aggregated/" + key
	list := &aggregatedList{Kind: kind, ID: path, SelfLink: selfLinkBase + path}
	prefix := "projects/" + project + "/"
	afterScope, afterItem := "", ""
	if i := strings.LastIndexByte(q.after, '/'); i >= 0 {
		afterScope, afterItem = prefix+q.after[:i], q.after[i+1:]
	}
	var items itemWriter
	n, last := 0, ""
	for _, scope := range slices.Sorted(maps.Keys(m)) {
		if !strings.HasPrefix(scope, prefix) || scope < afterScope {
			continue
		}
		c, start := m[scope], 0
		if scope == afterScope {
			start = afterName(c.names, afterItem)
		}
		short, inScope := strings.TrimPrefix(scope, prefix), 0
		for name, v := range scan(c, start, q.filter, seen) {
			if n == q.limit {
				list.NextPageToken = last
				break
			}
			switch {
			case n == 0:
				items.buf.WriteByte('{')
			case inScope == 0:
				items.buf.WriteString("]},")
			default:
				items.buf.WriteByte(',')
			}
			if inScope == 0 {
				items.add(short)
				items.buf.WriteString(":{")
				items.add(key)
				items.buf.WriteString(":[")
			}
			items.add(v)
			n, last, inScope = n+1, short+"/"+name, inScope+1
		}
		if list.NextPageToken != "" {
			break
		}
	}
	if n > 0 {
		items.buf.WriteString("]}}")
		list.items = items.buf.Bytes()
	}
	return list
}
