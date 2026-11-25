package arm

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

var storageProfileMembers = []string{"alignRegionalDisksToVMZone", "dataDisks", "diskControllerType", "imageReference", "osDisk"}

func (s *Server) patchVM(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "VirtualMachineUpdate", "", "identity", "plan", "properties", "tags", "zones"); err != nil {
		return nil, err
	}
	for _, k := range []string{"identity", "plan", "zones"} {
		if body[k] != nil {
			return nil, unsupported("updating %s with PATCH is not supported; use PUT", k)
		}
	}
	props, err := object(body, "properties")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(props, "VirtualMachineProperties", "properties", slices.Concat(vmKind.props, vmKind.readOnly)...); err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(props)) {
		if props[k] != nil && k != "storageProfile" && !slices.Contains(vmKind.readOnly, k) {
			return nil, unsupported("updating properties.%s with PATCH is not supported; use PUT", k)
		}
	}
	sp, err := object(props, "storageProfile")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(sp, "StorageProfile", "properties.storageProfile", storageProfileMembers...); err != nil {
		return nil, err
	}
	osDisk, err := object(sp, "osDisk")
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
	res, err := s.claim(vmID(c), "Update VM")
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, resourceNotFound(vmKind.typ, c.val("name"), c.val("rg"))
	}
	if err := checkPrecondition(c.r, res); err != nil {
		return nil, err
	}
	if name := str(osDisk, "name"); name != "" && !strings.EqualFold(name, str(osDiskOf(res), "name")) {
		return nil, changeNotAllowed("osDisk.name")
	}
	plan := &dataDiskPlan{kept: recordedDataDisks(res)}
	if _, ok := sp["dataDisks"]; ok {
		if plan, err = s.planDataDisks(res, res.id, props); err != nil {
			return nil, err
		}
	}
	render := func() []byte { return encode(s.renderVM(res, "")) }
	paths := computeOps(c, res.loc)
	if c.opFault != nil {
		op := s.begin(c, paths, 0, nil)
		return accepted(c, op, http.StatusOK, http.StatusOK, false, func() []byte { return withState(res, stateUpdating, render) }), nil
	}
	if hasTags {
		res.tags = tags
	}
	s.applyDataDisks(res, plan)
	s.touch(res)
	res.modified = s.now()
	op := s.change(c, res, paths, 0, stateUpdating, nil)
	return accepted(c, op, http.StatusOK, http.StatusOK, false, render), nil
}
