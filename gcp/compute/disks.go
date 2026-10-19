package compute

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	diskCreating = "CREATING"
	diskReady    = "READY"
	diskDeleting = "DELETING"
)

// Disk is a zonal persistent disk, in the API's JSON shape.
type Disk struct {
	Kind                        string            `json:"kind"`
	ID                          uint64            `json:"id,string"`
	CreationTimestamp           string            `json:"creationTimestamp"`
	Name                        string            `json:"name"`
	Description                 string            `json:"description,omitzero"`
	SizeGB                      int64             `json:"sizeGb,string,omitzero"`
	Zone                        string            `json:"zone"`
	Status                      string            `json:"status"`
	SelfLink                    string            `json:"selfLink"`
	SourceImage                 string            `json:"sourceImage,omitzero"`
	SourceImageID               uint64            `json:"sourceImageId,string,omitzero"`
	SourceSnapshot              string            `json:"sourceSnapshot,omitzero"`
	Type                        string            `json:"type"`
	Labels                      map[string]string `json:"labels,omitzero"`
	LabelFingerprint            string            `json:"labelFingerprint"`
	Users                       []string          `json:"users,omitzero"`
	LastAttachTimestamp         string            `json:"lastAttachTimestamp,omitzero"`
	LastDetachTimestamp         string            `json:"lastDetachTimestamp,omitzero"`
	PhysicalBlockSizeBytes      int64             `json:"physicalBlockSizeBytes,string,omitzero"`
	ResourcePolicies            []string          `json:"resourcePolicies,omitzero"`
	ProvisionedIops             int64             `json:"provisionedIops,string,omitzero"`
	ProvisionedThroughput       int64             `json:"provisionedThroughput,string,omitzero"`
	Architecture                string            `json:"architecture,omitzero"`
	DiskEncryptionKey           json.RawMessage   `json:"diskEncryptionKey,omitzero"`
	SourceImageEncryptionKey    json.RawMessage   `json:"sourceImageEncryptionKey,omitzero"`
	SourceSnapshotEncryptionKey json.RawMessage   `json:"sourceSnapshotEncryptionKey,omitzero"`
	GuestOSFeatures             json.RawMessage   `json:"guestOsFeatures,omitzero"`

	op *operation
}

var diskFields = fieldSet{names: []string{"name", "description", "status", "id", "sizeGb"}, labels: true}

func (d *Disk) filterValue(field string) string {
	switch field {
	case "name":
		return d.Name
	case "description":
		return d.Description
	case "status":
		return d.Status
	case "id":
		return strconv.FormatUint(d.ID, 10)
	case "sizeGb":
		return strconv.FormatInt(d.SizeGB, 10)
	}
	return d.Labels[strings.TrimPrefix(field, "labels.")]
}

func diskPath(zone, name string) string { return zone + "/disks/" + name }

// zonalType turns a machine or disk type reference into its URL in zone, the
// path of a zone such as "projects/p/zones/z". kind is "machineTypes" or
// "diskTypes". A bare name is the type in zone; any well-formed name is a type.
func zonalType(zone, kind, field, ref string) (string, *apiError) {
	name := ref
	if strings.Contains(ref, "/") {
		project, _, _ := strings.Cut(strings.TrimPrefix(zone, "projects/"), "/")
		inZone, n, ok := strings.Cut(relPath(ref, project), "/"+kind+"/")
		if !ok || inZone != zone {
			return "", invalid("Invalid value for field '%s': '%s'. The type must be in zone '%s'.", field, ref, zone)
		}
		name = n
	}
	if !validName(name) {
		return "", invalid("Invalid value for field '%s': '%s'.", field, ref)
	}
	return selfLinkBase + zone + "/" + kind + "/" + name, nil
}

// newDisk validates d and fills in what it leaves empty, for a disk in zone,
// the path of a zone. field prefixes the fields named in errors. The caller
// holds s.mu.
func (s *Server) newDisk(zone, field string, d Disk) (*Disk, *apiError) {
	if !validName(d.Name) {
		return nil, invalid("Invalid value for field '%s.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field, d.Name)
	}
	if err := validateLabels(field+".labels", d.Labels); err != nil {
		return nil, err
	}
	project, _, _ := strings.Cut(strings.TrimPrefix(zone, "projects/"), "/")
	src, err := s.resolveSource(project, field, d.SourceImage, d.SourceSnapshot)
	if err != nil {
		return nil, err
	}
	if d.SizeGB == 0 {
		d.SizeGB = src.sizeGB
	}
	if d.SizeGB < src.sizeGB {
		return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the image size (%d GB)", field, d.SizeGB, src.sizeGB)
	}
	if d.Type, err = zonalType(zone, "diskTypes", field+".type", cmp.Or(d.Type, "pd-standard")); err != nil {
		return nil, err
	}
	d.Kind = "compute#disk"
	d.ID = s.newID()
	d.CreationTimestamp = timestamp(s.now())
	d.Zone = selfLinkBase + zone
	d.SelfLink = selfLinkBase + diskPath(zone, d.Name)
	d.SourceImage, d.SourceImageID, d.SourceSnapshot = src.image, src.imageID, src.snapshot
	d.LabelFingerprint = fingerprint(d.Labels)
	d.PhysicalBlockSizeBytes = cmp.Or(d.PhysicalBlockSizeBytes, 4096)
	d.Status, d.Users, d.LastAttachTimestamp, d.LastDetachTimestamp, d.op = diskReady, nil, "", "", nil
	return &d, nil
}

func (s *Server) insertDisk(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Disk
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r, "insert")
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	d, err := s.newDisk(zone, "resource", want)
	if err != nil {
		return nil, err
	}
	path := diskPath(zone, d.Name)
	if _, ok := lookupIn(s.disks, zone, d.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	d.Status = diskCreating
	scoped(s.disks, zone).put(d.Name, d)
	d.op = s.startOperation(r, reqID, "insert", path, d.ID, nil, func() {
		d.Status, d.op = diskReady, nil
	})
	return encoded(d.op), nil
}

// lookupDisk returns the disk r names. The caller holds s.mu.
func (s *Server) lookupDisk(r *http.Request) (*Disk, *apiError) {
	zone, name := scopeOf(r), r.PathValue("name")
	d, ok := lookupIn(s.disks, zone, name)
	if !ok {
		return nil, notFound(diskPath(zone, name))
	}
	return d, nil
}

func (s *Server) getDisk(r *http.Request, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.lookupDisk(r)
	if err != nil {
		return nil, err
	}
	s.observe(d.op)
	return encoded(d), nil
}

func (s *Server) deleteDisk(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r, "delete")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	d, err := s.lookupDisk(r)
	if err != nil {
		return nil, err
	}
	s.finish(d.op)
	path := strings.TrimPrefix(d.SelfLink, selfLinkBase)
	if len(d.Users) > 0 {
		return nil, inUse("disk", path, strings.TrimPrefix(d.Users[0], selfLinkBase))
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "delete", path, d.ID, opFault, nil)), nil
	}
	d.Status = diskDeleting
	zone := scopeOf(r)
	d.op = s.startOperation(r, reqID, "delete", path, d.ID, nil, func() {
		s.disks[zone].remove(d.Name)
		d.op = nil
	})
	return encoded(d.op), nil
}

func (s *Server) listDisks(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, diskFields)
	if err != nil {
		return nil, err
	}
	zone := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.observer()
	return listScope(s.disks[zone], q, "compute#diskList", zone+"/disks", func(d *Disk) { seen(d.op) }), nil
}

func (s *Server) aggregatedDisks(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, diskFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.observer()
	return listAggregated(s.disks, r.PathValue("project"), q, "compute#diskAggregatedList", "disks", func(d *Disk) { seen(d.op) }), nil
}

// Disks returns every disk the server holds, ordered by selfLink.
func (s *Server) Disks() []Disk {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Disk
	each(s.disks, func(d *Disk) { out = append(out, jsonClone(d)) })
	return out
}

// LookupDisk returns the named disk in project and zone.
func (s *Server) LookupDisk(project, zone, name string) (Disk, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := lookupIn(s.disks, "projects/"+project+"/zones/"+zone, name)
	if !ok {
		return Disk{}, false
	}
	return jsonClone(d), true
}

// release takes user off d's users. The caller holds s.mu.
func (d *Disk) release(user string, now string) {
	if i := slices.Index(d.Users, user); i >= 0 {
		d.Users = slices.Delete(d.Users, i, i+1)
		d.LastDetachTimestamp = now
	}
}
