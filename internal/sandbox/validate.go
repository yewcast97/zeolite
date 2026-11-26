package sandbox

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Field names a part of a Config, so that whoever shows a FieldError can
// put it beside the right control.
type Field string

const (
	FieldID                Field = "id"
	FieldName              Field = "name"
	FieldAllowedInterfaces Field = "allowedInterfaces"
	FieldProbeURLs         Field = "gate.probeURLs"
	FieldProbePin          Field = "gate.probePin"
	FieldResolver          Field = "gate.resolver"
	FieldHostConditions    Field = "hostConditions"
	FieldTimezone          Field = "clock.timezone"
	FieldTimezoneProbeURL  Field = "clock.timezoneProbeURL"
	FieldCPUs              Field = "resources.cpus"
	FieldMemory            Field = "resources.memoryMB"
	FieldShm               Field = "resources.shmMB"
	FieldPacks             Field = "packs"
	FieldSharedFolder      Field = "sharedFolder"
	FieldHostTools         Field = "hostTools"
)

// FieldError says what is wrong with one field, in words for the user.
type FieldError struct {
	Field   Field
	Message string
}

func (e FieldError) Error() string { return string(e.Field) + ": " + e.Message }

var (
	idPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	interfacePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
)

// ValidID reports whether id can be a sandbox's ID: it names containers
// and folders, so it is a short word with nothing in it that a path or
// a command line reads.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidInterfaceName reports whether name can be the name of an
// Interface of this Mac.
func ValidInterfaceName(name string) bool { return interfacePattern.MatchString(name) }

// ValidSharedFolderName reports whether name can name a Shared Folder.
// It names a folder of the Mac, so it has the shape of a sandbox's ID.
func ValidSharedFolderName(name string) bool { return idPattern.MatchString(name) }

// ValidApplicationName reports whether name can name an Allowed
// Application: one entry of a folder, and nothing that leads out of it.
func ValidApplicationName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// Validate returns what is wrong with the config, in the order of its
// fields, or nil.
func (c Config) Validate() []FieldError {
	var errs []FieldError
	add := func(f Field, format string, args ...any) {
		errs = append(errs, FieldError{f, fmt.Sprintf(format, args...)})
	}

	if !ValidID(c.ID) {
		add(FieldID, "the ID must be 1 to 32 lower-case letters, digits or hyphens")
	}
	if name := strings.TrimSpace(c.Name); name == "" {
		add(FieldName, "give the sandbox a name")
	} else if utf8.RuneCountInString(name) > 64 {
		add(FieldName, "the name is longer than 64 characters")
	}

	if len(c.AllowedInterfaces) == 0 {
		add(FieldAllowedInterfaces, "choose what the sandbox leaves this Mac by")
	}
	named := map[string]bool{}
	for _, ifc := range c.AllowedInterfaces {
		switch {
		case !ValidInterfaceName(ifc.Name):
			add(FieldAllowedInterfaces, "%q is not a name such as en0 or utun4", ifc.Name)
			continue
		case named[ifc.Name]:
			add(FieldAllowedInterfaces, "%s is listed twice", ifc.Name)
		}
		named[ifc.Name] = true
		switch n := ifc.Expectations(); {
		case n > 1:
			add(FieldAllowedInterfaces, "%s: name the exit addresses you expect, or say that any will do, or any but physical, or physical: one of the four", ifc.Name)
		case n == 0:
			add(FieldAllowedInterfaces, "%s: enter the exit address you expect there, or say that any will do, or any but physical, or physical", ifc.Name)
		}
		for _, a := range ifc.ExpectedExitAddresses {
			if _, err := ParsePublicIPv4(a); err != nil {
				add(FieldAllowedInterfaces, "%s: %q: %s", ifc.Name, a, err)
			}
		}
	}
	if len(c.Gate.ProbeURLs) == 0 {
		add(FieldProbeURLs, "enter a probe URL")
	}
	for _, u := range c.Gate.ProbeURLs {
		if err := checkProbeURL(u); err != nil {
			add(FieldProbeURLs, "%q: %s", u, err)
		}
	}
	if c.Gate.ProbePin != "" {
		if raw, err := base64.StdEncoding.DecodeString(c.Gate.ProbePin); err != nil || len(raw) != sha256.Size {
			add(FieldProbePin, "the pin must be the base64 SHA-256 of a public key")
		}
	}
	if _, err := ParsePublicIPv4(c.Gate.Resolver); err != nil {
		add(FieldResolver, "enter the resolver as a public IPv4 address such as %s", DefaultResolver)
	}

	for _, h := range c.HostConditions {
		switch h.Kind {
		case WiFiSSID:
			if h.Value == "" || len(h.Value) > 32 {
				add(FieldHostConditions, "a Wi-Fi network name is 1 to 32 bytes")
			}
		default:
			add(FieldHostConditions, "unknown Host Condition %q", h.Kind)
		}
	}

	switch c.Clock.Timezone {
	case "":
		add(FieldTimezone, "choose a timezone")
	case TimezoneAuto:
		if err := checkProbeURL(c.Clock.TimezoneProbeURL); err != nil {
			add(FieldTimezoneProbeURL, "%s", err)
		}
	default:
		if _, err := time.LoadLocation(c.Clock.Timezone); err != nil || c.Clock.Timezone == "Local" {
			add(FieldTimezone, "%q is not an IANA timezone name", c.Clock.Timezone)
		}
	}

	if c.Resources.CPUs < 0.25 || c.Resources.CPUs > 64 {
		add(FieldCPUs, "CPUs must be between 0.25 and 64")
	}
	if c.Resources.ShmMB < MinShmMB {
		add(FieldShm, "/dev/shm must be at least %d MB", MinShmMB)
	}
	// /dev/shm is charged to the memory limit. The Home and /tmp are on
	// the VM's disk, and are not.
	if c.Resources.MemoryMB < c.Resources.ShmMB+512 {
		add(FieldMemory, "memory must exceed /dev/shm by at least 512 MB, which counts against it")
	}

	seen := map[string]bool{}
	for _, p := range c.Packs {
		if p == "" {
			add(FieldPacks, "a Capability Pack has no name")
		} else if seen[p] {
			add(FieldPacks, "Capability Pack %q is listed twice", p)
		}
		seen[p] = true
	}

	if c.SharedFolder != "" && !ValidSharedFolderName(c.SharedFolder) {
		add(FieldSharedFolder, "a Shared Folder's name is 1 to 32 lower-case letters, digits or hyphens, such as research")
	}

	// A Host Tool runs in the sandbox's Shared Folder, and what is added
	// to its Wall is added for Host Tools the sandbox has.
	switch {
	case c.HostTools.Enabled && c.SharedFolder == "":
		add(FieldHostTools, "Host Tools run in the sandbox's Shared Folder: give the sandbox one")
	case !c.HostTools.Enabled && (c.HostTools.GPU || c.HostTools.MacName || len(c.HostTools.Applications) > 0):
		add(FieldHostTools, "the GPU, this Mac's name and its Allowed Applications are given to Host Tools, which the sandbox does not have")
	}
	applications := map[string]bool{}
	for _, name := range c.HostTools.Applications {
		switch {
		case !ValidApplicationName(name):
			add(FieldHostTools, "an Allowed Application is the name of one folder of /Applications, such as Blender.app: %q is not", name)
		case applications[name]:
			add(FieldHostTools, "%s is an Allowed Application twice", name)
		}
		applications[name] = true
	}
	return errs
}

// ParseExitAddress reads an exit address as a probe answers it: one IPv4
// or IPv6 address, nothing else.
func ParseExitAddress(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, errors.New("not an IP address")
	}
	if a.Zone() != "" {
		return netip.Addr{}, errors.New("an exit address has no zone")
	}
	return a.Unmap(), nil
}

// ParsePublicIPv4 reads one public IPv4 address: what an expected exit
// address and a resolver are, since IPv4 is all that leaves a sandbox.
func ParsePublicIPv4(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, errors.New("not an IP address")
	}
	if !a.Is4() {
		return netip.Addr{}, errors.New("not an IPv4 address")
	}
	if !PublicAddress(a) {
		return netip.Addr{}, errors.New("not a public address")
	}
	return a, nil
}

// ProbePort is the one port a probe URL may name: the gatekeeper's probe
// reaches nothing else.
const ProbePort = "443"

// checkProbeURL checks a URL the gatekeeper fetches, a probe URL or the
// timezone probe URL: HTTPS on port 443 without credentials, at a public
// IPv4 address or at a name that could be a public one.
func checkProbeURL(raw string) error {
	if raw == "" {
		return errors.New("enter an https:// URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a URL")
	}
	if u.Scheme != "https" {
		return errors.New("the URL must start with https://")
	}
	if u.User != nil {
		return errors.New("the URL must not carry credentials")
	}
	if port := u.Port(); port != "" && port != ProbePort {
		return errors.New("the URL must use port " + ProbePort)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errors.New("the URL has no host")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		if _, err := ParsePublicIPv4(host); err != nil {
			return errors.New("the URL's address is " + err.Error())
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("the URL's host must be a public name")
	}
	return nil
}
