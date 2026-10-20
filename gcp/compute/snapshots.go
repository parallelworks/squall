package compute

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	snapshotCreating = "CREATING"
	snapshotReady    = "READY"
	snapshotDeleting = "DELETING"
)

// Snapshot is a disk snapshot, in the API's JSON shape. Take one with
// snapshots.insert or register one with [Server.PutSnapshot].
type Snapshot struct {
	Kind              string `json:"kind"`
	ID                uint64 `json:"id,string"`
	CreationTimestamp string `json:"creationTimestamp"`
	Name              string `json:"name"`
	Description       string `json:"description,omitzero"`
	Status            string `json:"status"`
	SourceDisk        string `json:"sourceDisk,omitzero"`
	SourceDiskID      uint64 `json:"sourceDiskId,string,omitzero"`
	// DiskSizeGB is the size of the disk the snapshot was taken of: 10 when zero.
	DiskSizeGB       int64             `json:"diskSizeGb,string"`
	StorageLocations []string          `json:"storageLocations,omitzero"`
	Labels           map[string]string `json:"labels,omitzero"`
	LabelFingerprint string            `json:"labelFingerprint"`
	SelfLink         string            `json:"selfLink"`

	op *operation
}

// snapshotInput is what snapshots.insert takes.
type snapshotInput struct {
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	SourceDisk       string            `json:"sourceDisk"`
	StorageLocations []string          `json:"storageLocations"`
	Labels           map[string]string `json:"labels"`
}

var snapshotFields = fieldSet{names: []string{"name", "description", "status", "id", "diskSizeGb", "sourceDisk"}, labels: true}

func (snap *Snapshot) filterValue(field string) string {
	switch field {
	case "name":
		return snap.Name
	case "description":
		return snap.Description
	case "status":
		return snap.Status
	case "id":
		return strconv.FormatUint(snap.ID, 10)
	case "diskSizeGb":
		return strconv.FormatInt(snap.DiskSizeGB, 10)
	case "sourceDisk":
		return snap.SourceDisk
	}
	return snap.Labels[strings.TrimPrefix(field, "labels.")]
}

func snapshotPath(global, name string) string { return global + "/snapshots/" + name }

func badName(field, name string) *apiError {
	return invalid("Invalid value for field '%s': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field, name)
}

// PutSnapshot registers snap in project, replacing a snapshot of the same
// name, and returns it as stored: READY, with its ID and selfLink.
func (s *Server) PutSnapshot(project string, snap Snapshot) (Snapshot, error) {
	if !validName(snap.Name) {
		return Snapshot{}, badName("resource.name", snap.Name)
	}
	if err := validateLabels("resource.labels", snap.Labels); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	global := "projects/" + project + "/global"
	stored := s.newSnapshot(global, snap)
	stored.Status = snapshotReady
	scoped(s.snapshots, global).put(stored.Name, stored)
	return jsonClone(stored), nil
}

// newSnapshot fills in what snap leaves empty, for a snapshot in global, the
// global scope of a project. The caller holds s.mu.
func (s *Server) newSnapshot(global string, snap Snapshot) *Snapshot {
	snap.Kind, snap.ID, snap.CreationTimestamp = "compute#snapshot", s.newID(), timestamp(s.now())
	snap.DiskSizeGB = cmp.Or(snap.DiskSizeGB, defaultDiskSizeGB)
	snap.Labels = maps.Clone(snap.Labels)
	snap.StorageLocations = slices.Clone(snap.StorageLocations)
	snap.LabelFingerprint = fingerprint(snap.Labels)
	snap.SelfLink = selfLinkBase + snapshotPath(global, snap.Name)
	snap.op = nil
	return &snap
}

// multiRegion is the multi-region a zone such as "us-east1-b" is in, where
// the API stores a snapshot of a disk there by default.
func multiRegion(zone string) string {
	name := zone[strings.LastIndexByte(zone, '/')+1:]
	area, _, _ := strings.Cut(name, "-")
	return area
}

func (s *Server) insertSnapshot(r *http.Request, opFault *apiError) (any, *apiError) {
	var in snapshotInput
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
	}
	if !validName(in.Name) {
		return nil, badName("resource.name", in.Name)
	}
	if err := validateLabels("resource.labels", in.Labels); err != nil {
		return nil, err
	}
	if in.SourceDisk == "" {
		return nil, invalid("Invalid value for field 'resource.sourceDisk': ''. A snapshot needs a source disk.")
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	path := snapshotPath(global, in.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	zone, name, ok := strings.Cut(relPath(in.SourceDisk, r.PathValue("project")), "/disks/")
	d, found := lookupIn(s.disks, zone, name)
	if !ok || !found {
		return nil, missingRef("resource.sourceDisk", in.SourceDisk, "disk")
	}
	if d.Status != diskReady {
		return nil, notReady(strings.TrimPrefix(d.SelfLink, selfLinkBase))
	}
	if _, ok := lookupIn(s.snapshots, global, in.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	locations := in.StorageLocations
	if len(locations) == 0 {
		locations = []string{multiRegion(zone)}
	}
	snap := s.newSnapshot(global, Snapshot{
		Name: in.Name, Description: in.Description, SourceDisk: d.SelfLink, SourceDiskID: d.ID,
		DiskSizeGB: int64(d.SizeGB), StorageLocations: locations, Labels: in.Labels,
	})
	snap.Status = snapshotCreating
	scoped(s.snapshots, global).put(snap.Name, snap)
	op := s.startOperation(r, reqID, "insert", path, snap.ID, nil, func() {
		snap.Status, snap.op = snapshotReady, nil
	})
	if op.complete != nil {
		snap.op = op
	}
	return encoded(op), nil
}

// lookupSnapshot returns the snapshot r names. The caller holds s.mu.
func (s *Server) lookupSnapshot(r *http.Request) (*Snapshot, *apiError) {
	global, name := scopeOf(r), r.PathValue("name")
	snap, ok := lookupIn(s.snapshots, global, name)
	if !ok {
		return nil, notFound(snapshotPath(global, name))
	}
	return snap, nil
}

func (s *Server) getSnapshot(r *http.Request, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.lookupSnapshot(r)
	if err != nil {
		return nil, err
	}
	if s.observe(snap.op); !s.snapshots[scopeOf(r)].has(snap.Name) {
		return nil, notFound(snapshotPath(scopeOf(r), snap.Name))
	}
	return encoded(snap), nil
}

func (s *Server) deleteSnapshot(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	snap, err := s.lookupSnapshot(r)
	if err != nil {
		return nil, err
	}
	if snap.op != nil {
		s.finish(snap.op)
		if snap, err = s.lookupSnapshot(r); err != nil {
			return nil, err
		}
	}
	path := strings.TrimPrefix(snap.SelfLink, selfLinkBase)
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "delete", path, snap.ID, opFault, nil)), nil
	}
	snap.Status = snapshotDeleting
	global := scopeOf(r)
	op := s.startOperation(r, reqID, "delete", path, snap.ID, nil, func() {
		s.snapshots[global].remove(snap.Name)
		snap.op = nil
	})
	if op.complete != nil {
		snap.op = op
	}
	return encoded(op), nil
}

func (s *Server) listSnapshots(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, snapshotFields)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.observer()
	return listScope(s.snapshots[global], q, "compute#snapshotList", global+"/snapshots", func(snap *Snapshot) { seen(snap.op) }), nil
}

// Snapshots returns every snapshot the server holds, ordered by selfLink.
func (s *Server) Snapshots() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snapshot
	each(s.snapshots, func(snap *Snapshot) { out = append(out, jsonClone(snap)) })
	return out
}

// LookupSnapshot returns the named snapshot in project.
func (s *Server) LookupSnapshot(project, name string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := lookupIn(s.snapshots, "projects/"+project+"/global", name)
	if !ok {
		return Snapshot{}, false
	}
	return jsonClone(snap), true
}
