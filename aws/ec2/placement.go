package ec2

import (
	"maps"
	"slices"
)

var placementStrategies = []string{"cluster", "spread", "partition"}

// PlacementGroup is a placement group.
type PlacementGroup struct {
	// Region is where the group is. The server sets it.
	Region    string
	GroupID   string
	GroupName string
	// Strategy is "cluster", "spread" or "partition".
	Strategy       string
	PartitionCount int
	SpreadLevel    string
	Tags           map[string]string
}

func (pg *PlacementGroup) clone() PlacementGroup {
	c := *pg
	c.Tags = maps.Clone(pg.Tags)
	return c
}

func (pg *PlacementGroup) id() string {
	if pg == nil {
		return ""
	}
	return pg.GroupID
}

type placementGroupXML struct {
	GroupName      string   `xml:"groupName"`
	GroupID        string   `xml:"groupId"`
	State          string   `xml:"state"`
	Strategy       string   `xml:"strategy"`
	PartitionCount int      `xml:"partitionCount,omitempty"`
	SpreadLevel    string   `xml:"spreadLevel,omitempty"`
	Tags           []tagXML `xml:"tagSet>item,omitempty"`
}

func (pg *PlacementGroup) xml() placementGroupXML {
	return placementGroupXML{
		GroupName: pg.GroupName, GroupID: pg.GroupID, State: statusAvailable, Strategy: pg.Strategy,
		PartitionCount: pg.PartitionCount, SpreadLevel: pg.SpreadLevel, Tags: tagSet(pg.Tags),
	}
}

func placementGroupFields(pg *PlacementGroup) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "group-name":
			return one(pg.GroupName), true
		case "spread-level":
			return one(pg.SpreadLevel), true
		case "state":
			return []string{statusAvailable}, true
		case "strategy":
			return one(pg.Strategy), true
		}
		return tagFilter(pg.Tags, name)
	}
}

func placementGroupUnknown(name string) *apiError {
	return clientError("InvalidPlacementGroup.Unknown", "The Placement Group '%s' is unknown.", name)
}

// placementGroup returns the group named name, or else with id. The caller holds s.mu.
func (rs *regionState) placementGroup(name, id string) (*PlacementGroup, *apiError) {
	if name != "" {
		if pg, ok := rs.placementGroups[name]; ok {
			return pg, nil
		}
		return nil, placementGroupUnknown(name)
	}
	for _, pg := range rs.placementGroups {
		if pg.GroupID == id {
			return pg, nil
		}
	}
	return nil, placementGroupUnknown(id)
}

type createPlacementGroupResponse struct {
	meta
	PlacementGroup placementGroupXML `xml:"placementGroup"`
}

func (s *Server) createPlacementGroup(c *call) (response, *apiError) {
	name, err := c.p.required("GroupName")
	if err != nil {
		return nil, err
	}
	strategy, err := c.p.required("Strategy")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(placementStrategies, strategy) {
		return nil, invalidValue("Invalid value '%s' for strategy.", strategy)
	}
	partitions, err := c.p.int("PartitionCount")
	if err != nil {
		return nil, err
	}
	if partitions != 0 && (strategy != "partition" || partitions < 1 || partitions > 7) {
		return nil, invalidValue("Invalid value '%d' for partitionCount.", partitions)
	}
	spread := c.p.get("SpreadLevel")
	switch {
	case spread != "" && (strategy != "spread" || spread != "host" && spread != "rack"):
		return nil, invalidValue("Invalid value '%s' for spreadLevel.", spread)
	case spread == "" && strategy == "spread":
		spread = "rack"
	}
	tags, err := c.p.tags("placement-group")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.region(c.region)
	if _, ok := rs.placementGroups[name]; ok {
		return nil, clientError("InvalidPlacementGroup.Duplicate", "The Placement Group '%s' already exists.", name)
	}
	pg := &PlacementGroup{
		Region: c.region, GroupID: s.newID("pg"), GroupName: name, Strategy: strategy,
		PartitionCount: partitions, SpreadLevel: spread, Tags: tags,
	}
	rs.placementGroups[name] = pg
	return &createPlacementGroupResponse{PlacementGroup: pg.xml()}, nil
}

type describePlacementGroupsResponse struct {
	meta
	PlacementGroups []placementGroupXML `xml:"placementGroupSet>item"`
}

func (s *Server) describePlacementGroups(c *call) (response, *apiError) {
	names, ids := c.p.list("GroupName"), c.p.list("GroupId")
	filters, err := c.p.filters(placementGroupFields(&PlacementGroup{}))
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	for _, name := range names {
		if _, err := rs.placementGroup(name, ""); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if _, err := rs.placementGroup("", id); err != nil {
			return nil, err
		}
	}
	resp := &describePlacementGroupsResponse{}
	for _, name := range slices.Sorted(maps.Keys(rs.placementGroups)) {
		pg := rs.placementGroups[name]
		if (len(names) == 0 && len(ids) == 0 || slices.Contains(names, name) || slices.Contains(ids, pg.GroupID)) &&
			matchFilters(filters, placementGroupFields(pg)) {
			resp.PlacementGroups = append(resp.PlacementGroups, pg.xml())
		}
	}
	return resp, nil
}

func (s *Server) deletePlacementGroup(c *call) (response, *apiError) {
	name, err := c.p.required("GroupName")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, err := rs.placementGroup(name, ""); err != nil {
		return nil, err
	}
	for _, inst := range rs.instances {
		if inst.PlacementGroup == name && inst.State != stateTerminated {
			return nil, clientError("InvalidPlacementGroup.InUse", "The placement group '%s' is in use and may not be deleted.", name)
		}
	}
	delete(rs.placementGroups, name)
	return &returnResponse{Return: true}, nil
}

// PlacementGroups returns every placement group the server holds, ordered
// by region and name.
func (s *Server) PlacementGroups() []PlacementGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PlacementGroup
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, name := range slices.Sorted(maps.Keys(rs.placementGroups)) {
			out = append(out, rs.placementGroups[name].clone())
		}
	}
	return out
}
