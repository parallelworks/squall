package compute

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"
)

const (
	addressExternal = "EXTERNAL"
	addressInternal = "INTERNAL"

	maxListResults = 500
	maxLabels      = 64
	maxLabelLength = 63
)

// Address is a regional address, in the API's JSON shape.
type Address struct {
	Kind              string            `json:"kind"`
	ID                uint64            `json:"id,string"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitzero"`
	Address           string            `json:"address,omitzero"`
	AddressType       string            `json:"addressType"`
	Purpose           string            `json:"purpose,omitzero"`
	NetworkTier       string            `json:"networkTier,omitzero"`
	IPVersion         string            `json:"ipVersion,omitzero"`
	Subnetwork        string            `json:"subnetwork,omitzero"`
	Network           string            `json:"network,omitzero"`
	Status            string            `json:"status"`
	Region            string            `json:"region"`
	SelfLink          string            `json:"selfLink"`
	Labels            map[string]string `json:"labels,omitzero"`
	LabelFingerprint  string            `json:"labelFingerprint"`
}

func (a *Address) clone() Address {
	c := *a
	c.Labels = maps.Clone(a.Labels)
	return c
}

func addressPath(region, name string) string { return region + "/addresses/" + name }

// addressSet holds one region's addresses, with names kept sorted for listing
// and an index of the IP addresses in use.
type addressSet struct {
	names  []string
	byName map[string]*Address
	byIP   map[string]string
}

func (set *addressSet) put(a *Address) {
	if old, ok := set.byName[a.Name]; ok {
		delete(set.byIP, old.Address)
	} else {
		i, _ := slices.BinarySearch(set.names, a.Name)
		set.names = slices.Insert(set.names, i, a.Name)
	}
	set.byName[a.Name] = a
	set.byIP[a.Address] = a.Name
}

func (set *addressSet) remove(name string) {
	a, ok := set.byName[name]
	if !ok {
		return
	}
	delete(set.byName, name)
	delete(set.byIP, a.Address)
	if i, found := slices.BinarySearch(set.names, name); found {
		set.names = slices.Delete(set.names, i, i+1)
	}
}

// region returns the address set of region, creating it. The caller holds s.mu.
func (s *Server) region(region string) *addressSet {
	set, ok := s.addresses[region]
	if !ok {
		set = &addressSet{byName: map[string]*Address{}, byIP: map[string]string{}}
		s.addresses[region] = set
	}
	return set
}

// lookup returns the named address in region. The caller holds s.mu.
func (s *Server) lookup(region, name string) (*Address, bool) {
	set, ok := s.addresses[region]
	if !ok {
		return nil, false
	}
	a, ok := set.byName[name]
	return a, ok
}

// validName reports whether name is a valid resource name: RFC 1035, 1 to 63
// characters, lowercase letters, digits and hyphens.
func validName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range []byte(name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validLabel reports whether s is a valid label key or, with key false, value:
// up to 63 lowercase letters, digits, underscores and dashes, international
// letters included, and a key starts with a lowercase letter.
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

// validate checks the fields of an address a caller sets.
func validate(a *Address) *apiError {
	if !validName(a.Name) {
		return invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", a.Name)
	}
	if t := a.AddressType; t != "" && t != addressExternal && t != addressInternal {
		return invalid("Invalid value for field 'resource.addressType': '%s'.", t)
	}
	if a.Address != "" {
		if ip, err := netip.ParseAddr(a.Address); err != nil || ip.Zone() != "" {
			return invalid("Invalid value for field 'resource.address': '%s'. Invalid IP address.", a.Address)
		}
	}
	return validateLabels("resource.labels", a.Labels)
}

func validateLabels(field string, labels map[string]string) *apiError {
	if len(labels) > maxLabels {
		return invalid("Invalid value for field '%s': %d labels, more than %d.", field, len(labels), maxLabels)
	}
	for k, v := range labels {
		if !validLabel(k, true) || !validLabel(v, false) {
			return invalid("Invalid value for field '%s': label '%s: %s' violates format constraints.", field, k, v)
		}
	}
	return nil
}

// fingerprint hashes labels, so it changes whenever they do.
func fingerprint(labels map[string]string) string {
	h := fnv.New64a()
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(h, "%s\x00%s\x00", k, labels[k])
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// store fills in what a leaves empty and stores it in region. The caller
// holds s.mu and has validated a.
func (s *Server) store(region string, a Address) (*Address, *apiError) {
	set := s.region(region)
	a.Kind = "compute#address"
	if a.ID == 0 {
		a.ID = s.newID()
	}
	a.CreationTimestamp = cmp.Or(a.CreationTimestamp, timestamp(s.now()))
	a.AddressType = cmp.Or(a.AddressType, addressExternal)
	if a.AddressType == addressExternal {
		a.NetworkTier = cmp.Or(a.NetworkTier, "PREMIUM")
	}
	a.Status = cmp.Or(a.Status, "RESERVED")
	a.Region = selfLinkBase + region
	a.SelfLink = selfLinkBase + addressPath(region, a.Name)
	a.LabelFingerprint = fingerprint(a.Labels)
	if a.Address == "" {
		a.Address = s.allocate(set, a.AddressType)
	} else {
		a.Address = netip.MustParseAddr(a.Address).String()
		if holder, taken := set.byIP[a.Address]; taken && holder != a.Name {
			return nil, invalid("IP address '%s' is already being used by another resource.", a.Address)
		}
	}
	stored := a.clone()
	set.put(&stored)
	return &stored, nil
}

// allocate returns the next free address of addressType in set. The caller holds s.mu.
func (s *Server) allocate(set *addressSet, addressType string) string {
	ip := s.nextIP[addressType]
	for {
		if _, taken := set.byIP[ip.String()]; !taken {
			break
		}
		ip = ip.Next()
	}
	s.nextIP[addressType] = ip.Next()
	return ip.String()
}

func (s *Server) insertAddress(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Address
	if err := decodeBody(r, &want); err != nil {
		return nil, err
	}
	if err := validate(&want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region := regionPath(r)
	path := addressPath(region, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return op, nil
	}
	if _, ok := s.lookup(region, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return s.finishOperation(reqID, region, "insert", path, 0, opFault), nil
	}
	want.ID, want.CreationTimestamp, want.Status = 0, "", ""
	a, err := s.store(region, want)
	if err != nil {
		return nil, err
	}
	return s.finishOperation(reqID, region, "insert", path, a.ID, nil), nil
}

func (s *Server) getAddress(r *http.Request, _ *apiError) (any, *apiError) {
	region, name := regionPath(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.lookup(region, name)
	if !ok {
		return nil, notFound(addressPath(region, name))
	}
	c := a.clone()
	return &c, nil
}

func (s *Server) deleteAddress(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := addressPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return op, nil
	}
	a, ok := s.lookup(region, name)
	if !ok {
		return nil, notFound(path)
	}
	if opFault == nil {
		s.addresses[region].remove(name)
	}
	return s.finishOperation(reqID, region, "delete", path, a.ID, opFault), nil
}

type setLabelsRequest struct {
	Labels           map[string]string `json:"labels"`
	LabelFingerprint string            `json:"labelFingerprint"`
}

func (s *Server) setAddressLabels(r *http.Request, opFault *apiError) (any, *apiError) {
	var req setLabelsRequest
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	if req.LabelFingerprint == "" {
		return nil, invalid("Required field 'labelFingerprint' not specified")
	}
	if err := validateLabels("labels", req.Labels); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := addressPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return op, nil
	}
	a, ok := s.lookup(region, name)
	if !ok {
		return nil, notFound(path)
	}
	if req.LabelFingerprint != a.LabelFingerprint {
		return nil, &apiError{http.StatusPreconditionFailed, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed"}
	}
	if opFault == nil {
		a.Labels = maps.Clone(req.Labels)
		a.LabelFingerprint = fingerprint(a.Labels)
	}
	return s.finishOperation(reqID, region, "setLabels", path, a.ID, opFault), nil
}

type addressList struct {
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	Items         []Address `json:"items,omitzero"`
	SelfLink      string    `json:"selfLink"`
	NextPageToken string    `json:"nextPageToken,omitzero"`
}

func (s *Server) listAddresses(r *http.Request, _ *apiError) (any, *apiError) {
	q := r.URL.Query()
	f, err := parseFilter(q.Get("filter"))
	if err != nil {
		return nil, invalid("Invalid value for field 'filter': %v", err)
	}
	if o := q.Get("orderBy"); o != "" && o != "name" {
		return nil, invalid("Invalid value for field 'orderBy': '%s'. Only ordering by name is supported.", o)
	}
	limit := maxListResults
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxListResults {
			return nil, invalid("Invalid value for field 'maxResults': '%s'. Must be between 0 and %d.", v, maxListResults)
		}
		if n > 0 {
			limit = n
		}
	}
	region := regionPath(r)
	collection := region + "/addresses"
	list := addressList{Kind: "compute#addressList", ID: collection, SelfLink: selfLinkBase + collection}
	after := q.Get("pageToken")
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.addresses[region]
	if !ok {
		return &list, nil
	}
	start, found := slices.BinarySearch(set.names, after)
	if found {
		start++
	}
	for _, name := range set.names[start:] {
		a := set.byName[name]
		if !f.matches(a) {
			continue
		}
		if len(list.Items) == limit {
			list.NextPageToken = list.Items[limit-1].Name
			break
		}
		list.Items = append(list.Items, a.clone())
	}
	return &list, nil
}

// Addresses returns every address the server holds, ordered by selfLink.
func (s *Server) Addresses() []Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Address
	for _, region := range slices.Sorted(maps.Keys(s.addresses)) {
		set := s.addresses[region]
		for _, name := range set.names {
			out = append(out, set.byName[name].clone())
		}
	}
	return out
}

// LookupAddress returns the named address in project and region.
func (s *Server) LookupAddress(project, region, name string) (Address, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.lookup("projects/"+project+"/regions/"+region, name)
	if !ok {
		return Address{}, false
	}
	return a.clone(), true
}

// PutAddress stores a in project and region as if it had been inserted,
// replacing an address of the same name, and returns it as stored. It fills in
// what a leaves empty, such as the ID and an IP address. Use it to set up
// state, such as a name another owner already holds.
func (s *Server) PutAddress(project, region string, a Address) (Address, error) {
	if err := validate(&a); err != nil {
		return Address{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.store("projects/"+project+"/regions/"+region, a)
	if err != nil {
		return Address{}, err
	}
	return stored.clone(), nil
}
