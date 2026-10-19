package compute

import (
	"cmp"
	"strings"
)

// defaultDiskSizeGB is the size of a disk created from no image, or from an
// image or snapshot that names no size.
const defaultDiskSizeGB = 10

// Image is a disk image that instances and disks are created from. Register
// one with [Server.PutImage].
type Image struct {
	Name string
	// Family, when set, lets callers name the image as
	// "projects/P/global/images/family/F". The newest image of a family wins.
	Family string
	// DiskSizeGB is the smallest disk the image fits on: 10 when zero.
	DiskSizeGB int64
	ID         uint64
	SelfLink   string
}

// Snapshot is a disk snapshot that instances and disks are created from.
// Register one with [Server.PutSnapshot].
type Snapshot struct {
	Name string
	// DiskSizeGB is the size of the disk the snapshot was taken of: 10 when zero.
	DiskSizeGB int64
	ID         uint64
	SelfLink   string
}

// PutImage registers img in project, replacing an image of the same name, and
// returns it with its ID and selfLink. A disk or instance can boot only from
// an image registered here. Public images live in their own projects, such
// as "debian-cloud".
func (s *Server) PutImage(project string, img Image) (Image, error) {
	if !validName(img.Name) || img.Family != "" && !validName(img.Family) {
		return Image{}, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", img.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := "projects/" + project + "/global/images/" + img.Name
	img.ID, img.SelfLink = s.newID(), selfLinkBase+path
	img.DiskSizeGB = cmp.Or(img.DiskSizeGB, defaultDiskSizeGB)
	s.images[path] = &img
	return img, nil
}

// PutSnapshot registers snap in project, replacing a snapshot of the same
// name, and returns it with its ID and selfLink.
func (s *Server) PutSnapshot(project string, snap Snapshot) (Snapshot, error) {
	if !validName(snap.Name) {
		return Snapshot{}, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", snap.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := "projects/" + project + "/global/snapshots/" + snap.Name
	snap.ID, snap.SelfLink = s.newID(), selfLinkBase+path
	snap.DiskSizeGB = cmp.Or(snap.DiskSizeGB, defaultDiskSizeGB)
	s.snapshots[path] = &snap
	return snap, nil
}

// relPath turns a resource reference, a full or partial URL, into a path
// such as "projects/p/zones/z/disks/d". A partial URL that names no project
// is in project.
func relPath(ref, project string) string {
	for _, version := range []string{"/compute/v1/", "/compute/beta/"} {
		if _, after, ok := strings.Cut(ref, version); ok && strings.HasPrefix(ref, "https://") {
			return after
		}
	}
	if strings.HasPrefix(ref, "projects/") {
		return ref
	}
	return "projects/" + project + "/" + ref
}

// source is what a new disk is created from.
type source struct {
	image, snapshot string
	imageID         uint64
	sizeGB          int64
}

// resolveSource finds the image or snapshot a disk is created from, either
// of which may be empty. field names the reference in errors. The caller
// holds s.mu.
func (s *Server) resolveSource(project, field, image, snapshot string) (source, *apiError) {
	switch {
	case image != "" && snapshot != "":
		return source{}, invalid("Invalid value for field '%s': only one of sourceImage and sourceSnapshot may be set.", field)
	case image != "":
		path := relPath(image, project)
		if p, family, ok := strings.Cut(path, "/global/images/family/"); ok {
			var newest *Image
			for key, img := range s.images {
				if strings.HasPrefix(key, p+"/global/images/") && img.Family == family && (newest == nil || img.ID > newest.ID) {
					newest = img
				}
			}
			if newest == nil {
				return source{}, notFound(path)
			}
			return source{image: newest.SelfLink, imageID: newest.ID, sizeGB: newest.DiskSizeGB}, nil
		}
		img, ok := s.images[path]
		if !ok {
			return source{}, notFound(path)
		}
		return source{image: img.SelfLink, imageID: img.ID, sizeGB: img.DiskSizeGB}, nil
	case snapshot != "":
		path := relPath(snapshot, project)
		snap, ok := s.snapshots[path]
		if !ok {
			return source{}, notFound(path)
		}
		return source{snapshot: snap.SelfLink, sizeGB: snap.DiskSizeGB}, nil
	}
	return source{sizeGB: defaultDiskSizeGB}, nil
}
