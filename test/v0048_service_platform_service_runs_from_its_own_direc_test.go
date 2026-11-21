//go:build !integration

package test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Requirement: service-platform service runs from its own directory and image.
// "E2E uses the deployed services" belongs to the E2E agent and "Change to
// one service selects only its tests" to the test runner (qa-tooling): not
// applicable to team-registry.

// The service is self-contained: no package of team-registry imports another
// service's or executor's code.
func TestTeamRegistryImportsNoOtherServiceCode(t *testing.T) {
	root := serviceDir(t)
	const module = "github.com/pandops/flowai-team-registry/"
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/flowai/platform/") ||
				(strings.HasPrefix(p, "github.com/pandops/flowai-") && !strings.HasPrefix(p, module)) {
				t.Errorf("%s imports %s", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go file found under the team-registry module")
	}
}
