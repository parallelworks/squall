package ec2

import (
	"maps"
	"strings"
)

// tagsOf returns where the resource with id keeps its tags. The caller holds s.mu.
func (rs *regionState) tagsOf(id string) (*map[string]string, *apiError) {
	prefix, _, _ := strings.Cut(id, "-")
	switch prefix {
	case "i":
		inst, err := rs.instance(id)
		if err != nil {
			return nil, err
		}
		return &inst.Tags, nil
	case "vol":
		vol, err := rs.volume(id)
		if err != nil {
			return nil, err
		}
		return &vol.Tags, nil
	case "ami":
		img, err := rs.image(id)
		if err != nil {
			return nil, err
		}
		return &img.Tags, nil
	case "pg":
		pg, err := rs.placementGroup("", id)
		if err != nil {
			return nil, err
		}
		return &pg.Tags, nil
	case "eni":
		ni, ok := rs.interfaces[id]
		if !ok {
			return nil, interfaceNotFound(id)
		}
		return &ni.Tags, nil
	case "eipalloc":
		a, ok := rs.addresses[id]
		if !ok {
			return nil, allocationNotFound(id)
		}
		return &a.Tags, nil
	}
	return nil, clientError("InvalidID", "The ID '%s' is not valid", id)
}

func (s *Server) createTags(c *call) (response, *apiError) {
	ids := c.p.list("ResourceId")
	if len(ids) == 0 {
		return nil, missing("ResourceId")
	}
	tags := map[string]string{}
	for _, n := range c.p.indexes("Tag") {
		tags[c.p.get("Tag."+n+".Key")] = c.p.get("Tag." + n + ".Value")
	}
	if len(tags) == 0 {
		return nil, missing("Tag")
	}
	if err := validateTags(tags); err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retag(c.region, ids, func(have map[string]string) map[string]string {
		out := maps.Clone(have)
		if out == nil {
			out = map[string]string{}
		}
		maps.Copy(out, tags)
		return out
	})
}

func (s *Server) deleteTags(c *call) (response, *apiError) {
	ids := c.p.list("ResourceId")
	if len(ids) == 0 {
		return nil, missing("ResourceId")
	}
	type match struct {
		value   string
		byValue bool
	}
	remove := map[string]match{}
	for _, n := range c.p.indexes("Tag") {
		key := "Tag." + n
		remove[c.p.get(key+".Key")] = match{c.p.get(key + ".Value"), c.p.values.Has(key + ".Value")}
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retag(c.region, ids, func(have map[string]string) map[string]string {
		if len(remove) == 0 {
			return nil
		}
		out := maps.Clone(have)
		for k, m := range remove {
			if v, ok := out[k]; ok && (!m.byValue || v == m.value) {
				delete(out, k)
			}
		}
		return out
	})
}

// retag replaces the tags of every resource with ids with what change makes
// of them, changing none unless all exist and stay within the tag limit. The
// caller holds s.mu.
func (s *Server) retag(region string, ids []string, change func(map[string]string) map[string]string) (response, *apiError) {
	rs := s.lookupRegion(region)
	targets := make([]*map[string]string, 0, len(ids))
	changed := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		tags, err := rs.tagsOf(id)
		if err != nil {
			return nil, err
		}
		next := change(*tags)
		if err := validateTags(next); err != nil {
			return nil, err
		}
		targets, changed = append(targets, tags), append(changed, next)
	}
	for i, tags := range targets {
		*tags = changed[i]
	}
	return &returnResponse{Return: true}, nil
}
