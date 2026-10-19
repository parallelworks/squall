package arm

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

var vmssKind = &kind{
	typ: "Microsoft.Compute/virtualMachineScaleSets", plural: "virtualMachineScaleSets", typeName: "VirtualMachineScaleSet",
	top: []string{"extendedLocation", "identity", "placement", "plan", "sku", "zones"},
	props: []string{
		"additionalCapabilities", "automaticRepairsPolicy", "constrainedMaximumCapacity",
		"doNotRunExtensionsOnOverprovisionedVMs", "externalHealthPolicy", "highSpeedInterconnectPlacement", "hostGroup",
		"lifecycleHooksProfile", "orchestrationMode", "overprovision", "platformFaultDomainCount", "priorityMixPolicy",
		"proximityPlacementGroup", "resiliencyPolicy", "skuProfile", "scaleInPolicy", "scheduledEventsPolicy",
		"singlePlacementGroup", "spotRestorePolicy", "upgradePolicy", "virtualMachineProfile",
		"zonalPlatformFaultDomainAlignMode", "zoneBalance",
	},
	readOnly: []string{"provisioningState", "timeCreated", "uniqueId"},
	located:  true, etag: quotedETag,
}

const (
	instanceType = "Microsoft.Compute/virtualMachineScaleSets/virtualMachines"
	maxCapacity  = 1000
)

type scaleSet struct {
	skuName, skuTier string
	capacity         int
	next             int
	instances        map[int]*instance
	nics             []nicConfig
	prefix           string
	profile          string
	userData         any
}

type nicConfig struct {
	name    string
	primary bool
	nsg     string
	ips     []ipConfigSpec
}

type ipConfigSpec struct {
	name, subnet string
	primary      bool
	pip          map[string]any
}

type instance struct {
	index        int
	vmID         string
	computerName string
	state        string
	created      time.Time
	latest       bool
	disk         string
	nics         []*instanceNIC
}

type instanceNIC struct {
	name    string
	primary bool
	ips     []*instanceIP
}

type instanceIP struct {
	spec    ipConfigSpec
	address netip.Addr
	public  netip.Addr
}

func (inst *instance) id(res *resource) string {
	return res.id + "/virtualMachines/" + strconv.Itoa(inst.index)
}

// sorted returns the scale set's VMs by instance ID.
func (sc *scaleSet) sorted() []*instance {
	out := slices.Collect(maps.Values(sc.instances))
	slices.SortFunc(out, func(a, b *instance) int { return cmp.Compare(a.index, b.index) })
	return out
}

func vmssID(c *call) string { return resourceID(c, vmssKind.typ, c.val("name")) }

// vmssBody is a parsed scale set create or update.
type vmssBody struct {
	*parsed
	sku      *skuSpec
	nics     []nicConfig
	prefix   string
	profile  string
	userData any
}

var vmssOSMembers = []string{
	"adminPassword", "adminUsername", "allowExtensionOperations", "computerNamePrefix", "customData",
	"linuxConfiguration", "requireGuestProvisionSignal", "secrets", "windowsConfiguration",
}

// skuSpec is a scale set's sku as a call gives it.
type skuSpec struct {
	name, tier  string
	capacity    int
	capacitySet bool
}

// parseSKU reads a scale set's sku from top, or nil when it has none.
func parseSKU(top map[string]any) (*skuSpec, *apiError) {
	sku, err := object(top, "sku")
	if err != nil || sku == nil {
		return nil, err
	}
	if err := checkMembers(sku, "Sku", "sku", "capacity", "name", "tier"); err != nil {
		return nil, err
	}
	spec := &skuSpec{name: str(sku, "name"), tier: str(sku, "tier")}
	if v, ok := sku["capacity"]; ok && v != nil {
		n, ok := v.(json.Number)
		c, convErr := n.Int64()
		if !ok || convErr != nil || c < 0 {
			return nil, invalidParameter("sku.capacity", "The value '%v' of sku.capacity is not valid.", v)
		}
		if c > maxCapacity {
			return nil, invalidParameter("sku.capacity", "The capacity %d exceeds the maximum of %d.", c, maxCapacity)
		}
		spec.capacity, spec.capacitySet = int(c), true
	}
	return spec, nil
}

func parseScaleSet(c *call) (*vmssBody, *apiError) {
	p, err := parseBody(c, vmssKind)
	if err != nil {
		return nil, err
	}
	b := &vmssBody{parsed: p}
	if b.sku, err = parseSKU(p.top); err != nil {
		return nil, err
	}
	delete(p.top, "sku")
	if mode := str(p.props, "orchestrationMode"); mode != "" && !strings.EqualFold(mode, "Uniform") {
		return nil, unsupported("orchestrationMode %s is not supported; only Uniform scale sets are", mode)
	}
	vmp, err := object(p.props, "virtualMachineProfile")
	if err != nil {
		return nil, err
	}
	if vmp == nil {
		return nil, requiredParameter("virtualMachineProfile")
	}
	if err := checkMembers(vmp, "VirtualMachineScaleSetVMProfile", "properties.virtualMachineProfile",
		"applicationProfile", "billingProfile", "capacityReservation", "diagnosticsProfile", "evictionPolicy",
		"extensionProfile", "hardwareProfile", "interconnectBlockProfile", "licenseType", "networkProfile", "osProfile",
		"priority", "scheduledEventsProfile", "securityPostureReference", "securityProfile", "serviceArtifactReference",
		"storageProfile", "timeCreated", "userData"); err != nil {
		return nil, err
	}
	delete(vmp, "timeCreated")
	b.userData = vmp["userData"]
	delete(vmp, "userData")
	os, err := object(vmp, "osProfile")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(os, "VirtualMachineScaleSetOSProfile", "properties.virtualMachineProfile.osProfile", vmssOSMembers...); err != nil {
		return nil, err
	}
	if os != nil {
		delete(os, "adminPassword")
		delete(os, "customData")
	}
	b.prefix = str(os, "computerNamePrefix")
	if b.nics, err = parseNICConfigs(vmp); err != nil {
		return nil, err
	}
	b.profile = string(encode(vmp))
	return b, nil
}

func parseNICConfigs(vmp map[string]any) ([]nicConfig, *apiError) {
	const base = "properties.virtualMachineProfile.networkProfile"
	np, err := object(vmp, "networkProfile")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(np, "VirtualMachineScaleSetNetworkProfile", base,
		"healthProbe", "interconnectGroupProfile", "networkApiVersion", "networkInterfaceConfigurations"); err != nil {
		return nil, err
	}
	raw, err := objects(np, "networkInterfaceConfigurations")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, requiredParameter("virtualMachineProfile.networkProfile.networkInterfaceConfigurations")
	}
	var out []nicConfig
	for i, rc := range raw {
		path := fmt.Sprintf("%s.networkInterfaceConfigurations[%d]", base, i)
		if err := checkMembers(rc, "VirtualMachineScaleSetNetworkConfiguration", path, "name", "properties", "tags"); err != nil {
			return nil, err
		}
		props, err := object(rc, "properties")
		if err != nil {
			return nil, err
		}
		if err := checkMembers(props, "VirtualMachineScaleSetNetworkConfigurationProperties", path+".properties",
			"auxiliaryMode", "auxiliarySku", "dnsSettings", "deleteOption", "disableTcpStateTracking",
			"enableAcceleratedNetworking", "enableFpga", "enableIPForwarding", "ipConfigurations",
			"networkSecurityGroup", "primary"); err != nil {
			return nil, err
		}
		cfg := nicConfig{name: str(rc, "name"), nsg: refID(props, "networkSecurityGroup")}
		if cfg.name == "" {
			return nil, requiredParameter(path + ".name")
		}
		cfg.primary, _ = boolean(props, "primary")
		ips, err := objects(props, "ipConfigurations")
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, requiredParameter(path + ".properties.ipConfigurations")
		}
		for j, ri := range ips {
			spec, err := parseIPConfigSpec(ri, fmt.Sprintf("%s.properties.ipConfigurations[%d]", path, j))
			if err != nil {
				return nil, err
			}
			if len(ips) == 1 {
				spec.primary = true
			}
			cfg.ips = append(cfg.ips, spec)
		}
		out = append(out, cfg)
	}
	if !slices.ContainsFunc(out, func(n nicConfig) bool { return n.primary }) {
		if len(out) > 1 {
			return nil, invalidParameter(base, "Virtual Machine Scale Set must have exactly one primary network interface configuration.")
		}
		out[0].primary = true
	}
	return out, nil
}

func parseIPConfigSpec(ri map[string]any, path string) (ipConfigSpec, *apiError) {
	if err := checkMembers(ri, "VirtualMachineScaleSetIPConfiguration", path, "name", "properties"); err != nil {
		return ipConfigSpec{}, err
	}
	props, err := object(ri, "properties")
	if err != nil {
		return ipConfigSpec{}, err
	}
	if err := checkMembers(props, "VirtualMachineScaleSetIPConfigurationProperties", path+".properties",
		"applicationGatewayBackendAddressPools", "applicationSecurityGroups", "loadBalancerBackendAddressPools",
		"loadBalancerInboundNatPools", "primary", "privateIPAddressVersion", "publicIPAddressConfiguration", "subnet"); err != nil {
		return ipConfigSpec{}, err
	}
	spec := ipConfigSpec{name: str(ri, "name"), subnet: refID(props, "subnet")}
	if spec.name == "" {
		return spec, requiredParameter(path + ".name")
	}
	if spec.subnet == "" {
		return spec, requiredParameter(path + ".properties.subnet")
	}
	if v := str(props, "privateIPAddressVersion"); v != "" && !strings.EqualFold(v, "IPv4") {
		return spec, unsupported("privateIPAddressVersion %s is not supported", v)
	}
	spec.primary, _ = boolean(props, "primary")
	if spec.pip, err = object(props, "publicIPAddressConfiguration"); err != nil {
		return spec, err
	}
	if spec.pip != nil {
		if err := checkMembers(spec.pip, "VirtualMachineScaleSetPublicIPAddressConfiguration", path+".properties.publicIPAddressConfiguration",
			"name", "properties", "sku", "tags"); err != nil {
			return spec, err
		}
		if str(spec.pip, "name") == "" {
			return spec, requiredParameter(path + ".properties.publicIPAddressConfiguration.name")
		}
	}
	return spec, nil
}

func (s *Server) putScaleSet(c *call) (*reply, *apiError) {
	b, err := parseScaleSet(c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	id := vmssID(c)
	existing, err := s.claim(id, "Update VM Scale Set")
	if err != nil {
		return nil, err
	}
	if err := checkPrecondition(c.r, existing); err != nil {
		return nil, err
	}
	res, status := existing, http.StatusOK
	switch {
	case res == nil && b.sku == nil:
		return nil, requiredParameter("sku")
	case res == nil && b.sku.name == "":
		return nil, requiredParameter("sku.name")
	case res != nil && b.sku != nil && !b.sku.capacitySet:
		return nil, requiredParameter("sku.capacity")
	case res == nil:
		res, status = s.newResource(c, vmssKind, id, b.parsed), http.StatusCreated
		res.scale = &scaleSet{instances: map[int]*instance{}}
	case res.loc != b.location:
		return nil, wrongLocation(res.name, res.loc, b.location, res.group)
	case b.prefix != "" && !strings.EqualFold(b.prefix, res.scale.prefix):
		return nil, changeNotAllowed("computerNamePrefix")
	}
	sc := res.scale
	capacity := sc.capacity
	if b.sku != nil {
		capacity = b.sku.capacity
	}
	render := func() []byte { return encode(s.renderScaleSet(res)) }
	paths := computeOps(c, b.location)
	if c.opFault != nil {
		op := s.begin(c, paths, 0, nil)
		if status == http.StatusCreated {
			sc.skuName, sc.skuTier, sc.capacity = b.sku.name, b.sku.tier, capacity
		}
		return accepted(c, op, status, status, false, func() []byte { return withState(res, putState(status), render) }), nil
	}
	if err := s.checkScaleOut(b.nics, capacity-len(sc.sorted())); err != nil {
		return nil, err
	}
	if sc.profile != "" && sc.profile != b.profile {
		for _, inst := range sc.instances {
			inst.latest = false
		}
	}
	if b.sku != nil {
		sc.skuName, sc.skuTier = cmp.Or(b.sku.name, sc.skuName), cmp.Or(b.sku.tier, sc.skuTier)
	}
	sc.nics, sc.prefix, sc.profile, sc.userData = b.nics, cmp.Or(sc.prefix, b.prefix, c.val("name")), b.profile, b.userData
	s.update(res, b.parsed)
	s.resources[key(id)] = res
	return s.rescale(c, res, capacity, status, putState(status)), nil
}

// rescale sets the scale set's capacity, creating or deleting VMs, in an
// operation that shows the scale set in state. The caller holds s.mu.
func (s *Server) rescale(c *call, res *resource, capacity, status int, state string) *reply {
	sc := res.scale
	live := sc.sorted()
	var added, removed []*instance
	for range capacity - len(live) {
		inst := s.newInstance(res)
		added = append(added, inst)
	}
	if capacity < len(live) {
		removed = live[capacity:]
	}
	sc.capacity = capacity
	op := s.change(c, res, computeOps(c, res.loc), 0, state, func() {
		for _, inst := range added {
			inst.state = stateSucceeded
		}
		for _, inst := range removed {
			s.dropInstance(sc, inst)
		}
	})
	if op.status == opInProgress {
		for _, inst := range added {
			inst.state = stateCreating
		}
		for _, inst := range removed {
			inst.state = stateDeleting
		}
	}
	return accepted(c, op, status, status, false, func() []byte { return encode(s.renderScaleSet(res)) })
}

// newInstance adds a VM to the scale set res, with the interfaces and
// addresses its profile describes. The caller holds s.mu.
func (s *Server) newInstance(res *resource) *instance {
	sc := res.scale
	inst := &instance{
		index: sc.next, vmID: s.newUUID(), computerName: computerName(sc.prefix, sc.next), state: stateSucceeded,
		created: s.now(), latest: true,
	}
	sc.next++
	inst.disk = fmt.Sprintf("%s_%d_OsDisk_1_%s", res.name, inst.index, s.newHex())
	for _, cfg := range sc.nics {
		nic := &instanceNIC{name: cfg.name, primary: cfg.primary}
		nicID := inst.id(res) + "/networkInterfaces/" + cfg.name
		for _, spec := range cfg.ips {
			holder := nicID + "/ipConfigurations/" + spec.name
			ip := &instanceIP{spec: spec, address: s.allocPrivate(spec.subnet, holder)}
			if spec.pip != nil {
				ip.public = s.allocPublic(holder)
			}
			nic.ips = append(nic.ips, ip)
		}
		inst.nics = append(inst.nics, nic)
	}
	sc.instances[inst.index] = inst
	return inst
}

// checkScaleOut fails adding n VMs with the interfaces nics when the address
// ranges cannot hold their addresses. The caller holds s.mu.
func (s *Server) checkScaleOut(nics []nicConfig, n int) *apiError {
	var subnets []string
	public := 0
	for range max(n, 0) {
		for _, cfg := range nics {
			for _, spec := range cfg.ips {
				subnets = append(subnets, spec.subnet)
				if spec.pip != nil {
					public++
				}
			}
		}
	}
	return s.checkFree(subnets, public, "")
}

// dropInstance removes a VM from its scale set. The caller holds s.mu.
func (s *Server) dropInstance(sc *scaleSet, inst *instance) {
	s.releaseInstance(inst)
	delete(sc.instances, inst.index)
}

// releaseInstance frees a scale set VM's addresses. The caller holds s.mu.
func (s *Server) releaseInstance(inst *instance) {
	for _, nic := range inst.nics {
		for _, ip := range nic.ips {
			s.releasePrivate(ip.spec.subnet, ip.address)
			if ip.public.IsValid() {
				delete(s.publicIPs, ip.public)
			}
		}
	}
}

func (s *Server) patchScaleSet(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "VirtualMachineScaleSetUpdate", "", "identity", "placement", "plan", "properties", "sku", "tags", "zones"); err != nil {
		return nil, err
	}
	for _, k := range []string{"identity", "placement", "plan", "zones"} {
		if body[k] != nil {
			return nil, unsupported("updating %s with PATCH is not supported; use PUT", k)
		}
	}
	if props, _ := object(body, "properties"); len(props) > 0 {
		return nil, unsupported("updating properties with PATCH is not supported; use PUT")
	}
	sku, err := parseSKU(body)
	if err != nil {
		return nil, err
	}
	_, hasTags := body["tags"]
	tags, err := tagsOf(body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(vmssID(c), "Update VM Scale Set")
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, resourceNotFound(vmssKind.typ, c.val("name"), c.val("rg"))
	}
	if err := checkPrecondition(c.r, res); err != nil {
		return nil, err
	}
	sc := res.scale
	capacity := sc.capacity
	if sku != nil && sku.capacitySet {
		capacity = sku.capacity
	}
	if c.opFault != nil {
		op := s.begin(c, computeOps(c, res.loc), 0, nil)
		return accepted(c, op, http.StatusOK, http.StatusOK, false, func() []byte {
			return withState(res, stateUpdating, func() []byte { return encode(s.renderScaleSet(res)) })
		}), nil
	}
	if err := s.checkScaleOut(sc.nics, capacity-len(sc.sorted())); err != nil {
		return nil, err
	}
	if sku != nil {
		sc.skuName, sc.skuTier = cmp.Or(sku.name, sc.skuName), cmp.Or(sku.tier, sc.skuTier)
	}
	if hasTags {
		res.tags = tags
	}
	s.touch(res)
	return s.rescale(c, res, capacity, http.StatusOK, stateUpdating), nil
}

func (s *Server) readScaleSet(c *call) (*resource, *apiError) {
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(vmssID(c))
	if res == nil {
		return nil, resourceNotFound(vmssKind.typ, c.val("name"), c.val("rg"))
	}
	return res, nil
}

func (s *Server) getScaleSet(c *call) (*reply, *apiError) {
	if err := checkExpand(c.query("$expand"), "userData"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.readScaleSet(c)
	if err != nil {
		return nil, err
	}
	m := s.renderScaleSet(res)
	if c.query("$expand") == "userData" && res.scale.userData != nil {
		m["properties"].(map[string]any)["virtualMachineProfile"].(map[string]any)["userData"] = res.scale.userData
	}
	return ok200(m), nil
}

func (s *Server) listScaleSets(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, vmssKind), func(res *resource) any { return s.renderScaleSet(res) })), nil
}

func (s *Server) deleteScaleSet(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	if op := s.deletion(vmssID(c)); op != nil {
		return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
	}
	res, err := s.claim(vmssID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	op := s.change(c, res, computeOps(c, res.loc), http.StatusNoContent, stateDeleting, func() { s.remove(res) })
	if op.status == opInProgress {
		for _, inst := range res.scale.instances {
			inst.state = stateDeleting
		}
	}
	return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
}

func (s *Server) deleteInstances(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "VirtualMachineScaleSetVMInstanceRequiredIDs", "", "instanceIds"); err != nil {
		return nil, err
	}
	raw, _ := body["instanceIds"].([]any)
	if len(raw) == 0 {
		return nil, requiredParameter("instanceIds")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(vmssID(c), "deleteInstances")
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, resourceNotFound(vmssKind.typ, c.val("name"), c.val("rg"))
	}
	sc := res.scale
	var doomed []*instance
	for _, v := range raw {
		sv, _ := v.(string)
		n, convErr := strconv.Atoi(sv)
		inst := sc.instances[n]
		if convErr != nil || inst == nil {
			return nil, invalidParameter("instanceIds", "The provided instanceId %v is not an active Virtual Machine Scale Set VM instanceId.", v)
		}
		if !slices.Contains(doomed, inst) {
			doomed = append(doomed, inst)
		}
	}
	op := s.change(c, res, computeOps(c, res.loc), http.StatusNoContent, stateUpdating, func() {
		for _, inst := range doomed {
			s.dropInstance(sc, inst)
		}
	})
	if c.opFault == nil {
		sc.capacity -= len(doomed)
	}
	if op.status == opInProgress {
		for _, inst := range doomed {
			inst.state = stateDeleting
		}
	}
	return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
}

func instanceNotFound() *apiError {
	return newError(http.StatusNotFound, "NotFound", "The entity was not found in this Azure location.")
}

func (s *Server) renderScaleSet(res *resource) map[string]any {
	sc := res.scale
	m, props := envelope(res)
	sku := map[string]any{"name": sc.skuName, "tier": cmp.Or(sc.skuTier, "Standard"), "capacity": sc.capacity}
	m["sku"] = sku
	props["uniqueId"] = res.guid
	props["timeCreated"] = timestamp(res.created)
	if _, ok := props["orchestrationMode"]; !ok {
		props["orchestrationMode"] = "Uniform"
	}
	if _, ok := props["upgradePolicy"]; !ok {
		props["upgradePolicy"] = map[string]any{"mode": "Manual"}
	}
	if vmp, _ := props["virtualMachineProfile"].(map[string]any); vmp != nil {
		vmp = maps.Clone(vmp)
		vmp["timeCreated"] = timestamp(res.created)
		props["virtualMachineProfile"] = vmp
	}
	return m
}

// computerName is a scale set VM's host name: the prefix and its instance ID
// in six base-36 digits.
func computerName(prefix string, index int) string {
	return fmt.Sprintf("%s%06s", prefix, strconv.FormatInt(int64(index), 36))
}

func (s *Server) renderInstance(res *resource, inst *instance, expand bool) map[string]any {
	sc := res.scale
	id := inst.id(res)
	vmp, _ := res.props["virtualMachineProfile"].(map[string]any)
	props := map[string]any{
		"latestModelApplied": inst.latest, "vmId": inst.vmID, "provisioningState": inst.state,
		"timeCreated": timestamp(inst.created),
	}
	storage := map[string]any{}
	if sp, _ := vmp["storageProfile"].(map[string]any); sp != nil {
		if img, ok := sp["imageReference"]; ok {
			storage["imageReference"] = img
		}
	}
	osType := "Linux"
	os, _ := vmp["osProfile"].(map[string]any)
	if os != nil && os["windowsConfiguration"] != nil {
		osType = "Windows"
	}
	storage["osDisk"] = map[string]any{
		"osType": osType, "name": inst.disk, "createOption": "FromImage",
		"managedDisk": map[string]any{"id": diskID(res.sub, res.group, inst.disk)},
	}
	props["storageProfile"] = storage
	instOS := maps.Clone(os)
	if instOS == nil {
		instOS = map[string]any{}
	}
	delete(instOS, "computerNamePrefix")
	instOS["computerName"] = inst.computerName
	props["osProfile"] = instOS
	var nics []any
	for _, nic := range inst.nics {
		nics = append(nics, map[string]any{"id": id + "/networkInterfaces/" + nic.name, "properties": map[string]any{"primary": nic.primary}})
	}
	props["networkProfile"] = map[string]any{"networkInterfaces": nics}
	if np, _ := vmp["networkProfile"].(map[string]any); np != nil {
		props["networkProfileConfiguration"] = map[string]any{"networkInterfaceConfigurations": np["networkInterfaceConfigurations"]}
	}
	if expand {
		props["instanceView"] = map[string]any{
			"computerName": instOS["computerName"],
			"statuses":     statuses(inst.state, powerRunning, inst.created),
		}
	}
	m := map[string]any{
		"name": res.name + "_" + strconv.Itoa(inst.index), "id": id, "type": instanceType, "location": res.loc,
		"instanceId": strconv.Itoa(inst.index), "sku": map[string]any{"name": sc.skuName, "tier": cmp.Or(sc.skuTier, "Standard")},
		"properties": props,
	}
	if res.tags != nil {
		m["tags"] = res.tags
	}
	return m
}

func (s *Server) listInstances(c *call) (*reply, *apiError) {
	for _, q := range []string{"$filter", "$select"} {
		if v := c.query(q); v != "" {
			return nil, unsupported("the %s '%s' is not supported", q, v)
		}
	}
	if err := checkExpand(c.query("$expand"), "instanceView"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.readScaleSet(c)
	if err != nil {
		return nil, err
	}
	expand := c.query("$expand") != ""
	return ok(listBody(res.scale.sorted(), func(inst *instance) any { return s.renderInstance(res, inst, expand) })), nil
}

// readInstance returns the scale set and VM the call names, counting the read. The caller holds s.mu.
func (s *Server) readInstance(c *call) (*resource, *instance, *apiError) {
	res, err := s.readScaleSet(c)
	if err != nil {
		return nil, nil, err
	}
	n, convErr := strconv.Atoi(c.val("index"))
	inst := res.scale.instances[n]
	if convErr != nil || inst == nil {
		return nil, nil, instanceNotFound()
	}
	return res, inst, nil
}

func (s *Server) getInstance(c *call) (*reply, *apiError) {
	if err := checkExpand(c.query("$expand"), "instanceView", "userData"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, inst, err := s.readInstance(c)
	if err != nil {
		return nil, err
	}
	return ok200(s.renderInstance(res, inst, strings.Contains(c.query("$expand"), "instanceView"))), nil
}

// lookupInstanceIP finds the IP configuration a call names. The caller holds s.mu.
func (s *Server) lookupInstanceIP(c *call) (*resource, *instance, *instanceIP, string, *apiError) {
	res, inst, err := s.readInstance(c)
	if err != nil {
		return nil, nil, nil, "", err
	}
	for _, nic := range inst.nics {
		if !strings.EqualFold(nic.name, c.val("nic")) {
			continue
		}
		for _, ip := range nic.ips {
			if strings.EqualFold(ip.spec.name, c.val("ip")) {
				return res, inst, ip, inst.id(res) + "/networkInterfaces/" + nic.name + "/ipConfigurations/" + ip.spec.name, nil
			}
		}
	}
	return nil, nil, nil, "", resourceNotFound("Microsoft.Compute/virtualMachineScaleSets/virtualMachines/networkInterfaces",
		c.val("name")+"/"+c.val("index")+"/"+c.val("nic"), c.val("rg"))
}

func (s *Server) getInstanceIPConfig(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, inst, ip, id, err := s.lookupInstanceIP(c)
	if err != nil {
		return nil, err
	}
	props := map[string]any{
		"privateIPAddress": ip.address.String(), "privateIPAllocationMethod": "Dynamic", "privateIPAddressVersion": "IPv4",
		"primary": ip.spec.primary, "provisioningState": inst.state, "subnet": ref(ip.spec.subnet),
	}
	if ip.spec.pip != nil {
		props["publicIPAddress"] = ref(id + "/publicIPAddresses/" + str(ip.spec.pip, "name"))
	}
	return ok200(map[string]any{"id": id, "name": ip.spec.name, "etag": weakETag(res.guid), "properties": props}), nil
}

func (s *Server) getInstancePublicIP(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, inst, ip, id, err := s.lookupInstanceIP(c)
	if err != nil {
		return nil, err
	}
	if ip.spec.pip == nil || !strings.EqualFold(str(ip.spec.pip, "name"), c.val("pip")) {
		return nil, resourceNotFound("Microsoft.Compute/virtualMachineScaleSets/virtualMachines/networkInterfaces/ipConfigurations/publicIPAddresses",
			c.val("pip"), c.val("rg"))
	}
	cfg, _ := ip.spec.pip["properties"].(map[string]any)
	props := maps.Clone(cfg)
	if props == nil {
		props = map[string]any{}
	}
	renderPublicIP(res.loc, ip.public, props)
	props["ipConfiguration"] = ref(id)
	props["provisioningState"] = inst.state
	sku := ip.spec.pip["sku"]
	if sku == nil {
		sku = map[string]any{"name": "Standard", "tier": "Regional"}
	}
	name := str(ip.spec.pip, "name")
	return ok200(map[string]any{
		"id": id + "/publicIPAddresses/" + name, "name": name, "etag": weakETag(res.guid), "sku": sku, "properties": props,
	}), nil
}

func (s *Server) listSKUs(c *call) (*reply, *apiError) {
	return ok(encode(map[string]any{"value": []any{}})), nil
}

// ScaleSet is a snapshot of a scale set.
type ScaleSet struct {
	ID                string
	Name              string
	Location          string
	Tags              map[string]string
	SKU               string
	Capacity          int
	ProvisioningState string
	VMs               []ScaleSetVM
}

// ScaleSetVM is a snapshot of a VM in a scale set.
type ScaleSetVM struct {
	ID                 string
	InstanceID         string
	ComputerName       string
	ProvisioningState  string
	PrivateIPAddresses []string
	PublicIPAddresses  []string
}

// ScaleSets returns the scale sets the server holds, ordered by ID, with
// their VMs ordered by instance ID.
func (s *Server) ScaleSets() []ScaleSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScaleSet
	for _, res := range s.resources {
		sc := res.scale
		if sc == nil {
			continue
		}
		set := ScaleSet{
			ID: res.id, Name: res.name, Location: res.loc, Tags: maps.Clone(res.tags), SKU: sc.skuName,
			Capacity: sc.capacity, ProvisioningState: res.state,
		}
		for _, inst := range sc.sorted() {
			vm := ScaleSetVM{
				ID: inst.id(res), InstanceID: strconv.Itoa(inst.index), ComputerName: inst.computerName,
				ProvisioningState: inst.state,
			}
			for _, nic := range inst.nics {
				for _, ip := range nic.ips {
					vm.PrivateIPAddresses = append(vm.PrivateIPAddresses, ip.address.String())
					if ip.public.IsValid() {
						vm.PublicIPAddresses = append(vm.PublicIPAddresses, ip.public.String())
					}
				}
			}
			set.VMs = append(set.VMs, vm)
		}
		out = append(out, set)
	}
	slices.SortFunc(out, func(a, b ScaleSet) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
