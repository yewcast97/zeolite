package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"zeolite/internal/sandbox"
)

// An agent finds the sandbox's MCP servers in its own registry: the file
// its maker has it read, in the shape its maker gave it. The supervisor
// writes the servers into every known agent's registry at each start.
//
// Every server is the same command there: this program, as the bridge,
// with the server's name. The bridge looks the server up, starts it and
// logs what crosses it. So an entry is the app's exactly when its
// command is this program: such entries are replaced at each start, with
// no list kept of what was written, and everything else in a registry is
// its user's and the agent's, and stays as it is.

// bridgeCommand is the word after this program's name in every entry of
// the app's.
const bridgeCommand = "mcp-bridge"

// registryError says why a registry was left as it is.
func registryError(file string, why any) error {
	return fmt.Errorf("the MCP registry %s is left as it is: %v", file, why)
}

// writeRegistries writes the sandbox's MCP servers into the registry of
// every agent the app knows, and returns what could not be written: an
// agent's own registry under home, and one that is the app's own file
// in own. There is always a server to write: every sandbox has the one
// by which an agent raises its hand.
func writeRegistries(home, own, self string, servers []string) []error {
	var errs []error
	for _, a := range sandbox.KnownAgents {
		r := a.Registry
		file := filepath.Join(home, r.File)
		if r.Variable != "" {
			file = filepath.Join(own, filepath.Base(r.File))
		}
		var err error
		switch {
		case r.Variable != "":
			err = writeOwnRegistry(file, self, r, servers)
		case r.Format == sandbox.RegistryTOML:
			err = mergeTOMLRegistry(file, self, r, servers)
		default:
			err = mergeJSONRegistry(file, self, r, servers)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// waitFor is how long an agent is told to wait for one call of a
// server, in the unit its registry counts in: one call can take
// minutes, where an agent would by itself give up after seconds.
func waitFor(r sandbox.Registry) int64 { return int64(mcpTimeout / r.TimeoutUnit) }

// jsonEntry is one server of the app's in a JSON registry, as the
// registry's agent writes one down.
func jsonEntry(self string, r sandbox.Registry, name string) json.RawMessage {
	entry := map[string]any{}
	if r.Type != "" {
		entry["type"] = r.Type
	}
	if r.CommandList {
		entry["command"] = []string{self, bridgeCommand, name}
	} else {
		entry["command"] = self
		entry["args"] = []string{bridgeCommand, name}
	}
	if r.Enabled {
		entry["enabled"] = true
	}
	if r.TimeoutKey != "" {
		entry[r.TimeoutKey] = waitFor(r)
	}
	data, _ := json.Marshal(entry)
	return data
}

// appsJSONEntry reports whether a server of a JSON registry is one the
// app wrote: its command is this program, as the bridge.
func appsJSONEntry(self string, raw json.RawMessage) bool {
	var entry struct {
		Command json.RawMessage `json:"command"`
		Args    []string        `json:"args"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return false
	}
	var program string
	if json.Unmarshal(entry.Command, &program) == nil {
		return program == self && len(entry.Args) > 0 && entry.Args[0] == bridgeCommand
	}
	var line []string
	if json.Unmarshal(entry.Command, &line) == nil {
		return len(line) > 1 && line[0] == self && line[1] == bridgeCommand
	}
	return false
}

// member is one key of a JSON object with its value as it was written.
type member struct {
	key   string
	value json.RawMessage
}

// members reads a JSON object's keys in their order, each with its
// value's own text. What is no JSON object, or holds anything after it,
// is an error.
func members(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return nil, errors.New("it is not a JSON object")
	}
	var out []member
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("it is not a JSON object")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		out = append(out, member{name, value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("something follows its JSON object")
	}
	return out, nil
}

// object writes members as a JSON object, one to a line, each value as
// it was.
func object(ms []member, indent string) []byte {
	if len(ms) == 0 {
		return []byte("{}")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, m := range ms {
		key, _ := json.Marshal(m.key)
		b.WriteString(indent + "  ")
		b.Write(key)
		b.WriteString(": ")
		b.Write(m.value)
		if i < len(ms)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(indent + "}")
	return b.Bytes()
}

// mergeJSONRegistry makes a JSON registry hold the sandbox's servers:
// the entries the app wrote before go, the ones of now come, and every
// other key and server keeps its place and its text. A file that is not
// strict JSON is left as it is, and said.
func mergeJSONRegistry(file, self string, r sandbox.Registry, servers []string) error {
	data, err := os.ReadFile(file)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return registryError(file, err)
	}
	var top []member
	if !missing && len(bytes.TrimSpace(data)) > 0 {
		if top, err = members(data); err != nil {
			return registryError(file, err)
		}
	}
	at := -1
	var held []member
	for i, m := range top {
		if m.key == r.Servers {
			at = i
			if held, err = members(m.value); err != nil {
				return registryError(file, fmt.Sprintf("its %q: %v", r.Servers, err))
			}
		}
	}
	var kept []member
	for _, m := range held {
		if !appsJSONEntry(self, m.value) {
			kept = append(kept, m)
		}
	}
	// A server of the user's own that has a name of the app's keeps it:
	// the app's is not written over it, and is said to be missing.
	var shadowed []string
	for _, name := range servers {
		taken := false
		for _, m := range kept {
			taken = taken || m.key == name
		}
		if taken {
			shadowed = append(shadowed, name)
			continue
		}
		kept = append(kept, member{name, jsonEntry(self, r, name)})
	}
	value := json.RawMessage(object(kept, "  "))
	if at >= 0 {
		top[at].value = value
	} else {
		top = append(top, member{r.Servers, value})
	}
	next := append(object(top, ""), '\n')
	if !bytes.Equal(next, data) {
		if err := replaceFile(file, next); err != nil {
			return registryError(file, err)
		}
	}
	if len(shadowed) > 0 {
		return fmt.Errorf("the MCP registry %s has servers of its own named %s: the sandbox's are not written over them", file, strings.Join(shadowed, ", "))
	}
	return nil
}

// writeOwnRegistry writes a registry that is the app's own file, whole:
// the sandbox's servers and nothing else.
func writeOwnRegistry(file, self string, r sandbox.Registry, servers []string) error {
	var held []member
	for _, name := range servers {
		held = append(held, member{name, jsonEntry(self, r, name)})
	}
	next := append(object([]member{{r.Servers, object(held, "  ")}}, ""), '\n')
	if err := replaceFile(file, next); err != nil {
		return registryError(file, err)
	}
	return nil
}

// replaceFile writes a file beside its place and moves it there, so that
// it is never read half written. It keeps the mode of the file that was
// there; a new one is its user's alone, as an agent's own files are.
func replaceFile(file string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(file); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".zeolite-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// tomlKey is one part of a TOML table's name: a bare word, or a string.
const tomlKey = `(?:[A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*')`

// tomlHeader is a line that begins a TOML table, and nothing else.
var tomlHeader = regexp.MustCompile(`^[ \t]*\[\[?[ \t]*` + tomlKey + `(?:[ \t]*\.[ \t]*` + tomlKey + `)*[ \t]*\]\]?[ \t]*(?:#.*)?$`)

// bareTOMLKey is a table's name that needs no quotes.
var bareTOMLKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlTable is a server of the app's as a TOML table.
func tomlTable(self string, r sandbox.Registry, name string) string {
	key := name
	if !bareTOMLKey.MatchString(name) {
		key = strconv.Quote(name)
	}
	table := fmt.Sprintf("[%s.%s]\ncommand = %s\nargs = [%s, %s]\n", r.Servers, key, strconv.Quote(self), strconv.Quote(bridgeCommand), strconv.Quote(name))
	if r.TimeoutKey != "" {
		table += fmt.Sprintf("%s = %d\n", r.TimeoutKey, waitFor(r))
	}
	return table
}

// appsTOMLServers are the servers of a parsed TOML registry that the app
// wrote: their command is this program, as the bridge.
func appsTOMLServers(self string, r sandbox.Registry, parsed map[string]any) map[string]bool {
	out := map[string]bool{}
	servers, _ := parsed[r.Servers].(map[string]any)
	for name, value := range servers {
		server, _ := value.(map[string]any)
		args, _ := server["args"].([]any)
		if server["command"] == self && len(args) > 0 && args[0] == bridgeCommand {
			out[name] = true
		}
	}
	return out
}

// withoutServers is a parsed TOML registry without some of its servers.
func withoutServers(r sandbox.Registry, parsed map[string]any, names map[string]bool) map[string]any {
	out := map[string]any{}
	for key, value := range parsed {
		out[key] = value
	}
	if servers, ok := parsed[r.Servers].(map[string]any); ok {
		rest := map[string]any{}
		for name, value := range servers {
			if !names[name] {
				rest[name] = value
			}
		}
		if len(rest) > 0 {
			out[r.Servers] = rest
		} else {
			delete(out, r.Servers)
		}
	}
	return out
}

// mergeTOMLRegistry makes a TOML registry hold the sandbox's servers.
// The file's text is cut where its tables begin; the tables the app
// wrote before are dropped, and the ones of now are put at its end:
// every other line stays as it was, its comments with it. The result is
// written only if it reads, as TOML, as the file did but for the app's
// own servers: a file that does not read, or that a cut would change,
// is left as it is, and said.
func mergeTOMLRegistry(file, self string, r sandbox.Registry, servers []string) error {
	data, err := os.ReadFile(file)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return registryError(file, err)
	}
	before := map[string]any{}
	if _, err := toml.Decode(string(data), &before); err != nil {
		return registryError(file, err)
	}
	stale := appsTOMLServers(self, r, before)

	// The file in pieces: what stands before its first table, and then
	// each table from its header to the next.
	var pieces []string
	piece := ""
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if tomlHeader.MatchString(strings.TrimRight(line, "\r\n")) && piece != "" {
			pieces = append(pieces, piece)
			piece = ""
		}
		piece += line
	}
	if piece != "" {
		pieces = append(pieces, piece)
	}
	var next strings.Builder
	for _, piece := range pieces {
		// A piece that is one of the app's servers, and nothing else.
		parsed := map[string]any{}
		if _, err := toml.Decode(piece, &parsed); err == nil && len(parsed) == 1 {
			if mine := appsTOMLServers(self, r, parsed); len(mine) == 1 && len(withoutServers(r, parsed, mine)) == 0 {
				continue
			}
		}
		next.WriteString(piece)
	}
	text := next.String()
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	sorted := append([]string(nil), servers...)
	sort.Strings(sorted)
	for _, name := range sorted {
		if text != "" && !strings.HasSuffix(text, "\n\n") {
			text += "\n"
		}
		text += tomlTable(self, r, name)
	}

	// It must read as the file did, but for the app's own servers.
	after := map[string]any{}
	if _, err := toml.Decode(text, &after); err != nil {
		return registryError(file, fmt.Sprintf("with the sandbox's servers it would not read: %v", err))
	}
	wrote := appsTOMLServers(self, r, after)
	if !reflect.DeepEqual(withoutServers(r, before, stale), withoutServers(r, after, wrote)) || len(wrote) != len(servers) {
		return registryError(file, "writing the sandbox's servers would change what else it says")
	}
	if text == string(data) {
		return nil
	}
	if err := replaceFile(file, []byte(text)); err != nil {
		return registryError(file, err)
	}
	return nil
}
