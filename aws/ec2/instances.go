package ec2

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	statePending      = "pending"
	stateRunning      = "running"
	stateShuttingDown = "shutting-down"
	stateTerminated   = "terminated"
	stateStopping     = "stopping"
	stateStopped      = "stopped"

	defaultInstanceType = "m1.small"
	timeFormat          = "2006-01-02T15:04:05.000Z"
)

var stateCodes = map[string]int{
	statePending: 0, stateRunning: 16, stateShuttingDown: 32, stateTerminated: 48, stateStopping: 64, stateStopped: 80,
}

// settledState is the state each transitional state settles in.
var settledState = map[string]string{
	statePending: stateRunning, stateStopping: stateStopped, stateShuttingDown: stateTerminated,
}

// Instance is an EC2 instance.
type Instance struct {
	// Region is where the instance is. The server sets it.
	Region        string
	InstanceID    string
	ReservationID string
	ImageID       string
	InstanceType  string
	Architecture  string
	// State is pending, running, stopping, stopped, shutting-down or terminated.
	State            string
	AvailabilityZone string
	PlacementGroup   string
	PlacementGroupID string
	// NetworkInterfaceID, SubnetID and PrivateIPAddress are the primary
	// interface's, until the instance is terminated.
	NetworkInterfaceID string
	SubnetID           string
	PrivateIPAddress   string
	// PublicIPAddress is the public IP address EC2 assigned the instance at
	// launch. It is released while the instance is stopped or has an Elastic
	// IP address.
	PublicIPAddress string
	RootDeviceName  string
	KeyName         string
	ClientToken     string
	// UserData is the user data, decoded.
	UserData     string
	EBSOptimized bool
	LaunchIndex  int
	LaunchTime   time.Time
	Tags         map[string]string
	// MetadataOptions, PrivateDNSNameOptions and CPUOptions are as launched,
	// with EC2's defaults for what the launch left out.
	MetadataOptions       MetadataOptions
	PrivateDNSNameOptions PrivateDNSNameOptions
	CPUOptions            CPUOptions

	assignPublic    bool
	metadataApplied bool
	wait            int
}

func (inst *Instance) clone() Instance {
	c := *inst
	c.Tags = maps.Clone(inst.Tags)
	return c
}

type instanceStateXML struct {
	Code int    `xml:"code"`
	Name string `xml:"name"`
}

func stateXML(state string) instanceStateXML {
	return instanceStateXML{stateCodes[state], state}
}

type placementXML struct {
	AvailabilityZone string `xml:"availabilityZone"`
	GroupName        string `xml:"groupName"`
	GroupID          string `xml:"groupId,omitempty"`
	Tenancy          string `xml:"tenancy"`
}

type stateReasonXML struct {
	Code    string `xml:"code"`
	Message string `xml:"message"`
}

type monitoringXML struct {
	State string `xml:"state"`
}

type instanceBlockDeviceXML struct {
	DeviceName string `xml:"deviceName"`
	EBS        struct {
		VolumeID            string `xml:"volumeId"`
		Status              string `xml:"status"`
		AttachTime          string `xml:"attachTime"`
		DeleteOnTermination bool   `xml:"deleteOnTermination"`
	} `xml:"ebs"`
}

type instanceXML struct {
	InstanceID         string                   `xml:"instanceId"`
	ImageID            string                   `xml:"imageId"`
	State              instanceStateXML         `xml:"instanceState"`
	PrivateDNSName     string                   `xml:"privateDnsName"`
	DNSName            string                   `xml:"dnsName"`
	Reason             string                   `xml:"reason"`
	KeyName            string                   `xml:"keyName,omitempty"`
	AmiLaunchIndex     int                      `xml:"amiLaunchIndex"`
	InstanceType       string                   `xml:"instanceType"`
	LaunchTime         string                   `xml:"launchTime"`
	Placement          placementXML             `xml:"placement"`
	Monitoring         monitoringXML            `xml:"monitoring"`
	SubnetID           string                   `xml:"subnetId,omitempty"`
	PrivateIPAddress   string                   `xml:"privateIpAddress,omitempty"`
	IPAddress          string                   `xml:"ipAddress,omitempty"`
	Groups             []groupXML               `xml:"groupSet>item"`
	StateReason        *stateReasonXML          `xml:"stateReason,omitempty"`
	Architecture       string                   `xml:"architecture"`
	RootDeviceType     string                   `xml:"rootDeviceType"`
	RootDeviceName     string                   `xml:"rootDeviceName"`
	BlockDevices       []instanceBlockDeviceXML `xml:"blockDeviceMapping>item"`
	VirtualizationType string                   `xml:"virtualizationType"`
	ClientToken        string                   `xml:"clientToken"`
	Tags               []tagXML                 `xml:"tagSet>item,omitempty"`
	Hypervisor         string                   `xml:"hypervisor"`
	NetworkInterfaces  []interfaceXML           `xml:"networkInterfaceSet>item"`
	EBSOptimized       bool                     `xml:"ebsOptimized"`
	CPUOptions         cpuOptionsXML            `xml:"cpuOptions"`
	MetadataOptions    metadataOptionsXML       `xml:"metadataOptions"`
	PrivateDNSOptions  privateDNSNameOptionsXML `xml:"privateDnsNameOptions"`
}

type reservationXML struct {
	ReservationID string        `xml:"reservationId"`
	Instances     []instanceXML `xml:"instancesSet>item"`
}

// regionView answers what rendering and filtering instances need to know
// about the rest of a region. The caller holds s.mu while using it.
type regionView struct {
	rs      *regionState
	public  map[string]*Address
	volumes map[string][]*Volume
}

func (rs *regionState) view() *regionView {
	v := &regionView{rs: rs, public: rs.publicAddresses(), volumes: map[string][]*Volume{}}
	for _, id := range slices.Sorted(maps.Keys(rs.volumes)) {
		if vol := rs.volumes[id]; vol.InstanceID != "" {
			v.volumes[vol.InstanceID] = append(v.volumes[vol.InstanceID], vol)
		}
	}
	for _, vols := range v.volumes {
		slices.SortFunc(vols, func(a, b *Volume) int { return cmp.Compare(a.Device, b.Device) })
	}
	return v
}

// primary returns inst's primary interface while it is attached.
func (v *regionView) primary(inst *Instance) *NetworkInterface {
	if ni, ok := v.rs.interfaces[inst.NetworkInterfaceID]; ok && ni.InstanceID == inst.InstanceID {
		return ni
	}
	return nil
}

func (v *regionView) publicIP(inst *Instance) string {
	if a := v.public[inst.NetworkInterfaceID]; a != nil && v.primary(inst) != nil {
		return a.PublicIP
	}
	return ""
}

func (v *regionView) instanceXML(inst *Instance) instanceXML {
	out := instanceXML{
		InstanceID:     inst.InstanceID,
		ImageID:        inst.ImageID,
		State:          stateXML(inst.State),
		KeyName:        inst.KeyName,
		AmiLaunchIndex: inst.LaunchIndex,
		InstanceType:   inst.InstanceType,
		LaunchTime:     inst.LaunchTime.UTC().Format(timeFormat),
		Placement: placementXML{
			AvailabilityZone: inst.AvailabilityZone, GroupName: inst.PlacementGroup,
			GroupID: inst.PlacementGroupID, Tenancy: "default",
		},
		Monitoring:         monitoringXML{"disabled"},
		SubnetID:           inst.SubnetID,
		PrivateIPAddress:   inst.PrivateIPAddress,
		IPAddress:          v.publicIP(inst),
		Architecture:       inst.Architecture,
		RootDeviceType:     "ebs",
		RootDeviceName:     inst.RootDeviceName,
		VirtualizationType: "hvm",
		ClientToken:        inst.ClientToken,
		Tags:               tagSet(inst.Tags),
		Hypervisor:         "xen",
		EBSOptimized:       inst.EBSOptimized,
		CPUOptions:         cpuOptionsXML(inst.CPUOptions),
		MetadataOptions:    inst.MetadataOptions.xml(inst.metadataApplied),
		PrivateDNSOptions:  privateDNSNameOptionsXML(inst.PrivateDNSNameOptions),
	}
	switch {
	case inst.PrivateIPAddress == "":
	case inst.PrivateDNSNameOptions.HostnameType == hostnameResourceName:
		out.PrivateDNSName = privateDNSName(inst.Region, inst.InstanceID)
	default:
		out.PrivateDNSName = privateDNSName(inst.Region, "ip-"+strings.ReplaceAll(inst.PrivateIPAddress, ".", "-"))
	}
	switch inst.State {
	case statePending:
		out.StateReason = &stateReasonXML{"pending", "pending"}
	case stateStopping, stateStopped, stateShuttingDown, stateTerminated:
		out.StateReason = &stateReasonXML{"Client.UserInitiatedShutdown", "Client.UserInitiatedShutdown: User initiated shutdown"}
	}
	if ni := v.primary(inst); ni != nil {
		for _, g := range ni.SecurityGroupIDs {
			out.Groups = append(out.Groups, groupXML{g})
		}
		out.NetworkInterfaces = []interfaceXML{ni.xml(v.public[ni.NetworkInterfaceID])}
	}
	for _, vol := range v.volumes[inst.InstanceID] {
		var d instanceBlockDeviceXML
		d.DeviceName = vol.Device
		d.EBS.VolumeID, d.EBS.Status, d.EBS.DeleteOnTermination = vol.VolumeID, vol.AttachmentState, vol.DeleteOnTermination
		d.EBS.AttachTime = vol.AttachTime.UTC().Format(timeFormat)
		out.BlockDevices = append(out.BlockDevices, d)
	}
	return out
}

func privateDNSName(region, host string) string {
	if region == "us-east-1" {
		return host + ".ec2.internal"
	}
	return host + "." + region + ".compute.internal"
}

// instanceFields returns the values of inst's DescribeInstances filters.
func (v *regionView) instanceFields(inst *Instance) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "architecture":
			return one(inst.Architecture), true
		case "availability-zone":
			return one(inst.AvailabilityZone), true
		case "block-device-mapping.volume-id":
			var ids []string
			for _, vol := range v.volumes[inst.InstanceID] {
				ids = append(ids, vol.VolumeID)
			}
			return ids, true
		case "client-token":
			return one(inst.ClientToken), true
		case "image-id":
			return one(inst.ImageID), true
		case "instance-id":
			return one(inst.InstanceID), true
		case "instance-state-code":
			return []string{strconv.Itoa(stateCodes[inst.State])}, true
		case "instance-state-name":
			return one(inst.State), true
		case "instance-type":
			return one(inst.InstanceType), true
		case "ip-address":
			return one(v.publicIP(inst)), true
		case "key-name":
			return one(inst.KeyName), true
		case "network-interface.network-interface-id":
			return one(inst.NetworkInterfaceID), true
		case "placement-group-name":
			return one(inst.PlacementGroup), true
		case "private-ip-address":
			return one(inst.PrivateIPAddress), true
		case "reservation-id":
			return one(inst.ReservationID), true
		case "subnet-id":
			return one(inst.SubnetID), true
		}
		return tagFilter(inst.Tags, name)
	}
}

// validID reports whether id is prefix, a hyphen and 8 or 17 hex digits.
func validID(prefix, id string) bool {
	rest, ok := strings.CutPrefix(id, prefix+"-")
	if !ok || len(rest) != 8 && len(rest) != 17 {
		return false
	}
	return strings.Trim(rest, "0123456789abcdef") == ""
}

func checkInstanceIDs(ids []string) *apiError {
	for _, id := range ids {
		if !validID("i", id) {
			return clientError("InvalidInstanceID.Malformed", "Invalid id: %q", id)
		}
	}
	return nil
}

func instanceNotFound(ids ...string) *apiError {
	if len(ids) == 1 {
		return clientError("InvalidInstanceID.NotFound", "The instance ID '%s' does not exist", ids[0])
	}
	return clientError("InvalidInstanceID.NotFound", "The instance IDs '%s' do not exist", strings.Join(ids, ", "))
}

// instance returns the instance with id. The caller holds s.mu.
func (rs *regionState) instance(id string) (*Instance, *apiError) {
	if err := checkInstanceIDs([]string{id}); err != nil {
		return nil, err
	}
	inst, ok := rs.instances[id]
	if !ok {
		return nil, instanceNotFound(id)
	}
	return inst, nil
}

// instancesNamed returns the instances with ids, in order. The caller holds s.mu.
func (rs *regionState) instancesNamed(ids []string) ([]*Instance, *apiError) {
	var out []*Instance
	var missing []string
	for _, id := range ids {
		if inst, ok := rs.instances[id]; ok {
			out = append(out, inst)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, instanceNotFound(missing...)
	}
	return out, nil
}

// transition starts inst toward the state it settles in. The caller holds s.mu.
func (s *Server) transition(inst *Instance, state string) {
	inst.State, inst.wait = state, max(s.settleAfter, 0)
}

// advance counts a describe of inst, settling it once enough have seen it in
// a transitional state. The caller holds s.mu.
func (s *Server) advance(rs *regionState, inst *Instance) {
	if _, ok := settledState[inst.State]; !ok || s.settleAfter < 0 {
		return
	}
	if inst.wait > 0 {
		inst.wait--
		return
	}
	s.settle(rs, inst)
}

// settle completes inst's transition. A terminated instance lets go of its
// interface and volumes, deleting those marked to delete on termination. The
// caller holds s.mu.
func (s *Server) settle(rs *regionState, inst *Instance) {
	next, ok := settledState[inst.State]
	if !ok {
		return
	}
	inst.State, inst.wait = next, 0
	inst.metadataApplied = true
	if next != stateTerminated {
		return
	}
	s.releasePublicIP(inst)
	if ni, ok := rs.interfaces[inst.NetworkInterfaceID]; ok && ni.InstanceID == inst.InstanceID {
		if ni.DeleteOnTermination {
			s.removeInterface(rs, ni)
		} else {
			ni.Status, ni.InstanceID, ni.AttachmentID = statusAvailable, "", ""
		}
	}
	inst.NetworkInterfaceID, inst.SubnetID, inst.PrivateIPAddress = "", "", ""
	for _, vol := range rs.volumes {
		if vol.InstanceID != inst.InstanceID {
			continue
		}
		if vol.DeleteOnTermination {
			delete(rs.volumes, vol.VolumeID)
		} else {
			vol.detach()
		}
	}
}

// releasePublicIP returns inst's public IP address to the pool. The caller holds s.mu.
func (s *Server) releasePublicIP(inst *Instance) {
	if inst.PublicIPAddress != "" {
		delete(s.publicIPs, inst.PublicIPAddress)
		inst.PublicIPAddress = ""
	}
}

// assignPublicIP gives inst a public IP address if it launched with one and
// has none, nor an Elastic IP address. The caller holds s.mu.
func (s *Server) assignPublicIP(rs *regionState, inst *Instance) {
	if !inst.assignPublic || inst.PublicIPAddress != "" || inst.State != statePending && inst.State != stateRunning ||
		rs.addressOn(inst.NetworkInterfaceID) != nil {
		return
	}
	inst.PublicIPAddress = s.allocatePublic()
	s.publicIPs[inst.PublicIPAddress] = inst.Region + "/" + inst.InstanceID
}

// launch is what a RunInstances call asks for.
type launch struct {
	image, instanceType     string
	count                   int
	subnet, privateIP       string
	interfaceID, desc       string
	groups                  []string
	public, deleteInterface bool
	zone, group, groupID    string
	devices                 []deviceOverride
	userData, keyName       string
	ebsOptimized            bool
	tags                    map[string]map[string]string
	token                   string
	interfaceType           string
	metadata                MetadataOptions
	dnsName                 PrivateDNSNameOptions
	cpu                     CPUOptions
}

// deviceOverride is a RunInstances block device mapping; the set fields say
// which of the image's settings it replaces.
type deviceOverride struct {
	BlockDeviceMapping
	setDelete, setEncrypted bool
}

type runRecord struct {
	reservationID string
	instanceIDs   []string
}

type runInstancesResponse struct {
	meta
	ReservationID string        `xml:"reservationId"`
	Instances     []instanceXML `xml:"instancesSet>item"`
}

func (s *Server) runTarget(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.tokenTarget(c, "instances"); ok {
		return c.region + "/" + id
	}
	c.id = unusedID(s, "i", s.lookupRegion(c.region).instances)
	return c.region + "/" + c.id
}

func parseLaunch(p *params) (*launch, *apiError) {
	l := &launch{instanceType: cmp.Or(p.get("InstanceType"), defaultInstanceType)}
	var err *apiError
	if l.image, err = p.required("ImageId"); err != nil {
		return nil, err
	}
	minCount, err := requiredInt(p, "MinCount")
	if err != nil {
		return nil, err
	}
	if l.count, err = requiredInt(p, "MaxCount"); err != nil {
		return nil, err
	}
	if minCount < 1 || l.count < minCount {
		return nil, invalidValue("Invalid value '%d' for minCount. Must be between 1 and maxCount (%d).", minCount, l.count)
	}
	l.subnet, l.privateIP, l.groups = p.get("SubnetId"), p.get("PrivateIpAddress"), p.list("SecurityGroupId")
	if err := l.readInterface(p); err != nil {
		return nil, err
	}
	l.zone, l.group, l.groupID = p.get("Placement.AvailabilityZone"), p.get("Placement.GroupName"), p.get("Placement.GroupId")
	if t := p.get("Placement.Tenancy"); t != "" && t != "default" {
		return nil, invalidValue("squall: tenancy '%s' is not supported.", t)
	}
	if l.devices, err = readDevices(p); err != nil {
		return nil, err
	}
	if v := p.get("UserData"); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, invalidValue("Invalid BASE64 encoding of user data.")
		}
		l.userData = string(b)
	}
	l.keyName = p.get("KeyName")
	if l.ebsOptimized, err = p.bool("EbsOptimized", false); err != nil {
		return nil, err
	}
	if l.tags, err = p.tagSpecs("instance", "volume", "network-interface"); err != nil {
		return nil, err
	}
	if l.metadata, err = readMetadataOptions(p); err != nil {
		return nil, err
	}
	if l.dnsName, err = readPrivateDNSNameOptions(p); err != nil {
		return nil, err
	}
	if l.cpu, err = readCPUOptions(p); err != nil {
		return nil, err
	}
	l.token = p.get("ClientToken")
	return l, p.done()
}

func requiredInt(p *params, key string) (int, *apiError) {
	if p.values.Get(key) == "" {
		return 0, missing(key)
	}
	return p.int(key)
}

func (l *launch) readInterface(p *params) *apiError {
	specs := p.indexes("NetworkInterface")
	if len(specs) == 0 {
		l.deleteInterface = true
		return nil
	}
	if len(specs) > 1 {
		return invalidValue("squall: only one network interface, at device index 0, is supported.")
	}
	switch {
	case l.subnet != "":
		return clientError("InvalidParameterCombination", "Network interfaces and an instance-level subnet ID may not be specified on the same request")
	case len(l.groups) > 0:
		return clientError("InvalidParameterCombination", "Network interfaces and an instance-level security groups may not be specified on the same request")
	case l.privateIP != "":
		return clientError("InvalidParameterCombination", "Network interfaces and an instance-level private IP address may not be specified on the same request")
	}
	n := "NetworkInterface." + specs[0] + "."
	switch p.get(n + "DeviceIndex") {
	case "":
		return invalidValue("Each network interface requires a device index.")
	case "0":
	default:
		return invalidValue("squall: only one network interface, at device index 0, is supported.")
	}
	l.interfaceID, l.subnet, l.privateIP = p.get(n+"NetworkInterfaceId"), p.get(n+"SubnetId"), p.get(n+"PrivateIpAddress")
	l.desc, l.groups = p.get(n+"Description"), p.list(n+"SecurityGroupId")
	var err *apiError
	if p.get(n+"InterfaceType") == "efa-only" {
		return invalidValue("An efa-only network interface cannot be the primary network interface (device index 0).")
	}
	if l.interfaceType, err = p.enum(n+"InterfaceType", "", interfaceTypes...); err != nil {
		return err
	}
	card, err := p.int(n + "NetworkCardIndex")
	if err != nil {
		return err
	}
	if card != 0 {
		return invalidValue("Invalid network card index %d. The primary network interface must be assigned to network card index 0.", card)
	}
	if l.public, err = p.bool(n+"AssociatePublicIpAddress", false); err != nil {
		return err
	}
	if l.deleteInterface, err = p.bool(n+"DeleteOnTermination", l.interfaceID == ""); err != nil {
		return err
	}
	if l.interfaceID != "" && (l.subnet != "" || l.privateIP != "" || l.desc != "" || len(l.groups) > 0 || l.public) {
		return clientError("InvalidParameterCombination", "A network interface ID cannot be specified with a subnet, private IP address, description, security groups or public IP address.")
	}
	return nil
}

func readDevices(p *params) ([]deviceOverride, *apiError) {
	var out []deviceOverride
	for _, n := range p.indexes("BlockDeviceMapping") {
		k := "BlockDeviceMapping." + n + "."
		var d deviceOverride
		var err *apiError
		if d.DeviceName, err = p.required(k + "DeviceName"); err != nil {
			return nil, err
		}
		d.SnapshotID, d.VolumeType = p.get(k+"Ebs.SnapshotId"), p.get(k+"Ebs.VolumeType")
		if d.VolumeSize, err = p.int(k + "Ebs.VolumeSize"); err != nil {
			return nil, err
		}
		if d.Iops, err = p.int(k + "Ebs.Iops"); err != nil {
			return nil, err
		}
		if d.Throughput, err = p.int(k + "Ebs.Throughput"); err != nil {
			return nil, err
		}
		d.setDelete, d.setEncrypted = p.values.Has(k+"Ebs.DeleteOnTermination"), p.values.Has(k+"Ebs.Encrypted")
		if d.DeleteOnTermination, err = p.bool(k+"Ebs.DeleteOnTermination", true); err != nil {
			return nil, err
		}
		if d.Encrypted, err = p.bool(k+"Ebs.Encrypted", false); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// devicesFor merges l's block device mappings into image's.
func (l *launch) devicesFor(image *Image) ([]BlockDeviceMapping, *apiError) {
	out := slices.Clone(image.BlockDeviceMappings)
	for _, d := range l.devices {
		i := slices.IndexFunc(out, func(m BlockDeviceMapping) bool { return m.DeviceName == d.DeviceName })
		if i < 0 {
			if d.VolumeSize == 0 && d.SnapshotID == "" {
				return nil, clientError("InvalidBlockDeviceMapping", "The device '%s' needs a volume size or a snapshot ID.", d.DeviceName)
			}
			m := d.BlockDeviceMapping
			if err := checkVolume(m); err != nil {
				return nil, err
			}
			if m.VolumeSize == 0 {
				m.VolumeSize = defaultVolumeSize
			}
			out = append(out, m)
			continue
		}
		m := &out[i]
		if d.VolumeSize != 0 {
			if d.VolumeSize < m.VolumeSize {
				return nil, clientError("InvalidBlockDeviceMapping", "Volume of size %dGB is smaller than snapshot '%s', expect size >= %dGB",
					d.VolumeSize, m.SnapshotID, m.VolumeSize)
			}
			m.VolumeSize = d.VolumeSize
		}
		if d.VolumeType != "" && d.VolumeType != m.VolumeType {
			m.VolumeType, m.Iops, m.Throughput = d.VolumeType, 0, 0
		}
		given := BlockDeviceMapping{VolumeType: m.VolumeType, Iops: d.Iops, Throughput: d.Throughput}
		if m.VolumeType == "io1" || m.VolumeType == "io2" {
			given.Iops = cmp.Or(d.Iops, m.Iops)
		}
		if err := checkVolume(given); err != nil {
			return nil, err
		}
		if m.VolumeType == "gp2" {
			m.Iops = 0
		}
		m.Iops, m.Throughput = cmp.Or(d.Iops, m.Iops), cmp.Or(d.Throughput, m.Throughput)
		if d.setDelete {
			m.DeleteOnTermination = d.DeleteOnTermination
		}
		if d.setEncrypted {
			m.Encrypted = d.Encrypted
		}
	}
	for i := range out {
		fillVolume(&out[i])
	}
	return out, nil
}

func (s *Server) runInstances(c *call) (response, *apiError) {
	l, err := parseLaunch(c.p)
	if err != nil {
		return nil, err
	}
	fingerprint := c.p.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.replay(c.region, "instances", l.token, fingerprint)
	if err != nil {
		return nil, err
	}
	if t != nil {
		run := t.created.(runRecord)
		rs := s.lookupRegion(c.region)
		v := rs.view()
		resp := &runInstancesResponse{ReservationID: run.reservationID}
		for _, id := range run.instanceIDs {
			if inst, ok := rs.instances[id]; ok {
				resp.Instances = append(resp.Instances, v.instanceXML(inst))
			}
		}
		return resp, nil
	}
	rs := s.lookupRegion(c.region)
	image, devices, ni, err := s.checkLaunch(rs, c.region, l)
	if err != nil {
		return nil, err
	}
	rs = s.region(c.region)
	reservation := s.newID("r")
	now := time.Now()
	zone := cmp.Or(l.zone, c.region+"a")
	if ni != nil {
		zone = ni.AvailabilityZone
	}
	run := runRecord{reservationID: reservation}
	for i := range l.count {
		id := c.id
		if i > 0 || id == "" {
			id = unusedID(s, "i", rs.instances)
		}
		attached := ni
		if attached == nil {
			created, err := s.storeInterface(c.region, NetworkInterface{
				SubnetID: l.subnet, AvailabilityZone: zone, Description: l.desc, PrivateIPAddress: l.privateIP,
				SecurityGroupIDs: l.groups, Tags: maps.Clone(l.tags["network-interface"]), InstanceID: id,
				InterfaceType: l.interfaceType,
			})
			if err != nil {
				return nil, err
			}
			attached = created
		} else {
			attached.Status, attached.InstanceID, attached.AttachmentID = statusInUse, id, s.newID("eni-attach")
		}
		attached.DeleteOnTermination = l.deleteInterface
		for _, m := range devices {
			vol := s.newVolume(rs, "", c.region, zone, m, l.tags["volume"], now)
			vol.State, vol.InstanceID, vol.Device, vol.AttachmentState = statusInUse, id, m.DeviceName, attachmentAttached
			vol.DeleteOnTermination, vol.AttachTime = m.DeleteOnTermination, now
		}
		inst := &Instance{
			Region: c.region, InstanceID: id, ReservationID: reservation, ImageID: image.ImageID,
			InstanceType: l.instanceType, Architecture: image.Architecture, AvailabilityZone: zone,
			PlacementGroup: l.group, PlacementGroupID: l.groupID, NetworkInterfaceID: attached.NetworkInterfaceID, SubnetID: attached.SubnetID,
			PrivateIPAddress: attached.PrivateIPAddress, RootDeviceName: image.RootDeviceName, KeyName: l.keyName,
			ClientToken: l.token, UserData: l.userData, EBSOptimized: l.ebsOptimized, LaunchIndex: i, LaunchTime: now,
			Tags: maps.Clone(l.tags["instance"]), MetadataOptions: l.metadata, PrivateDNSNameOptions: l.dnsName,
			CPUOptions: l.cpu, assignPublic: l.public,
		}
		s.transition(inst, statePending)
		rs.instances[id] = inst
		s.assignPublicIP(rs, inst)
		run.instanceIDs = append(run.instanceIDs, id)
	}
	s.remember(c.region, "instances", l.token, fingerprint, run.instanceIDs[0], run)
	v := rs.view()
	resp := &runInstancesResponse{ReservationID: reservation}
	for _, id := range run.instanceIDs {
		resp.Instances = append(resp.Instances, v.instanceXML(rs.instances[id]))
	}
	return resp, nil
}

// checkLaunch checks everything a launch could fail on before it changes
// anything, and returns the image, the block devices and the existing
// interface to attach, if any. It fills in l's CPU options with the instance
// type's defaults. The caller holds s.mu.
func (s *Server) checkLaunch(rs *regionState, region string, l *launch) (*Image, []BlockDeviceMapping, *NetworkInterface, *apiError) {
	image, err := rs.image(l.image)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := s.checkInstanceType(l.instanceType, image.Architecture, "InvalidParameterValue"); err != nil {
		return nil, nil, nil, err
	}
	if l.cpu, err = cpuOptionsFor(l.cpu, s.instanceTypes[l.instanceType]); err != nil {
		return nil, nil, nil, err
	}
	if l.group != "" || l.groupID != "" {
		pg, err := rs.placementGroup(l.group, l.groupID)
		if err != nil {
			return nil, nil, nil, err
		}
		l.group, l.groupID = pg.GroupName, pg.GroupID
	}
	if l.zone != "" && !strings.HasPrefix(l.zone, region) {
		return nil, nil, nil, invalidValue("Invalid availability zone: [%s]", l.zone)
	}
	if l.count > 1 && (l.interfaceID != "" || l.privateIP != "") {
		return nil, nil, nil, clientError("InvalidParameterCombination", "A network interface ID or private IP address cannot be specified when launching more than one instance.")
	}
	var ni *NetworkInterface
	switch {
	case l.interfaceID != "":
		var ok bool
		if ni, ok = rs.interfaces[l.interfaceID]; !ok {
			return nil, nil, nil, interfaceNotFound(l.interfaceID)
		}
		if ni.Status != statusAvailable {
			return nil, nil, nil, clientError("InvalidNetworkInterface.InUse", "Interface: [%s] in use.", l.interfaceID)
		}
		if l.interfaceType != "" && l.interfaceType != cmp.Or(ni.InterfaceType, interfaceTypeDefault) {
			return nil, nil, nil, clientError("InvalidParameterCombination", "The network interface '%s' is of type %s, not %s.", ni.NetworkInterfaceID, cmp.Or(ni.InterfaceType, interfaceTypeDefault), l.interfaceType)
		}
		if l.zone != "" && l.zone != ni.AvailabilityZone {
			return nil, nil, nil, clientError("InvalidParameterCombination", "The network interface '%s' is in %s, not %s.", ni.NetworkInterfaceID, ni.AvailabilityZone, l.zone)
		}
	case l.subnet == "":
		return nil, nil, nil, clientError("VPCIdNotSpecified", "No default VPC for this user. GroupName is only supported for EC2-Classic and default VPC.")
	case l.privateIP != "":
		if _, taken := s.privateIPs[privateKey(region, l.subnet, l.privateIP)]; taken {
			return nil, nil, nil, clientError("InvalidIPAddress.InUse", "Address %s is in use.", l.privateIP)
		}
	}
	devices, err := l.devicesFor(image)
	if err != nil {
		return nil, nil, nil, err
	}
	return image, devices, ni, nil
}

type describeInstancesResponse struct {
	meta
	Reservations []reservationXML `xml:"reservationSet>item"`
	NextToken    string           `xml:"nextToken,omitempty"`
}

func (s *Server) describeInstances(c *call) (response, *apiError) {
	ids := c.p.list("InstanceId")
	if err := checkInstanceIDs(ids); err != nil {
		return nil, err
	}
	empty := &regionView{rs: &regionState{}}
	filters, err := c.p.filters(empty.instanceFields(&Instance{}))
	if err != nil {
		return nil, err
	}
	limit, after, err := c.p.page("instancesSet", len(ids) > 0, maxResults, "i")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, err := rs.instancesNamed(ids); err != nil {
		return nil, err
	}
	var asked []*Instance
	for _, id := range slices.Sorted(maps.Keys(rs.instances)) {
		if len(ids) == 0 || slices.Contains(ids, id) {
			inst := rs.instances[id]
			s.advance(rs, inst)
			asked = append(asked, inst)
		}
	}
	v := rs.view()
	asked = slices.DeleteFunc(asked, func(inst *Instance) bool { return !matchFilters(filters, v.instanceFields(inst)) })
	page, next := paginate(asked, func(inst *Instance) string { return inst.InstanceID }, after, limit)
	resp := &describeInstancesResponse{NextToken: next}
	index := map[string]int{}
	for _, inst := range page {
		i, ok := index[inst.ReservationID]
		if !ok {
			i = len(resp.Reservations)
			index[inst.ReservationID] = i
			resp.Reservations = append(resp.Reservations, reservationXML{ReservationID: inst.ReservationID})
		}
		resp.Reservations[i].Instances = append(resp.Reservations[i].Instances, v.instanceXML(inst))
	}
	return resp, nil
}

type stateChangeXML struct {
	InstanceID    string           `xml:"instanceId"`
	CurrentState  instanceStateXML `xml:"currentState"`
	PreviousState instanceStateXML `xml:"previousState"`
}

type stateChangeResponse struct {
	meta
	Instances []stateChangeXML `xml:"instancesSet>item"`
}

// changeStates checks every instance the call names with check and only then
// changes each with change, so a call that fails changes nothing.
func (s *Server) changeStates(c *call, check func(*Instance) *apiError, change func(*regionState, *Instance)) (response, *apiError) {
	ids := c.p.list("InstanceId")
	if len(ids) == 0 {
		return nil, missing("InstanceId")
	}
	if err := checkInstanceIDs(ids); err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	insts, err := rs.instancesNamed(ids)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		if err := check(inst); err != nil {
			return nil, err
		}
	}
	resp := &stateChangeResponse{}
	for _, inst := range insts {
		previous := inst.State
		change(rs, inst)
		resp.Instances = append(resp.Instances, stateChangeXML{inst.InstanceID, stateXML(inst.State), stateXML(previous)})
	}
	return resp, nil
}

func incorrectState(inst *Instance, verb string) *apiError {
	return clientError("IncorrectInstanceState", "The instance '%s' is not in a state from which it can be %s.", inst.InstanceID, verb)
}

func (s *Server) startInstances(c *call) (response, *apiError) {
	return s.changeStates(c, func(inst *Instance) *apiError {
		switch inst.State {
		case stateStopped, statePending, stateRunning:
			return nil
		}
		return incorrectState(inst, "started")
	}, func(rs *regionState, inst *Instance) {
		if inst.State == stateStopped {
			s.transition(inst, statePending)
			s.assignPublicIP(rs, inst)
		}
	})
}

// readShutdownFlags reads the Force and SkipOsShutdown flags of a stop or
// terminate, which change nothing here: no instance runs an OS to shut down.
func readShutdownFlags(p *params) *apiError {
	for _, key := range []string{"Force", "SkipOsShutdown"} {
		if _, err := p.bool(key, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) stopInstances(c *call) (response, *apiError) {
	if err := readShutdownFlags(c.p); err != nil {
		return nil, err
	}
	return s.changeStates(c, func(inst *Instance) *apiError {
		switch inst.State {
		case stateRunning, stateStopping, stateStopped:
			return nil
		}
		return incorrectState(inst, "stopped")
	}, func(_ *regionState, inst *Instance) {
		if inst.State == stateRunning {
			s.transition(inst, stateStopping)
			s.releasePublicIP(inst)
		}
	})
}

func (s *Server) terminateInstances(c *call) (response, *apiError) {
	if err := readShutdownFlags(c.p); err != nil {
		return nil, err
	}
	return s.changeStates(c, func(*Instance) *apiError { return nil }, func(_ *regionState, inst *Instance) {
		if inst.State != stateShuttingDown && inst.State != stateTerminated {
			s.transition(inst, stateShuttingDown)
			s.releasePublicIP(inst)
		}
	})
}

func (s *Server) modifyInstanceAttribute(c *call) (response, *apiError) {
	id, err := c.p.required("InstanceId")
	if err != nil {
		return nil, err
	}
	instanceType := c.p.get("InstanceType.Value")
	if attribute := c.p.get("Attribute"); attribute != "" {
		if attribute != "instanceType" {
			return nil, invalidValue("squall: only the instanceType attribute can be modified.")
		}
		instanceType = c.p.get("Value")
	}
	if instanceType == "" {
		return nil, missing("InstanceType")
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, err := s.lookupRegion(c.region).instance(id)
	if err != nil {
		return nil, err
	}
	if err := s.checkInstanceType(instanceType, inst.Architecture, "InvalidInstanceAttributeValue"); err != nil {
		return nil, err
	}
	if inst.State != stateStopped {
		return nil, clientError("IncorrectInstanceState", "The instance '%s' is not in the 'stopped' state.", id)
	}
	cpu := s.instanceTypes[instanceType].defaultCPUOptions()
	cpu.AmdSevSnp = inst.CPUOptions.AmdSevSnp
	inst.InstanceType, inst.CPUOptions = instanceType, cpu
	return &returnResponse{Return: true}, nil
}

// Instances returns every instance the server holds, terminated ones
// included, ordered by region and ID.
func (s *Server) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Instance
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.instances)) {
			out = append(out, rs.instances[id].clone())
		}
	}
	return out
}

// LookupInstance returns the instance with id in region.
func (s *Server) LookupInstance(region, id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.lookupRegion(region).instances[id]
	if !ok {
		return Instance{}, false
	}
	return inst.clone(), true
}

// Settle completes every transition in progress, as enough describes would:
// pending instances run, stopping ones stop, shutting-down ones terminate,
// and volumes finish creating, attaching and detaching.
func (s *Server) Settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range s.regions {
		for _, inst := range rs.instances {
			s.settle(rs, inst)
		}
		for _, vol := range rs.volumes {
			vol.settle()
		}
	}
}

// ForgetInstance removes the terminated instance with id in region, as EC2
// does some time after it terminates.
func (s *Server) ForgetInstance(region, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(region)
	inst, ok := rs.instances[id]
	if !ok {
		return fmt.Errorf("instance %s in %s: %w", id, region, ErrNotFound)
	}
	if inst.State != stateTerminated {
		return fmt.Errorf("ec2: instance %s is %s, not terminated", id, inst.State)
	}
	delete(rs.instances, id)
	return nil
}
