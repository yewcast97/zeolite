package configstore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zeolite/internal/sandbox"
)

func TestSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "zeolite")
	s := Open(dir)
	if configs, err := s.Load(); err != nil || configs != nil {
		t.Fatalf("an empty store loaded %v, %v", configs, err)
	}

	a := sandbox.NewConfig("a1", "Research")
	a.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun4", ExpectedExitAddresses: []string{"203.0.113.7"}},
		{Name: "en0", AnyExitAddress: true},
	}
	a.Gate = sandbox.GateConfig{
		ProbeURLs: []string{"https://probe.example.com/ip"},
		Resolver:  "9.9.9.9",
	}
	a.HostConditions = []sandbox.HostCondition{{Kind: sandbox.WiFiSSID, Value: "Office"}}
	a.Clock = sandbox.ClockPolicy{Timezone: "Europe/Berlin"}
	a.Packs = []string{"web-test-browser"}
	a.SharedFolder = "research"
	b := sandbox.NewConfig("b2", "New")
	want := []sandbox.Config{a, b}

	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Open(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded\n%+v\nwant\n%+v", got, want)
	}

	// The file is the user's alone.
	info, err := os.Stat(filepath.Join(dir, "sandboxes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the file's mode is %o", mode)
	}

	if err := s.Save(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(); len(got) != 0 {
		t.Errorf("after saving none, loaded %v", got)
	}
}

// The app keeps no key: a Config, and whatever a Config holds, has no
// field for a variable, a key, a token or a secret, so the store's file
// has none either.
func TestAConfigHasNoPlaceForAKey(t *testing.T) {
	seen := 0
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, where string) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem(), where)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				name := where + "." + field.Name
				seen++
				for _, never := range []string{"env", "key", "token", "secret", "password", "credential"} {
					if strings.Contains(strings.ToLower(field.Name+" "+field.Tag.Get("json")), never) {
						t.Errorf("%s: a config has a place for a %s", name, never)
					}
				}
				walk(field.Type, name)
			}
		}
	}
	walk(reflect.TypeOf(sandbox.Config{}), "Config")
	if seen < 15 {
		t.Fatalf("looked at %d fields: the test would prove little", seen)
	}
}

// A file that is not JSON is an error, and is left as it is.
func TestAFileThatIsNotJSONIsNotLoaded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "zeolite")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sandboxes.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir).Load(); err == nil {
		t.Error("a file that is not JSON was loaded")
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Errorf("a file that is not JSON was written again: %q", data)
	}
}
