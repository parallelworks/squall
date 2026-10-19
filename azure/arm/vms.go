package arm

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

var vmKind = &kind{
	typ: "Microsoft.Compute/virtualMachines", plural: "virtualMachines", typeName: "VirtualMachine",
	top:         []string{"extendedLocation", "identity", "placement", "plan", "zones"},
	topReadOnly: []string{"managedBy", "resources"},
	props: []string{
		"additionalCapabilities", "applicationProfile", "availabilitySet", "billingProfile", "capacityReservation",
		"diagnosticsProfile", "evictionPolicy", "extensionsTimeBudget", "hardwareProfile", "host", "hostGroup",
		"interconnectBlockProfile", "licenseType", "networkProfile", "osProfile", "platformFaultDomain", "priority",
		"proximityPlacementGroup", "resiliencyProfile", "scheduledEventsPolicy", "scheduledEventsProfile",
		"securityProfile", "storageProfile", "userData", "virtualMachineScaleSet",
	},
	readOnly: []string{"instanceView", "provisioningState", "timeCreated", "vmId"},
	located:  true, etag: quotedETag,
}

const (
	powerRunning      = "running"
	powerDeallocated  = "deallocated"
	powerStarting     = "starting"
	powerDeallocating = "deallocating"
)

type vmState struct {
	power    string
	userData any
	nics     []vmNIC
	changed  time.Time
}

type vmNIC struct {
	id      string
	primary bool
	delete  bool
}

// osProfileMembers are the members of a VM's osProfile; secret ones are not returned.
var osProfileMembers = []string{
	"adminPassword", "adminUsername", "allowExtensionOperations", "computerName", "customData",
	"linuxConfiguration", "requireGuestProvisionSignal", "secrets", "windowsConfiguration",
}

func vmID(c *call) string { return resourceID(c, vmKind.typ, c.val("name")) }

// vmBody is a parsed VM create or update.
type vmBody struct {
	*parsed
	nics     []vmNIC
	osDisk   map[string]any
	osName   string
	userData any
}

func parseVM(c *call) (*vmBody, *apiError) {
	p, err := parseBody(c, vmKind)
	if err != nil {
		return nil, err
	}
	b := &vmBody{parsed: p, userData: p.props["userData"]}
	delete(p.props, "userData")
	hw, err := object(p.props, "hardwareProfile")
	if err != nil {
		return nil, err
	}
	if str(hw, "vmSize") == "" {
		return nil, requiredParameter("hardwareProfile.vmSize")
	}
	np, err := object(p.props, "networkProfile")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(np, "NetworkProfile", "properties.networkProfile",
		"interconnectGroupProfile", "networkApiVersion", "networkInterfaceConfigurations", "networkInterfaces"); err != nil {
		return nil, err
	}
	if _, ok := np["networkInterfaceConfigurations"]; ok {
		return nil, unsupported("networkProfile.networkInterfaceConfigurations is not supported; create network interfaces and reference them")
	}
	refs, err := objects(np, "networkInterfaces")
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, requiredParameter("networkProfile")
	}
	for i, r := range refs {
		path := "properties.networkProfile.networkInterfaces[" + strconv.Itoa(i) + "]"
		if err := checkMembers(r, "NetworkInterfaceReference", path, "id", "properties"); err != nil {
			return nil, err
		}
		rp, err := object(r, "properties")
		if err != nil {
			return nil, err
		}
		if err := checkMembers(rp, "NetworkInterfaceReferenceProperties", path+".properties", "deleteOption", "primary"); err != nil {
			return nil, err
		}
		n := vmNIC{id: str(r, "id"), delete: strings.EqualFold(str(rp, "deleteOption"), "Delete")}
		n.primary, _ = boolean(rp, "primary")
		if n.id == "" {
			return nil, requiredParameter("networkProfile.networkInterfaces[" + strconv.Itoa(i) + "].id")
		}
		b.nics = append(b.nics, n)
	}
	if !slices.ContainsFunc(b.nics, func(n vmNIC) bool { return n.primary }) {
		b.nics[0].primary = true
	}
	storage, err := object(p.props, "storageProfile")
	if err != nil {
		return nil, err
	}
	if storage == nil {
		storage = map[string]any{}
		p.props["storageProfile"] = storage
	}
	if b.osDisk, err = object(storage, "osDisk"); err != nil {
		return nil, err
	}
	if b.osDisk == nil {
		b.osDisk = map[string]any{}
		storage["osDisk"] = b.osDisk
	}
	b.osName = str(b.osDisk, "name")
	os, err := object(p.props, "osProfile")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(os, "OSProfile", "properties.osProfile", osProfileMembers...); err != nil {
		return nil, err
	}
	if os != nil {
		delete(os, "adminPassword")
		delete(os, "customData")
	}
	return b, nil
}

// checkCreate checks what only a new VM needs.
func (b *vmBody) checkCreate() *apiError {
	if strings.EqualFold(str(b.osDisk, "createOption"), "Attach") {
		if refID(b.osDisk, "managedDisk") == "" {
			return requiredParameter("storageProfile.osDisk.managedDisk.id")
		}
		return nil
	}
	storage, _ := object(b.props, "storageProfile")
	if _, ok := storage["imageReference"]; !ok {
		return requiredParameter("storageProfile.imageReference")
	}
	os, _ := object(b.props, "osProfile")
	if os == nil {
		return requiredParameter("osProfile")
	}
	if str(os, "adminUsername") == "" {
		return requiredParameter("osProfile.adminUsername")
	}
	return nil
}

func (s *Server) putVM(c *call) (*reply, *apiError) {
	b, err := parseVM(c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	id := vmID(c)
	existing := s.settled(id)
	if err := checkPrecondition(c.r, existing); err != nil {
		return nil, err
	}
	res, status := existing, http.StatusOK
	if res == nil {
		if err := b.checkCreate(); err != nil {
			return nil, err
		}
		res, status = s.newResource(c, vmKind, id, b.parsed), http.StatusCreated
	} else if err := s.checkVMUpdate(res, b); err != nil {
		return nil, err
	}
	for i, n := range b.nics {
		target := s.settled(n.id)
		if target == nil || target.kind != nicKind || target.loc != b.location {
			return nil, invalidParameter("networkProfile.networkInterfaces["+strconv.Itoa(i)+"].id",
				"Network interface %s was not found in location %s.", n.id, b.location)
		}
		if vm := target.nic.vm; vm != "" && key(vm) != key(id) {
			return nil, newError(http.StatusBadRequest, "NicInUse", "Network Interface %s is used by existing resource %s.", target.id, vm)
		}
	}
	render := func() []byte { return encode(s.renderVM(res, "")) }
	paths := computeOps(c, b.location)
	if c.opFault != nil {
		op := s.begin(c, paths, 0, nil)
		if status == http.StatusCreated {
			s.fillVM(res, b)
		}
		return accepted(c, op, status, status, false, func() []byte { return withState(res, putState(status), render) }), nil
	}
	s.applyVM(res, b)
	s.resources[key(id)] = res
	op := s.change(c, res, paths, 0, putState(status), func() {
		if res.vm.power == powerStarting {
			res.vm.power, res.vm.changed = powerRunning, s.now()
		}
	})
	if op.status == opInProgress && status == http.StatusCreated {
		res.vm.power = powerStarting
	}
	return accepted(c, op, status, status, false, render), nil
}

// checkVMUpdate rejects an update that changes what a VM cannot change. The caller holds s.mu.
func (s *Server) checkVMUpdate(res *resource, b *vmBody) *apiError {
	if res.loc != b.location {
		return wrongLocation(res.name, res.loc, b.location, res.group)
	}
	if b.osName != "" && !strings.EqualFold(b.osName, str(osDiskOf(res), "name")) {
		return changeNotAllowed("osDisk.name")
	}
	oldOS, _ := object(res.props, "osProfile")
	newOS, _ := object(b.props, "osProfile")
	if name := str(newOS, "computerName"); name != "" && oldOS != nil && !strings.EqualFold(name, str(oldOS, "computerName")) {
		return changeNotAllowed("osProfile.computerName")
	}
	return nil
}

// fillVM completes b with what Azure assigns a new VM: the OS disk's name, ID
// and type, and the computer name. The caller holds s.mu.
func (s *Server) fillVM(res *resource, b *vmBody) {
	disk := b.osDisk
	if res.vm != nil {
		old := osDiskOf(res)
		for _, k := range []string{"name", "osType", "createOption", "deleteOption"} {
			if v, ok := old[k]; ok && str(disk, k) == "" {
				disk[k] = v
			}
		}
		md, _ := disk["managedDisk"].(map[string]any)
		md = maps.Clone(md)
		if md == nil {
			md = map[string]any{}
		}
		md["id"] = refID(old, "managedDisk")
		disk["managedDisk"] = md
		if os, _ := object(b.props, "osProfile"); os == nil {
			if old, ok := res.props["osProfile"]; ok {
				b.props["osProfile"] = old
			}
		}
		return
	}
	if str(disk, "createOption") == "" {
		disk["createOption"] = "FromImage"
	}
	if str(disk, "deleteOption") == "" {
		disk["deleteOption"] = "Detach"
	}
	md, _ := disk["managedDisk"].(map[string]any)
	md = maps.Clone(md)
	if md == nil {
		md = map[string]any{}
	}
	if str(md, "id") == "" {
		if str(disk, "name") == "" {
			disk["name"] = res.name + "_OsDisk_1_" + s.newHex()
		}
		md["id"] = diskID(res.sub, res.group, str(disk, "name"))
	} else if str(disk, "name") == "" {
		disk["name"] = md["id"].(string)[strings.LastIndex(md["id"].(string), "/")+1:]
	}
	disk["managedDisk"] = md
	if str(disk, "osType") == "" {
		disk["osType"] = "Linux"
		if os, _ := object(b.props, "osProfile"); os != nil && os["windowsConfiguration"] != nil {
			disk["osType"] = "Windows"
		}
	}
	if os, _ := object(b.props, "osProfile"); os != nil && str(os, "computerName") == "" {
		os["computerName"] = res.name
	}
}

// osDiskOf returns the recorded OS disk of the VM res.
func osDiskOf(res *resource) map[string]any {
	storage, _ := res.props["storageProfile"].(map[string]any)
	disk, _ := storage["osDisk"].(map[string]any)
	return disk
}

// diskID is the ID Azure gives a managed disk it creates, with the group
// upper-cased as Azure writes it.
func diskID(sub, group, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + strings.ToUpper(group) + "/providers/Microsoft.Compute/disks/" + name
}

// applyVM records b as the body of res and attaches its interfaces. The caller holds s.mu.
func (s *Server) applyVM(res *resource, b *vmBody) {
	s.fillVM(res, b)
	if res.vm == nil {
		res.vm = &vmState{power: powerRunning, changed: s.now()}
	}
	for _, n := range res.vm.nics {
		if nic := s.resources[key(n.id)]; nic != nil && key(nic.nic.vm) == key(res.id) {
			nic.nic.vm = ""
		}
	}
	res.vm.nics, res.vm.userData = b.nics, b.userData
	for _, n := range b.nics {
		s.resources[key(n.id)].nic.vm = res.id
	}
	s.update(res, b.parsed)
}

// primaryNIC returns the key of the primary interface of the VM id. The caller holds s.mu.
func (s *Server) primaryNIC(id string) string {
	vm := s.resources[key(id)]
	if vm == nil || vm.vm == nil {
		return ""
	}
	for _, n := range vm.vm.nics {
		if n.primary {
			return key(n.id)
		}
	}
	return ""
}

func (s *Server) renderVM(res *resource, expand string) map[string]any {
	m, props := envelope(res)
	props["vmId"] = res.guid
	props["timeCreated"] = timestamp(res.created)
	for e := range strings.SplitSeq(expand, ",") {
		switch strings.TrimSpace(e) {
		case "instanceView":
			props["instanceView"] = s.instanceView(res)
		case "userData":
			if res.vm.userData != nil {
				props["userData"] = res.vm.userData
			}
		}
	}
	return m
}

var provisioningDisplay = map[string]string{
	stateSucceeded: "Provisioning succeeded", stateCreating: "Creating", stateUpdating: "Updating", stateDeleting: "Deleting",
}

func statuses(state, power string, at time.Time) []any {
	return []any{
		map[string]any{
			"code": "ProvisioningState/" + strings.ToLower(state), "level": "Info",
			"displayStatus": provisioningDisplay[state], "time": timestamp(at),
		},
		map[string]any{"code": "PowerState/" + power, "level": "Info", "displayStatus": "VM " + power},
	}
}

func (s *Server) instanceView(res *resource) map[string]any {
	view := map[string]any{"statuses": statuses(res.state, res.vm.power, res.vm.changed)}
	if os, _ := object(res.props, "osProfile"); os != nil {
		view["computerName"] = str(os, "computerName")
	}
	return view
}

// checkExpand rejects an $expand the server does not model.
func checkExpand(expand string, allowed ...string) *apiError {
	if expand == "" {
		return nil
	}
	for e := range strings.SplitSeq(expand, ",") {
		if !slices.Contains(allowed, strings.TrimSpace(e)) {
			return invalidParameter("$expand", "The value '%s' of parameter '$expand' is not supported.", expand)
		}
	}
	return nil
}

func (s *Server) getVM(c *call) (*reply, *apiError) {
	expand := c.query("$expand")
	if err := checkExpand(expand, "instanceView", "userData"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.readVM(c)
	if err != nil {
		return nil, err
	}
	return ok200(s.renderVM(res, expand)), nil
}

func (s *Server) readVM(c *call) (*resource, *apiError) {
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(vmID(c))
	if res == nil {
		return nil, resourceNotFound(vmKind.typ, c.val("name"), c.val("rg"))
	}
	return res, nil
}

func (s *Server) vmInstanceView(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.readVM(c)
	if err != nil {
		return nil, err
	}
	return ok200(s.instanceView(res)), nil
}

func (s *Server) listVMs(c *call) (*reply, *apiError) {
	if f := c.query("$filter"); f != "" {
		return nil, unsupported("the $filter '%s' is not supported", f)
	}
	expand := c.query("$expand")
	if err := checkExpand(expand, "instanceView"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, vmKind), func(res *resource) any { return s.renderVM(res, expand) })), nil
}

// powerVM deallocates or starts a VM.
func (s *Server) powerVM(target string) handler {
	transition := powerStarting
	if target == powerDeallocated {
		transition = powerDeallocating
	}
	return func(c *call) (*reply, *apiError) {
		if c.query("hibernate") == "true" {
			return nil, unsupported("hibernation is not supported")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		res := s.settled(vmID(c))
		if res == nil {
			return nil, resourceNotFound(vmKind.typ, c.val("name"), c.val("rg"))
		}
		op := s.change(c, res, computeOps(c, res.loc), http.StatusNoContent, stateUpdating, func() {
			res.vm.power, res.vm.changed = target, s.now()
		})
		if op.status == opInProgress {
			res.vm.power, res.vm.changed = transition, s.now()
		}
		return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
	}
}

func (s *Server) deleteVM(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res := s.settled(vmID(c))
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	op := s.change(c, res, computeOps(c, res.loc), http.StatusNoContent, stateDeleting, func() {
		for _, n := range res.vm.nics {
			nic := s.resources[key(n.id)]
			if nic == nil || key(nic.nic.vm) != key(res.id) {
				continue
			}
			nic.nic.vm = ""
			if n.delete {
				s.remove(nic)
			}
		}
		delete(s.resources, key(res.id))
	})
	return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
}

// VirtualMachine is a snapshot of a virtual machine.
type VirtualMachine struct {
	ID                string
	Name              string
	Location          string
	Tags              map[string]string
	VMID              string
	Size              string
	ProvisioningState string
	// PowerState is the VM's power state without its prefix, such as "running" or "deallocated".
	PowerState        string
	NetworkInterfaces []string
	OSDiskID          string
}

// VirtualMachines returns the virtual machines the server holds, not
// counting scale set VMs, ordered by ID.
func (s *Server) VirtualMachines() []VirtualMachine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []VirtualMachine
	for _, res := range s.resources {
		if res.vm == nil {
			continue
		}
		hw, _ := object(res.props, "hardwareProfile")
		disk := osDiskOf(res)
		vm := VirtualMachine{
			ID: res.id, Name: res.name, Location: res.loc, Tags: maps.Clone(res.tags), VMID: res.guid,
			Size: str(hw, "vmSize"), ProvisioningState: res.state, PowerState: res.vm.power, OSDiskID: refID(disk, "managedDisk"),
		}
		for _, n := range res.vm.nics {
			vm.NetworkInterfaces = append(vm.NetworkInterfaces, n.id)
		}
		out = append(out, vm)
	}
	slices.SortFunc(out, func(a, b VirtualMachine) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
