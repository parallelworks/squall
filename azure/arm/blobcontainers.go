package arm

import (
	"cmp"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// containerNames are the names the Blob service takes for a container.
var containerNames = regexp.MustCompile(`^[a-z0-9](?:-?[a-z0-9])*$`)

// systemContainers are the special containers a caller can create, as Azure allows.
var systemContainers = []string{"$root", "$web"}

// containerNameError fails a container name Azure does not take, or returns nil.
func containerNameError(name string) *apiError {
	if slices.Contains(systemContainers, name) {
		return nil
	}
	switch {
	case len(name) < 3 || len(name) > 63:
		return newError(http.StatusBadRequest, "OutOfRangeInput", "The specified resource name length is not within the permissible limits.")
	case !containerNames.MatchString(name):
		return newError(http.StatusBadRequest, "InvalidResourceName", "The specified resource name contains invalid characters.")
	}
	return nil
}

func (s *Server) createContainerData(c *blobCall) (*blobReply, *apiError) {
	if err := containerNameError(c.container); err != nil {
		return nil, err
	}
	props := map[string]any{}
	switch access := strings.ToLower(c.r.Header.Get("x-ms-blob-public-access")); access {
	case "":
	case "container", "blob":
		props["publicAccess"] = strings.ToUpper(access[:1]) + access[1:]
	default:
		return nil, invalidHeaderValue("x-ms-blob-public-access", access)
	}
	if m := metadataOf(c.r); m != nil {
		meta := map[string]any{}
		for k, v := range m {
			meta[k] = v
		}
		props["metadata"] = meta
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, _, err := s.containerOf(c)
	if acct == nil {
		return nil, err
	}
	if err == nil {
		return nil, containers.exists()
	}
	res := &resource{
		kind: containerKind, id: containerID(acct, c.container), name: c.container, sub: acct.sub, group: acct.group,
		top: map[string]any{}, props: props, state: stateSucceeded, created: s.now(), guid: s.newUUID(),
	}
	s.touch(res)
	s.resources[key(res.id)] = res
	h := http.Header{}
	h.Set("ETag", quote(res.etag))
	h.Set("Last-Modified", res.created.UTC().Format(http.TimeFormat))
	return &blobReply{status: http.StatusCreated, header: h}, nil
}

func (s *Server) deleteContainerData(c *blobCall) (*blobReply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, cont, err := s.containerOf(c)
	if err != nil {
		return nil, err
	}
	if err := s.containerProtected(cont, true); err != nil {
		return nil, err
	}
	s.deleteContainer(acct, cont)
	return &blobReply{status: http.StatusAccepted}, nil
}

func (s *Server) containerProperties(c *blobCall) (*blobReply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cont, err := s.containerOf(c)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("ETag", quote(cont.etag))
	h.Set("Last-Modified", cont.created.UTC().Format(http.TimeFormat))
	h.Set("x-ms-lease-status", "unlocked")
	h.Set("x-ms-lease-state", "available")
	h.Set("x-ms-has-immutability-policy", strconv.FormatBool(cont.worm != nil && cont.worm.policy != nil))
	h.Set("x-ms-has-legal-hold", strconv.FormatBool(cont.worm.held()))
	h.Set("x-ms-immutable-storage-with-versioning-enabled", strconv.FormatBool(s.containerVersionLevel(cont)))
	h.Set("x-ms-default-encryption-scope", "$account-encryption-key")
	h.Set("x-ms-deny-encryption-scope-override", "false")
	if access := str(cont.props, "publicAccess"); access != "" && access != "None" {
		h.Set("x-ms-blob-public-access", strings.ToLower(access))
	}
	meta, _ := cont.props["metadata"].(map[string]any)
	for k, v := range meta {
		if v, ok := v.(string); ok {
			h.Set("x-ms-meta-"+k, v)
		}
	}
	return &blobReply{status: http.StatusOK, header: h}, nil
}

type containerList struct {
	XMLName    xml.Name       `xml:"EnumerationResults"`
	Endpoint   string         `xml:"ServiceEndpoint,attr"`
	Prefix     *string        `xml:"Prefix,omitempty"`
	Marker     *string        `xml:"Marker,omitempty"`
	MaxResults *int           `xml:"MaxResults,omitempty"`
	Containers []containerXML `xml:"Containers>Container"`
	NextMarker string         `xml:"NextMarker"`
}

type containerXML struct {
	Name       string `xml:"Name"`
	Deleted    bool   `xml:"Deleted,omitempty"`
	Version    string `xml:"Version,omitempty"`
	Properties struct {
		LastModified           string `xml:"Last-Modified"`
		ETag                   string `xml:"Etag"`
		LeaseStatus            string `xml:"LeaseStatus,omitempty"`
		LeaseState             string `xml:"LeaseState"`
		PublicAccess           string `xml:"PublicAccess,omitempty"`
		DefaultEncryptionScope string `xml:"DefaultEncryptionScope"`
		DenyOverride           bool   `xml:"DenyEncryptionScopeOverride"`
		HasImmutabilityPolicy  bool   `xml:"HasImmutabilityPolicy"`
		HasLegalHold           bool   `xml:"HasLegalHold"`
		VersionLevelWORM       bool   `xml:"ImmutableStorageWithVersioningEnabled"`
		DeletedTime            string `xml:"DeletedTime,omitempty"`
		RemainingRetentionDays *int   `xml:"RemainingRetentionDays,omitempty"`
	} `xml:"Properties"`
	Metadata metadataXML `xml:"Metadata,omitempty"`
}

func (s *Server) renderContainerXML(res *resource, metadata bool) containerXML {
	var x containerXML
	x.Name = res.name
	x.Properties.LastModified = res.created.UTC().Format(http.TimeFormat)
	x.Properties.ETag = quote(res.etag)
	x.Properties.LeaseStatus, x.Properties.LeaseState = "unlocked", "available"
	x.Properties.DefaultEncryptionScope = "$account-encryption-key"
	x.Properties.HasImmutabilityPolicy = res.worm != nil && res.worm.policy != nil
	x.Properties.HasLegalHold = res.worm.held()
	x.Properties.VersionLevelWORM = s.containerVersionLevel(res)
	if access := str(res.props, "publicAccess"); access != "" && access != "None" {
		x.Properties.PublicAccess = strings.ToLower(access)
	}
	if meta, _ := res.props["metadata"].(map[string]any); metadata && len(meta) > 0 {
		x.Metadata = metadataXML{}
		for k, v := range meta {
			if v, ok := v.(string); ok {
				x.Metadata[k] = v
			}
		}
	}
	return x
}

func (s *Server) listContainersData(c *blobCall) (*blobReply, *apiError) {
	var withDeleted, withMetadata bool
	if v := c.query.Get("include"); v != "" {
		for item := range strings.SplitSeq(v, ",") {
			switch strings.ToLower(strings.TrimSpace(item)) {
			case "deleted":
				withDeleted = true
			case "metadata":
				withMetadata = true
			case "system":
			default:
				return nil, invalidQueryValue("include", v)
			}
		}
	}
	limit, err := maxResults(c.query)
	if err != nil {
		return nil, err
	}
	after := ""
	if m := c.query.Get("marker"); m != "" {
		b, err := base64.RawURLEncoding.DecodeString(m)
		if err != nil {
			return nil, invalidQueryValue("marker", m)
		}
		after = string(b)
	}
	prefix := c.query.Get("prefix")
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.accountNamed(c.account)
	if acct == nil {
		return nil, newError(http.StatusNotFound, "ResourceNotFound", "The specified resource does not exist.")
	}
	type entry struct {
		key string
		x   containerXML
	}
	var all []entry
	under := key(acct.id + "/" + blobService.path + "/default/" + containerKind.plural + "/")
	for k, res := range s.resources {
		if res.kind == containerKind && strings.HasPrefix(k, under) {
			all = append(all, entry{res.name + "\x00", s.renderContainerXML(res, withMetadata)})
		}
	}
	if withDeleted {
		now := s.now()
		for _, d := range s.deletedContainers(acct) {
			x := s.renderContainerXML(d.res, withMetadata)
			x.Deleted, x.Version = true, d.version
			x.Properties.LeaseStatus = ""
			x.Properties.LeaseState = "expired"
			x.Properties.DeletedTime = d.deleted.UTC().Format(http.TimeFormat)
			days := daysLeft(d.deleted, now, d.keep)
			x.Properties.RemainingRetentionDays = &days
			all = append(all, entry{d.res.name + "\x00" + d.version, x})
		}
	}
	slices.SortFunc(all, func(a, b entry) int { return cmp.Compare(a.key, b.key) })
	out := containerList{
		Endpoint: endpoint(c), Prefix: optional(c.query, "prefix"), Marker: optional(c.query, "marker"),
	}
	if c.query.Has("maxresults") {
		out.MaxResults = &limit
	}
	last := ""
	for _, e := range all {
		if !strings.HasPrefix(e.x.Name, prefix) || e.key <= after {
			continue
		}
		if len(out.Containers) == limit {
			out.NextMarker = base64.RawURLEncoding.EncodeToString([]byte(last))
			break
		}
		out.Containers = append(out.Containers, e.x)
		last = e.key
	}
	return &blobReply{status: http.StatusOK, body: out}, nil
}
