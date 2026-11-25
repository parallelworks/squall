package compute

import (
	"net/http"
	"slices"
	"strconv"
)

// AcceleratorType is an accelerator a zone offers, in the API's JSON shape.
type AcceleratorType struct {
	Kind                    string `json:"kind"`
	ID                      uint64 `json:"id,string"`
	CreationTimestamp       string `json:"creationTimestamp"`
	Name                    string `json:"name"`
	Description             string `json:"description"`
	Zone                    string `json:"zone"`
	SelfLink                string `json:"selfLink"`
	MaximumCardsPerInstance int    `json:"maximumCardsPerInstance"`
}

// defaultAccelerators are what a zone offers unless [WithAcceleratorTypes] says otherwise.
var defaultAccelerators = []string{"nvidia-l4", "nvidia-tesla-t4"}

var knownAccelerators = map[string]struct {
	description string
	maxCards    int
}{
	"nvidia-a100-80gb":  {"NVIDIA A100 80GB", 8},
	"nvidia-h100-80gb":  {"NVIDIA H100 80GB", 8},
	"nvidia-l4":         {"NVIDIA L4", 8},
	"nvidia-tesla-a100": {"NVIDIA A100 40GB", 16},
	"nvidia-tesla-p100": {"NVIDIA Tesla P100", 4},
	"nvidia-tesla-p4":   {"NVIDIA Tesla P4", 4},
	"nvidia-tesla-t4":   {"NVIDIA T4", 4},
	"nvidia-tesla-v100": {"NVIDIA V100", 8},
}

// WithAcceleratorTypes sets the accelerator types zone offers, such as
// "nvidia-tesla-t4", in place of the built-in ones, nvidia-l4 and
// nvidia-tesla-t4. With no names the zone offers none.
func WithAcceleratorTypes(zone string, names ...string) Option {
	return func(s *Server) { s.accelerators[zone] = slices.Clone(names) }
}

var acceleratorFields = fieldSet{names: []string{"name", "description", "id", "zone"}}

func (a *AcceleratorType) filterValue(field string) string {
	switch field {
	case "name":
		return a.Name
	case "description":
		return a.Description
	case "id":
		return strconv.FormatUint(a.ID, 10)
	case "zone":
		return a.Zone
	}
	return ""
}

// acceleratorTypes returns the accelerator types zone offers, in project.
func (s *Server) acceleratorTypes(project, zone string) *collection[*AcceleratorType] {
	names, ok := s.accelerators[zone]
	if !ok {
		names = defaultAccelerators
	}
	c := &collection[*AcceleratorType]{byName: map[string]*AcceleratorType{}}
	zoneLink := selfLinkBase + "projects/" + project + "/zones/" + zone
	for i, name := range names {
		known, ok := knownAccelerators[name]
		if !ok {
			known.description, known.maxCards = name, 8
		}
		c.put(name, &AcceleratorType{
			Kind:                    "compute#acceleratorType",
			ID:                      s.zoneIDs[zone]*1000 + uint64(i) + 1,
			CreationTimestamp:       "1969-12-31T16:00:00.000-08:00",
			Name:                    name,
			Description:             known.description,
			Zone:                    zoneLink,
			SelfLink:                zoneLink + "/acceleratorTypes/" + name,
			MaximumCardsPerInstance: known.maxCards,
		})
	}
	return c
}

func (s *Server) listAcceleratorTypes(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, acceleratorFields)
	if err != nil {
		return nil, err
	}
	project, zone := r.PathValue("project"), r.PathValue("zone")
	return listScope(s.acceleratorTypes(project, zone), q, "compute#acceleratorTypeList", scopeOf(r)+"/acceleratorTypes", nil), nil
}

func (s *Server) aggregatedAcceleratorTypes(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, acceleratorFields)
	if err != nil {
		return nil, err
	}
	project := r.PathValue("project")
	byZone := map[string]*collection[*AcceleratorType]{}
	for zone := range s.zoneRegion {
		byZone["projects/"+project+"/zones/"+zone] = s.acceleratorTypes(project, zone)
	}
	return listAggregated(byZone, project, q, "compute#acceleratorTypeAggregatedList", "acceleratorTypes", nil), nil
}

func (s *Server) getAcceleratorType(r *http.Request, _ *apiError) (any, *apiError) {
	project, zone, name := r.PathValue("project"), r.PathValue("zone"), r.PathValue("name")
	a, ok := s.acceleratorTypes(project, zone).byName[name]
	if !ok {
		return nil, notFound(scopeOf(r) + "/acceleratorTypes/" + name)
	}
	return a, nil
}
