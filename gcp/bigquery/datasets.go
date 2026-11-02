package bigquery

import (
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/parallelworks/squall/access"
)

const (
	maxDatasetID   = 1024
	maxLabels      = 64
	maxLabelLength = 63
	defaultList    = 50
)

// outputOnly are the dataset fields a client may send back but the server sets.
type outputOnly struct {
	Kind             string          `json:"kind"`
	Etag             string          `json:"etag"`
	ID               string          `json:"id"`
	SelfLink         string          `json:"selfLink"`
	CreationTime     json.RawMessage `json:"creationTime"`
	LastModifiedTime json.RawMessage `json:"lastModifiedTime"`
	Type             string          `json:"type"`
}

func validDatasetID(id string) bool {
	if id == "" || len(id) > maxDatasetID {
		return false
	}
	return !strings.ContainsFunc(id, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_'
	})
}

func invalidDatasetID(id string) *apiError {
	return invalid("Invalid dataset ID %q. Dataset IDs must be alphanumeric (plus underscores) and must be at most 1024 characters long.", id)
}

// validLabel reports whether s is a valid label key or, with key false, value.
func validLabel(s string, key bool) bool {
	if utf8.RuneCountInString(s) > maxLabelLength || key && s == "" {
		return false
	}
	for i, c := range s {
		letter := unicode.IsLetter(c) && !unicode.IsUpper(c)
		if !letter && (i == 0 && key || !unicode.IsDigit(c) && c != '_' && c != '-') {
			return false
		}
	}
	return true
}

func validateLabels(labels map[string]string) *apiError {
	if len(labels) > maxLabels {
		return invalid("Too many labels: %d, the limit is %d.", len(labels), maxLabels)
	}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if !validLabel(k, true) {
			return invalid("Invalid label key %q. Keys must be 1 to 63 lowercase letters, digits, underscores or dashes, starting with a letter.", k)
		}
		if !validLabel(labels[k], false) {
			return invalid("Invalid label value %q for key %q. Values must be at most 63 lowercase letters, digits, underscores or dashes.", labels[k], k)
		}
	}
	return nil
}

func (s *Server) insert(c *call) (any, *apiError) {
	var in struct {
		outputOnly
		DatasetReference *DatasetReference `json:"datasetReference"`
		FriendlyName     string            `json:"friendlyName"`
		Description      string            `json:"description"`
		Location         string            `json:"location"`
		Labels           map[string]string `json:"labels"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if in.DatasetReference == nil || in.DatasetReference.DatasetID == "" {
		return nil, invalid("Required parameter is missing: datasetReference.datasetId")
	}
	if p := in.DatasetReference.ProjectID; p != "" && p != c.project {
		return nil, invalid("Invalid value for datasetReference.projectId: %q does not match the project in the request path, %q.", p, c.project)
	}
	if in.Type != "" && in.Type != "DEFAULT" {
		return nil, invalid("squall: dataset type %q is not supported", in.Type)
	}
	id := in.DatasetReference.DatasetID
	if !validDatasetID(id) {
		return nil, invalidDatasetID(id)
	}
	if err := validateLabels(in.Labels); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := datasetKey{c.project, id}
	if _, ok := s.datasets[key]; ok {
		return nil, duplicate(c.project, id)
	}
	now := s.now()
	d := &dataset{
		Kind:             "bigquery#dataset",
		ID:               c.project + ":" + id,
		SelfLink:         apiBase + "projects/" + c.project + "/datasets/" + id,
		DatasetReference: DatasetReference{DatasetID: id, ProjectID: c.project},
		FriendlyName:     in.FriendlyName,
		Description:      in.Description,
		Labels:           maps.Clone(in.Labels),
		CreationTime:     now.UnixMilli(),
		Location:         cmp.Or(in.Location, "US"),
		Type:             "DEFAULT",
		tables:           map[string]bool{},
	}
	d.touch(now)
	s.datasets[key] = d
	return d.clone(), nil
}

// lookup finds the call's dataset. The caller holds s.mu.
func (s *Server) lookup(c *call) (*dataset, *apiError) {
	if !validDatasetID(c.dataset) {
		return nil, invalidDatasetID(c.dataset)
	}
	d, ok := s.datasets[datasetKey{c.project, c.dataset}]
	if !ok {
		return nil, notFound(c.project, c.dataset)
	}
	return d, nil
}

func (s *Server) get(c *call) (any, *apiError) {
	if v := c.URL.Query().Get("datasetView"); v != "" && !slices.Contains([]string{"DATASET_VIEW_UNSPECIFIED", "METADATA", "ACL", "FULL"}, v) {
		return nil, invalid("Invalid value for datasetView: %q", v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	return d.clone(), nil
}

func (s *Server) patch(c *call) (any, *apiError) {
	var in struct {
		outputOnly
		DatasetReference *DatasetReference  `json:"datasetReference"`
		FriendlyName     *string            `json:"friendlyName"`
		Description      *string            `json:"description"`
		Labels           map[string]*string `json:"labels"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if ref := in.DatasetReference; ref != nil && (ref.DatasetID != "" && ref.DatasetID != c.dataset || ref.ProjectID != "" && ref.ProjectID != c.project) {
		return nil, invalid("Invalid value for datasetReference: it does not match the dataset in the request path.")
	}
	if m := c.URL.Query().Get("updateMode"); m != "" && !slices.Contains([]string{"UPDATE_MODE_UNSPECIFIED", "UPDATE_METADATA", "UPDATE_ACL", "UPDATE_FULL"}, m) {
		return nil, invalid("Invalid value for updateMode: %q", m)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if etag := c.Header.Get("If-Match"); etag != "" && etag != d.Etag {
		return nil, preconditionFailed()
	}
	labels := maps.Clone(d.Labels)
	for k, v := range in.Labels {
		if v == nil {
			delete(labels, k)
			continue
		}
		if labels == nil {
			labels = map[string]string{}
		}
		labels[k] = *v
	}
	if err := validateLabels(labels); err != nil {
		return nil, err
	}
	if len(labels) == 0 {
		labels = nil
	}
	d.Labels = labels
	if in.FriendlyName != nil {
		d.FriendlyName = *in.FriendlyName
	}
	if in.Description != nil {
		d.Description = *in.Description
	}
	d.touch(s.now())
	return d.clone(), nil
}

func (s *Server) delete(c *call) (any, *apiError) {
	contents := false
	if v := c.URL.Query().Get("deleteContents"); v != "" {
		var err error
		if contents, err = strconv.ParseBool(v); err != nil || v != "true" && v != "false" {
			return nil, invalid("Invalid value for deleteContents: %q", v)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.lookup(c)
	if err != nil {
		return nil, err
	}
	if len(d.tables) > 0 {
		if !contents {
			return nil, inUse(c.project, c.dataset)
		}
		if missing := s.policy.Check(access.Call{Method: methodDelete, Resource: c.resource(), Needs: []string{"bigquery.tables.delete"}}); missing != nil {
			return nil, c.denied(missing[0])
		}
	}
	delete(s.datasets, datasetKey{c.project, c.dataset})
	return nil, nil
}

func (s *Server) list(c *call) (any, *apiError) {
	q := c.URL.Query()
	if q.Get("filter") != "" {
		return nil, invalid("squall: dataset list filters are not supported")
	}
	all := false
	if v := q.Get("all"); v != "" {
		var err error
		if all, err = strconv.ParseBool(v); err != nil || v != "true" && v != "false" {
			return nil, invalid("Invalid value for all: %q", v)
		}
	}
	limit := defaultList
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalid("Invalid value for maxResults: %q", v)
		}
		if n > 0 {
			limit = n
		}
	}
	token := q.Get("pageToken")
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for k := range s.datasets {
		if k.project == c.project && (all || !strings.HasPrefix(k.id, "_")) {
			ids = append(ids, k.id)
		}
	}
	slices.Sort(ids)
	start := 0
	if token != "" {
		if !validDatasetID(token) {
			return nil, invalid("Invalid page token %q.", token)
		}
		start, _ = slices.BinarySearch(ids, token)
	}
	type entry struct {
		Kind             string            `json:"kind"`
		ID               string            `json:"id"`
		DatasetReference DatasetReference  `json:"datasetReference"`
		FriendlyName     string            `json:"friendlyName,omitzero"`
		Labels           map[string]string `json:"labels,omitzero"`
		Location         string            `json:"location"`
	}
	out := struct {
		Kind          string  `json:"kind"`
		Etag          string  `json:"etag"`
		Datasets      []entry `json:"datasets,omitzero"`
		NextPageToken string  `json:"nextPageToken,omitzero"`
	}{Kind: "bigquery#datasetList"}
	end := min(start+limit, len(ids))
	h := fnv.New64a()
	for _, id := range ids[start:end] {
		d := s.datasets[datasetKey{c.project, id}]
		out.Datasets = append(out.Datasets, entry{d.Kind, d.ID, d.DatasetReference, d.FriendlyName, maps.Clone(d.Labels), d.Location})
		h.Write([]byte(d.Etag))
	}
	out.Etag = fmt.Sprintf("%016x", h.Sum64())
	if end < len(ids) {
		out.NextPageToken = ids[end]
	}
	return out, nil
}
