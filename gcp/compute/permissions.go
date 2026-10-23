package compute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need a permission p lacks,
// as Compute Engine refuses them, and records what each call needed. Without
// it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// basePermissions is what Compute Engine checks for each method, from each
// method's "IAM Permissions" in the REST reference.
var basePermissions = map[string][]string{
	"compute.addresses.list":      {"compute.addresses.list"},
	"compute.addresses.insert":    {"compute.addresses.create|compute.addresses.createInternal"},
	"compute.addresses.get":       {"compute.addresses.get"},
	"compute.addresses.delete":    {"compute.addresses.delete|compute.addresses.deleteInternal"},
	"compute.addresses.setLabels": {"compute.addresses.setLabels"},

	"compute.zones.list": {"compute.zones.list"},
	"compute.zones.get":  {"compute.zones.get"},

	"compute.instances.bulkInsert":      {"compute.instances.create"},
	"compute.instances.insert":          {"compute.instances.create"},
	"compute.instances.list":            {"compute.instances.list"},
	"compute.instances.aggregatedList":  {"compute.instances.list"},
	"compute.instances.get":             {"compute.instances.get"},
	"compute.instances.delete":          {"compute.instances.delete"},
	"compute.instances.stop":            {"compute.instances.stop"},
	"compute.instances.start":           {"compute.instances.start"},
	"compute.instances.setLabels":       {"compute.instances.setLabels"},
	"compute.instances.setMachineType":  {"compute.instances.setMachineType"},
	"compute.instances.addAccessConfig": {"compute.instances.addAccessConfig", "compute.subnetworks.useExternalIp|compute.networks.useExternalIp"},
	"compute.instances.attachDisk":      {"compute.instances.attachDisk"},
	"compute.instances.detachDisk":      {"compute.instances.detachDisk"},

	"compute.disks.list":           {"compute.disks.list"},
	"compute.disks.aggregatedList": {"compute.disks.list"},
	"compute.disks.insert":         {"compute.disks.create"},
	"compute.disks.get":            {"compute.disks.get"},
	"compute.disks.delete":         {"compute.disks.delete"},

	"compute.snapshots.list":   {"compute.snapshots.list"},
	"compute.snapshots.insert": {"compute.snapshots.create"},
	"compute.snapshots.get":    {"compute.snapshots.get"},
	"compute.snapshots.delete": {"compute.snapshots.delete"},

	"compute.resourcePolicies.list":   {"compute.resourcePolicies.list"},
	"compute.resourcePolicies.insert": {"compute.resourcePolicies.create"},
	"compute.resourcePolicies.get":    {"compute.resourcePolicies.get"},
	"compute.resourcePolicies.delete": {"compute.resourcePolicies.delete"},

	"compute.instanceTemplates.list":   {"compute.instanceTemplates.list"},
	"compute.instanceTemplates.insert": {"compute.instanceTemplates.create"},
	"compute.instanceTemplates.get":    {"compute.instanceTemplates.get"},
	"compute.instanceTemplates.delete": {"compute.instanceTemplates.delete"},

	"compute.instanceGroupManagers.aggregatedList": {"compute.instanceGroupManagers.list"},

	"compute.routes.list":   {"compute.routes.list"},
	"compute.routes.insert": {"compute.routes.create", "compute.networks.updatePolicy"},
	"compute.routes.get":    {"compute.routes.get"},
	"compute.routes.delete": {"compute.routes.delete"},

	"compute.zoneOperations.get":    {"compute.zoneOperations.get"},
	"compute.zoneOperations.wait":   {"compute.zoneOperations.get"},
	"compute.regionOperations.get":  {"compute.regionOperations.get"},
	"compute.regionOperations.wait": {"compute.regionOperations.get"},
	"compute.globalOperations.get":  {"compute.globalOperations.get"},
	"compute.globalOperations.wait": {"compute.globalOperations.get"},
}

// requestFields is the part of a request body that adds permissions.
type requestFields struct {
	permissionFields
	InstanceProperties     *permissionFields `json:"instanceProperties"`
	SourceInstanceTemplate string            `json:"sourceInstanceTemplate"`
	Properties             *permissionFields `json:"properties"`

	AddressType    string `json:"addressType"`
	SourceImage    string `json:"sourceImage"`
	SourceSnapshot string `json:"sourceSnapshot"`
	SourceDisk     string `json:"sourceDisk"`
	Source         string `json:"source"`
	Mode           string `json:"mode"`
	NatIP          string `json:"natIP"`
}

type permissionFields struct {
	Disks []struct {
		Source           string `json:"source"`
		Mode             string `json:"mode"`
		InitializeParams *struct {
			SourceImage    string            `json:"sourceImage"`
			SourceSnapshot string            `json:"sourceSnapshot"`
			Labels         map[string]string `json:"labels"`
		} `json:"initializeParams"`
	} `json:"disks"`
	NetworkInterfaces []struct {
		Network       string `json:"network"`
		Subnetwork    string `json:"subnetwork"`
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
	Metadata           json.RawMessage   `json:"metadata"`
	Tags               json.RawMessage   `json:"tags"`
	Labels             map[string]string `json:"labels"`
	Subnetwork         string            `json:"subnetwork"`
	ServiceAccounts    []json.RawMessage `json:"serviceAccounts"`
	ResourcePolicies   []string          `json:"resourcePolicies"`
	DeletionProtection bool              `json:"deletionProtection"`
}

// needs returns every permission a call to method needs, reading the fields
// of r's body that add permissions and leaving the body for the handler.
func needs(method string, r *http.Request) []string {
	perms := basePermissions[method]
	if r.Method != http.MethodPost {
		return perms
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var f requestFields
	if err != nil || json.Unmarshal(body, &f) != nil {
		return perms
	}
	perms = append([]string(nil), perms...)
	add := func(cond bool, more ...string) {
		if cond {
			perms = append(perms, more...)
		}
	}
	switch method {
	case "compute.instances.insert":
		perms = append(perms, f.needs()...)
		add(f.DeletionProtection, "compute.instances.setDeletionProtection")
	case "compute.instances.bulkInsert":
		add(f.SourceInstanceTemplate != "", "compute.instanceTemplates.useReadOnly")
		if f.InstanceProperties != nil {
			perms = append(perms, f.InstanceProperties.needs()...)
		}
	case "compute.instanceTemplates.insert":
		if p := f.Properties; p != nil {
			for _, ni := range p.NetworkInterfaces {
				add(ni.Network != "", "compute.networks.get")
				add(ni.Subnetwork != "", "compute.subnetworks.get")
			}
			for _, d := range p.Disks {
				add(d.InitializeParams != nil && d.InitializeParams.SourceImage != "", "compute.images.get")
			}
		}
	case "compute.addresses.insert":
		add(len(f.Labels) > 0, "compute.addresses.setLabels")
		add(f.Subnetwork != "", "compute.subnetworks.use")
	case "compute.disks.insert":
		add(f.SourceImage != "", "compute.images.useReadOnly")
		add(f.SourceSnapshot != "", "compute.snapshots.useReadOnly")
		add(len(f.Labels) > 0, "compute.disks.setLabels")
	case "compute.snapshots.insert":
		add(f.SourceDisk != "", "compute.disks.createSnapshot")
		add(len(f.Labels) > 0, "compute.snapshots.setLabels")
	case "compute.instances.attachDisk":
		add(f.Source != "", diskUse(f.Mode))
	case "compute.instances.addAccessConfig":
		add(f.NatIP != "", "compute.addresses.use")
	}
	return perms
}

func (f *permissionFields) needs() []string {
	var perms []string
	add := func(cond bool, more ...string) {
		if cond {
			perms = append(perms, more...)
		}
	}
	for _, d := range f.Disks {
		if ip := d.InitializeParams; ip != nil {
			perms = append(perms, "compute.disks.create")
			add(ip.SourceImage != "", "compute.images.useReadOnly")
			add(ip.SourceSnapshot != "", "compute.snapshots.useReadOnly")
			add(len(ip.Labels) > 0, "compute.disks.setLabels")
		}
		add(d.Source != "", diskUse(d.Mode))
	}
	for _, ni := range f.NetworkInterfaces {
		add(ni.Network != "", "compute.networks.use")
		add(ni.Subnetwork != "", "compute.subnetworks.use")
		for _, ac := range ni.AccessConfigs {
			if ni.Subnetwork != "" {
				perms = append(perms, "compute.subnetworks.useExternalIp")
			} else {
				perms = append(perms, "compute.networks.useExternalIp")
			}
			add(ac.NatIP != "", "compute.addresses.use")
		}
	}
	add(len(f.Metadata) > 0 && string(f.Metadata) != "null", "compute.instances.setMetadata")
	add(len(f.Tags) > 0 && string(f.Tags) != "null", "compute.instances.setTags")
	add(len(f.Labels) > 0, "compute.instances.setLabels")
	add(len(f.ServiceAccounts) > 0, "compute.instances.setServiceAccount", "iam.serviceAccounts.actAs")
	add(len(f.ResourcePolicies) > 0, "compute.resourcePolicies.use")
	return perms
}

func diskUse(mode string) string {
	if mode == "READ_ONLY" {
		return "compute.disks.useReadOnly"
	}
	return "compute.disks.use"
}

func permissionDenied(missing []string, resource string) *apiError {
	perm, _, _ := strings.Cut(missing[0], "|")
	return &apiError{
		Status:  http.StatusForbidden,
		Reason:  "forbidden",
		Message: fmt.Sprintf("Required '%s' permission for '%s'", perm, resource),
	}
}
