package ec2

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	archX86            = "x86_64"
	defaultRootDevice  = "/dev/xvda"
	defaultVolumeSize  = 8
	maxInstanceTypes   = 100
	defaultImageVolume = "gp3"
)

var architectures = []string{"i386", archX86, "arm64", "x86_64_mac", "arm64_mac"}

// Image is an Amazon Machine Image (AMI).
type Image struct {
	// Region is where the image is. The server sets it.
	Region      string
	ImageID     string
	Name        string
	Description string
	// Architecture is "x86_64" unless set.
	Architecture string
	// RootDeviceName is "/dev/xvda" unless set.
	RootDeviceName string
	// BlockDeviceMappings are the volumes an instance launches with. Unless
	// set, the image has an 8 GiB gp3 root volume deleted on termination.
	BlockDeviceMappings []BlockDeviceMapping
	// OwnerID is empty for an image of the caller's own, which the owner
	// "self" matches.
	OwnerID      string
	OwnerAlias   string
	Public       bool
	CreationDate time.Time
	Tags         map[string]string
}

// BlockDeviceMapping is an EBS volume an image or instance has at a device.
type BlockDeviceMapping struct {
	DeviceName string
	SnapshotID string
	// VolumeSize is in GiB.
	VolumeSize          int
	VolumeType          string
	Iops                int
	Throughput          int
	DeleteOnTermination bool
	Encrypted           bool
}

func (img *Image) clone() Image {
	c := *img
	c.BlockDeviceMappings = slices.Clone(img.BlockDeviceMappings)
	c.Tags = maps.Clone(img.Tags)
	return c
}

type ebsXML struct {
	SnapshotID          string `xml:"snapshotId,omitempty"`
	VolumeSize          int    `xml:"volumeSize"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
	VolumeType          string `xml:"volumeType"`
	Iops                int    `xml:"iops,omitempty"`
	Throughput          int    `xml:"throughput,omitempty"`
	Encrypted           bool   `xml:"encrypted"`
}

type blockDeviceMappingXML struct {
	DeviceName string `xml:"deviceName"`
	EBS        ebsXML `xml:"ebs"`
}

type imageXML struct {
	ImageID             string                  `xml:"imageId"`
	ImageLocation       string                  `xml:"imageLocation"`
	ImageState          string                  `xml:"imageState"`
	ImageOwnerID        string                  `xml:"imageOwnerId"`
	CreationDate        string                  `xml:"creationDate"`
	IsPublic            bool                    `xml:"isPublic"`
	Architecture        string                  `xml:"architecture"`
	ImageType           string                  `xml:"imageType"`
	ImageOwnerAlias     string                  `xml:"imageOwnerAlias,omitempty"`
	Name                string                  `xml:"name"`
	Description         string                  `xml:"description,omitempty"`
	RootDeviceType      string                  `xml:"rootDeviceType"`
	RootDeviceName      string                  `xml:"rootDeviceName"`
	BlockDeviceMappings []blockDeviceMappingXML `xml:"blockDeviceMapping>item"`
	VirtualizationType  string                  `xml:"virtualizationType"`
	Tags                []tagXML                `xml:"tagSet>item,omitempty"`
	Hypervisor          string                  `xml:"hypervisor"`
	EnaSupport          bool                    `xml:"enaSupport"`
}

func (img *Image) xml() imageXML {
	out := imageXML{
		ImageID: img.ImageID, ImageLocation: img.Name, ImageState: statusAvailable, ImageOwnerID: img.OwnerID,
		CreationDate: img.CreationDate.UTC().Format(timeFormat), IsPublic: img.Public, Architecture: img.Architecture,
		ImageType: "machine", ImageOwnerAlias: img.OwnerAlias, Name: img.Name, Description: img.Description,
		RootDeviceType: "ebs", RootDeviceName: img.RootDeviceName, VirtualizationType: "hvm",
		Tags: tagSet(img.Tags), Hypervisor: "xen", EnaSupport: true,
	}
	for _, m := range img.BlockDeviceMappings {
		out.BlockDeviceMappings = append(out.BlockDeviceMappings, blockDeviceMappingXML{m.DeviceName, ebsXML{
			SnapshotID: m.SnapshotID, VolumeSize: m.VolumeSize, DeleteOnTermination: m.DeleteOnTermination,
			VolumeType: m.VolumeType, Iops: m.Iops, Throughput: m.Throughput, Encrypted: m.Encrypted,
		}})
	}
	return out
}

func imageFields(img *Image) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "architecture":
			return one(img.Architecture), true
		case "description":
			return one(img.Description), true
		case "image-id":
			return one(img.ImageID), true
		case "image-type":
			return []string{"machine"}, true
		case "is-public":
			return []string{strconv.FormatBool(img.Public)}, true
		case "name":
			return one(img.Name), true
		case "owner-alias":
			return one(img.OwnerAlias), true
		case "owner-id":
			return one(img.OwnerID), true
		case "root-device-name":
			return one(img.RootDeviceName), true
		case "root-device-type":
			return []string{"ebs"}, true
		case "state":
			return []string{statusAvailable}, true
		case "virtualization-type":
			return []string{"hvm"}, true
		}
		return tagFilter(img.Tags, name)
	}
}

func (img *Image) ownedBy(owners []string) bool {
	return len(owners) == 0 || slices.ContainsFunc(owners, func(o string) bool {
		return o == "self" && img.OwnerID == "" || o != "" && (o == img.OwnerID || o == img.OwnerAlias)
	})
}

func checkImageIDs(ids []string) *apiError {
	for _, id := range ids {
		if !strings.HasPrefix(id, "ami-") {
			return clientError("InvalidAMIID.Malformed", "Invalid id: %q (expecting \"ami-...\")", id)
		}
	}
	return nil
}

func imageNotFound(ids ...string) *apiError {
	return clientError("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", strings.Join(ids, ", "))
}

// image returns the image with id. The caller holds s.mu.
func (rs *regionState) image(id string) (*Image, *apiError) {
	if err := checkImageIDs([]string{id}); err != nil {
		return nil, err
	}
	img, ok := rs.images[id]
	if !ok {
		return nil, imageNotFound(id)
	}
	return img, nil
}

type describeImagesResponse struct {
	meta
	Images    []imageXML `xml:"imagesSet>item"`
	NextToken string     `xml:"nextToken,omitempty"`
}

func (s *Server) describeImages(c *call) (response, *apiError) {
	ids, owners := c.p.list("ImageId"), c.p.list("Owner")
	if err := checkImageIDs(ids); err != nil {
		return nil, err
	}
	filters, err := c.p.filters(imageFields(&Image{}))
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"IncludeDeprecated", "IncludeDisabled"} {
		if _, err := c.p.bool(key, false); err != nil {
			return nil, err
		}
	}
	limit, after, err := c.p.page("ImageIds", len(ids) > 0, maxResults, "ami")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	var missing []string
	for _, id := range ids {
		if _, ok := rs.images[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, imageNotFound(missing...)
	}
	var matched []*Image
	for _, id := range slices.Sorted(maps.Keys(rs.images)) {
		img := rs.images[id]
		if (len(ids) == 0 || slices.Contains(ids, id)) && img.ownedBy(owners) && matchFilters(filters, imageFields(img)) {
			matched = append(matched, img)
		}
	}
	page, next := paginate(matched, func(img *Image) string { return img.ImageID }, after, limit)
	resp := &describeImagesResponse{NextToken: next}
	for _, img := range page {
		resp.Images = append(resp.Images, img.xml())
	}
	return resp, nil
}

// PutImage registers img in region, replacing an image with the same ID, and
// returns it as stored. It fills in what img leaves empty, such as the ID,
// architecture and root volume.
func (s *Server) PutImage(region string, img Image) (Image, error) {
	if img.ImageID != "" && !strings.HasPrefix(img.ImageID, "ami-") {
		return Image{}, invalidValue("Invalid image ID '%s'.", img.ImageID)
	}
	if img.Architecture == "" {
		img.Architecture = archX86
	}
	if !slices.Contains(architectures, img.Architecture) {
		return Image{}, invalidValue("Invalid architecture '%s'.", img.Architecture)
	}
	if err := validateTags(img.Tags); err != nil {
		return Image{}, err
	}
	if img.RootDeviceName == "" {
		img.RootDeviceName = defaultRootDevice
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(img.BlockDeviceMappings) == 0 {
		img.BlockDeviceMappings = []BlockDeviceMapping{{
			DeviceName: img.RootDeviceName, SnapshotID: s.newID("snap"), VolumeSize: defaultVolumeSize, DeleteOnTermination: true,
		}}
	}
	img.BlockDeviceMappings = slices.Clone(img.BlockDeviceMappings)
	for i := range img.BlockDeviceMappings {
		m := &img.BlockDeviceMappings[i]
		if m.DeviceName == "" {
			return Image{}, errors.New("ec2: an image's block device mapping needs a device name")
		}
		if m.VolumeType == "" {
			m.VolumeType = defaultImageVolume
		}
		if m.VolumeSize == 0 {
			m.VolumeSize = defaultVolumeSize
		}
		if err := checkVolume(*m); err != nil {
			return Image{}, err
		}
		fillVolume(m)
	}
	if img.CreationDate.IsZero() {
		img.CreationDate = time.Now()
	}
	rs := s.region(region)
	if img.ImageID == "" {
		img.ImageID = unusedID(s, "ami", rs.images)
	}
	img.Region = region
	stored := img.clone()
	rs.images[img.ImageID] = &stored
	return stored.clone(), nil
}

// Images returns every image the server holds, ordered by region and ID.
func (s *Server) Images() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Image
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.images)) {
			out = append(out, rs.images[id].clone())
		}
	}
	return out
}

// InstanceType is an instance type DescribeInstanceTypes reports and
// RunInstances accepts.
type InstanceType struct {
	InstanceType string
	// Architectures are the architectures the type runs, such as "x86_64".
	Architectures []string
	VCPUs         int
	MemoryMiB     int
}

var builtinInstanceTypes = map[string]InstanceType{}

func init() {
	for _, t := range []InstanceType{
		{defaultInstanceType, []string{"i386", archX86}, 1, 1740},
		{"t3.micro", []string{archX86}, 2, 1024},
		{"t3.small", []string{archX86}, 2, 2048},
		{"t3.medium", []string{archX86}, 2, 4096},
		{"t3.large", []string{archX86}, 2, 8192},
		{"m5.large", []string{archX86}, 2, 8192},
		{"m5.xlarge", []string{archX86}, 4, 16384},
		{"c5.large", []string{archX86}, 2, 4096},
		{"c5.xlarge", []string{archX86}, 4, 8192},
		{"t4g.micro", []string{"arm64"}, 2, 1024},
		{"c7g.large", []string{"arm64"}, 2, 4096},
	} {
		builtinInstanceTypes[t.InstanceType] = t
	}
}

// PutInstanceType adds t to the instance types the server knows, replacing
// one with the same name. Its architecture is "x86_64" unless set.
func (s *Server) PutInstanceType(t InstanceType) error {
	if t.InstanceType == "" {
		return errors.New("ec2: an instance type needs a name")
	}
	if len(t.Architectures) == 0 {
		t.Architectures = []string{archX86}
	}
	for _, arch := range t.Architectures {
		if !slices.Contains(architectures, arch) {
			return invalidValue("Invalid architecture '%s'.", arch)
		}
	}
	t.Architectures = slices.Clone(t.Architectures)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instanceTypes[t.InstanceType] = t
	return nil
}

// checkInstanceType checks that name is a known type that runs arch, failing
// with unknownCode for a type it does not know. The caller holds s.mu.
func (s *Server) checkInstanceType(name, arch, unknownCode string) *apiError {
	t, ok := s.instanceTypes[name]
	if !ok {
		return clientError(unknownCode, "Invalid value '%s' for InstanceType.", name)
	}
	if !slices.Contains(t.Architectures, arch) {
		return invalidValue("The architecture '%s' of the specified instance type does not match the architecture '%s' of the specified AMI.",
			strings.Join(t.Architectures, ", "), arch)
	}
	return nil
}

type instanceTypeXML struct {
	InstanceType      string `xml:"instanceType"`
	CurrentGeneration bool   `xml:"currentGeneration"`
	VCPUInfo          struct {
		DefaultVCPUs int `xml:"defaultVCpus"`
	} `xml:"vCpuInfo"`
	MemoryInfo struct {
		SizeInMiB int `xml:"sizeInMiB"`
	} `xml:"memoryInfo"`
	ProcessorInfo struct {
		SupportedArchitectures []string `xml:"supportedArchitectures>item"`
	} `xml:"processorInfo"`
	SupportedRootDeviceTypes     []string `xml:"supportedRootDeviceTypes>item"`
	SupportedVirtualizationTypes []string `xml:"supportedVirtualizationTypes>item"`
}

func instanceTypeFields(t *InstanceType) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "instance-type":
			return one(t.InstanceType), true
		case "processor-info.supported-architecture":
			return t.Architectures, true
		case "vcpu-info.default-vcpus":
			return []string{strconv.Itoa(t.VCPUs)}, true
		case "memory-info.size-in-mib":
			return []string{strconv.Itoa(t.MemoryMiB)}, true
		}
		return nil, false
	}
}

type describeInstanceTypesResponse struct {
	meta
	InstanceTypes []instanceTypeXML `xml:"instanceTypeSet>item"`
	NextToken     string            `xml:"nextToken,omitempty"`
}

func (s *Server) describeInstanceTypes(c *call) (response, *apiError) {
	names := c.p.list("InstanceType")
	filters, err := c.p.filters(instanceTypeFields(&InstanceType{}))
	if err != nil {
		return nil, err
	}
	if _, err := c.p.bool("IncludeUnsupportedInRegion", false); err != nil {
		return nil, err
	}
	limit, after, err := c.p.page("InstanceTypes", false, maxInstanceTypes, "")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var missing []string
	for _, name := range names {
		if _, ok := s.instanceTypes[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, clientError("InvalidInstanceType", "The following supplied instance types do not exist: [%s]", strings.Join(missing, ", "))
	}
	var matched []InstanceType
	for _, name := range slices.Sorted(maps.Keys(s.instanceTypes)) {
		t := s.instanceTypes[name]
		if (len(names) == 0 || slices.Contains(names, name)) && matchFilters(filters, instanceTypeFields(&t)) {
			matched = append(matched, t)
		}
	}
	page, next := paginate(matched, func(t InstanceType) string { return t.InstanceType }, after, limit)
	resp := &describeInstanceTypesResponse{NextToken: next}
	for _, t := range page {
		out := instanceTypeXML{
			InstanceType: t.InstanceType, CurrentGeneration: t.InstanceType != defaultInstanceType,
			SupportedRootDeviceTypes: []string{"ebs"}, SupportedVirtualizationTypes: []string{"hvm"},
		}
		out.VCPUInfo.DefaultVCPUs, out.MemoryInfo.SizeInMiB = t.VCPUs, t.MemoryMiB
		out.ProcessorInfo.SupportedArchitectures = t.Architectures
		resp.InstanceTypes = append(resp.InstanceTypes, out)
	}
	return resp, nil
}
