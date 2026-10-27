package arm

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
)

var vmExtensionKind = &kind{
	typ: "Microsoft.Compute/virtualMachines/extensions", plural: "extensions", typeName: "VirtualMachineExtension",
	props: []string{
		"autoUpgradeMinorVersion", "enableAutomaticUpgrade", "forceUpdateTag", "protectedSettings", "protectedSettingsFromKeyVault",
		"provisionAfterExtensions", "publisher", "settings", "suppressFailures", "type", "typeHandlerVersion",
	},
	readOnly: []string{"instanceView", "provisioningState"},
	located:  true,
}

const vmExtension = compute + "/virtualMachines/{vm}/extensions/{name}"

func extensionVMID(c *call) string { return resourceID(c, vmKind.typ, c.val("vm")) }

func extensionID(c *call) string { return extensionVMID(c) + "/extensions/" + c.val("name") }

// monitorOps are the paths of a VM extension operation, whose Location URL
// is its Azure-AsyncOperation URL with monitor=true added.
func monitorOps(c *call, location string) opPaths {
	paths := computeOps(c, location)
	paths.result, paths.resultStatus, paths.locationQuery = paths.status, http.StatusOK, cacheMonitor
	return paths
}

func (s *Server) registerExtensionRoutes() {
	k := vmExtensionKind
	s.add("GET", vmExtension[:strings.LastIndex(vmExtension, "/")], k.action("read"), false, s.listExtensions)
	s.add("PUT", vmExtension, k.action("write"), true, s.put(putSpec{
		kind: k, id: extensionID, parent: extensionVMID, paths: monitorOps, render: renderExtension,
		parentMissing: func(c *call) *apiError { return nestedParentNotFound(c.val("vm")) },
	}))
	s.add("GET", vmExtension, k.action("read"), false, s.getExtension)
	s.add("DELETE", vmExtension, k.action("delete"), true, s.deleteCompute(k, extensionID, monitorOps))
}

// nestedParentNotFound fails a write of a nested resource whose parent, name, does not exist.
func nestedParentNotFound(name string) *apiError {
	return newError(http.StatusNotFound, "ParentResourceNotFound",
		"Can not perform requested operation on nested resource. Parent resource '%s' not found.", name)
}

// renderExtension renders the extension res without its protectedSettings, which Azure does not return.
func renderExtension(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "protectedSettings")
	return m
}

// extensionVM checks that the call's group and VM exist. The caller holds s.mu.
func (s *Server) extensionVM(c *call) *apiError {
	if err := s.readableGroup(c); err != nil {
		return err
	}
	if vm := s.resources[key(extensionVMID(c))]; vm == nil || vm.vm == nil {
		return resourceNotFound(vmKind.typ, c.val("vm"), c.val("rg"))
	}
	return nil
}

func (s *Server) getExtension(c *call) (*reply, *apiError) {
	if c.query("$expand") != "" {
		return nil, unsupported("$expand is not supported on %s", vmExtensionKind.typ)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.extensionVM(c); err != nil {
		return nil, err
	}
	res := s.read(extensionID(c))
	if res == nil || res.kind != vmExtensionKind {
		return nil, resourceNotFound(vmKind.typ, c.val("vm")+"/extensions/"+c.val("name"), c.val("rg"))
	}
	return ok200(renderExtension(res)), nil
}

func (s *Server) listExtensions(c *call) (*reply, *apiError) {
	if c.query("$expand") != "" {
		return nil, unsupported("$expand is not supported on %s", vmExtensionKind.typ)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.extensionVM(c); err != nil {
		return nil, err
	}
	prefix := key(extensionVMID(c)) + "/extensions/"
	items := slices.DeleteFunc(s.listed(c, vmExtensionKind), func(res *resource) bool { return !strings.HasPrefix(key(res.id), prefix) })
	return ok(listBody(items, func(res *resource) any { return renderExtension(res) })), nil
}

// removeExtensions deletes the extensions of the VM vm. The caller holds s.mu.
func (s *Server) removeExtensions(vm *resource) {
	prefix := key(vm.id) + "/extensions/"
	for k, res := range s.resources {
		if res.kind == vmExtensionKind && strings.HasPrefix(k, prefix) {
			delete(s.resources, k)
		}
	}
}

// VMExtension is a snapshot of a virtual machine extension.
type VMExtension struct {
	ID string
	// VirtualMachine is the ID of the VM it belongs to.
	VirtualMachine     string
	Name               string
	Location           string
	Publisher          string
	Type               string
	TypeHandlerVersion string
	// Settings and ProtectedSettings are the JSON the caller sent, or nil.
	Settings          json.RawMessage
	ProtectedSettings json.RawMessage
	ProvisioningState string
	Tags              map[string]string
}

// VMExtensions returns the virtual machine extensions the server holds, ordered by ID.
func (s *Server) VMExtensions() []VMExtension {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []VMExtension
	for _, res := range s.resources {
		if res.kind != vmExtensionKind {
			continue
		}
		e := VMExtension{
			ID: res.id, VirtualMachine: res.id[:strings.LastIndex(strings.ToLower(res.id), "/extensions/")], Name: res.name, Location: res.loc,
			Publisher: str(res.props, "publisher"), Type: str(res.props, "type"), TypeHandlerVersion: str(res.props, "typeHandlerVersion"),
			ProvisioningState: res.state, Tags: maps.Clone(res.tags),
		}
		if v, ok := res.props["settings"]; ok {
			e.Settings = encode(v)
		}
		if v, ok := res.props["protectedSettings"]; ok {
			e.ProtectedSettings = encode(v)
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b VMExtension) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
