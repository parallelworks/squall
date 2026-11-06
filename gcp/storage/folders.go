package storage

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxFolderDepth = 15

// ManagedFolder is a managed folder, in the API's JSON shape. Its Name ends with "/".
type ManagedFolder struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	SelfLink       string `json:"selfLink"`
	Name           string `json:"name"`
	Bucket         string `json:"bucket"`
	Metageneration int64  `json:"metageneration,string"`
	CreateTime     string `json:"createTime"`
	UpdateTime     string `json:"updateTime"`
}

// folderInput is what managedFolders.insert takes; the fields the server sets are ignored.
type folderInput struct {
	Name           string          `json:"name"`
	Bucket         json.RawMessage `json:"bucket"`
	Kind           json.RawMessage `json:"kind"`
	ID             json.RawMessage `json:"id"`
	SelfLink       json.RawMessage `json:"selfLink"`
	Metageneration json.RawMessage `json:"metageneration"`
	CreateTime     json.RawMessage `json:"createTime"`
	UpdateTime     json.RawMessage `json:"updateTime"`
}

// folderName is name with the trailing "/" every managed folder name has.
func folderName(name string) string {
	if name == "" || strings.HasSuffix(name, "/") {
		return name
	}
	return name + "/"
}

// validFolderName reports whether name, ending with "/", follows the managed folder naming rules.
func validFolderName(name string) bool {
	base := strings.TrimSuffix(name, "/")
	return base != "" && len(name) <= 1024 && utf8.ValidString(name) && !strings.ContainsAny(name, "\r\n") &&
		base != "." && base != ".." && !strings.HasPrefix(name, ".well-known/acme-challenge/") &&
		strings.Count(name, "/") <= maxFolderDepth
}

func folderTarget(r *http.Request) string {
	return "b/" + r.PathValue("bucket") + "/managedFolders/" + folderName(r.PathValue("folder"))
}

// folderInsertTarget names the managed folder a managedFolders.insert creates, from its body.
func folderInsertTarget(r *http.Request) string {
	prefix := "b/" + r.PathValue("bucket") + "/managedFolders"
	var named struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(peekBody(r, maxBody), &named) != nil || named.Name == "" {
		return prefix
	}
	return prefix + "/" + folderName(named.Name)
}

func noSuchFolder(bucket, name string) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: fmt.Sprintf("No such managed folder: %s/%s", bucket, name)}
}

// folderBucket returns the named bucket for a managed folder call needing permission. The caller holds s.mu.
func (s *Server) folderBucket(name, permission string) (*bucket, *apiError) {
	b, err := s.lookup(name, permission)
	if err != nil {
		return nil, err
	}
	if !b.uniformAccess() {
		return nil, &apiError{
			Status: http.StatusConflict, Reason: "conflict",
			Message: "Managed folders can only be used in buckets that have uniform bucket-level access enabled.",
		}
	}
	return b, nil
}

// putFolder creates the managed folder name in the named bucket. The caller holds s.mu.
func (s *Server) putFolder(bucketName, name string) (*ManagedFolder, *apiError) {
	if name == "" {
		return nil, required("resource.name")
	}
	name = folderName(name)
	if !validFolderName(name) {
		return nil, invalid("Invalid managed folder name: '%s'", name)
	}
	b, err := s.folderBucket(bucketName, "storage.managedFolders.create")
	if err != nil {
		return nil, err
	}
	if _, ok := b.folders[name]; ok {
		return nil, &apiError{Status: http.StatusConflict, Reason: "conflict", Message: fmt.Sprintf("The managed folder %s/%s already exists.", b.Name, name)}
	}
	_, now := s.timestamp()
	f := &ManagedFolder{
		Kind: "storage#managedFolder", ID: b.Name + "/" + name,
		SelfLink: apiBase + "b/" + b.Name + "/managedFolders/" + url.PathEscape(name),
		Name:     name, Bucket: b.Name, Metageneration: 1, CreateTime: now, UpdateTime: now,
	}
	if b.folders == nil {
		b.folders = map[string]*ManagedFolder{}
	}
	b.folders[name] = f
	return f, nil
}

func (s *Server) insertFolder(r *http.Request) (any, *apiError) {
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	var in folderInput
	if err := unmarshalStrict(body, &in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.putFolder(r.PathValue("bucket"), in.Name)
	if err != nil {
		return nil, err
	}
	return *f, nil
}

// folder returns the managed folder a call names, once its preconditions hold. The caller holds s.mu.
func (s *Server) folder(r *http.Request, permission string) (*bucket, *ManagedFolder, *apiError) {
	b, err := s.folderBucket(r.PathValue("bucket"), permission)
	if err != nil {
		return nil, nil, err
	}
	name := folderName(r.PathValue("folder"))
	f, ok := b.folders[name]
	if !ok {
		return nil, nil, noSuchFolder(b.Name, name)
	}
	if err := checkMatch(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch", f.Metageneration); err != nil {
		return nil, nil, err
	}
	return b, f, nil
}

func (s *Server) getFolder(r *http.Request) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, f, err := s.folder(r, "storage.managedFolders.get")
	if err != nil {
		return nil, err
	}
	return *f, nil
}

func (s *Server) deleteFolder(r *http.Request) (any, *apiError) {
	allowNonEmpty, err := boolParam(r, "allowNonEmpty")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, f, err := s.folder(r, "storage.managedFolders.delete")
	if err != nil {
		return nil, err
	}
	if !allowNonEmpty && b.folderHolds(f.Name) {
		return nil, &apiError{
			Status: http.StatusConflict, Reason: "conflict",
			Message: fmt.Sprintf("The managed folder %s/%s is not empty.", b.Name, f.Name),
		}
	}
	delete(b.folders, f.Name)
	return nil, nil
}

// folderHolds reports whether any object generation or other managed folder in b is under name.
func (b *bucket) folderHolds(name string) bool {
	for o := range b.objects {
		if strings.HasPrefix(o, name) {
			return true
		}
	}
	for other := range b.folders {
		if other != name && strings.HasPrefix(other, name) {
			return true
		}
	}
	return false
}

type folderList struct {
	Kind          string          `json:"kind"`
	Items         []ManagedFolder `json:"items,omitzero"`
	NextPageToken string          `json:"nextPageToken,omitzero"`
}

func (s *Server) listFolders(r *http.Request) (any, *apiError) {
	prefix := r.URL.Query().Get("prefix")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.folderBucket(r.PathValue("bucket"), "storage.managedFolders.list")
	if err != nil {
		return nil, err
	}
	all := b.managedFolders(prefix)
	items, next, err := page(r, "pageSize", all, func(f ManagedFolder) string { return f.Name })
	if err != nil {
		return nil, err
	}
	return folderList{Kind: "storage#managedFolders", Items: items, NextPageToken: next}, nil
}

// managedFolders returns b's managed folders whose names start with prefix, ordered by name.
func (b *bucket) managedFolders(prefix string) []ManagedFolder {
	var out []ManagedFolder
	for _, name := range slices.Sorted(maps.Keys(b.folders)) {
		if strings.HasPrefix(name, prefix) {
			out = append(out, *b.folders[name])
		}
	}
	return out
}

// ManagedFolders returns the named bucket's managed folders, ordered by name.
func (s *Server) ManagedFolders(bucketName string) []ManagedFolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	return b.managedFolders("")
}

// PutManagedFolder creates a managed folder in the named bucket, as managedFolders.insert does, adding the trailing "/" if name lacks it.
func (s *Server) PutManagedFolder(bucketName, name string) (ManagedFolder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.putFolder(bucketName, name)
	if err != nil {
		return ManagedFolder{}, err
	}
	return *f, nil
}
