package arm

import (
	"cmp"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

var (
	galleryKind = &kind{
		typ: "Microsoft.Compute/galleries", plural: "galleries", typeName: "Gallery",
		top:      []string{"identity"},
		props:    []string{"description", "sharingProfile", "softDeletePolicy"},
		readOnly: []string{"identifier", "provisioningState", "sharingStatus"},
		located:  true,
	}
	galleryImageKind = &kind{
		typ: "Microsoft.Compute/galleries/images", plural: "images", typeName: "GalleryImage",
		props: []string{
			"allowUpdateImage", "architecture", "description", "disallowed", "endOfLifeDate", "eula", "features", "hyperVGeneration",
			"identifier", "osState", "osType", "privacyStatementUri", "purchasePlan", "recommended", "releaseNoteUri",
		},
		readOnly: []string{"provisioningState"},
		located:  true,
	}
	galleryVersionKind = &kind{
		typ: "Microsoft.Compute/galleries/images/versions", plural: "versions", typeName: "GalleryImageVersion",
		props:    []string{"publishingProfile", "restore", "safetyProfile", "securityProfile", "storageProfile"},
		readOnly: []string{"imageMetadataProfiles", "provisioningState", "replicationStatus", "validationsProfile"},
		located:  true,
	}
)

const (
	gallery        = compute + "/galleries/{gallery}"
	galleryImage   = gallery + "/images/{image}"
	galleryVersion = galleryImage + "/versions/{version}"
)

// versionName is the Major.Minor.Patch form a gallery image version's name takes.
var versionName = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// versionState is what a gallery image version holds beyond its body.
type versionState struct {
	// sources are the resource it is made from, or its disks' sources, OS disk first.
	sources []string
	// sizeGB is the size of its OS disk's source, when that is a disk or snapshot.
	sizeGB int
	// failure is the error its creation ends in, if any.
	failure *apiError
}

func galleryID(c *call) string { return resourceID(c, galleryKind.typ, c.val("gallery")) }

func galleryImageID(c *call) string { return galleryID(c) + "/images/" + c.val("image") }

func galleryVersionID(c *call) string { return galleryImageID(c) + "/versions/" + c.val("version") }

// galleryName is the name a not-found error gives the gallery resource id:
// its path after Microsoft.Compute/galleries, such as g/images/i.
func galleryName(id string) string {
	_, rest, _ := strings.Cut(strings.ToLower(id), "/providers/microsoft.compute/galleries/")
	return id[len(id)-len(rest):]
}

// capsOps are the paths of a gallery operation, whose Azure-AsyncOperation
// and Location URLs share one path, the Location URL adding monitor=true.
func capsOps(c *call, location string) opPaths {
	path := "/subscriptions/" + c.val("sub") + "/providers/Microsoft.Compute/locations/" + location + "/capsOperations/"
	return opPaths{status: path, result: path, resultStatus: http.StatusOK, locationQuery: cacheMonitor}
}

// galleryLevel is a gallery resource type: galleries, image definitions or image versions.
type galleryLevel struct {
	kind    *kind
	path    string
	id      func(*call) string
	parent  func(*call) string
	prepare func(s *Server, res *resource, p *parsed) (func(), *apiError)
}

var galleryLevels = []*galleryLevel{
	{kind: galleryKind, path: gallery, id: galleryID},
	{kind: galleryImageKind, path: galleryImage, id: galleryImageID, parent: galleryID, prepare: (*Server).prepareGalleryImage},
	{kind: galleryVersionKind, path: galleryVersion, id: galleryVersionID, parent: galleryImageID, prepare: (*Server).prepareGalleryVersion},
}

func (s *Server) registerGalleryRoutes() {
	s.add("GET", sub+"/providers/Microsoft.Compute/locations/{loc}/capsOperations/{op}", "Microsoft.Compute/locations/capsOperations/read", false,
		s.onePathOperation(cacheMonitor, s.operationStatus))
	for _, l := range galleryLevels {
		spec := putSpec{kind: l.kind, id: l.id, parent: l.parent, paths: capsOps, render: func(res *resource) map[string]any { return renderGallery(res, false) }}
		if l.prepare != nil {
			spec.prepare = func(res *resource, p *parsed) (func(), *apiError) { return l.prepare(s, res, p) }
		}
		if l.kind == galleryVersionKind {
			spec.finish = func(res *resource) *apiError { return res.version.failure }
		}
		s.add("GET", l.path[:strings.LastIndex(l.path, "/")], l.kind.action("read"), false, s.listGallery(l))
		s.add("PUT", l.path, l.kind.action("write"), true, s.put(spec))
		s.add("GET", l.path, l.kind.action("read"), false, s.getGallery(l))
		s.add("DELETE", l.path, l.kind.action("delete"), true, s.deleteCompute(l.kind, l.id, capsOps))
	}
}

// prepareGalleryImage validates p as the new body of the image definition
// res, and returns how to apply it. The caller holds s.mu.
func (s *Server) prepareGalleryImage(res *resource, p *parsed) (func(), *apiError) {
	for _, k := range []string{"osType", "osState"} {
		if str(p.props, k) == "" {
			return nil, requiredParameter(k)
		}
	}
	id, err := object(p.props, "identifier")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(id, "GalleryImageIdentifier", "properties.identifier", "offer", "publisher", "sku"); err != nil {
		return nil, err
	}
	oldID, _ := res.props["identifier"].(map[string]any)
	for _, k := range []string{"publisher", "offer", "sku"} {
		if str(id, k) == "" {
			return nil, requiredParameter("identifier." + k)
		}
		if old := str(oldID, k); old != "" && !strings.EqualFold(old, str(id, k)) {
			return nil, changeNotAllowed("galleryImage.properties.identifier." + k)
		}
	}
	for _, k := range []string{"osType", "osState", "hyperVGeneration"} {
		if old := str(res.props, k); old != "" && !strings.EqualFold(old, str(p.props, k)) {
			return nil, changeNotAllowed("galleryImage.properties." + k)
		}
	}
	return func() { s.update(res, p) }, nil
}

// prepareGalleryVersion validates p as the new body of the image version
// res, and returns how to apply it. A source that is missing or in another
// location fails the call; a snapshot whose copy has not completed fails the
// version's creation. The caller holds s.mu.
func (s *Server) prepareGalleryVersion(res *resource, p *parsed) (func(), *apiError) {
	if !versionName.MatchString(res.name) {
		return nil, invalidParameter("galleryImageVersionName",
			"The image version name is invalid. The image version name should follow Major(int).Minor(int).Patch(int) format, for e.g: 1.0.0, 2018.12.1 etc")
	}
	sp, err := object(p.props, "storageProfile")
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, requiredParameter("storageProfile")
	}
	if err := checkMembers(sp, "GalleryImageVersionStorageProfile", "properties.storageProfile", "dataDiskImages", "osDiskImage", "source"); err != nil {
		return nil, err
	}
	top := refID(sp, "source")
	var disks []string
	osImage, err := object(sp, "osDiskImage")
	if err != nil {
		return nil, err
	}
	if id := refID(osImage, "source"); id != "" {
		disks = append(disks, id)
	}
	data, err := objects(sp, "dataDiskImages")
	if err != nil {
		return nil, err
	}
	for _, d := range data {
		if id := refID(d, "source"); id != "" {
			disks = append(disks, id)
		}
	}
	if (top == "") == (len(disks) == 0) {
		return nil, invalidParameter("storageProfile",
			"The gallery artifact version source can only be specified either directly under storageProfile or within individual OS or data disks. One and only one source type (user image, snapshot, disk, virtual machine) can be provided.")
	}
	st := &versionState{}
	if top != "" {
		if err := s.checkVersionSource(res, top); err != nil {
			return nil, err
		}
		st.sources = []string{top}
	}
	for i, id := range disks {
		typ := computeType(id)
		if typ != "disks" && typ != "snapshots" {
			return nil, invalidParameter("storageProfile", "The source id: %s must be a managed disk or snapshot", id)
		}
		src := s.settled(id)
		if src == nil || src.disk == nil && src.snap == nil || src.loc != res.loc {
			return nil, sourceNotFound(id)
		}
		if st.failure == nil && !ready(src) {
			st.failure = copyInProgress(src)
		}
		if i == 0 && osImage != nil {
			st.sizeGB = intOf(src.props["diskSizeGB"])
		}
		st.sources = append(st.sources, src.id)
	}
	if res.version != nil && !slices.EqualFunc(res.version.sources, st.sources, func(a, b string) bool { return key(a) == key(b) }) {
		return nil, changeNotAllowed("galleryImageVersion.properties.storageProfile.source.id")
	}
	pp, err := object(p.props, "publishingProfile")
	if err != nil {
		return nil, err
	}
	regions, err := objects(pp, "targetRegions")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range regions {
		if str(r, "name") == "" {
			return nil, requiredParameter("publishingProfile.targetRegions.name")
		}
		names = append(names, str(r, "name"))
	}
	if len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return normalizeLocation(n) == res.loc }) {
		return nil, invalidParameter("publishingProfile.targetRegions",
			"Gallery image version publishing profile regions %s must contain the location of image version %s", strings.Join(names, ", "), res.loc)
	}
	return func() {
		res.version = st
		s.update(res, p)
	}, nil
}

// checkVersionSource checks the source id of the whole image version res: a
// VM or another image version must exist, and a managed image is recorded,
// not checked. The caller holds s.mu.
func (s *Server) checkVersionSource(res *resource, id string) *apiError {
	switch computeType(id) {
	case "disks", "snapshots":
		return invalidParameter("storageProfile.source.id", "The source id: %s must either be a managed image, virtual machine or another gallery image version", id)
	case "virtualmachines":
		if src := s.settled(id); src == nil || src.vm == nil || src.loc != res.loc {
			return sourceNotFound(id)
		}
	case "galleries":
		if src := s.settled(id); src == nil || src.version == nil {
			return sourceNotFound(id)
		}
	}
	return nil
}

func sourceNotFound(id string) *apiError {
	return invalidParameter("storageProfile",
		"Source %s is not found. Please check source exists, and is in the same region as gallery image version being created", id)
}

// computeType is the Microsoft.Compute type the resource id names, in lower
// case and without its parents, such as disks or galleries.
func computeType(id string) string {
	_, rest, ok := strings.Cut(strings.ToLower(id), "/providers/microsoft.compute/")
	if !ok {
		return ""
	}
	typ, _, _ := strings.Cut(rest, "/")
	return typ
}

func renderGallery(res *resource, expand bool) map[string]any {
	m, props := envelope(res)
	switch res.kind {
	case galleryKind:
		props["identifier"] = map[string]any{"uniqueName": res.sub + "-" + strings.ToUpper(res.name)}
	case galleryImageKind:
		if _, ok := props["architecture"]; !ok {
			props["architecture"] = "x64"
		}
	case galleryVersionKind:
		renderVersion(res, props, expand)
	}
	return m
}

// renderVersion adds what Azure reports for the image version res: its
// publishing profile's defaults, its OS disk's size and, when expand is set,
// its replication status.
func renderVersion(res *resource, props map[string]any, expand bool) {
	pp, _ := props["publishingProfile"].(map[string]any)
	pp = maps.Clone(pp)
	if pp == nil {
		pp = map[string]any{}
	}
	replicas := cmp.Or(intOf(pp["replicaCount"]), 1)
	account := cmp.Or(str(pp, "storageAccountType"), "Standard_LRS")
	pp["replicaCount"], pp["storageAccountType"], pp["publishedDate"] = replicas, account, timestamp(res.created)
	if _, ok := pp["excludeFromLatest"]; !ok {
		pp["excludeFromLatest"] = false
	}
	regions, _ := objects(pp, "targetRegions")
	if len(regions) == 0 {
		regions = []map[string]any{{"name": res.loc}}
	}
	targets := make([]any, 0, len(regions))
	var summary []any
	for _, r := range regions {
		r = maps.Clone(r)
		if _, ok := r["regionalReplicaCount"]; !ok {
			r["regionalReplicaCount"] = replicas
		}
		if _, ok := r["storageAccountType"]; !ok {
			r["storageAccountType"] = account
		}
		targets = append(targets, r)
		status := map[string]any{"region": r["name"], "state": "Replicating", "details": "", "progress": 0}
		switch res.state {
		case stateSucceeded:
			status["state"], status["progress"] = "Completed", 100
		case stateFailed:
			status["state"] = stateFailed
			if res.version != nil && res.version.failure != nil {
				status["details"] = "Replication failed in this region due to '" + res.version.failure.Message + "'."
			}
		}
		summary = append(summary, status)
	}
	pp["targetRegions"] = targets
	props["publishingProfile"] = pp
	if sp, _ := props["storageProfile"].(map[string]any); sp != nil && res.version != nil && res.version.sizeGB > 0 {
		sp = maps.Clone(sp)
		if os, _ := sp["osDiskImage"].(map[string]any); os != nil {
			os = maps.Clone(os)
			os["sizeInGB"] = res.version.sizeGB
			sp["osDiskImage"] = os
		}
		props["storageProfile"] = sp
	}
	if _, ok := props["safetyProfile"]; !ok {
		props["safetyProfile"] = map[string]any{"allowDeletionOfReplicatedLocations": false, "reportedForPolicyViolation": false}
	}
	if expand {
		aggregated := "InProgress"
		switch res.state {
		case stateSucceeded:
			aggregated = "Completed"
		case stateFailed:
			aggregated = stateFailed
		}
		props["replicationStatus"] = map[string]any{"aggregatedState": aggregated, "summary": summary}
	}
}

func (s *Server) getGallery(l *galleryLevel) handler {
	return func(c *call) (*reply, *apiError) {
		expand := c.query("$expand")
		if l.kind != galleryVersionKind && expand != "" {
			return nil, unsupported("$expand is not supported on %s", l.kind.typ)
		}
		if err := checkExpand(expand, "ReplicationStatus"); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		id := l.id(c)
		res := s.read(id)
		if res == nil || res.kind != l.kind {
			return nil, resourceNotFound(galleryKind.typ, galleryName(id), c.val("rg"))
		}
		return ok200(renderGallery(res, expand != "")), nil
	}
}

func (s *Server) listGallery(l *galleryLevel) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		items := s.listed(c, l.kind)
		if l.parent != nil {
			prefix := key(l.parent(c)) + "/"
			items = slices.DeleteFunc(items, func(res *resource) bool { return !strings.HasPrefix(key(res.id), prefix) })
		}
		return ok(listBody(items, func(res *resource) any { return renderGallery(res, false) })), nil
	}
}

// GalleryImageVersion is a snapshot of a gallery image version.
type GalleryImageVersion struct {
	ID       string
	Location string
	// Sources are the resource it is made from, or the disks and snapshots
	// its disks are made from, OS disk first.
	Sources []string
	// ProvisioningState is Creating until it is built, then Succeeded or Failed.
	ProvisioningState string
	// Error is the message of the error its creation failed with, if any.
	Error string
	Tags  map[string]string
}

// GalleryImageVersions returns the gallery image versions the server holds, ordered by ID.
func (s *Server) GalleryImageVersions() []GalleryImageVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []GalleryImageVersion
	for _, res := range s.resources {
		if res.version == nil {
			continue
		}
		v := GalleryImageVersion{
			ID: res.id, Location: res.loc, Sources: slices.Clone(res.version.sources), ProvisioningState: res.state, Tags: maps.Clone(res.tags),
		}
		if res.state == stateFailed && res.version.failure != nil {
			v.Error = res.version.failure.Message
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b GalleryImageVersion) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
