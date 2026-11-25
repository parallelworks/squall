package arm

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// ResourceSKU is a SKU a subscription is offered in one location, as
// Microsoft.Compute lists resource SKUs, such as the VM size Standard_D2s_v3
// in eastus.
type ResourceSKU struct {
	// ResourceType is the type the SKU is a size of, virtualMachines unless set.
	ResourceType string
	Name         string
	Tier         string
	Size         string
	Family       string
	Location     string
	// Zones are the availability zones of Location that offer the SKU.
	Zones []string
	// Capabilities are the SKU's capabilities by name, such as vCPUs or
	// AcceleratedNetworkingEnabled, with values as Azure writes them, such as
	// "2" or "True".
	Capabilities map[string]string
	Restrictions []SKURestriction
}

// SKURestriction keeps the subscription from using a SKU in its location, or
// in some of the location's zones.
type SKURestriction struct {
	// Zones, when set, are the zones the restriction applies to, making it a
	// Zone restriction; without them it is a Location restriction.
	Zones []string
	// ReasonCode is why, QuotaId or NotAvailableForSubscription.
	ReasonCode string
}

// WithResourceSKUs sets the resource SKUs the server lists, in the order
// given. The default lists none.
func WithResourceSKUs(skus ...ResourceSKU) Option {
	return func(s *Server) { s.skus = slices.Clone(skus) }
}

// listSKUs lists the SKUs [WithResourceSKUs] sets, in one location when
// $filter names it, as the only filter Azure takes.
func (s *Server) listSKUs(c *call) (*reply, *apiError) {
	location := ""
	if f := c.query("$filter"); strings.TrimSpace(f) != "" {
		v, rest, err := filterClause(f, "location")
		if err != nil || strings.TrimSpace(rest) != "" {
			return nil, unsupported("the $filter '%s' is not supported; use location eq 'location'", f)
		}
		location = normalizeLocation(v)
	}
	if v := c.query("includeExtendedLocations"); v != "" && v != "true" && v != "false" {
		return nil, invalidParameter("includeExtendedLocations", "The value '%s' of parameter 'includeExtendedLocations' is not valid.", v)
	}
	var items []ResourceSKU
	for _, sku := range s.skus {
		if location == "" || normalizeLocation(sku.Location) == location {
			items = append(items, sku)
		}
	}
	return ok(listBody(items, renderResourceSKU)), nil
}

func renderResourceSKU(sku ResourceSKU) any {
	loc := normalizeLocation(sku.Location)
	info := map[string]any{"location": loc}
	if len(sku.Zones) > 0 {
		info["zones"] = sku.Zones
	}
	m := map[string]any{
		"resourceType": cmp.Or(sku.ResourceType, "virtualMachines"),
		"name":         sku.Name,
		"locations":    []string{loc},
		"locationInfo": []any{info},
		"capabilities": []any{},
		"restrictions": []any{},
	}
	for k, v := range map[string]string{"tier": sku.Tier, "size": sku.Size, "family": sku.Family} {
		if v != "" {
			m[k] = v
		}
	}
	capabilities := make([]any, 0, len(sku.Capabilities))
	for _, name := range slices.Sorted(maps.Keys(sku.Capabilities)) {
		capabilities = append(capabilities, map[string]any{"name": name, "value": sku.Capabilities[name]})
	}
	m["capabilities"] = capabilities
	restrictions := make([]any, 0, len(sku.Restrictions))
	for _, r := range sku.Restrictions {
		typ, info := "Location", map[string]any{"locations": []string{loc}}
		if len(r.Zones) > 0 {
			typ, info["zones"] = "Zone", r.Zones
		}
		restrictions = append(restrictions, map[string]any{
			"type": typ, "values": []string{loc}, "restrictionInfo": info, "reasonCode": r.ReasonCode,
		})
	}
	m["restrictions"] = restrictions
	return m
}
