package acpbridge

import (
	"errors"
	"net/url"
	"path"
	"strings"

	"zeolite/internal/sandbox"
)

// folder is the one folder an ACP client and the agent it works with
// both have: the sandbox's Shared Folder, as its path on this Mac and
// as the workstation mounts it. A path is mapped from one to the other
// by its text: it is cleaned, so that no ".." in it leads out, and is
// in the folder where the folder's path begins it, whole names apart.
// Nothing is asked of a disk. A link in the folder leads where it
// leads: that is not known here.
type folder struct {
	mac         string
	workstation string
}

func newFolder(sharedFolder string) (folder, error) {
	mac := path.Clean(sharedFolder)
	if !strings.HasPrefix(sharedFolder, "/") || mac == "/" || strings.ContainsRune(sharedFolder, 0) {
		return folder{}, errors.New("the sandbox's Shared Folder is not known by its path on this Mac")
	}
	return folder{mac: mac, workstation: sandbox.SharedFolderPath}, nil
}

// within maps an absolute path that is in the folder from, or is it, to
// the same place in the folder to.
func within(p, from, to string) (string, bool) {
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", false
	}
	p = path.Clean(p)
	if p == from {
		return to, true
	}
	if rest, ok := strings.CutPrefix(p, from+"/"); ok {
		return to + "/" + rest, true
	}
	return "", false
}

// toWorkstation maps a path of this Mac that is in the Shared Folder to
// the workstation's path of the same place.
func (f folder) toWorkstation(p string) (string, bool) { return within(p, f.mac, f.workstation) }

// toMac maps a path of the workstation that is in the Shared Folder to
// this Mac's path of the same place.
func (f folder) toMac(p string) (string, bool) { return within(p, f.workstation, f.mac) }

// filePath is the path a file: URI names, where it names one on the
// machine of whoever wrote it: no other host, and a path from the root.
// What a URI writes as %2F is a separator, as it is for whoever opens
// the path.
func filePath(u *url.URL) (string, bool) {
	if (u.Host != "" && u.Host != "localhost") || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return "", false
	}
	return u.Path, true
}

// fileURI writes a path as a file: URI, with the part after its "#",
// by which a client names lines of the file.
func fileURI(p, fragment string) string {
	return (&url.URL{Scheme: "file", Path: p, Fragment: fragment}).String()
}

// webAddress writes an http: or https: URI anew, without credentials.
func webAddress(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery, Fragment: u.Fragment}).String()
}

// uriToAgent is a URI of the client's as the agent is given it. A file
// in the Shared Folder is named by the workstation's path. Any other
// file is named by its own name alone, and what is written as a bare
// path is taken for a file: no path of this Mac outside the Shared
// Folder reaches the workstation. A web address stays one. Of a URI of
// any other kind, which is the client's own and may hold a path, the
// kind alone is left.
func (f folder) uriToAgent(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", true
	}
	switch u.Scheme {
	case "file", "":
		if p, ok := filePath(u); ok {
			if mapped, ok := f.toWorkstation(p); ok {
				if u.Scheme == "" {
					return mapped, true
				}
				return fileURI(mapped, u.Fragment), true
			}
		}
		named := u.Path
		if named == "" {
			named, _ = url.PathUnescape(u.Opaque)
		}
		if named = path.Base(path.Clean("/" + named)); named == "/" {
			return "", true
		}
		return url.PathEscape(named), true
	case "http", "https":
		return webAddress(u), true
	}
	return u.Scheme + ":", true
}

// uriToClient is a URI of the agent's as the client is given it, or
// that it is given none: a file in the Shared Folder is named by this
// Mac's path, a web address stays one, and nothing else crosses, which
// a client might open as a file of this Mac's or by a program of it.
func (f folder) uriToClient(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch u.Scheme {
	case "file":
		if p, ok := filePath(u); ok {
			if mapped, ok := f.toMac(p); ok {
				return fileURI(mapped, u.Fragment), true
			}
		}
	case "http", "https":
		return webAddress(u), true
	}
	return "", false
}
