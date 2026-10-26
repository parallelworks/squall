package arm

import (
	"net/http"
	"strings"
)

var roleAssignmentKind = &kind{
	typ: "Microsoft.Authorization/roleAssignments", plural: "roleAssignments", typeName: "RoleAssignment",
	props: []string{
		"condition", "conditionVersion", "delegatedManagedIdentityResourceId", "description", "principalId", "principalType", "roleDefinitionId",
	},
	readOnly: []string{"createdBy", "createdOn", "scope", "updatedBy", "updatedOn"},
}

const roleAssignment = rg + "/providers/Microsoft.Authorization/roleAssignments/{name}"

func roleAssignmentID(c *call) string { return resourceID(c, roleAssignmentKind.typ, c.val("name")) }

func (s *Server) registerAuthorizationRoutes() {
	s.add("PUT", roleAssignment, roleAssignmentKind.action("write"), false, s.putRoleAssignment)
	s.add("DELETE", roleAssignment, roleAssignmentKind.action("delete"), false, s.deleteRoleAssignment)
}

// putRoleAssignment creates a role assignment at a group's scope. Putting one
// again with the same principal and role answers 200; changing either fails
// with RoleAssignmentUpdateNotPermitted, and assigning a role a principal
// already holds at the scope under another name fails with RoleAssignmentExists.
func (s *Server) putRoleAssignment(c *call) (*reply, *apiError) {
	p, err := parseBody(c, roleAssignmentKind)
	if err != nil {
		return nil, err
	}
	principal, role := str(p.props, "principalId"), str(p.props, "roleDefinitionId")
	if principal == "" || role == "" {
		return nil, unsupported("a role assignment needs properties.principalId and properties.roleDefinitionId")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	id := roleAssignmentID(c)
	if existing := s.resources[key(id)]; existing != nil {
		if !sameAssignment(existing, principal, role) {
			return nil, newError(http.StatusBadRequest, "RoleAssignmentUpdateNotPermitted",
				"Tenant ID, application ID, principal ID, and scope are not allowed to be updated.")
		}
		s.update(existing, p)
		return ok200(renderRoleAssignment(existing)), nil
	}
	gk := groupKey(c)
	for _, res := range s.resources {
		if res.kind == roleAssignmentKind && res.inGroup(gk) && sameAssignment(res, principal, role) {
			return nil, newError(http.StatusConflict, "RoleAssignmentExists", "The role assignment already exists.")
		}
	}
	res := s.newResource(c, roleAssignmentKind, id, p)
	s.resources[key(id)] = res
	return &reply{status: http.StatusCreated, body: encode(renderRoleAssignment(res))}, nil
}

func sameAssignment(res *resource, principal, role string) bool {
	return strings.EqualFold(str(res.props, "principalId"), principal) && sameRole(str(res.props, "roleDefinitionId"), role)
}

// sameRole compares role definition IDs by their GUID, as one scoped to a
// subscription and one scoped to the tenant name the same role.
func sameRole(a, b string) bool {
	return strings.EqualFold(a[strings.LastIndex(a, "/")+1:], b[strings.LastIndex(b, "/")+1:])
}

// deleteRoleAssignment answers with the assignment it deletes, or 204 when
// there is none.
func (s *Server) deleteRoleAssignment(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res := s.resources[key(roleAssignmentID(c))]
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return ok200(renderRoleAssignment(res)), nil
}

// renderRoleAssignment shows the assignment's scope and times, with no
// provisioningState or location, as Azure writes it.
func renderRoleAssignment(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "provisioningState")
	props["scope"] = "/subscriptions/" + res.sub + "/resourceGroups/" + res.group
	props["createdOn"], props["updatedOn"] = timestamp(res.created), timestamp(res.created)
	return m
}
