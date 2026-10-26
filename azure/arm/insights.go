package arm

import (
	"net/http"
	"strings"
)

var componentKind = &kind{
	typ: "Microsoft.Insights/components", plural: "components", typeName: "ApplicationInsightsComponent",
	top: []string{"kind"},
	props: []string{
		"Application_Type", "DisableIpMasking", "DisableLocalAuth", "Flow_Type", "ForceCustomerStorageForProfiler", "HockeyAppId",
		"ImmediatePurgeDataOn30Days", "IngestionMode", "Request_Source", "RetentionInDays", "SamplingPercentage", "WorkspaceResourceId",
		"publicNetworkAccessForIngestion", "publicNetworkAccessForQuery",
	},
	readOnly: []string{
		"AppId", "ApplicationId", "ConnectionString", "CreationDate", "HockeyAppToken", "InstrumentationKey", "LaMigrationDate", "Name",
		"PrivateLinkScopedResources", "TenantId", "provisioningState",
	},
	located: true, etag: quotedETag,
}

const components = rg + "/providers/Microsoft.Insights/components"

func componentID(c *call) string { return resourceID(c, componentKind.typ, c.val("name")) }

func (s *Server) registerInsightsRoutes() {
	s.add("GET", components, componentKind.action("read"), false, s.listComponents)
	s.add("PUT", components+"/{name}", componentKind.action("write"), false, s.put(putSpec{
		kind: componentKind, id: componentID, render: renderComponent, prepare: s.prepareComponent, sync: true, createdStatus: http.StatusOK,
	}))
	s.add("GET", components+"/{name}", componentKind.action("read"), false, s.getComponent)
}

func (s *Server) prepareComponent(res *resource, p *parsed) (func(), *apiError) {
	if str(p.top, "kind") == "" || str(p.props, "Application_Type") == "" {
		return nil, unsupported("an Application Insights component needs kind and properties.Application_Type")
	}
	return func() { s.update(res, p) }, nil
}

func (s *Server) getComponent(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(componentID(c))
	if res == nil || res.kind != componentKind {
		return nil, resourceNotFound(componentKind.typ, c.val("name"), c.val("rg"))
	}
	return ok200(renderComponent(res)), nil
}

func (s *Server) listComponents(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	return ok(listBody(s.listed(c, componentKind), func(res *resource) any { return renderComponent(res) })), nil
}

// renderComponent writes the type, and the namespace in the ID, in lower
// case, as Application Insights does, and the keys it derives for the
// component.
func renderComponent(res *resource) map[string]any {
	m, props := envelope(res)
	m["type"] = strings.ToLower(componentKind.typ)
	m["id"] = strings.Replace(res.id, "/providers/"+componentKind.typ+"/", "/providers/"+strings.ToLower(componentKind.typ)+"/", 1)
	instrumentationKey := derivedUUID(res, "instrumentation")
	props["Name"], props["ApplicationId"], props["AppId"] = res.name, res.name, res.guid
	props["InstrumentationKey"] = instrumentationKey
	props["ConnectionString"] = "InstrumentationKey=" + instrumentationKey + ";IngestionEndpoint=https://" + res.loc + "-0.in.applicationinsights.azure.com/"
	props["CreationDate"] = timestamp(res.created)
	return m
}
