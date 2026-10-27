package ec2

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotPending   = "pending"
	snapshotCompleted = "completed"
	storageStandard   = "standard"
)

// Snapshot is an EBS snapshot.
type Snapshot struct {
	// Region is where the snapshot is. The server sets it.
	Region     string
	SnapshotID string
	VolumeID   string
	// VolumeSize is in GiB.
	VolumeSize  int
	Description string
	Encrypted   bool
	// State is "pending" or "completed".
	State     string
	StartTime time.Time
	Tags      map[string]string
	// SourceRegion and SourceSnapshotID name the snapshot a copy was made
	// from, and are empty for a snapshot of a volume.
	SourceRegion     string
	SourceSnapshotID string

	wait int
}

func (snap *Snapshot) clone() Snapshot {
	c := *snap
	c.Tags = maps.Clone(snap.Tags)
	return c
}

func (snap *Snapshot) progress() string {
	if snap.State == snapshotCompleted {
		return "100%"
	}
	return "0%"
}

// advanceSnapshot counts a describe of snap, completing it once enough have
// seen it pending. The caller holds s.mu.
func (s *Server) advanceSnapshot(snap *Snapshot) {
	if snap.State != snapshotPending || s.settleAfter < 0 {
		return
	}
	if snap.wait > 0 {
		snap.wait--
		return
	}
	snap.State = snapshotCompleted
}

type snapshotXML struct {
	SnapshotID  string   `xml:"snapshotId"`
	VolumeID    string   `xml:"volumeId"`
	Status      string   `xml:"status"`
	StartTime   string   `xml:"startTime"`
	Progress    string   `xml:"progress"`
	VolumeSize  int      `xml:"volumeSize"`
	Description string   `xml:"description"`
	Encrypted   bool     `xml:"encrypted"`
	StorageTier string   `xml:"storageTier"`
	Tags        []tagXML `xml:"tagSet>item,omitempty"`
}

func (snap *Snapshot) xml() snapshotXML {
	return snapshotXML{
		SnapshotID: snap.SnapshotID, VolumeID: snap.VolumeID, Status: snap.State, StartTime: snap.StartTime.UTC().Format(timeFormat),
		Progress: snap.progress(), VolumeSize: snap.VolumeSize, Description: snap.Description, Encrypted: snap.Encrypted,
		StorageTier: storageStandard, Tags: tagSet(snap.Tags),
	}
}

func snapshotFields(snap *Snapshot) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "description":
			return one(snap.Description), true
		case "encrypted":
			return []string{strconv.FormatBool(snap.Encrypted)}, true
		case "progress":
			return []string{snap.progress()}, true
		case "snapshot-id":
			return one(snap.SnapshotID), true
		case "status":
			return one(snap.State), true
		case "storage-tier":
			return []string{storageStandard}, true
		case "volume-id":
			return one(snap.VolumeID), true
		case "volume-size":
			return []string{strconv.Itoa(snap.VolumeSize)}, true
		}
		return tagFilter(snap.Tags, name)
	}
}

func checkSnapshotIDs(ids []string) *apiError {
	for _, id := range ids {
		if !strings.HasPrefix(id, "snap-") {
			return clientError("InvalidSnapshotID.Malformed", "Invalid id: %q (expecting \"snap-...\")", id)
		}
	}
	return nil
}

func snapshotNotFound(id string) *apiError {
	return clientError("InvalidSnapshot.NotFound", "The snapshot '%s' does not exist.", id)
}

// snapshot returns the snapshot with id. The caller holds s.mu.
func (rs *regionState) snapshot(id string) (*Snapshot, *apiError) {
	if err := checkSnapshotIDs([]string{id}); err != nil {
		return nil, err
	}
	snap, ok := rs.snapshots[id]
	if !ok {
		return nil, snapshotNotFound(id)
	}
	return snap, nil
}

type createSnapshotResponse struct {
	meta
	snapshotXML
}

func (s *Server) createSnapshotTarget(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.id = unusedID(s, "snap", s.lookupRegion(c.region).snapshots)
	return c.region + "/" + c.id
}

func (s *Server) createSnapshot(c *call) (response, *apiError) {
	volumeID, err := c.p.required("VolumeId")
	if err != nil {
		return nil, err
	}
	desc := c.p.get("Description")
	tags, err := c.p.tags("snapshot")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	vol, err := rs.volume(volumeID)
	if err != nil {
		return nil, err
	}
	id := c.id
	if id == "" {
		id = unusedID(s, "snap", rs.snapshots)
	}
	snap := &Snapshot{
		Region: c.region, SnapshotID: id, VolumeID: volumeID, VolumeSize: vol.Size, Description: desc, Encrypted: vol.Encrypted,
		State: snapshotPending, StartTime: time.Now(), Tags: maps.Clone(tags), wait: max(s.settleAfter, 0),
	}
	rs.snapshots[id] = snap
	return &createSnapshotResponse{snapshotXML: snap.xml()}, nil
}

// copyVolumeID is the volume ID a copy reports, as EC2 gives copies an
// arbitrary one.
const copyVolumeID = "vol-ffffffff"

type copySnapshotResponse struct {
	meta
	SnapshotID string   `xml:"snapshotId"`
	Tags       []tagXML `xml:"tagSet>item,omitempty"`
}

func (s *Server) copySnapshot(c *call) (response, *apiError) {
	sourceRegion, err := c.p.required("SourceRegion")
	if err != nil {
		return nil, err
	}
	sourceID, err := c.p.required("SourceSnapshotId")
	if err != nil {
		return nil, err
	}
	desc := c.p.get("Description")
	encrypted, err := c.p.bool("Encrypted", false)
	if err != nil {
		return nil, err
	}
	tags, err := c.p.tags("snapshot")
	if err != nil {
		return nil, err
	}
	// The SDKs fill in the deprecated PresignedUrl and DestinationRegion
	// themselves, and the key only matters to encryption, which is not modeled.
	for _, k := range []string{"PresignedUrl", "DestinationRegion", "KmsKeyId"} {
		c.p.get(k)
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.lookupRegion(sourceRegion).snapshot(sourceID)
	if err != nil {
		return nil, err
	}
	if src.State != snapshotCompleted {
		return nil, clientError("IncorrectState", "Snapshot %s is in invalid state - %s", sourceID, src.State)
	}
	rs := s.region(c.region)
	id := c.id
	if id == "" {
		id = unusedID(s, "snap", rs.snapshots)
	}
	snap := &Snapshot{
		Region: c.region, SnapshotID: id, VolumeID: copyVolumeID, VolumeSize: src.VolumeSize, Description: desc,
		Encrypted: src.Encrypted || encrypted, State: snapshotPending, StartTime: time.Now(), Tags: maps.Clone(tags),
		SourceRegion: sourceRegion, SourceSnapshotID: sourceID, wait: max(s.settleAfter, 0),
	}
	rs.snapshots[id] = snap
	return &copySnapshotResponse{SnapshotID: id, Tags: tagSet(snap.Tags)}, nil
}

type describeSnapshotsResponse struct {
	meta
	Snapshots []snapshotXML `xml:"snapshotSet>item"`
	NextToken string        `xml:"nextToken,omitempty"`
}

func (s *Server) describeSnapshots(c *call) (response, *apiError) {
	ids, owners := c.p.list("SnapshotId"), c.p.list("Owner")
	if err := checkSnapshotIDs(ids); err != nil {
		return nil, err
	}
	filters, err := c.p.filters(snapshotFields(&Snapshot{}))
	if err != nil {
		return nil, err
	}
	limit, after, err := c.p.page("SnapshotIds", len(ids) > 0, maxResults, "snap")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	for _, id := range ids {
		if _, ok := rs.snapshots[id]; !ok {
			return nil, snapshotNotFound(id)
		}
	}
	// Every snapshot is the caller's own, so only "self" matches any.
	mine := len(owners) == 0 || slices.Contains(owners, "self")
	var matched []*Snapshot
	for _, id := range slices.Sorted(maps.Keys(rs.snapshots)) {
		if len(ids) > 0 && !slices.Contains(ids, id) {
			continue
		}
		snap := rs.snapshots[id]
		s.advanceSnapshot(snap)
		if mine && matchFilters(filters, snapshotFields(snap)) {
			matched = append(matched, snap)
		}
	}
	page, next := paginate(matched, func(snap *Snapshot) string { return snap.SnapshotID }, after, limit)
	resp := &describeSnapshotsResponse{NextToken: next}
	for _, snap := range page {
		resp.Snapshots = append(resp.Snapshots, snap.xml())
	}
	return resp, nil
}

func (s *Server) deleteSnapshot(c *call) (response, *apiError) {
	id, err := c.p.required("SnapshotId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, err := rs.snapshot(id); err != nil {
		return nil, err
	}
	for _, imageID := range slices.Sorted(maps.Keys(rs.images)) {
		if slices.ContainsFunc(rs.images[imageID].BlockDeviceMappings, func(m BlockDeviceMapping) bool { return m.SnapshotID == id }) {
			return nil, clientError("InvalidSnapshot.InUse", "The snapshot %s is currently in use by %s", id, imageID)
		}
	}
	delete(rs.snapshots, id)
	return &returnResponse{Return: true}, nil
}

// Snapshots returns every snapshot the server holds, ordered by region and ID.
func (s *Server) Snapshots() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snapshot
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.snapshots)) {
			out = append(out, rs.snapshots[id].clone())
		}
	}
	return out
}
