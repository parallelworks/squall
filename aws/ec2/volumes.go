package ec2

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	volumeCreating      = "creating"
	attachmentAttaching = "attaching"
	attachmentAttached  = "attached"
	attachmentDetaching = "detaching"

	defaultVolumeType = "gp2"
	maxVolumeResults  = 500
)

var volumeTypes = []string{"standard", "io1", "io2", "gp2", "gp3", "sc1", "st1"}

// Volume is an EBS volume.
type Volume struct {
	// Region is where the volume is. The server sets it.
	Region           string
	VolumeID         string
	AvailabilityZone string
	// Size is in GiB.
	Size       int
	VolumeType string
	Iops       int
	Throughput int
	SnapshotID string
	Encrypted  bool
	// State is "creating", "available" or "in-use".
	State      string
	CreateTime time.Time
	Tags       map[string]string
	// InstanceID, Device, AttachmentState, AttachTime and DeleteOnTermination
	// are set while the volume is attached. AttachmentState is "attaching",
	// "attached" or "detaching".
	InstanceID          string
	Device              string
	AttachmentState     string
	AttachTime          time.Time
	DeleteOnTermination bool

	wait int
}

func (vol *Volume) clone() Volume {
	c := *vol
	c.Tags = maps.Clone(vol.Tags)
	return c
}

func (vol *Volume) detach() {
	vol.State, vol.InstanceID, vol.Device, vol.AttachmentState = statusAvailable, "", "", ""
	vol.AttachTime, vol.DeleteOnTermination, vol.wait = time.Time{}, false, 0
}

// settle completes the volume's creation, attachment or detachment.
func (vol *Volume) settle() {
	switch {
	case vol.State == volumeCreating:
		vol.State = statusAvailable
	case vol.AttachmentState == attachmentAttaching:
		vol.AttachmentState = attachmentAttached
	case vol.AttachmentState == attachmentDetaching:
		vol.detach()
	}
	vol.wait = 0
}

func (vol *Volume) transitional() bool {
	return vol.State == volumeCreating || vol.AttachmentState == attachmentAttaching || vol.AttachmentState == attachmentDetaching
}

// advanceVolume counts a describe of vol, settling it once enough have seen
// it in a transitional state. The caller holds s.mu.
func (s *Server) advanceVolume(vol *Volume) {
	if !vol.transitional() || s.settleAfter < 0 {
		return
	}
	if vol.wait > 0 {
		vol.wait--
		return
	}
	vol.settle()
}

// checkVolume rejects IOPS and throughput that m's volume type does not take, as EC2 does, rather than adjusting them.
func checkVolume(m BlockDeviceMapping) *apiError {
	if m.VolumeType == "" {
		m.VolumeType = defaultVolumeType
	}
	if !slices.Contains(volumeTypes, m.VolumeType) {
		return invalidValue("Value (%s) for parameter volumeType is invalid.", m.VolumeType)
	}
	if m.Throughput != 0 {
		if m.VolumeType != "gp3" {
			return clientError("InvalidParameterCombination", "Throughput is only valid for gp3 volume types.")
		}
		if m.Throughput < 125 || m.Throughput > 1000 {
			return invalidValue("Throughput (%d) is out of range for gp3 volumes.", m.Throughput)
		}
	}
	switch m.VolumeType {
	case "gp3":
		if m.Iops != 0 && (m.Iops < 3000 || m.Iops > 16000) {
			return invalidValue("Volume iops of %d is out of range for gp3 volumes.", m.Iops)
		}
	case "io1", "io2":
		if m.Iops == 0 {
			return clientError("InvalidParameterCombination", "The parameter iops must be specified for %s volumes.", m.VolumeType)
		}
		if m.Iops < 100 || m.Iops > 64000 {
			return invalidValue("Volume iops of %d is out of range for %s volumes.", m.Iops, m.VolumeType)
		}
	default:
		if m.Iops != 0 {
			return clientError("InvalidParameterCombination", "The parameter iops is not supported for %s volumes.", m.VolumeType)
		}
	}
	return nil
}

// fillVolume sets the type, gp2 unless set, and the IOPS and throughput EC2 reports for it.
func fillVolume(m *BlockDeviceMapping) {
	if m.VolumeType == "" {
		m.VolumeType = defaultVolumeType
	}
	switch m.VolumeType {
	case "gp3":
		m.Iops, m.Throughput = cmp.Or(m.Iops, 3000), cmp.Or(m.Throughput, 125)
	case "gp2":
		m.Iops = min(max(3*m.VolumeSize, 100), 16000)
	}
}

// newVolume stores a volume with id, or a fresh ID, created from m in zone.
// The caller holds s.mu.
func (s *Server) newVolume(rs *regionState, id, region, zone string, m BlockDeviceMapping, tags map[string]string, now time.Time) *Volume {
	if id == "" {
		id = unusedID(s, "vol", rs.volumes)
	}
	vol := &Volume{
		Region: region, VolumeID: id, AvailabilityZone: zone, Size: m.VolumeSize,
		VolumeType: m.VolumeType, Iops: m.Iops, Throughput: m.Throughput, SnapshotID: m.SnapshotID,
		Encrypted: m.Encrypted, State: volumeCreating, CreateTime: now, Tags: maps.Clone(tags), wait: max(s.settleAfter, 0),
	}
	rs.volumes[vol.VolumeID] = vol
	return vol
}

type volumeAttachmentXML struct {
	VolumeID            string `xml:"volumeId"`
	InstanceID          string `xml:"instanceId"`
	Device              string `xml:"device"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type volumeXML struct {
	VolumeID           string                `xml:"volumeId"`
	Size               int                   `xml:"size"`
	SnapshotID         string                `xml:"snapshotId"`
	AvailabilityZone   string                `xml:"availabilityZone"`
	Status             string                `xml:"status"`
	CreateTime         string                `xml:"createTime"`
	Attachments        []volumeAttachmentXML `xml:"attachmentSet>item"`
	Tags               []tagXML              `xml:"tagSet>item,omitempty"`
	VolumeType         string                `xml:"volumeType"`
	Iops               int                   `xml:"iops,omitempty"`
	Throughput         int                   `xml:"throughput,omitempty"`
	Encrypted          bool                  `xml:"encrypted"`
	MultiAttachEnabled bool                  `xml:"multiAttachEnabled"`
}

func (vol *Volume) attachmentXML() volumeAttachmentXML {
	return volumeAttachmentXML{
		VolumeID: vol.VolumeID, InstanceID: vol.InstanceID, Device: vol.Device, Status: vol.AttachmentState,
		AttachTime: vol.AttachTime.UTC().Format(timeFormat), DeleteOnTermination: vol.DeleteOnTermination,
	}
}

func (vol *Volume) xml() volumeXML {
	out := volumeXML{
		VolumeID: vol.VolumeID, Size: vol.Size, SnapshotID: vol.SnapshotID, AvailabilityZone: vol.AvailabilityZone,
		Status: vol.State, CreateTime: vol.CreateTime.UTC().Format(timeFormat), Tags: tagSet(vol.Tags),
		VolumeType: vol.VolumeType, Iops: vol.Iops, Throughput: vol.Throughput, Encrypted: vol.Encrypted,
	}
	if vol.InstanceID != "" {
		out.Attachments = []volumeAttachmentXML{vol.attachmentXML()}
	}
	return out
}

func volumeFields(vol *Volume) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "attachment.delete-on-termination":
			if vol.InstanceID == "" {
				return nil, true
			}
			return []string{strconv.FormatBool(vol.DeleteOnTermination)}, true
		case "attachment.device":
			return one(vol.Device), true
		case "attachment.instance-id":
			return one(vol.InstanceID), true
		case "attachment.status":
			return one(vol.AttachmentState), true
		case "availability-zone":
			return one(vol.AvailabilityZone), true
		case "encrypted":
			return []string{strconv.FormatBool(vol.Encrypted)}, true
		case "size":
			return []string{strconv.Itoa(vol.Size)}, true
		case "snapshot-id":
			return one(vol.SnapshotID), true
		case "status":
			return one(vol.State), true
		case "volume-id":
			return one(vol.VolumeID), true
		case "volume-type":
			return one(vol.VolumeType), true
		}
		return tagFilter(vol.Tags, name)
	}
}

func volumeNotFound(id string) *apiError {
	return clientError("InvalidVolume.NotFound", "The volume '%s' does not exist.", id)
}

// volume returns the volume with id. The caller holds s.mu.
func (rs *regionState) volume(id string) (*Volume, *apiError) {
	vol, ok := rs.volumes[id]
	if !ok {
		return nil, volumeNotFound(id)
	}
	return vol, nil
}

type createVolumeResponse struct {
	meta
	volumeXML
}

func (s *Server) createVolumeTarget(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.tokenTarget(c, "volumes"); ok {
		return c.region + "/" + id
	}
	c.id = unusedID(s, "vol", s.lookupRegion(c.region).volumes)
	return c.region + "/" + c.id
}

func (s *Server) createVolume(c *call) (response, *apiError) {
	zone, err := c.p.required("AvailabilityZone")
	if err != nil {
		return nil, err
	}
	var m BlockDeviceMapping
	if m.VolumeSize, err = c.p.int("Size"); err != nil {
		return nil, err
	}
	m.SnapshotID, m.VolumeType = c.p.get("SnapshotId"), c.p.get("VolumeType")
	if m.Iops, err = c.p.int("Iops"); err != nil {
		return nil, err
	}
	if m.Throughput, err = c.p.int("Throughput"); err != nil {
		return nil, err
	}
	if m.Encrypted, err = c.p.bool("Encrypted", false); err != nil {
		return nil, err
	}
	tags, err := c.p.tags("volume")
	if err != nil {
		return nil, err
	}
	token := c.p.get("ClientToken")
	if err := c.p.done(); err != nil {
		return nil, err
	}
	switch {
	case m.VolumeSize == 0 && m.SnapshotID == "":
		return nil, clientError("MissingParameter", "The request must contain the parameter size or snapshotId")
	case m.VolumeSize == 0:
		m.VolumeSize = defaultVolumeSize
	case m.VolumeSize < 1 || m.VolumeSize > 65536:
		return nil, invalidValue("Volume size %d is out of range.", m.VolumeSize)
	}
	if !strings.HasPrefix(zone, c.region) {
		return nil, invalidValue("Invalid availability zone: [%s]", zone)
	}
	if err := checkVolume(m); err != nil {
		return nil, err
	}
	fillVolume(&m)
	fingerprint := c.p.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.replay(c.region, "volumes", token, fingerprint)
	if err != nil {
		return nil, err
	}
	if t != nil {
		created := t.created.(Volume)
		return &createVolumeResponse{volumeXML: created.xml()}, nil
	}
	rs := s.region(c.region)
	vol := s.newVolume(rs, c.id, c.region, zone, m, tags, time.Now())
	s.remember(c.region, "volumes", token, fingerprint, vol.VolumeID, vol.clone())
	return &createVolumeResponse{volumeXML: vol.xml()}, nil
}

type describeVolumesResponse struct {
	meta
	Volumes   []volumeXML `xml:"volumeSet>item"`
	NextToken string      `xml:"nextToken,omitempty"`
}

func (s *Server) describeVolumes(c *call) (response, *apiError) {
	ids := c.p.list("VolumeId")
	filters, err := c.p.filters(volumeFields(&Volume{}))
	if err != nil {
		return nil, err
	}
	limit, after, err := c.p.page("VolumeIds", len(ids) > 0, maxVolumeResults, "vol")
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
		if _, ok := rs.volumes[id]; !ok {
			return nil, volumeNotFound(id)
		}
	}
	var matched []*Volume
	for _, id := range slices.Sorted(maps.Keys(rs.volumes)) {
		if len(ids) > 0 && !slices.Contains(ids, id) {
			continue
		}
		vol := rs.volumes[id]
		s.advanceVolume(vol)
		if matchFilters(filters, volumeFields(vol)) {
			matched = append(matched, vol)
		}
	}
	page, next := paginate(matched, func(vol *Volume) string { return vol.VolumeID }, after, limit)
	resp := &describeVolumesResponse{NextToken: next}
	for _, vol := range page {
		resp.Volumes = append(resp.Volumes, vol.xml())
	}
	return resp, nil
}

func (s *Server) deleteVolume(c *call) (response, *apiError) {
	id, err := c.p.required("VolumeId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	vol, err := rs.volume(id)
	if err != nil {
		return nil, err
	}
	if vol.InstanceID != "" {
		return nil, clientError("VolumeInUse", "Volume %s is currently attached to %s", id, vol.InstanceID)
	}
	delete(rs.volumes, id)
	return &returnResponse{Return: true}, nil
}

type volumeAttachmentResponse struct {
	meta
	volumeAttachmentXML
}

func (s *Server) attachVolume(c *call) (response, *apiError) {
	device, err := c.p.required("Device")
	if err != nil {
		return nil, err
	}
	instanceID, err := c.p.required("InstanceId")
	if err != nil {
		return nil, err
	}
	id, err := c.p.required("VolumeId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	vol, err := rs.volume(id)
	if err != nil {
		return nil, err
	}
	inst, err := rs.instance(instanceID)
	if err != nil {
		return nil, err
	}
	switch {
	case vol.InstanceID != "":
		return nil, clientError("VolumeInUse", "%s is already attached to an instance", id)
	case vol.State != statusAvailable:
		return nil, clientError("IncorrectState", "Volume '%s' is in the '%s' state.", id, vol.State)
	case inst.State != stateRunning && inst.State != stateStopped:
		return nil, clientError("IncorrectState", "Instance '%s' is not 'running'.", instanceID)
	case vol.AvailabilityZone != inst.AvailabilityZone:
		return nil, clientError("InvalidVolume.ZoneMismatch", "The volume '%s' is not in the same availability zone as instance '%s'", id, instanceID)
	}
	for _, other := range rs.volumes {
		if other.InstanceID == instanceID && other.Device == device {
			return nil, invalidValue("Invalid value '%s' for unixDevice. Attachment point %s is already in use", device, device)
		}
	}
	vol.State, vol.InstanceID, vol.Device, vol.AttachmentState = statusInUse, instanceID, device, attachmentAttaching
	vol.AttachTime, vol.DeleteOnTermination, vol.wait = time.Now(), false, max(s.settleAfter, 0)
	return &volumeAttachmentResponse{volumeAttachmentXML: vol.attachmentXML()}, nil
}

func (s *Server) detachVolume(c *call) (response, *apiError) {
	id, err := c.p.required("VolumeId")
	if err != nil {
		return nil, err
	}
	instanceID, device := c.p.get("InstanceId"), c.p.get("Device")
	if _, err := c.p.bool("Force", false); err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	vol, err := rs.volume(id)
	if err != nil {
		return nil, err
	}
	switch {
	case vol.InstanceID == "":
		return nil, clientError("IncorrectState", "Volume '%s' is in the '%s' state.", id, vol.State)
	case vol.AttachmentState == attachmentDetaching:
		return nil, clientError("IncorrectState", "Volume '%s' is in the '%s' state.", id, attachmentDetaching)
	case instanceID != "" && instanceID != vol.InstanceID || device != "" && device != vol.Device:
		return nil, clientError("InvalidAttachment.NotFound", "Volume '%s' is not attached to instance '%s' at '%s'", id, instanceID, device)
	}
	if inst, ok := rs.instances[vol.InstanceID]; ok && inst.RootDeviceName == vol.Device && inst.State != stateStopped {
		return nil, clientError("OperationNotPermitted", "Unable to detach root volume '%s' from instance '%s'", id, vol.InstanceID)
	}
	vol.AttachmentState, vol.wait = attachmentDetaching, max(s.settleAfter, 0)
	return &volumeAttachmentResponse{volumeAttachmentXML: vol.attachmentXML()}, nil
}

// Volumes returns every volume the server holds, ordered by region and ID.
func (s *Server) Volumes() []Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Volume
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.volumes)) {
			out = append(out, rs.volumes[id].clone())
		}
	}
	return out
}
