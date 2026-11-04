package arm

import (
	"cmp"
	"encoding/base64"
	"encoding/xml"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// includes are the datasets a List Blobs call asks for.
type includes struct {
	snapshots, versions, deleted, deletedWithVersions, uncommitted, metadata bool
}

func parseIncludes(v string) (includes, *apiError) {
	var inc includes
	if v == "" {
		return inc, nil
	}
	for item := range strings.SplitSeq(v, ",") {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "snapshots":
			inc.snapshots = true
		case "versions":
			inc.versions = true
		case "deleted":
			inc.deleted = true
		case "deletedwithversions":
			inc.deletedWithVersions = true
		case "uncommittedblobs":
			inc.uncommitted = true
		case "metadata":
			inc.metadata = true
		case "copy", "tags", "immutabilitypolicy", "legalhold":
		default:
			return inc, invalidQueryValue("include", v)
		}
	}
	return inc, nil
}

// maxResults reads a list's maxresults parameter.
func maxResults(q map[string][]string) (int, *apiError) {
	v := first(q, "maxresults")
	if v == "" {
		return maxListResults, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, invalidQueryValue("maxresults", v)
	}
	if n <= 0 {
		return 0, newError(http.StatusBadRequest, "OutOfRangeQueryParameterValue",
			"One of the query parameters specified in the request URI is outside the permissible range. Parameter maxresults, value %d.", n)
	}
	return min(n, maxListResults), nil
}

// listItem is one entry of a blob list: a blob, or a BlobPrefix.
type listItem struct {
	name, sub string
	blob      *blobXML
}

// key orders the item; a prefix sorts before the names it holds and after any shorter name.
func (it listItem) key() string {
	if it.blob == nil {
		return "p" + it.name
	}
	return "b" + it.name + "\x00" + it.sub
}

// after reports whether it comes after the item whose key is marker.
func (it listItem) after(marker string) bool {
	if p, ok := strings.CutPrefix(marker, "p"); ok {
		return !strings.HasPrefix(it.name, p) && it.name > p
	}
	return it.key()[1:] > marker[1:]
}

func (s *Server) listBlobs(c *blobCall) (*blobReply, *apiError) {
	inc, err := parseIncludes(c.query.Get("include"))
	if err != nil {
		return nil, err
	}
	limit, err := maxResults(c.query)
	if err != nil {
		return nil, err
	}
	marker, err := decodeMarker(c.query.Get("marker"))
	if err != nil {
		return nil, err
	}
	prefix, delimiter, startFrom := c.query.Get("prefix"), c.query.Get("delimiter"), c.query.Get("startFrom")
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cont, aerr := s.containerOf(c)
	if aerr != nil {
		return nil, aerr
	}
	now := s.now()
	out := blobList{
		Endpoint: endpoint(c), Container: c.container,
		Prefix: optional(c.query, "prefix"), Marker: optional(c.query, "marker"), Delimiter: optional(c.query, "delimiter"),
	}
	if c.query.Has("maxresults") {
		out.MaxResults = &limit
	}
	var page []listItem
	more := false
	lastPrefix := ""
	var listedNames []string
	for name := range cont.blobs {
		if strings.HasPrefix(name, prefix) && name >= startFrom {
			listedNames = append(listedNames, name)
		}
	}
	slices.Sort(listedNames)
	for _, name := range listedNames {
		if more {
			break
		}
		p := ""
		if delimiter != "" {
			if i := strings.Index(name[len(prefix):], delimiter); i >= 0 {
				p = name[:len(prefix)+i+len(delimiter)]
			}
		}
		if p != "" && p == lastPrefix || !purgeBlob(cont, name, now) {
			continue
		}
		items := blobItems(name, cont.blobs[name], inc, now)
		if len(items) == 0 {
			continue
		}
		if p != "" {
			lastPrefix = p
			items = []listItem{{name: p}}
		}
		for _, it := range items {
			if marker != "" && !it.after(marker) {
				continue
			}
			if len(page) == limit {
				more = true
				break
			}
			page = append(page, it)
		}
	}
	for _, it := range page {
		if it.blob != nil {
			out.Blobs.Items = append(out.Blobs.Items, it.blob)
		} else {
			out.Blobs.Items = append(out.Blobs.Items, &blobPrefixXML{Name: it.name})
		}
	}
	if more {
		out.NextMarker = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1].key()))
	}
	return &blobReply{status: http.StatusOK, body: out}, nil
}

func decodeMarker(m string) (string, *apiError) {
	if m == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil || len(b) == 0 || b[0] != 'b' && b[0] != 'p' {
		return "", invalidQueryValue("marker", m)
	}
	return string(b), nil
}

// blobItems lists what the blob name holds that inc asks for, in the order
// the Blob service lists them: snapshots oldest first, then versions oldest
// first, then the blob.
func blobItems(name string, b *blobName, inc includes, now time.Time) []listItem {
	var out []listItem
	add := func(sub string, x *blobXML) {
		x.Name = name
		out = append(out, listItem{name: name, sub: sub, blob: x})
	}
	shown := func(e *blobEntry) bool { return e.deleted.IsZero() || inc.deleted }
	if inc.snapshots {
		for _, e := range sortedBy(b.snapshots, func(e *blobEntry) string { return e.snapshot }) {
			if shown(e) {
				x := renderBlob(e, inc, now)
				x.Snapshot = e.snapshot
				add("0"+e.snapshot, x)
			}
		}
	}
	if inc.versions {
		all := b.versions
		if b.current != nil && b.current.versionID != "" {
			all = append(slices.Clone(all), b.current)
		}
		for _, e := range sortedBy(all, func(e *blobEntry) string { return e.versionID }) {
			if shown(e) {
				x := renderBlob(e, inc, now)
				x.VersionID = e.versionID
				x.IsCurrentVersion = e == b.current
				add("1"+e.versionID, x)
			}
		}
	}
	switch {
	case b.current != nil && (b.current.versionID == "" || !inc.versions):
		x := renderBlob(b.current, inc, now)
		x.VersionID = b.current.versionID
		x.IsCurrentVersion = b.current.versionID != ""
		add("2", x)
	case b.deleted != nil && inc.deleted:
		add("2", renderBlob(b.deleted, inc, now))
	case b.current == nil && inc.deletedWithVersions && len(b.versions) > 0:
		x := renderBlob(b.versions[len(b.versions)-1], inc, now)
		x.Deleted, x.HasVersionsOnly = true, true
		add("2", x)
	case b.current == nil && b.deleted == nil && inc.uncommitted && len(b.uncommitted) > 0:
		add("2", &blobXML{Properties: blobPropsXML{ContentLength: 0, BlobType: "BlockBlob"}})
	}
	return out
}

func sortedBy(list []*blobEntry, id func(*blobEntry) string) []*blobEntry {
	return slices.SortedStableFunc(slices.Values(list), func(a, b *blobEntry) int { return cmp.Compare(id(a), id(b)) })
}

func renderBlob(e *blobEntry, inc includes, now time.Time) *blobXML {
	p := blobPropsXML{
		CreationTime: e.created.Format(http.TimeFormat), LastModified: e.modified.Format(http.TimeFormat), ETag: e.etag,
		ContentLength: e.size, ContentType: e.headers.contentType, ContentEncoding: e.headers.contentEncoding,
		ContentLanguage: e.headers.contentLanguage, CacheControl: e.headers.cacheControl, ContentDisposition: e.headers.contentDisposition,
		BlobType: "BlockBlob", AccessTier: "Hot", AccessTierInferred: true, ServerEncrypted: true,
	}
	if e.md5 != nil {
		p.ContentMD5 = base64.StdEncoding.EncodeToString(e.md5)
	}
	x := &blobXML{Properties: p}
	if e.deleted.IsZero() {
		if e.snapshot == "" {
			x.Properties.LeaseStatus, x.Properties.LeaseState = "unlocked", "available"
		}
	} else {
		x.Deleted = true
		x.Properties.DeletedTime = e.deleted.Format(http.TimeFormat)
		days := e.remainingDays(now)
		x.Properties.RemainingRetentionDays = &days
	}
	if inc.metadata && len(e.metadata) > 0 {
		x.Metadata = metadataXML(e.metadata)
	}
	return x
}

// endpoint is the service endpoint a list names, as the call reached it.
func endpoint(c *blobCall) string {
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + c.r.Host + "/"
	if !strings.HasPrefix(strings.ToLower(c.r.Host), c.account+".") {
		base += c.account + "/"
	}
	return base
}

func optional(q map[string][]string, name string) *string {
	if _, ok := q[name]; !ok {
		return nil
	}
	return new(first(q, name))
}

type blobList struct {
	XMLName    xml.Name `xml:"EnumerationResults"`
	Endpoint   string   `xml:"ServiceEndpoint,attr"`
	Container  string   `xml:"ContainerName,attr"`
	Prefix     *string  `xml:"Prefix,omitempty"`
	Marker     *string  `xml:"Marker,omitempty"`
	MaxResults *int     `xml:"MaxResults,omitempty"`
	Delimiter  *string  `xml:"Delimiter,omitempty"`
	Blobs      struct {
		Items []any
	} `xml:"Blobs"`
	NextMarker string `xml:"NextMarker"`
}

type blobXML struct {
	XMLName          xml.Name     `xml:"Blob"`
	Name             string       `xml:"Name"`
	Snapshot         string       `xml:"Snapshot,omitempty"`
	VersionID        string       `xml:"VersionId,omitempty"`
	IsCurrentVersion bool         `xml:"IsCurrentVersion,omitempty"`
	Deleted          bool         `xml:"Deleted,omitempty"`
	Properties       blobPropsXML `xml:"Properties"`
	Metadata         metadataXML  `xml:"Metadata,omitempty"`
	HasVersionsOnly  bool         `xml:"HasVersionsOnly,omitempty"`
}

type blobPrefixXML struct {
	XMLName xml.Name `xml:"BlobPrefix"`
	Name    string   `xml:"Name"`
}

type blobPropsXML struct {
	CreationTime           string `xml:"Creation-Time,omitempty"`
	LastModified           string `xml:"Last-Modified,omitempty"`
	ETag                   string `xml:"Etag,omitempty"`
	ContentLength          int64  `xml:"Content-Length"`
	ContentType            string `xml:"Content-Type,omitempty"`
	ContentEncoding        string `xml:"Content-Encoding,omitempty"`
	ContentLanguage        string `xml:"Content-Language,omitempty"`
	ContentMD5             string `xml:"Content-MD5,omitempty"`
	CacheControl           string `xml:"Cache-Control,omitempty"`
	ContentDisposition     string `xml:"Content-Disposition,omitempty"`
	BlobType               string `xml:"BlobType"`
	AccessTier             string `xml:"AccessTier,omitempty"`
	AccessTierInferred     bool   `xml:"AccessTierInferred,omitempty"`
	LeaseStatus            string `xml:"LeaseStatus,omitempty"`
	LeaseState             string `xml:"LeaseState,omitempty"`
	ServerEncrypted        bool   `xml:"ServerEncrypted,omitempty"`
	DeletedTime            string `xml:"DeletedTime,omitempty"`
	RemainingRetentionDays *int   `xml:"RemainingRetentionDays,omitempty"`
}

// metadataXML renders metadata as an element per name, in name order.
type metadataXML map[string]string

func (m metadataXML) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	if len(m) == 0 {
		return nil
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if err := enc.EncodeElement(m[k], xml.StartElement{Name: xml.Name{Local: k}}); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}
