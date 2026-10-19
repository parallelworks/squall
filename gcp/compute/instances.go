package compute

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	statusProvisioning = "PROVISIONING"
	statusStaging      = "STAGING"
	statusRunning      = "RUNNING"
	statusStopping     = "STOPPING"
	statusTerminated   = "TERMINATED"

	maxNetworkInterfaces = 8
	maxBulkCount         = 1000
)

// Instance is a VM instance, in the API's JSON shape. Fields the server only
// records, such as guestAccelerators, keep the JSON the caller sent.
type Instance struct {
	Kind                       string             `json:"kind"`
	ID                         uint64             `json:"id,string"`
	CreationTimestamp          string             `json:"creationTimestamp"`
	Name                       string             `json:"name"`
	Description                string             `json:"description,omitzero"`
	Tags                       Tags               `json:"tags"`
	MachineType                string             `json:"machineType"`
	Status                     string             `json:"status"`
	Zone                       string             `json:"zone"`
	CanIPForward               bool               `json:"canIpForward"`
	NetworkInterfaces          []NetworkInterface `json:"networkInterfaces,omitzero"`
	Disks                      []AttachedDisk     `json:"disks,omitzero"`
	Metadata                   Metadata           `json:"metadata"`
	ServiceAccounts            []ServiceAccount   `json:"serviceAccounts,omitzero"`
	SelfLink                   string             `json:"selfLink"`
	Scheduling                 Scheduling         `json:"scheduling"`
	Labels                     map[string]string  `json:"labels,omitzero"`
	LabelFingerprint           string             `json:"labelFingerprint"`
	Fingerprint                string             `json:"fingerprint"`
	DeletionProtection         bool               `json:"deletionProtection"`
	StartRestricted            bool               `json:"startRestricted"`
	LastStartTimestamp         string             `json:"lastStartTimestamp,omitzero"`
	LastStopTimestamp          string             `json:"lastStopTimestamp,omitzero"`
	ResourcePolicies           []string           `json:"resourcePolicies,omitzero"`
	Hostname                   string             `json:"hostname,omitzero"`
	MinCPUPlatform             string             `json:"minCpuPlatform,omitzero"`
	GuestAccelerators          json.RawMessage    `json:"guestAccelerators,omitzero"`
	ReservationAffinity        json.RawMessage    `json:"reservationAffinity,omitzero"`
	NetworkPerformanceConfig   json.RawMessage    `json:"networkPerformanceConfig,omitzero"`
	ShieldedInstanceConfig     json.RawMessage    `json:"shieldedInstanceConfig,omitzero"`
	ConfidentialInstanceConfig json.RawMessage    `json:"confidentialInstanceConfig,omitzero"`
	AdvancedMachineFeatures    json.RawMessage    `json:"advancedMachineFeatures,omitzero"`

	op *operation
}

// Tags are an instance's network tags.
type Tags struct {
	Items       []string `json:"items,omitzero"`
	Fingerprint string   `json:"fingerprint"`
}

// Metadata is an instance's metadata.
type Metadata struct {
	Kind        string         `json:"kind"`
	Fingerprint string         `json:"fingerprint"`
	Items       []MetadataItem `json:"items,omitzero"`
}

// MetadataItem is one metadata key and value.
type MetadataItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ServiceAccount is the service account an instance runs as.
type ServiceAccount struct {
	Email  string   `json:"email"`
	Scopes []string `json:"scopes,omitzero"`
}

// Scheduling is an instance's scheduling options.
type Scheduling struct {
	OnHostMaintenance         string          `json:"onHostMaintenance,omitzero"`
	AutomaticRestart          *bool           `json:"automaticRestart,omitzero"`
	Preemptible               bool            `json:"preemptible"`
	ProvisioningModel         string          `json:"provisioningModel,omitzero"`
	InstanceTerminationAction string          `json:"instanceTerminationAction,omitzero"`
	MaxRunDuration            json.RawMessage `json:"maxRunDuration,omitzero"`
	NodeAffinities            json.RawMessage `json:"nodeAffinities,omitzero"`
}

// NetworkInterface is one of an instance's network interfaces.
type NetworkInterface struct {
	Kind          string          `json:"kind"`
	Name          string          `json:"name"`
	Network       string          `json:"network,omitzero"`
	Subnetwork    string          `json:"subnetwork,omitzero"`
	NetworkIP     string          `json:"networkIP,omitzero"`
	AccessConfigs []AccessConfig  `json:"accessConfigs,omitzero"`
	Fingerprint   string          `json:"fingerprint"`
	StackType     string          `json:"stackType,omitzero"`
	NicType       string          `json:"nicType,omitzero"`
	AliasIPRanges json.RawMessage `json:"aliasIpRanges,omitzero"`
	QueueCount    int             `json:"queueCount,omitzero"`
}

// AccessConfig is a network interface's external address.
type AccessConfig struct {
	Kind        string `json:"kind"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	NatIP       string `json:"natIP,omitzero"`
	NetworkTier string `json:"networkTier,omitzero"`

	// ephemeral is set for an address the instance holds only while it runs.
	ephemeral bool
}

// AttachedDisk is a disk attached to an instance. InitializeParams is only
// ever sent, to create the disk; the instance reports the disk's Source.
type AttachedDisk struct {
	Kind              string            `json:"kind"`
	Type              string            `json:"type"`
	Mode              string            `json:"mode"`
	Source            string            `json:"source,omitzero"`
	DeviceName        string            `json:"deviceName"`
	Index             int               `json:"index"`
	Boot              bool              `json:"boot"`
	AutoDelete        bool              `json:"autoDelete"`
	Interface         string            `json:"interface,omitzero"`
	DiskSizeGB        int64             `json:"diskSizeGb,string,omitzero"`
	InitializeParams  *InitializeParams `json:"initializeParams,omitzero"`
	GuestOSFeatures   json.RawMessage   `json:"guestOsFeatures,omitzero"`
	DiskEncryptionKey json.RawMessage   `json:"diskEncryptionKey,omitzero"`
}

// InitializeParams describe a disk created with its instance.
type InitializeParams struct {
	DiskName                    string            `json:"diskName,omitzero"`
	DiskSizeGB                  int64             `json:"diskSizeGb,string,omitzero"`
	DiskType                    string            `json:"diskType,omitzero"`
	SourceImage                 string            `json:"sourceImage,omitzero"`
	SourceSnapshot              string            `json:"sourceSnapshot,omitzero"`
	Labels                      map[string]string `json:"labels,omitzero"`
	Description                 string            `json:"description,omitzero"`
	ResourcePolicies            []string          `json:"resourcePolicies,omitzero"`
	ProvisionedIops             int64             `json:"provisionedIops,string,omitzero"`
	ProvisionedThroughput       int64             `json:"provisionedThroughput,string,omitzero"`
	Architecture                string            `json:"architecture,omitzero"`
	SourceImageEncryptionKey    json.RawMessage   `json:"sourceImageEncryptionKey,omitzero"`
	SourceSnapshotEncryptionKey json.RawMessage   `json:"sourceSnapshotEncryptionKey,omitzero"`
}

var instanceFields = fieldSet{names: []string{"name", "description", "status", "id", "machineType"}, labels: true}

func (inst *Instance) filterValue(field string) string {
	switch field {
	case "name":
		return inst.Name
	case "description":
		return inst.Description
	case "status":
		return inst.Status
	case "id":
		return strconv.FormatUint(inst.ID, 10)
	case "machineType":
		return inst.MachineType
	}
	return inst.Labels[strings.TrimPrefix(field, "labels.")]
}

func instancePath(zone, name string) string { return zone + "/instances/" + name }

// instanceInput is an instances.insert body: an instance, and the parameters
// that only a request carries.
type instanceInput struct {
	Instance
	Params json.RawMessage `json:"params"`
}

// hash is a fingerprint of v's JSON.
func hash(v any) string {
	h := fnv.New64a()
	_ = json.NewEncoder(h).Encode(v)
	return hex.EncodeToString(h.Sum(nil))
}

// claim is an IP address a new instance takes when it is stored.
type claim struct {
	key     string
	address *Address
}

// newInstance validates props and builds the instance name in zone from it,
// with the disks to create for it and the IP addresses to claim. field
// prefixes the fields named in errors. A bulk instance takes no disk names,
// existing disks or fixed IP addresses. The caller holds s.mu.
func (s *Server) newInstance(zone, region, name, field string, props Instance, bulk bool) (*Instance, []*Disk, []claim, *apiError) {
	inst := jsonClone(&props)
	if !validName(name) {
		return nil, nil, nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", name)
	}
	if err := validateLabels(field+".labels", inst.Labels); err != nil {
		return nil, nil, nil, err
	}
	if inst.MachineType == "" {
		return nil, nil, nil, invalid("Invalid value for field '%s.machineType': ''. Machine type must be specified.", field)
	}
	var err *apiError
	if inst.MachineType, err = zonalType(zone, "machineTypes", field+".machineType", inst.MachineType); err != nil {
		return nil, nil, nil, err
	}
	if inst.DeletionProtection {
		return nil, nil, nil, invalid("Invalid value for field '%s.deletionProtection': true. squall does not support deletion protection.", field)
	}
	if err := validateScheduling(field+".scheduling", &inst.Scheduling); err != nil {
		return nil, nil, nil, err
	}
	path := instancePath(zone, name)
	inst.Kind, inst.ID, inst.Name = "compute#instance", s.newID(), name
	inst.CreationTimestamp = timestamp(s.now())
	inst.Zone, inst.SelfLink = selfLinkBase+zone, selfLinkBase+path
	inst.Status, inst.StartRestricted = statusProvisioning, false
	inst.LastStartTimestamp, inst.LastStopTimestamp, inst.op = "", "", nil
	inst.LabelFingerprint = fingerprint(inst.Labels)
	inst.Tags.Fingerprint = hash(inst.Tags.Items)
	inst.Metadata.Kind, inst.Metadata.Fingerprint = "compute#metadata", hash(inst.Metadata.Items)
	keys := map[string]bool{}
	for _, item := range inst.Metadata.Items {
		if keys[item.Key] || item.Key == "" {
			return nil, nil, nil, invalid("Invalid value for field '%s.metadata': duplicate or empty key '%s'.", field, item.Key)
		}
		keys[item.Key] = true
	}
	if len(inst.ServiceAccounts) > 1 {
		return nil, nil, nil, invalid("Invalid value for field '%s.serviceAccounts': at most one service account may be specified.", field)
	}
	policies := make([]string, 0, len(inst.ResourcePolicies))
	for _, ref := range inst.ResourcePolicies {
		link, err := s.resolvePolicy(region, ref)
		if err != nil {
			return nil, nil, nil, err
		}
		policies = append(policies, link)
	}
	inst.ResourcePolicies = policies
	inst.Fingerprint = hash(inst.ID)

	disks, err := s.attachNewDisks(&inst, zone, field, bulk)
	if err != nil {
		return nil, nil, nil, err
	}
	claims, err := s.configureInterfaces(&inst, region, field, bulk)
	if err != nil {
		return nil, nil, nil, err
	}
	return &inst, disks, claims, nil
}

func validateScheduling(field string, sch *Scheduling) *apiError {
	sch.OnHostMaintenance = cmp.Or(sch.OnHostMaintenance, "MIGRATE")
	sch.ProvisioningModel = cmp.Or(sch.ProvisioningModel, "STANDARD")
	if sch.AutomaticRestart == nil {
		sch.AutomaticRestart = new(true)
	}
	for _, check := range []struct{ name, value, allowed string }{
		{"onHostMaintenance", sch.OnHostMaintenance, "MIGRATE TERMINATE"},
		{"provisioningModel", sch.ProvisioningModel, "STANDARD SPOT FLEX_START RESERVATION_BOUND"},
		{"instanceTerminationAction", cmp.Or(sch.InstanceTerminationAction, "STOP"), "STOP DELETE"},
	} {
		if !slices.Contains(strings.Fields(check.allowed), check.value) {
			return invalid("Invalid value for field '%s.%s': '%s'.", field, check.name, check.value)
		}
	}
	return nil
}

// attachNewDisks checks inst's disks and builds the ones it creates. The
// caller holds s.mu.
func (s *Server) attachNewDisks(inst *Instance, zone, field string, bulk bool) ([]*Disk, *apiError) {
	if len(inst.Disks) == 0 || !inst.Disks[0].Boot {
		return nil, invalid("Invalid value for field '%s.disks': ''. The first disk must be a boot disk.", field)
	}
	var created []*Disk
	devices := map[string]bool{}
	disks := slices.Clone(inst.Disks)
	for i := range disks {
		ad := &disks[i]
		f := fmt.Sprintf("%s.disks[%d]", field, i)
		if i > 0 && ad.Boot {
			return nil, invalid("Invalid value for field '%s.boot': true. Only the first disk can be a boot disk.", f)
		}
		ad.Kind, ad.Index = "compute#attachedDisk", i
		ad.Type, ad.Mode = cmp.Or(ad.Type, "PERSISTENT"), cmp.Or(ad.Mode, "READ_WRITE")
		ad.Interface = cmp.Or(ad.Interface, "SCSI")
		if ad.Type != "PERSISTENT" && ad.Type != "SCRATCH" || ad.Mode != "READ_WRITE" && ad.Mode != "READ_ONLY" {
			return nil, invalid("Invalid value for field '%s': type '%s' and mode '%s'.", f, ad.Type, ad.Mode)
		}
		ad.DeviceName = cmp.Or(ad.DeviceName, fmt.Sprintf("persistent-disk-%d", i))
		if devices[ad.DeviceName] {
			return nil, invalid("Invalid value for field '%s.deviceName': '%s'. Device names must be unique.", f, ad.DeviceName)
		}
		devices[ad.DeviceName] = true
		switch {
		case ad.Source != "" && ad.InitializeParams != nil:
			return nil, invalid("Invalid value for field '%s': a disk takes either a source or initializeParams.", f)
		case ad.Source != "":
			if bulk {
				return nil, invalid("Invalid value for field '%s.source': bulk insertions cannot attach existing disks.", f)
			}
			d, err := s.attachable(zone, f+".source", ad.Source, ad.Mode, inst.SelfLink)
			if err != nil {
				return nil, err
			}
			ad.Source, ad.DiskSizeGB = d.SelfLink, d.SizeGB
		case ad.Type == "SCRATCH":
			ad.AutoDelete, ad.DiskSizeGB, ad.InitializeParams = true, 375, nil
		case ad.InitializeParams == nil:
			return nil, invalid("Invalid value for field '%s': a disk needs a source or initializeParams.", f)
		default:
			ip := ad.InitializeParams
			if bulk && ip.DiskName != "" {
				return nil, invalid("Invalid value for field '%s.initializeParams.diskName': '%s'. Specifying name for disks created during bulk insertions is not supported.", f, ip.DiskName)
			}
			name := ip.DiskName
			if name == "" {
				name = inst.Name
				if !ad.Boot {
					name = fmt.Sprintf("%s-%d", inst.Name, i)
				}
			}
			d, err := s.newDisk(zone, f+".initializeParams", Disk{
				Name: name, Description: ip.Description, SizeGB: cmp.Or(ip.DiskSizeGB, ad.DiskSizeGB), Type: ip.DiskType,
				SourceImage: ip.SourceImage, SourceSnapshot: ip.SourceSnapshot, Labels: maps.Clone(ip.Labels),
				ResourcePolicies: ip.ResourcePolicies, ProvisionedIops: ip.ProvisionedIops, ProvisionedThroughput: ip.ProvisionedThroughput,
				Architecture: ip.Architecture, DiskEncryptionKey: ad.DiskEncryptionKey,
				SourceImageEncryptionKey: ip.SourceImageEncryptionKey, SourceSnapshotEncryptionKey: ip.SourceSnapshotEncryptionKey,
			})
			if err != nil {
				return nil, err
			}
			if slices.ContainsFunc(created, func(c *Disk) bool { return c.Name == d.Name }) {
				return nil, alreadyExists(diskPath(zone, d.Name))
			}
			created = append(created, d)
			ad.Source, ad.DiskSizeGB, ad.InitializeParams = d.SelfLink, d.SizeGB, nil
		}
	}
	inst.Disks = disks
	return created, nil
}

// attachable returns the existing disk ref names, if user can attach it in
// mode. The caller holds s.mu.
func (s *Server) attachable(zone, field, ref, mode, user string) (*Disk, *apiError) {
	inZone, name, ok := strings.Cut(relPath(ref, projectOf(zone)), "/disks/")
	if !ok || inZone != zone {
		return nil, invalid("Invalid value for field '%s': '%s'. The disk must be in zone '%s'.", field, ref, zone)
	}
	d, ok := lookupIn(s.disks, zone, name)
	if !ok {
		return nil, notFound(diskPath(zone, name))
	}
	if d.Status != diskReady {
		return nil, notReady(diskPath(zone, name))
	}
	if len(d.Users) > 0 && mode == "READ_WRITE" {
		return nil, inUse("disk", diskPath(zone, name), strings.TrimPrefix(d.Users[0], selfLinkBase))
	}
	if slices.Contains(d.Users, user) {
		return nil, invalid("Invalid value for field '%s': '%s'. The disk is already attached.", field, ref)
	}
	return d, nil
}

func projectOf(scope string) string {
	project, _, _ := strings.Cut(strings.TrimPrefix(scope, "projects/"), "/")
	return project
}

// configureInterfaces checks inst's network interfaces and returns the fixed
// IP addresses they claim. The caller holds s.mu.
func (s *Server) configureInterfaces(inst *Instance, region, field string, bulk bool) ([]claim, *apiError) {
	if len(inst.NetworkInterfaces) == 0 || len(inst.NetworkInterfaces) > maxNetworkInterfaces {
		return nil, invalid("Invalid value for field '%s.networkInterfaces': an instance needs 1 to %d network interfaces.", field, maxNetworkInterfaces)
	}
	project := projectOf(region)
	var claims []claim
	nics := slices.Clone(inst.NetworkInterfaces)
	for i := range nics {
		nic := &nics[i]
		f := fmt.Sprintf("%s.networkInterfaces[%d]", field, i)
		nic.Kind, nic.Name, nic.StackType = "compute#networkInterface", fmt.Sprintf("nic%d", i), cmp.Or(nic.StackType, "IPV4_ONLY")
		if nic.Subnetwork != "" {
			inRegion, name, ok := strings.Cut(relPath(nic.Subnetwork, project), "/subnetworks/")
			if !ok || inRegion != region || !validName(name) {
				return nil, invalid("Invalid value for field '%s.subnetwork': '%s'. The subnetwork must be in region '%s'.", f, nic.Subnetwork, region)
			}
			nic.Subnetwork = selfLinkBase + inRegion + "/subnetworks/" + name
		}
		if nic.Network != "" || nic.Subnetwork == "" {
			ref := cmp.Or(nic.Network, "default")
			if !strings.Contains(ref, "/") {
				ref = "global/networks/" + ref
			}
			p := relPath(ref, project)
			if _, name, ok := strings.Cut(p, "/global/networks/"); !ok || !validName(name) {
				return nil, invalid("Invalid value for field '%s.network': '%s'.", f, nic.Network)
			}
			nic.Network = selfLinkBase + p
		}
		nic.Fingerprint = hash([]any{inst.ID, i})
		if nic.NetworkIP != "" {
			if bulk {
				return nil, invalid("Invalid value for field '%s.networkIP': '%s'. Bulk insertions cannot set an internal IP address.", f, nic.NetworkIP)
			}
			c, err := s.claimIP(region, addressInternal, f+".networkIP", nic.NetworkIP)
			if err != nil {
				return nil, err
			}
			nic.NetworkIP = c.key[strings.LastIndexByte(c.key, '/')+1:]
			claims = append(claims, c)
		}
		if len(nic.AccessConfigs) > 1 {
			return nil, invalid("Invalid value for field '%s.accessConfigs': At most one access config currently supported.", f)
		}
		nic.AccessConfigs = slices.Clone(nic.AccessConfigs)
		for j := range nic.AccessConfigs {
			ac := &nic.AccessConfigs[j]
			c, err := s.configureAccess(ac, region, fmt.Sprintf("%s.accessConfigs[%d]", f, j), bulk)
			if err != nil {
				return nil, err
			}
			if c.key != "" {
				claims = append(claims, c)
			}
		}
	}
	inst.NetworkInterfaces = nics
	return claims, nil
}

// configureAccess fills in ac and returns the reserved address it claims, if
// any. The caller holds s.mu.
func (s *Server) configureAccess(ac *AccessConfig, region, field string, bulk bool) (claim, *apiError) {
	ac.Kind, ac.Type = "compute#accessConfig", cmp.Or(ac.Type, "ONE_TO_ONE_NAT")
	ac.Name, ac.NetworkTier = cmp.Or(ac.Name, "External NAT"), cmp.Or(ac.NetworkTier, "PREMIUM")
	if ac.Type != "ONE_TO_ONE_NAT" {
		return claim{}, invalid("Invalid value for field '%s.type': '%s'.", field, ac.Type)
	}
	if ac.NatIP == "" {
		ac.ephemeral = true
		return claim{}, nil
	}
	if bulk {
		return claim{}, invalid("Invalid value for field '%s.natIP': '%s'. Bulk insertions cannot set an external IP address.", field, ac.NatIP)
	}
	c, err := s.claimIP(region, addressExternal, field+".natIP", ac.NatIP)
	if err != nil {
		return claim{}, err
	}
	if c.address == nil {
		return claim{}, invalid("Invalid value for field '%s.natIP': '%s'. The specified external IP address '%s' was not found in region '%s'.", field, ac.NatIP, ac.NatIP, region[strings.LastIndexByte(region, '/')+1:])
	}
	ac.NatIP = c.address.Address
	return c, nil
}

// claimIP checks that an instance can take ip, of addressType, in region:
// it is free, or held by a reserved address there that nothing uses. The
// caller holds s.mu.
func (s *Server) claimIP(region, addressType, field, ip string) (claim, *apiError) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return claim{}, invalid("Invalid value for field '%s': '%s'. Invalid IPv4 address.", field, ip)
	}
	key := ipKey(region, addressType, addr.String())
	holder, taken := s.ipHolders[key]
	if !taken {
		return claim{key: key}, nil
	}
	if a := s.addressAt(holder); a != nil && a.AddressType == addressType && strings.HasPrefix(holder, region+"/") && len(a.Users) == 0 {
		return claim{key: key, address: a}, nil
	}
	return claim{}, invalid("IP address '%s' is already being used by another resource.", addr)
}

// addressAt returns the address at path, if it is one. The caller holds s.mu.
func (s *Server) addressAt(path string) *Address {
	region, name, ok := strings.Cut(path, "/addresses/")
	if !ok {
		return nil
	}
	a, _ := s.lookup(region, name)
	return a
}

// storeInstance adds inst, its new disks and its claims to the server. The caller holds s.mu.
func (s *Server) storeInstance(zone, region string, inst *Instance, disks []*Disk, claims []claim) {
	now := timestamp(s.now())
	for _, d := range disks {
		d.Status = diskCreating
		scoped(s.disks, zone).put(d.Name, d)
	}
	for _, ad := range inst.Disks {
		if d := s.diskAt(ad.Source); d != nil {
			d.Users, d.LastAttachTimestamp = append(d.Users, inst.SelfLink), now
		}
	}
	path := strings.TrimPrefix(inst.SelfLink, selfLinkBase)
	for _, c := range claims {
		if c.address != nil {
			c.address.Status, c.address.Users = "IN_USE", []string{inst.SelfLink}
			continue
		}
		s.ipHolders[c.key] = path
	}
	for i := range inst.NetworkInterfaces {
		nic := &inst.NetworkInterfaces[i]
		if nic.NetworkIP == "" {
			nic.NetworkIP = s.allocate(region, addressInternal)
			s.ipHolders[ipKey(region, addressInternal, nic.NetworkIP)] = path
		}
	}
	s.assignEphemeral(inst)
	scoped(s.instances, zone).put(inst.Name, inst)
}

// assignEphemeral gives inst's ephemeral access configs an external address.
// The caller holds s.mu.
func (s *Server) assignEphemeral(inst *Instance) {
	path := strings.TrimPrefix(inst.SelfLink, selfLinkBase)
	for i := range inst.NetworkInterfaces {
		for j := range inst.NetworkInterfaces[i].AccessConfigs {
			if ac := &inst.NetworkInterfaces[i].AccessConfigs[j]; ac.ephemeral && ac.NatIP == "" {
				ac.NatIP = s.allocate("", addressExternal)
				s.ipHolders[ac.NatIP] = path
			}
		}
	}
}

// releaseIPs gives up inst's addresses: only its ephemeral external ones when
// it stops, or all of them when it goes away. The caller holds s.mu.
func (s *Server) releaseIPs(inst *Instance, all bool) {
	zone := strings.TrimPrefix(inst.Zone, selfLinkBase)
	region := "projects/" + projectOf(zone) + "/regions/" + s.zoneRegion[zone[strings.LastIndexByte(zone, '/')+1:]]
	path := strings.TrimPrefix(inst.SelfLink, selfLinkBase)
	free := func(key string) {
		holder, ok := s.ipHolders[key]
		switch {
		case !ok:
		case holder == path:
			delete(s.ipHolders, key)
		default:
			if a := s.addressAt(holder); a != nil && slices.Contains(a.Users, inst.SelfLink) {
				a.Status, a.Users = "RESERVED", nil
			}
		}
	}
	for i := range inst.NetworkInterfaces {
		nic := &inst.NetworkInterfaces[i]
		if all {
			free(ipKey(region, addressInternal, nic.NetworkIP))
		}
		for j := range nic.AccessConfigs {
			ac := &nic.AccessConfigs[j]
			if ac.ephemeral && ac.NatIP != "" {
				free(ac.NatIP)
				ac.NatIP = ""
			} else if all && ac.NatIP != "" {
				free(ac.NatIP)
			}
		}
	}
}

// diskAt returns the disk link names. The caller holds s.mu.
func (s *Server) diskAt(link string) *Disk {
	zone, name, ok := strings.Cut(strings.TrimPrefix(link, selfLinkBase), "/disks/")
	if !ok {
		return nil
	}
	d, _ := lookupIn(s.disks, zone, name)
	return d
}

// provisioned is the effect of creating insts with disks: they run.
func (s *Server) provisioned(insts []*Instance, disks []*Disk) func() {
	return func() {
		now := timestamp(s.now())
		for _, inst := range insts {
			inst.Status, inst.LastStartTimestamp, inst.op = statusRunning, now, nil
		}
		for _, d := range disks {
			if d.Status == diskCreating {
				d.Status, d.op = diskReady, nil
			}
		}
	}
}

// track makes op the change in progress on insts and disks, while it is in
// progress. The caller holds s.mu.
func track(op *operation, insts []*Instance, disks []*Disk, staging bool) {
	if op.complete == nil {
		return
	}
	for _, inst := range insts {
		inst.op = op
	}
	for _, d := range disks {
		d.op = op
	}
	if staging {
		op.staging = insts
	}
}

func (s *Server) insertInstance(r *http.Request, opFault *apiError) (any, *apiError) {
	var in instanceInput
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
	}
	for _, param := range []string{"sourceInstanceTemplate", "sourceMachineImage"} {
		if r.URL.Query().Has(param) {
			return nil, invalid("Invalid value for field '%s': squall does not support creating an instance from a template or machine image.", param)
		}
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	zone, region := scopeOf(r), s.regionOfZone(r)
	path := instancePath(zone, in.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.instances, zone, in.Name); ok {
		return nil, alreadyExists(path)
	}
	inst, disks, claims, err := s.newInstance(zone, region, in.Name, "resource", in.Instance, false)
	if err != nil {
		return nil, err
	}
	for _, d := range disks {
		if _, ok := lookupIn(s.disks, zone, d.Name); ok {
			return nil, alreadyExists(diskPath(zone, d.Name))
		}
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	s.storeInstance(zone, region, inst, disks, claims)
	insts := []*Instance{inst}
	op := s.startOperation(r, reqID, "insert", path, inst.ID, nil, s.provisioned(insts, disks))
	track(op, insts, disks, true)
	return encoded(op), nil
}

// bulkInsertInput is an instances.bulkInsert body.
type bulkInsertInput struct {
	Count                  int64                       `json:"count,string"`
	MinCount               int64                       `json:"minCount,string"`
	NamePattern            string                      `json:"namePattern"`
	PerInstanceProperties  map[string]perInstanceInput `json:"perInstanceProperties"`
	InstanceProperties     json.RawMessage             `json:"instanceProperties"`
	SourceInstanceTemplate string                      `json:"sourceInstanceTemplate"`
	LocationPolicy         json.RawMessage             `json:"locationPolicy"`
}

type perInstanceInput struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
}

// withBulkTarget makes a bulkInsert's fault rule resource the first instance
// it names, in name order, or the zone's instances when it names none.
func withBulkTarget(r *http.Request, collection string) string {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var in struct {
		PerInstanceProperties map[string]json.RawMessage `json:"perInstanceProperties"`
	}
	if err != nil || json.Unmarshal(body, &in) != nil || len(in.PerInstanceProperties) == 0 {
		return collection
	}
	return collection + "/" + slices.Min(slices.Collect(maps.Keys(in.PerInstanceProperties)))
}

// mergePatch applies patch to target as a JSON Merge Patch (RFC 7396), as a
// bulkInsert applies its instanceProperties to its sourceInstanceTemplate.
func mergePatch(target, patch json.RawMessage) json.RawMessage {
	if len(patch) == 0 {
		return target
	}
	return encoded(mergeValues(decodeValue(target), decodeValue(patch)))
}

func mergeValues(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
		} else {
			t[k] = mergeValues(t[k], v)
		}
	}
	return t
}

// decodeValue decodes JSON the server has already checked, keeping numbers exact.
func decodeValue(b []byte) any {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	return v
}

// bulkNames are the names a bulkInsert creates: those it lists, in order, and
// then as many from its pattern as count asks for. The caller holds s.mu.
func (s *Server) bulkNames(zone string, in *bulkInsertInput) ([]string, *apiError) {
	names := slices.Sorted(maps.Keys(in.PerInstanceProperties))
	for _, name := range names {
		if p := in.PerInstanceProperties[name]; p.Name != "" && p.Name != name {
			return nil, invalid("Invalid value for field 'resource.perInstanceProperties': name '%s' does not match its key '%s'.", p.Name, name)
		}
	}
	extra := int(in.Count) - len(names)
	switch {
	case extra < 0:
		return nil, invalid("Invalid value for field 'resource.count': '%d'. Count must not be smaller than the number of instances in perInstanceProperties.", in.Count)
	case extra == 0:
		return names, nil
	case in.NamePattern == "":
		return nil, invalid("Invalid value for field 'resource.namePattern': ''. A name pattern is required when count exceeds the number of instances in perInstanceProperties.")
	}
	prefix, rest, _ := strings.Cut(in.NamePattern, "#")
	width := 1 + len(rest) - len(strings.TrimLeft(rest, "#"))
	suffix := strings.TrimLeft(rest, "#")
	if !strings.Contains(in.NamePattern, "#") || strings.Contains(suffix, "#") || width > 18 {
		return nil, invalid("Invalid value for field 'resource.namePattern': '%s'. The pattern must hold one run of 1 to 18 '#' characters.", in.NamePattern)
	}
	next := uint64(0)
	if c := s.instances[zone]; c != nil {
		for _, name := range c.names {
			digits, hasPrefix := strings.CutPrefix(name, prefix)
			if digits, ok := strings.CutSuffix(digits, suffix); hasPrefix && ok && len(digits) == width {
				if n, err := strconv.ParseUint(digits, 10, 64); err == nil {
					next = max(next, n)
				}
			}
		}
	}
	for len(names) < int(in.Count) {
		next++
		name := fmt.Sprintf("%s%0*d%s", prefix, width, next, suffix)
		if len(strconv.FormatUint(next, 10)) > width {
			return nil, invalid("Invalid value for field 'resource.namePattern': '%s'. The pattern has run out of names.", in.NamePattern)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (s *Server) bulkInsertInstances(r *http.Request, opFault *apiError) (any, *apiError) {
	var in bulkInsertInput
	if err := decodeStrict(r, &in); err != nil {
		return nil, err
	}
	if in.Count < 1 || in.Count > maxBulkCount {
		return nil, invalid("Invalid value for field 'resource.count': '%d'. Count must be between 1 and %d.", in.Count, maxBulkCount)
	}
	in.MinCount = cmp.Or(in.MinCount, in.Count)
	if in.MinCount < 1 || in.MinCount > in.Count {
		return nil, invalid("Invalid value for field 'resource.minCount': '%d'. Must be between 1 and count.", in.MinCount)
	}
	props, field := &Instance{}, "resource.instanceProperties"
	if string(in.InstanceProperties) == "null" {
		in.InstanceProperties = nil
	}
	switch {
	case in.InstanceProperties != nil:
		if err := unmarshalStrict(in.InstanceProperties, props); err != nil {
			return nil, err
		}
	case in.SourceInstanceTemplate == "":
		return nil, invalid("Invalid value for field 'resource': instanceProperties or sourceInstanceTemplate must be set.")
	default:
		field = "resource.sourceInstanceTemplate"
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	zone, region := scopeOf(r), s.regionOfZone(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if in.SourceInstanceTemplate != "" {
		template, err := s.templateProperties(projectOf(zone), in.SourceInstanceTemplate)
		if err != nil {
			return nil, err
		}
		*props = Instance{}
		if err := unmarshalStrict(mergePatch(template, in.InstanceProperties), props); err != nil {
			return nil, err
		}
	}
	if props.Name != "" {
		return nil, invalid("Invalid JSON payload received. Unknown name \"name\" at '%s': Cannot find field.", field)
	}
	names, err := s.bulkNames(zone, &in)
	if err != nil {
		return nil, err
	}
	type built struct {
		inst   *Instance
		disks  []*Disk
		claims []claim
	}
	var (
		batch     []built
		insts     []*Instance
		disks     []*Disk
		conflicts []operationError
		creating  = map[string]bool{}
	)
	for _, name := range names {
		p := *props
		p.Hostname = cmp.Or(in.PerInstanceProperties[name].Hostname, p.Hostname)
		inst, d, c, err := s.newInstance(zone, region, name, field, p, true)
		if err != nil {
			return nil, err
		}
		if _, ok := lookupIn(s.instances, zone, name); ok {
			conflicts = append(conflicts, operationError{"RESOURCE_ALREADY_EXISTS", alreadyExists(instancePath(zone, name)).Message})
		}
		for _, disk := range d {
			if _, ok := lookupIn(s.disks, zone, disk.Name); ok || creating[disk.Name] {
				conflicts = append(conflicts, operationError{"RESOURCE_ALREADY_EXISTS", alreadyExists(diskPath(zone, disk.Name)).Message})
			}
			creating[disk.Name] = true
		}
		batch = append(batch, built{inst, d, c})
		insts, disks = append(insts, inst), append(disks, d...)
	}
	conflicted := len(conflicts) > 0 && opFault == nil
	if conflicted {
		opFault = &apiError{
			Status: http.StatusConflict, Reason: "resourceAlreadyExists", Message: conflicts[0].Message,
			opErrors: append([]operationError{{"VM_MIN_COUNT_NOT_REACHED", fmt.Sprintf("Requested minimum count of %d VMs could not be created.", in.MinCount)}}, conflicts...),
		}
	}
	location := "zones/" + r.PathValue("zone")
	status := bulkInsertStatus{Status: "DONE", TargetVMCount: len(names)}
	metadata := &bulkInsertMetadata{map[string]bulkInsertStatus{location: status}}
	target := "projects/" + r.PathValue("project")
	if opFault != nil {
		op := s.startOperation(r, reqID, "bulkInsert", target, 0, opFault, nil)
		if !conflicted {
			status.FailedToCreateVMCount = len(names)
		}
		metadata.PerLocationStatus[location], op.BulkInsert = status, metadata
		return encoded(op), nil
	}
	for _, b := range batch {
		s.storeInstance(zone, region, b.inst, b.disks, b.claims)
	}
	created := status
	created.CreatedVMCount = len(names)
	provisioned := s.provisioned(insts, disks)
	op := s.startOperation(r, reqID, "bulkInsert", target, 0, nil, func() {
		provisioned()
		metadata.PerLocationStatus[location] = created
	})
	if op.complete != nil {
		status.Status = "CREATING"
		metadata.PerLocationStatus[location] = status
	}
	op.BulkInsert = metadata
	track(op, insts, disks, true)
	return encoded(op), nil
}
