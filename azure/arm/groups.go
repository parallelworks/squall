package arm

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const groupType = "Microsoft.Resources/resourceGroups"

func (g *group) render() map[string]any {
	m := map[string]any{
		"id": g.id, "name": g.name, "type": groupType, "location": g.location,
		"properties": map[string]any{"provisioningState": g.state},
	}
	if g.tags != nil {
		m["tags"] = g.tags
	}
	if g.managedBy != "" {
		m["managedBy"] = g.managedBy
	}
	return m
}

func (s *Server) getGroup(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupKey(c)]
	if !ok {
		return nil, groupNotFound(c.val("rg"))
	}
	s.observe(g.pending)
	if g, ok = s.groups[groupKey(c)]; !ok {
		return nil, groupNotFound(c.val("rg"))
	}
	return ok200(g.render()), nil
}

func ok200(v any) *reply { return ok(encode(v)) }

// checkGroup answers a HEAD: 204 when the group exists, 404 when not.
func (s *Server) checkGroup(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.groups[groupKey(c)]; ok {
		s.observe(g.pending)
	}
	if _, ok := s.groups[groupKey(c)]; ok {
		return &reply{status: http.StatusNoContent}, nil
	}
	return &reply{status: http.StatusNotFound}, nil
}

func (s *Server) putGroup(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "ResourceGroup", "", "id", "name", "type", "location", "managedBy", "tags", "properties"); err != nil {
		return nil, err
	}
	props, err := object(body, "properties")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(props, "ResourceGroupProperties", "properties", "provisioningState"); err != nil {
		return nil, err
	}
	location := normalizeLocation(str(body, "location"))
	if location == "" {
		return nil, locationRequired()
	}
	tags, err := tagsOf(body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, exists := s.groups[groupKey(c)]
	status := http.StatusOK
	switch {
	case exists && g.state == stateDeleting:
		return nil, groupBeingDeleted(g.name)
	case exists && g.location != location:
		return nil, newError(http.StatusConflict, "InvalidResourceGroupLocation",
			"Invalid resource group location '%s'. The Resource group already exists in location '%s'.", location, g.location)
	case !exists:
		g = &group{id: groupID(c), name: c.val("rg"), sub: c.val("sub"), location: location, state: stateSucceeded}
		s.groups[groupKey(c)] = g
		status = http.StatusCreated
	}
	g.tags, g.managedBy = tags, str(body, "managedBy")
	return &reply{status: status, body: encode(g.render())}, nil
}

func (s *Server) deleteGroup(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gk := groupKey(c)
	g, ok := s.groups[gk]
	if !ok {
		return nil, groupNotFound(c.val("rg"))
	}
	if g.pending != nil {
		return accepted(c, g.pending, http.StatusOK, http.StatusAccepted, true, noBody), nil
	}
	for _, res := range s.resources {
		if res.inGroup(gk) {
			s.finish(res.pending)
		}
	}
	op := s.begin(c, groupOps(c), http.StatusOK, func() {
		for _, res := range s.resources {
			if res.inGroup(gk) {
				s.remove(res)
			}
		}
		if s.groups[gk] == g {
			delete(s.groups, gk)
		}
	})
	if op.status == opInProgress {
		g.state, g.pending = stateDeleting, op
	}
	return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
}

func (s *Server) listGroups(c *call) (*reply, *apiError) {
	match, err := parseTagFilter(c.query("$filter"))
	if err != nil {
		return nil, err
	}
	top := -1
	if v := c.query("$top"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 {
			return nil, badRequest("The value '%s' of the $top query parameter is not valid.", v)
		}
		top = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := key(c.val("sub")) + "/"
	observe := s.observer()
	for k, g := range s.groups {
		if strings.HasPrefix(k, prefix) {
			observe(g.pending)
		}
	}
	var out []*group
	for k, g := range s.groups {
		if strings.HasPrefix(k, prefix) && match(g.tags) {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b *group) int { return cmp.Compare(key(a.name), key(b.name)) })
	if top >= 0 && len(out) > top {
		out = out[:top]
	}
	return ok(listBody(out, func(g *group) any { return g.render() })), nil
}

// parseTagFilter parses the resource group list filters ARM takes on tags,
// "tagName eq 'k'" and "tagName eq 'k' and tagValue eq 'v'". Tag names match
// without regard to case, values exactly.
func parseTagFilter(filter string) (func(map[string]string) bool, *apiError) {
	if strings.TrimSpace(filter) == "" {
		return func(map[string]string) bool { return true }, nil
	}
	name, rest, err := filterClause(filter, "tagName")
	if err != nil {
		return nil, err
	}
	value, hasValue := "", false
	if rest = strings.TrimSpace(rest); rest != "" {
		after, ok := cutWord(rest, "and")
		if !ok {
			return nil, unsupportedFilter(filter)
		}
		if value, rest, err = filterClause(after, "tagValue"); err != nil {
			return nil, err
		}
		if strings.TrimSpace(rest) != "" {
			return nil, unsupportedFilter(filter)
		}
		hasValue = true
	}
	return func(tags map[string]string) bool {
		for k, v := range tags {
			if strings.EqualFold(k, name) && (!hasValue || v == value) {
				return true
			}
		}
		return false
	}, nil
}

func unsupportedFilter(filter string) *apiError {
	return unsupported("the $filter '%s' is not supported; use tagName eq 'name', optionally followed by and tagValue eq 'value'", filter)
}

// filterClause parses "field eq 'quoted'" at the start of s, returning the
// unquoted value and what follows.
func filterClause(s, field string) (value, rest string, err *apiError) {
	after, ok := cutWord(s, field)
	if !ok {
		return "", "", unsupportedFilter(s)
	}
	if after, ok = cutWord(after, "eq"); !ok || !strings.HasPrefix(after, "'") {
		return "", "", unsupportedFilter(s)
	}
	var b strings.Builder
	for i := 1; i < len(after); i++ {
		if after[i] != '\'' {
			b.WriteByte(after[i])
			continue
		}
		if i+1 < len(after) && after[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), after[i+1:], nil
	}
	return "", "", badRequest("The $filter '%s' has an unterminated string.", s)
}

// cutWord cuts the keyword word, matched without regard to case, from the
// start of s, which must follow it with a space or a quote.
func cutWord(s, word string) (string, bool) {
	s = strings.TrimLeft(s, " ")
	if len(s) <= len(word) || !strings.EqualFold(s[:len(word)], word) || s[len(word)] != ' ' && s[len(word)] != '\'' {
		return "", false
	}
	return strings.TrimLeft(s[len(word):], " "), true
}

// ResourceGroup is a snapshot of a resource group.
type ResourceGroup struct {
	ID                string
	Name              string
	Location          string
	Tags              map[string]string
	ProvisioningState string
}

// ResourceGroups returns the resource groups the server holds, ordered by ID.
func (s *Server) ResourceGroups() []ResourceGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ResourceGroup, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, ResourceGroup{ID: g.id, Name: g.name, Location: g.location, Tags: maps.Clone(g.tags), ProvisioningState: g.state})
	}
	slices.SortFunc(out, func(a, b ResourceGroup) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
