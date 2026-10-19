package compute

import (
	"encoding/json"
	"maps"
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
	Kind          string            `json:"kind"`
	ID            string            `json:"id"`
	Items         []json.RawMessage `json:"items,omitzero"`
	SelfLink      string            `json:"selfLink"`
	NextPageToken string            `json:"nextPageToken,omitzero"`
}

// aggregatedList is an aggregatedList response: items grouped by scope, such
// as "zones/us-east1-b", under a key naming the resource, such as "instances".
type aggregatedList struct {
	Kind          string                                  `json:"kind"`
	ID            string                                  `json:"id"`
	Items         map[string]map[string][]json.RawMessage `json:"items,omitzero"`
	SelfLink      string                                  `json:"selfLink"`
	NextPageToken string                                  `json:"nextPageToken,omitzero"`
}

// listScope answers a list of c, the collection at path. seen, when not nil,
// is called for each resource the page scans, before the filter applies. The
// caller holds s.mu.
func listScope[T filterable](c *collection[T], q listQuery, kind, path string, seen func(T)) *resourceList {
	list := &resourceList{Kind: kind, ID: path, SelfLink: selfLinkBase + path}
	if c == nil {
		return list
	}
	last := ""
	for _, name := range c.names[afterName(c.names, q.after):] {
		v := c.byName[name]
		if seen != nil {
			seen(v)
		}
		if !q.filter.matches(v) {
			continue
		}
		if len(list.Items) == q.limit {
			list.NextPageToken = last
			break
		}
		list.Items = append(list.Items, encoded(v))
		last = name
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
	n, last := 0, ""
	for _, scope := range slices.Sorted(maps.Keys(m)) {
		if !strings.HasPrefix(scope, prefix) || scope < afterScope {
			continue
		}
		c, start := m[scope], 0
		if scope == afterScope {
			start = afterName(c.names, afterItem)
		}
		short := strings.TrimPrefix(scope, prefix)
		for _, name := range c.names[start:] {
			v := c.byName[name]
			if seen != nil {
				seen(v)
			}
			if !q.filter.matches(v) {
				continue
			}
			if n == q.limit {
				list.NextPageToken = last
				return list
			}
			if list.Items == nil {
				list.Items = map[string]map[string][]json.RawMessage{}
			}
			if list.Items[short] == nil {
				list.Items[short] = map[string][]json.RawMessage{}
			}
			list.Items[short][key] = append(list.Items[short][key], encoded(v))
			n, last = n+1, short+"/"+name
		}
	}
	return list
}
