package compute

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Zone is a zone, in the API's JSON shape.
type Zone struct {
	Kind              string `json:"kind"`
	ID                uint64 `json:"id,string"`
	CreationTimestamp string `json:"creationTimestamp"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	Region            string `json:"region"`
	SelfLink          string `json:"selfLink"`
	SupportsPzs       bool   `json:"supportsPzs"`
}

func defaultZones() map[string][]string {
	return map[string][]string{
		"asia-east1":   {"asia-east1-a", "asia-east1-b", "asia-east1-c"},
		"europe-west1": {"europe-west1-b", "europe-west1-c", "europe-west1-d"},
		"europe-west4": {"europe-west4-a", "europe-west4-b", "europe-west4-c"},
		"us-central1":  {"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
		"us-east1":     {"us-east1-b", "us-east1-c", "us-east1-d"},
		"us-east4":     {"us-east4-a", "us-east4-b", "us-east4-c"},
		"us-west1":     {"us-west1-a", "us-west1-b", "us-west1-c"},
	}
}

// indexZones maps each zone to its region and a stable ID, once the options have set them.
func (s *Server) indexZones() {
	s.zoneRegion, s.zoneIDs = map[string]string{}, map[string]uint64{}
	for _, region := range slices.Sorted(maps.Keys(s.regionZones)) {
		for _, zone := range s.regionZones[region] {
			s.zoneRegion[zone] = region
			s.zoneIDs[zone] = 2000 + uint64(len(s.zoneIDs))
		}
	}
}

// checkZone fails a call on a resource in a zone the server does not have.
func (s *Server) checkZone(r *http.Request) *apiError {
	zone := r.PathValue("zone")
	if zone == "" || strings.HasSuffix(r.URL.Path, "/zones/"+zone) {
		return nil
	}
	if _, ok := s.zoneRegion[zone]; !ok {
		return invalid("Invalid value for field 'zone': '%s'. Unknown zone.", zone)
	}
	return nil
}

// regionOfZone is the region path of r's zone, such as "projects/p/regions/us-east1".
func (s *Server) regionOfZone(r *http.Request) string {
	return "projects/" + r.PathValue("project") + "/regions/" + s.zoneRegion[r.PathValue("zone")]
}

var zoneFields = fieldSet{names: []string{"name", "description", "status", "id", "region"}}

func (z *Zone) filterValue(field string) string {
	switch field {
	case "name":
		return z.Name
	case "description":
		return z.Description
	case "status":
		return z.Status
	case "id":
		return strconv.FormatUint(z.ID, 10)
	case "region":
		return z.Region
	}
	return ""
}

func (s *Server) zone(project, name string) *Zone {
	return &Zone{
		Kind:              "compute#zone",
		ID:                s.zoneIDs[name],
		CreationTimestamp: "1969-12-31T16:00:00.000-08:00",
		Name:              name,
		Description:       name,
		Status:            "UP",
		Region:            selfLinkBase + "projects/" + project + "/regions/" + s.zoneRegion[name],
		SelfLink:          selfLinkBase + "projects/" + project + "/zones/" + name,
	}
}

func (s *Server) listZones(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, zoneFields)
	if err != nil {
		return nil, err
	}
	project := r.PathValue("project")
	zones := &collection[*Zone]{byName: map[string]*Zone{}}
	for zone := range s.zoneRegion {
		zones.put(zone, s.zone(project, zone))
	}
	return listScope(zones, q, "compute#zoneList", "projects/"+project+"/zones", nil), nil
}

func (s *Server) getZone(r *http.Request, _ *apiError) (any, *apiError) {
	project, name := r.PathValue("project"), r.PathValue("zone")
	if _, ok := s.zoneRegion[name]; !ok {
		return nil, notFound("projects/" + project + "/zones/" + name)
	}
	return s.zone(project, name), nil
}
