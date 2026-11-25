package arm

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

const maxDataDiskLun = 63

var dataDiskMembers = []string{
	"caching", "createOption", "deleteOption", "detachOption", "diskIOPSReadWrite", "diskMBpsReadWrite", "diskSizeGB",
	"image", "lun", "managedDisk", "name", "sourceResource", "toBeDetached", "vhd", "writeAcceleratorEnabled",
}

// dataDisk is a data disk a VM's storage profile names.
type dataDisk struct {
	body map[string]any
	id   string
	// disk is the managed disk, when the server holds it.
	disk   *resource
	detach bool
}

// dataDiskPlan is the data disks a write gives a VM: set ones replace what
// it has, and otherwise kept are what it had.
type dataDiskPlan struct {
	set   bool
	disks []dataDisk
	kept  []any
}

// planDataDisks parses the data disks props, the properties a write sends,
// ask the VM id to hold, and checks that each can be attached. existing is
// the VM as it was, or nil for a new one. The caller holds s.mu.
func (s *Server) planDataDisks(existing *resource, id string, props map[string]any) (*dataDiskPlan, *apiError) {
	sp, err := object(props, "storageProfile")
	if err != nil {
		return nil, err
	}
	plan := &dataDiskPlan{}
	if _, plan.set = sp["dataDisks"]; !plan.set {
		if existing == nil {
			plan.set = true
		} else {
			plan.kept = recordedDataDisks(existing)
		}
		return plan, nil
	}
	raw, err := objects(sp, "dataDisks")
	if err != nil {
		return nil, err
	}
	luns := map[int64]bool{}
	for i, d := range raw {
		path := fmt.Sprintf("properties.storageProfile.dataDisks[%d]", i)
		if err := checkMembers(d, "DataDisk", path, dataDiskMembers...); err != nil {
			return nil, err
		}
		for _, k := range []string{"image", "sourceResource", "vhd"} {
			if d[k] != nil {
				return nil, unsupported("%s.%s is not supported", path, k)
			}
		}
		switch option := str(d, "createOption"); {
		case option == "":
			return nil, requiredParameter(path + ".createOption")
		case !strings.EqualFold(option, "Attach"):
			return nil, unsupported("createOption %s is not supported for a data disk; only Attach is", option)
		}
		if d["lun"] == nil {
			return nil, requiredParameter(path + ".lun")
		}
		n, _ := d["lun"].(json.Number)
		lun, convErr := n.Int64()
		if convErr != nil || lun < 0 || lun > maxDataDiskLun {
			return nil, invalidParameter(path+".lun", "The value '%v' of %s.lun is not valid.", d["lun"], path)
		}
		if luns[lun] {
			return nil, invalidParameter("dataDisks", "A disk at LUN %d already exists.", lun)
		}
		luns[lun] = true
		dd := dataDisk{body: maps.Clone(d), id: refID(d, "managedDisk")}
		if dd.id == "" {
			return nil, requiredParameter(path + ".managedDisk.id")
		}
		delete(dd.body, "diskIOPSReadWrite")
		delete(dd.body, "diskMBpsReadWrite")
		dd.detach, _ = boolean(d, "toBeDetached")
		if res := s.settled(dd.id); res != nil && res.disk != nil {
			dd.disk = res
		}
		if dd.disk != nil && !dd.detach && dd.disk.disk.vm != "" && key(dd.disk.disk.vm) != key(id) {
			return nil, diskAttached(dd.disk.name, dd.disk.disk.vm)
		}
		plan.disks = append(plan.disks, dd)
	}
	return plan, nil
}

// recordedDataDisks returns the data disks the VM vm holds, as it shows them.
func recordedDataDisks(vm *resource) []any {
	sp, _ := vm.props["storageProfile"].(map[string]any)
	disks, _ := sp["dataDisks"].([]any)
	return disks
}

// applyDataDisks gives the VM vm the data disks plan names, attaching the
// ones it did not hold and releasing the ones it no longer does. The caller
// holds s.mu.
func (s *Server) applyDataDisks(vm *resource, plan *dataDiskPlan) {
	listed := plan.kept
	if plan.set {
		listed = []any{}
		held := map[string]bool{}
		for _, d := range plan.disks {
			if d.detach {
				continue
			}
			body := d.body
			body["toBeDetached"] = false
			if str(body, "deleteOption") == "" {
				body["deleteOption"] = "Detach"
			}
			name := d.id[strings.LastIndex(d.id, "/")+1:]
			if d.disk != nil {
				md, _ := body["managedDisk"].(map[string]any)
				md = maps.Clone(md)
				md["id"] = d.disk.id
				body["managedDisk"] = md
				name = d.disk.name
				if size, ok := d.disk.props["diskSizeGB"]; ok {
					body["diskSizeGB"] = size
				}
				if key(d.disk.disk.vm) != key(vm.id) {
					d.disk.disk.vm, d.disk.disk.owned = vm.id, s.now()
				}
				held[key(d.disk.id)] = true
			}
			if str(body, "name") == "" {
				body["name"] = name
			}
			listed = append(listed, body)
		}
		os := key(refID(osDiskOf(vm), "managedDisk"))
		for _, res := range s.resources {
			if res.disk != nil && key(res.disk.vm) == key(vm.id) && !held[key(res.id)] && key(res.id) != os {
				res.disk.vm, res.disk.owned = "", s.now()
			}
		}
	}
	sp, _ := vm.props["storageProfile"].(map[string]any)
	sp = maps.Clone(sp)
	if sp == nil {
		sp = map[string]any{}
	}
	if listed == nil {
		delete(sp, "dataDisks")
	} else {
		sp["dataDisks"] = listed
	}
	vm.props["storageProfile"] = sp
}

// releaseDataDisks detaches the VM vm, which is being deleted, from its data
// disks, deleting those whose deleteOption is Delete. The caller holds s.mu.
func (s *Server) releaseDataDisks(vm *resource) {
	deleting := map[string]bool{}
	for _, d := range recordedDataDisks(vm) {
		body, _ := d.(map[string]any)
		deleting[key(refID(body, "managedDisk"))] = strings.EqualFold(str(body, "deleteOption"), "Delete")
	}
	for _, res := range s.resources {
		if res.disk == nil || key(res.disk.vm) != key(vm.id) {
			continue
		}
		res.disk.vm, res.disk.owned = "", s.now()
		if deleting[key(res.id)] {
			s.remove(res)
		}
	}
}

// dateDataDisks dates the data disks of the VM vm, which was just deallocated
// or started, as its OS disk is dated. The caller holds s.mu.
func (s *Server) dateDataDisks(vm *resource) {
	os := key(refID(osDiskOf(vm), "managedDisk"))
	for _, res := range s.resources {
		if res.disk != nil && key(res.disk.vm) == key(vm.id) && key(res.id) != os {
			res.disk.owned = s.now()
		}
	}
}
