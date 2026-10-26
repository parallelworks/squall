package arm

import (
	"maps"
	"net/http"
	"strings"
)

var workspaceKind = &kind{
	typ: "Microsoft.MachineLearningServices/workspaces", plural: "workspaces", typeName: "Workspace",
	top: []string{"identity", "kind", "sku"},
	props: []string{
		"allowPublicAccessWhenBehindVnet", "applicationInsights", "associatedWorkspaces", "containerRegistry", "description", "discoveryUrl",
		"enableDataIsolation", "encryption", "featureStoreSettings", "friendlyName", "hbiWorkspace", "hubResourceId", "imageBuildCompute",
		"keyVault", "managedNetwork", "primaryUserAssignedIdentity", "publicNetworkAccess", "serverlessComputeSettings",
		"serviceManagedResourcesSettings", "sharedPrivateLinkResources", "storageAccount", "v1LegacyMode", "workspaceHubConfig",
	},
	readOnly: []string{
		"mlFlowTrackingUri", "notebookInfo", "privateEndpointConnections", "privateLinkCount", "provisioningState",
		"serviceProvisionedResourceGroup", "storageHnsEnabled", "tenantId", "workspaceId",
	},
	located: true,
}

const workspace = rg + "/providers/Microsoft.MachineLearningServices/workspaces/{name}"

// workspaceLocation is the query a Microsoft.MachineLearningServices operation's Location URL adds to its path.
const workspaceLocation = "type=location"

func workspaceID(c *call) string { return resourceID(c, workspaceKind.typ, c.val("name")) }

// workspaceOps are the paths of a workspace operation, whose
// Azure-AsyncOperation and Location URLs share one path.
func workspaceOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.MachineLearningServices/locations/" + location + "/workspaceOperationsStatus/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK, locationQuery: workspaceLocation}
}

func (s *Server) registerMachineLearningRoutes() {
	s.add("GET", sub+"/providers/Microsoft.MachineLearningServices/locations/{loc}/workspaceOperationsStatus/{op}",
		"Microsoft.MachineLearningServices/locations/workspaceOperationsStatus/read", false, s.onePathOperation(workspaceLocation, s.operationStatus))
	s.add("PUT", workspace, workspaceKind.action("write"), true, s.put(putSpec{
		kind: workspaceKind, id: workspaceID, paths: workspaceOps, render: renderWorkspace, createdStatus: http.StatusOK, acceptedBody: noBody,
	}))
	s.add("GET", workspace, workspaceKind.action("read"), false, s.getWorkspace)
	s.add("DELETE", workspace, workspaceKind.action("delete"), true, s.deleteWorkspace)
}

func (s *Server) getWorkspace(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(workspaceID(c))
	if res == nil || res.kind != workspaceKind {
		return nil, resourceNotFound(workspaceKind.typ, c.val("name"), c.val("rg"))
	}
	return ok200(renderWorkspace(res)), nil
}

// deleteWorkspace deletes the workspace outright, purged or not, polled as
// [Server.onePathOperation] describes.
func (s *Server) deleteWorkspace(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res, err := s.claim(workspaceID(c), "")
	if err != nil {
		return nil, err
	}
	if res == nil || res.kind != workspaceKind {
		return &reply{status: http.StatusNoContent}, nil
	}
	op := s.change(c, res, workspaceOps(c, res.loc), 0, stateDeleting, func() { s.remove(res) })
	return acceptedOnePath(c, op, workspaceLocation), nil
}

// renderWorkspace adds the workspace's ID, its system-assigned identity's
// principal and the URLs Azure Machine Learning reports for it.
func renderWorkspace(res *resource) map[string]any {
	m, props := envelope(res)
	props["workspaceId"] = res.guid
	props["creationTime"] = timestamp(res.created)
	props["discoveryUrl"] = "https://" + res.loc + ".api.azureml.ms/discovery"
	props["mlFlowTrackingUri"] = "azureml://" + res.loc + ".api.azureml.ms/mlflow/v1.0" + res.id
	if identity, _ := res.top["identity"].(map[string]any); strings.Contains(str(identity, "type"), "SystemAssigned") {
		identity = maps.Clone(identity)
		identity["principalId"] = derivedUUID(res, "principal")
		m["identity"] = identity
	}
	return m
}
