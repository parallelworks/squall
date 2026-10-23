package arm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need an action p lacks, as
// Azure RBAC refuses them, and records what each call needed. Without it
// every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// policyActions are the RBAC actions a route checks where they differ from
// its fault method name. Microsoft.Compute documents no
// locations/operationResults/read; its operation results are read with
// locations/operations/read.
var policyActions = map[string]string{
	"Microsoft.Compute/locations/operationResults/read": "Microsoft.Compute/locations/operations/read",
}

// link is an action a request needs on another resource it references.
type link struct {
	// path leads to the ID in the request body; "[]" steps into each element of an array.
	path   []string
	action string
}

func linkAt(action, path string) link { return link{path: strings.Split(path, "."), action: action} }

// linkedActions are the linked access checks ARM makes on resources a write references.
var linkedActions = map[string][]link{
	"Microsoft.Network/networkInterfaces/write": {
		linkAt("Microsoft.Network/virtualNetworks/subnets/join/action", "properties.ipConfigurations.[].properties.subnet.id"),
		linkAt("Microsoft.Network/publicIPAddresses/join/action", "properties.ipConfigurations.[].properties.publicIPAddress.id"),
		linkAt("Microsoft.Network/networkSecurityGroups/join/action", "properties.networkSecurityGroup.id"),
	},
	"Microsoft.Network/privateDnsZones/virtualNetworkLinks/write": {
		linkAt("Microsoft.Network/virtualNetworks/join/action", "properties.virtualNetwork.id"),
	},
	"Microsoft.Compute/virtualMachines/write": {
		linkAt("Microsoft.Network/networkInterfaces/join/action", "properties.networkProfile.networkInterfaces.[].id"),
		linkAt("Microsoft.Compute/disks/read", "properties.storageProfile.osDisk.managedDisk.id"),
		linkAt(imageRead, "properties.storageProfile.imageReference.id"),
	},
	"Microsoft.Compute/virtualMachineScaleSets/write": {
		linkAt("Microsoft.Network/virtualNetworks/subnets/join/action",
			"properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations.[].properties.ipConfigurations.[].properties.subnet.id"),
		linkAt("Microsoft.Network/networkSecurityGroups/join/action",
			"properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations.[].properties.networkSecurityGroup.id"),
		linkAt(imageRead, "properties.virtualMachineProfile.storageProfile.imageReference.id"),
	},
}

// imageRead stands for the read action on an image ID, which depends on whether it names a managed image or a gallery image version.
const imageRead = "image read"

func imageAction(id string) string {
	if strings.Contains(strings.ToLower(id), "/providers/microsoft.compute/galleries/") {
		return "Microsoft.Compute/galleries/images/versions/read"
	}
	return "Microsoft.Compute/images/read"
}

// scoped is an action needed on a scope.
type scoped struct {
	action, scope string
}

// needs returns the action a call to rt checks on its own scope and the
// actions it needs on the resources its body references, leaving the body
// for the handler.
func needs(rt *route, r *http.Request) (string, []scoped) {
	action := rt.action
	if a, ok := policyActions[action]; ok {
		action = a
	}
	links := linkedActions[action]
	if len(links) == 0 || r.Method != http.MethodPut {
		return action, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var doc any
	if err != nil || json.Unmarshal(body, &doc) != nil {
		return action, nil
	}
	var linked []scoped
	for _, l := range links {
		for _, id := range idsAt(doc, l.path) {
			a := l.action
			if a == imageRead {
				a = imageAction(id)
			}
			if !slices.Contains(linked, scoped{a, id}) {
				linked = append(linked, scoped{a, id})
			}
		}
	}
	return action, linked
}

// idsAt returns the non-empty strings at path in v, matching keys without regard to case.
func idsAt(v any, path []string) []string {
	if len(path) == 0 {
		if s, ok := v.(string); ok && s != "" {
			return []string{s}
		}
		return nil
	}
	if path[0] == "[]" {
		var out []string
		items, _ := v.([]any)
		for _, item := range items {
			out = append(out, idsAt(item, path[1:])...)
		}
		return out
	}
	m, _ := v.(map[string]any)
	for k, child := range m {
		if strings.EqualFold(k, path[0]) {
			return idsAt(child, path[1:])
		}
	}
	return nil
}

// authorize checks a call against the policy and returns the error ARM
// answers when it is refused.
func (s *Server) authorize(rt *route, r *http.Request, scope string) *apiError {
	if s.policy == nil {
		return nil
	}
	action, linked := needs(rt, r)
	call := access.Call{Method: rt.action, Resource: scope, Needs: []string{action}}
	for _, l := range linked {
		call.Needs = append(call.Needs, l.action)
	}
	missing := s.policy.Check(call)
	if missing == nil {
		return nil
	}
	if slices.Contains(missing, action) {
		return newError(http.StatusForbidden, "AuthorizationFailed",
			"The client '%s' with object id '%s' does not have authorization to perform action '%s' over scope '%s' or the scope is invalid. If access was recently granted, please refresh your credentials.",
			clientID, clientID, action, scope)
	}
	var actions, scopes []string
	for _, l := range linked {
		if slices.Contains(missing, l.action) {
			actions = append(actions, l.action)
			scopes = append(scopes, l.scope)
		}
	}
	return newError(http.StatusForbidden, "LinkedAuthorizationFailed",
		"The client '%s' with object id '%s' has permission to perform action '%s' on scope '%s'; however, it does not have permission to perform action(s) '%s' on the linked scope(s) '%s' (respectively) or the linked scope(s) are invalid.",
		clientID, clientID, action, scope, strings.Join(actions, ","), strings.Join(scopes, ","))
}

// clientID is the object ID refusals name, as the server does not read the caller's token.
const clientID = "00000000-0000-0000-0000-000000000000"
