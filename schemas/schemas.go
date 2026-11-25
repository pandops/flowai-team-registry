// Package schemas embeds the JSON Schemas of every message and request/reply
// body team-registry publishes or answers. The files are the contract that
// consumers read; this package lets the service validate what it receives and
// publishes.
package schemas

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.json
var files embed.FS

// FS returns the embedded schema files.
func FS() embed.FS { return files }

const baseURL = "https://flowai.local/schemas/team-registry/"

var (
	once     sync.Once
	compiled map[string]*jsonschema.Schema
	errs     map[string]error
)

func load() {
	compiled = map[string]*jsonschema.Schema{}
	errs = map[string]error{}
	entries, _ := fs.ReadDir(files, ".")
	c := jsonschema.NewCompiler()
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		data, err := files.ReadFile(e.Name())
		if err != nil {
			errs[e.Name()] = fmt.Errorf("compile %s: %w", e.Name(), err)
			continue
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			errs[e.Name()] = fmt.Errorf("compile %s: %w", e.Name(), err)
			continue
		}
		if err := c.AddResource(baseURL+e.Name(), doc); err != nil {
			errs[e.Name()] = fmt.Errorf("compile %s: %w", e.Name(), err)
			continue
		}
		names = append(names, e.Name())
	}
	for _, n := range names {
		s, err := c.Compile(baseURL + n)
		if err != nil {
			errs[n] = fmt.Errorf("compile %s: %w", n, err)
			continue
		}
		compiled[n] = s
	}
}

// Validate validates data against the schema file name (for example
// "event.team_created.schema.json").
func Validate(name string, data []byte) error {
	once.Do(load)
	if err, ok := errs[name]; ok {
		return err
	}
	s, ok := compiled[name]
	if !ok {
		return fmt.Errorf("unknown schema %q", name)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return s.Validate(inst)
}

var published = regexp.MustCompile(`^team-registry\.(event|audit)\.([a-z]+(?:_[a-z]+)*)\.pub$`)

// ForSubject returns the schema file name of a published subject
// (team-registry.event.<kind>.pub -> event.<kind>.schema.json).
func ForSubject(subject string) (string, error) {
	m := published.FindStringSubmatch(subject)
	if m == nil {
		return "", fmt.Errorf("team-registry publishes nothing on %q", subject)
	}
	name := m[1] + "." + m[2] + ".schema.json"
	if _, err := fs.Stat(files, name); err != nil {
		return "", fmt.Errorf("no schema for %q", subject)
	}
	return name, nil
}
