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
	"strings"
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
	Users             []string          `json:"users,omitzero"`
}

func (a *Address) clone() Address {
	c := *a
	c.Labels = maps.Clone(a.Labels)
	c.Users = slices.Clone(a.Users)
	return c
}

func addressPath(region, name string) string { return region + "/addresses/" + name }

// lookup returns the named address in region. The caller holds s.mu.
func (s *Server) lookup(region, name string) (*Address, bool) {
	return lookupIn(s.addresses, region, name)
}

var addressFields = fieldSet{names: []string{"name", "description", "status", "address", "addressType", "id"}, labels: true}

func (a *Address) filterValue(field string) string {
	switch field {
	case "name":
		return a.Name
	case "description":
		return a.Description
	case "status":
		return a.Status
	case "address":
		return a.Address
	case "addressType":
		return a.AddressType
	case "id":
		return strconv.FormatUint(a.ID, 10)
	}
	return a.Labels[strings.TrimPrefix(field, "labels.")]
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
	set := scoped(s.addresses, region)
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
	path := addressPath(region, a.Name)
	if old, ok := set.byName[a.Name]; ok {
		delete(s.ipHolders, ipKey(region, old.AddressType, old.Address))
	}
	if a.Address == "" {
		a.Address = s.allocate(region, a.AddressType)
	} else {
		a.Address = netip.MustParseAddr(a.Address).String()
		if holder, taken := s.ipHolders[ipKey(region, a.AddressType, a.Address)]; taken && holder != path {
			return nil, invalid("IP address '%s' is already being used by another resource.", a.Address)
		}
	}
	s.ipHolders[ipKey(region, a.AddressType, a.Address)] = path
	stored := a.clone()
	set.put(stored.Name, &stored)
	return &stored, nil
}

// ipKey identifies an IP address where it must be unique: an external address
// across every project and region, as public addresses are, and an internal
// one within its region. Real internal addresses are unique per subnetwork;
// regions stand in for subnetworks here.
func ipKey(region, addressType, ip string) string {
	if addressType == addressExternal {
		return ip
	}
	return region + "/" + ip
}

// allocate returns the next free address of addressType for region. The caller holds s.mu.
func (s *Server) allocate(region, addressType string) string {
	ip := s.nextIP[addressType]
	for {
		if _, taken := s.ipHolders[ipKey(region, addressType, ip.String())]; !taken {
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
	reqID, err := requestID(r, "insert")
	if err != nil {
		return nil, err
	}
	region := regionPath(r)
	path := addressPath(region, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := s.lookup(region, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	want.ID, want.CreationTimestamp, want.Status, want.Users = 0, "", "", nil
	a, err := s.store(region, want)
	if err != nil {
		return nil, err
	}
	return encoded(s.startOperation(r, reqID, "insert", path, a.ID, nil, nil)), nil
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
	reqID, err := requestID(r, "delete")
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := addressPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	a, ok := s.lookup(region, name)
	if !ok {
		return nil, notFound(path)
	}
	if len(a.Users) > 0 {
		return nil, inUse("address", path, a.Users[0])
	}
	if opFault == nil {
		s.addresses[region].remove(name)
		delete(s.ipHolders, ipKey(region, a.AddressType, a.Address))
	}
	return encoded(s.startOperation(r, reqID, "delete", path, a.ID, opFault, nil)), nil
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
	reqID, err := requestID(r, "setLabels")
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := addressPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	a, ok := s.lookup(region, name)
	if !ok {
		return nil, notFound(path)
	}
	if req.LabelFingerprint != a.LabelFingerprint {
		return nil, fingerprintMismatch()
	}
	if opFault == nil {
		a.Labels = maps.Clone(req.Labels)
		a.LabelFingerprint = fingerprint(a.Labels)
	}
	return encoded(s.startOperation(r, reqID, "setLabels", path, a.ID, opFault, nil)), nil
}

func (s *Server) listAddresses(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, addressFields)
	if err != nil {
		return nil, err
	}
	region := regionPath(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.addresses[region], q, "compute#addressList", region+"/addresses", nil), nil
}

// Addresses returns every address the server holds, ordered by selfLink.
func (s *Server) Addresses() []Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Address
	each(s.addresses, func(a *Address) { out = append(out, a.clone()) })
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
