package arm

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	exportType      = "Microsoft.CostManagement/exports"
	exportsProvider = "Microsoft.CostManagementExports"
	exports         = sub + "/providers/Microsoft.CostManagement/exports"
)

// export is a Cost Management export at a subscription's scope.
type export struct {
	sub, name string
	etag      string
	location  string
	identity  map[string]any
	props     map[string]any
}

func (e *export) id() string {
	return "/subscriptions/" + e.sub + "/providers/" + exportType + "/" + e.name
}

func exportKey(c *call) string { return key(c.val("sub") + "/" + c.val("name")) }

func (s *Server) registerExportRoutes() {
	const action = exportType + "/"
	s.add("GET", exports, action+"read", false, s.listExports)
	s.add("GET", exports+"/{name}", action+"read", false, s.getExport)
	s.add("PUT", exports+"/{name}", action+"write", false, s.putExport)
	s.add("DELETE", exports+"/{name}", action+"delete", false, s.deleteExport)
}

// costError is an error in the shape Cost Management answers, whose code is
// the HTTP status and whose message ends with a request ID. The caller holds s.mu.
func (s *Server) costError(status int, format string, args ...any) *apiError {
	e := newError(status, strconv.Itoa(status), format, args...)
	e.Message += " (Request ID: " + s.newUUID() + ")"
	return e
}

// exportNotFound is the error a get or delete of a missing export answers. The caller holds s.mu.
func (s *Server) exportNotFound(c *call) *apiError {
	return s.costError(http.StatusNotFound, "No export found for %s, Subscription: %s.", c.val("name"), c.val("sub"))
}

var exportProps = []string{
	"compressionMode", "dataOverwriteBehavior", "definition", "deliveryInfo", "exportDescription", "format", "partitionData", "schedule",
}

// parseExport reads an export body, returning its eTag and the export it describes.
func parseExport(c *call) (string, *export, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return "", nil, err
	}
	if err := checkMembers(body, "Export", "", "eTag", "id", "identity", "location", "name", "properties", "systemData", "type"); err != nil {
		return "", nil, err
	}
	props, err := object(body, "properties")
	if err != nil {
		return "", nil, err
	}
	known := slices.Concat(exportProps, []string{"nextRunTimeEstimate", "runHistory", "systemSuspensionContext"})
	if err := checkMembers(props, "ExportProperties", "properties", known...); err != nil {
		return "", nil, err
	}
	if props["definition"] == nil || props["deliveryInfo"] == nil {
		return "", nil, unsupported("an export needs properties.definition and properties.deliveryInfo")
	}
	e := &export{sub: c.val("sub"), name: c.val("name"), location: str(body, "location"), props: map[string]any{}}
	for _, name := range exportProps {
		if v, ok := props[name]; ok && v != nil {
			e.props[name] = v
		}
	}
	if e.identity, err = object(body, "identity"); err != nil {
		return "", nil, err
	}
	if schedule, _ := e.props["schedule"].(map[string]any); schedule != nil {
		if period, _ := schedule["recurrencePeriod"].(map[string]any); period != nil {
			for _, k := range []string{"from", "to"} {
				if t, err := time.Parse(time.RFC3339Nano, str(period, k)); err == nil {
					period[k] = timestamp(t)
				}
			}
		}
	}
	return str(body, "eTag"), e, nil
}

// putExport creates an export, answering 201, or updates one, answering
// 200. An update needs the export's current eTag in the body.
func (s *Server) putExport(c *call) (*reply, *apiError) {
	etag, e, err := parseExport(c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.providers[providerKey(c.val("sub"), exportsProvider)]; p != nil && p.state == providerRegistering {
		s.observe(p.pending)
		if p.state == providerRegistering {
			return nil, s.costError(http.StatusServiceUnavailable,
				"The %s resource provider is being registered for subscription %s. Try again later.", exportsProvider, c.val("sub"))
		}
	}
	status := http.StatusCreated
	if existing := s.exports[exportKey(c)]; existing != nil {
		switch {
		case etag == "":
			return nil, s.costError(http.StatusBadRequest,
				"Etag value required for update operation of report: %s.Please get the latest to avoid overwrite.", c.val("name"))
		case strings.Trim(etag, `"`) != strings.Trim(existing.etag, `"`):
			return nil, s.costError(http.StatusPreconditionFailed,
				"Etag value is not the latest for report: %s.Please get the latest to avoid overwrite.", c.val("name"))
		}
		e.name, e.location, e.identity = existing.name, existing.location, existing.identity
		status = http.StatusOK
	} else if e.identity != nil {
		e.identity = maps.Clone(e.identity)
		e.identity["principalId"], e.identity["tenantId"] = s.newUUID(), clientID
	}
	e.etag = quotedETag(s.newUUID())
	s.exports[exportKey(c)] = e
	return &reply{status: status, body: encode(renderExport(e))}, nil
}

func (s *Server) getExport(c *call) (*reply, *apiError) {
	if c.query("$expand") != "" {
		return nil, unsupported("$expand is not supported on %s", exportType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.exports[exportKey(c)]
	if e == nil {
		return nil, s.exportNotFound(c)
	}
	return ok200(renderExport(e)), nil
}

func (s *Server) listExports(c *call) (*reply, *apiError) {
	if c.query("$expand") != "" {
		return nil, unsupported("$expand is not supported on %s", exportType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []*export
	for _, e := range s.exports {
		if strings.EqualFold(e.sub, c.val("sub")) {
			items = append(items, e)
		}
	}
	slices.SortFunc(items, func(a, b *export) int { return cmp.Compare(key(a.name), key(b.name)) })
	return ok(listBody(items, func(e *export) any { return renderExport(e) })), nil
}

// deleteExport answers 200 with no body, or 404 when there is no export.
func (s *Server) deleteExport(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exports[exportKey(c)] == nil {
		return nil, s.exportNotFound(c)
	}
	delete(s.exports, exportKey(c))
	return &reply{status: http.StatusOK}, nil
}

// renderExport shows the export as Cost Management writes it: without its
// destination's type and with a null rootFolderPath when it has none.
func renderExport(e *export) map[string]any {
	m := map[string]any{"id": e.id(), "name": e.name, "type": exportType, "eTag": e.etag}
	if e.location != "" {
		m["location"] = e.location
	}
	if e.identity != nil {
		m["identity"] = e.identity
	}
	props := maps.Clone(e.props)
	if delivery, _ := props["deliveryInfo"].(map[string]any); delivery != nil {
		if dest, _ := delivery["destination"].(map[string]any); dest != nil {
			dest = maps.Clone(dest)
			delete(dest, "type")
			if _, ok := dest["rootFolderPath"]; !ok {
				dest["rootFolderPath"] = nil
			}
			delivery = maps.Clone(delivery)
			delivery["destination"] = dest
			props["deliveryInfo"] = delivery
		}
	}
	m["properties"] = props
	return m
}

// Export is a snapshot of a Cost Management export.
type Export struct {
	ID string
	// Scope is the export's scope, such as /subscriptions/{id}.
	Scope string
	Name  string
	// DefinitionType is properties.definition.type, such as Usage, and Timeframe its timeframe, such as MonthToDate.
	DefinitionType string
	Timeframe      string
	// Destination is the storage account ID the export delivers to, with its Container and RootFolderPath.
	Destination    string
	Container      string
	RootFolderPath string
	Format         string
	// Recurrence, RecurrenceFrom, RecurrenceTo and Status are its schedule's.
	Recurrence     string
	RecurrenceFrom time.Time
	RecurrenceTo   time.Time
	Status         string
	ETag           string
}

// Exports returns the Cost Management exports the server holds, ordered by ID.
func (s *Server) Exports() []Export {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Export, 0, len(s.exports))
	for _, e := range s.exports {
		def, _ := e.props["definition"].(map[string]any)
		delivery, _ := e.props["deliveryInfo"].(map[string]any)
		dest, _ := delivery["destination"].(map[string]any)
		schedule, _ := e.props["schedule"].(map[string]any)
		period, _ := schedule["recurrencePeriod"].(map[string]any)
		from, _ := time.Parse(time.RFC3339Nano, str(period, "from"))
		to, _ := time.Parse(time.RFC3339Nano, str(period, "to"))
		out = append(out, Export{
			ID: e.id(), Scope: "/subscriptions/" + e.sub, Name: e.name,
			DefinitionType: str(def, "type"), Timeframe: str(def, "timeframe"),
			Destination: str(dest, "resourceId"), Container: str(dest, "container"), RootFolderPath: str(dest, "rootFolderPath"),
			Format: str(e.props, "format"), Recurrence: str(schedule, "recurrence"), RecurrenceFrom: from, RecurrenceTo: to,
			Status: str(schedule, "status"), ETag: e.etag,
		})
	}
	slices.SortFunc(out, func(a, b Export) int { return cmp.Compare(key(a.ID), key(b.ID)) })
	return out
}
