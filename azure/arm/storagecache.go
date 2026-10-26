package arm

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

var amlfsKind = &kind{
	typ: "Microsoft.StorageCache/amlFilesystems", plural: "amlFilesystems", typeName: "AmlFilesystem",
	top:      []string{"identity", "sku", "zones"},
	props:    []string{"encryptionSettings", "filesystemSubnet", "hsm", "maintenanceWindow", "storageCapacityTiB"},
	readOnly: []string{"clientInfo", "health", "provisioningState", "throughputProvisionedMBps"},
	located:  true,
}

const amlfs = rg + "/providers/Microsoft.StorageCache/amlFilesystems"

// cacheMonitor is the query a StorageCache operation's Location URL adds to its path.
const cacheMonitor = "monitor=true"

func amlfsID(c *call) string { return resourceID(c, amlfsKind.typ, c.val("name")) }

// cacheOps are the paths of a Microsoft.StorageCache operation, whose
// Azure-AsyncOperation and Location URLs share one path.
func cacheOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.StorageCache/locations/" + location + "/ascOperations/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK}
}

func (s *Server) registerStorageCacheRoutes() {
	s.add("GET", sub+"/providers/Microsoft.StorageCache/locations/{loc}/ascOperations/{op}", "Microsoft.StorageCache/locations/ascOperations/read", false,
		s.onePathOperation(cacheMonitor, s.operationStatus))
	s.add("POST", sub+"/providers/Microsoft.StorageCache/checkAmlFSSubnets", "Microsoft.StorageCache/checkAmlFSSubnets/action", false, s.checkAmlfsSubnets)
	s.add("GET", amlfs, amlfsKind.action("read"), false, s.listAmlfs)
	s.add("PUT", amlfs+"/{name}", amlfsKind.action("write"), true, s.put(putSpec{
		kind: amlfsKind, id: amlfsID, paths: cacheOps, render: renderAmlfs, prepare: s.prepareAmlfs,
	}))
	s.add("GET", amlfs+"/{name}", amlfsKind.action("read"), false, s.getAmlfs)
	s.add("DELETE", amlfs+"/{name}", amlfsKind.action("delete"), true, s.deleteAmlfs)
}

// prepareAmlfs validates p as the new body of the file system res, and
// returns how to apply it. A new one takes an address in its subnet for its
// management service. The caller holds s.mu.
func (s *Server) prepareAmlfs(res *resource, p *parsed) (func(), *apiError) {
	subnet := str(p.props, "filesystemSubnet")
	if sku, _ := p.top["sku"].(map[string]any); subnet == "" || str(sku, "name") == "" || p.props["storageCapacityTiB"] == nil {
		return nil, unsupported("an AML file system needs sku.name, filesystemSubnet and storageCapacityTiB")
	}
	return s.holdAddress(res, p, subnet, "filesystemSubnet")
}

// checkAmlfsSubnets answers that any subnet will do, as subnets are not modeled.
func (s *Server) checkAmlfsSubnets(c *call) (*reply, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "AmlFilesystemSubnetInfo", "", "filesystemSubnet", "location", "sku", "storageCapacityTiB"); err != nil {
		return nil, err
	}
	if str(body, "filesystemSubnet") == "" {
		return nil, unsupported("a subnet check needs filesystemSubnet")
	}
	return &reply{status: http.StatusOK}, nil
}

func (s *Server) getAmlfs(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(amlfsID(c))
	if res == nil || res.kind != amlfsKind {
		return nil, resourceNotFound(amlfsKind.typ, c.val("name"), c.val("rg"))
	}
	return ok200(renderAmlfs(res)), nil
}

func (s *Server) listAmlfs(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, amlfsKind), func(res *resource) any { return renderAmlfs(res) })), nil
}

func (s *Server) deleteAmlfs(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(amlfsID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != amlfsKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	op := s.change(c, res, cacheOps(c, res.loc), 0, stateDeleting, func() { s.remove(res) })
	return acceptedOnePath(c, op, cacheMonitor), nil
}

// renderAmlfs adds what Azure reports for a file system: its health, its
// throughput, which its SKU names per TiB, and, once created, how to mount it.
func renderAmlfs(res *resource) map[string]any {
	m, props := envelope(res)
	props["health"] = map[string]any{"state": "Available", "statusDescription": "The cluster is available"}
	if res.state != stateSucceeded {
		props["health"] = map[string]any{"state": "Transitioning", "statusDescription": "The cluster is in the process of changing states"}
	}
	sku, _ := res.top["sku"].(map[string]any)
	perTiB, _ := strconv.Atoi(str(sku, "name")[strings.LastIndex(str(sku, "name"), "-")+1:])
	if n, ok := res.props["storageCapacityTiB"].(json.Number); ok && perTiB > 0 {
		if capacity, err := n.Float64(); err == nil {
			props["throughputProvisionedMBps"] = int(capacity * float64(perTiB))
		}
	}
	info := map[string]any{"mgsAddress": "", "mountCommand": "", "lustreVersion": ""}
	if res.state != stateCreating && res.held != nil {
		address := res.held.address.String()
		info = map[string]any{
			"mgsAddress":    address,
			"mountCommand":  "mount -t lustre -o noatime,user_xattr " + address + "@tcp0:/lustrefs /<local path>",
			"lustreVersion": "2.15",
		}
	}
	props["clientInfo"] = info
	return m
}
