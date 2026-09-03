/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package build holds checks about how this repository is PACKAGED, as opposed
// to how it behaves.
//
// It exists because of a whole class of failure that every other test tier is
// blind to: code that compiles and passes locally but cannot be built into a
// container image. The developer's working tree has every file in it; the
// Docker build context has only what .dockerignore lets through. Nothing in
// `go build ./...`, `go vet`, the linter or envtest can see that difference.
package build

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
)

// TestEmbeddedFilesSurviveTheDockerContext is the regression test for a real
// CI failure.
//
// # What happened
//
// The Grafana dashboards were added in Sprint 3 with a `//go:embed
// dashboards/*.json` directive. `.dockerignore` ignores everything and
// re-includes only `**/*.go`, `go.mod` and `go.sum`, so the JSON files never
// reached the build context. The result:
//
//	internal/observability/dashboards.go:48:12:
//	    pattern dashboards/*.json: no matching files found
//
// `//go:embed` is resolved by the COMPILER, so a pattern matching nothing is a
// build error rather than an empty variable. That is the good part — it fails
// loudly. The bad part is WHERE it fails: only inside the container. Every
// local build, every test, every lint run succeeded, because they all see a
// working tree that has the files. Four sprints passed before an image was
// built.
//
// # Why the test looks like this
//
// It uses Docker's own ignore matcher (github.com/moby/patternmatcher) rather
// than reimplementing the pattern semantics. Reimplementing them would mean a
// second, subtly different definition of what the build context contains —
// which is the same category of drift the test exists to catch, moved one level
// up.
func TestEmbeddedFilesSurviveTheDockerContext(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	matcher := dockerIgnoreMatcher(t, root)

	embeds := findEmbedPatterns(t, root)
	if len(embeds) == 0 {
		t.Fatal("no //go:embed directives were found; either they are gone (delete this test) " +
			"or the scan is broken")
	}

	for _, e := range embeds {
		matches, err := filepath.Glob(filepath.Join(e.Dir, e.Pattern))
		if err != nil {
			t.Errorf("%s: pattern %q is malformed: %v", e.File, e.Pattern, err)
			continue
		}
		if len(matches) == 0 {
			t.Errorf("%s: pattern %q matches nothing on disk; this is already a compile error",
				e.File, e.Pattern)
			continue
		}

		for _, abs := range matches {
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				t.Fatalf("relativising %s: %v", abs, err)
			}
			rel = filepath.ToSlash(rel)

			excluded, err := matcher.MatchesOrParentMatches(rel)
			if err != nil {
				t.Fatalf("matching %s against .dockerignore: %v", rel, err)
			}
			if excluded {
				t.Errorf(
					"%s embeds %s, but .dockerignore excludes it from the Docker build context.\n"+
						"    The image build will fail with:\n"+
						"        %s: pattern %s: no matching files found\n"+
						"    and it will fail ONLY in the container — every local build sees the file.\n"+
						"    Fix by adding to .dockerignore:\n"+
						"        !%s",
					e.File, rel, e.File, e.Pattern, filepath.Dir(rel)+"/*"+filepath.Ext(rel))
			}
		}
	}
}

// TestGoModAndSumReachTheContext guards the other half of the same edge.
//
// A deny-by-default .dockerignore whose re-include for go.sum was dropped
// produces "go.sum not found", which reads like a dependency problem rather
// than a packaging one and sends people to the wrong file.
func TestGoModAndSumReachTheContext(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	matcher := dockerIgnoreMatcher(t, root)

	for _, required := range []string{"go.mod", "go.sum"} {
		excluded, err := matcher.MatchesOrParentMatches(required)
		if err != nil {
			t.Fatalf("matching %s: %v", required, err)
		}
		if excluded {
			t.Errorf(".dockerignore excludes %s, so no image can be built", required)
		}
	}
}

// TestTestFilesDoNotReachTheContext checks the exclusion is still doing its job.
//
// Test files in an image are dead weight and, more to the point, they drag
// their own dependencies into the build — which is how a container ends up
// needing envtest binaries it will never run.
func TestTestFilesDoNotReachTheContext(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	matcher := dockerIgnoreMatcher(t, root)

	excluded, err := matcher.MatchesOrParentMatches("internal/controller/canary_test.go")
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if !excluded {
		t.Error("_test.go files are reaching the Docker build context")
	}
}

// embedRef is one //go:embed pattern and where it was declared.
type embedRef struct {
	// File is the repo-relative Go file carrying the directive.
	File string
	// Dir is the absolute directory that file lives in; embed patterns are
	// resolved relative to it.
	Dir string
	// Pattern is a single pattern from the directive.
	Pattern string
}

// findEmbedPatterns scans every non-test Go file for //go:embed directives.
//
// Non-test only, deliberately: a directive in a _test.go file is irrelevant
// here, because test files are excluded from the image on purpose.
func findEmbedPatterns(t *testing.T, root string) []embedRef {
	t.Helper()

	var out []embedRef
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", ".build", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			// A file this test cannot parse is not this test's problem; the
			// compiler will report it far more clearly.
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		for _, group := range file.Comments {
			for _, c := range group.List {
				text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
				if !strings.HasPrefix(text, "go:embed ") {
					continue
				}
				for pattern := range strings.FieldsSeq(strings.TrimPrefix(text, "go:embed ")) {
					out = append(out, embedRef{
						File:    filepath.ToSlash(rel),
						Dir:     filepath.Dir(path),
						Pattern: pattern,
					})
				}
			}
		}
		_ = ast.Inspect
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	return out
}

// dockerIgnoreMatcher builds Docker's own matcher from the repository's
// .dockerignore.
func dockerIgnoreMatcher(t *testing.T, root string) *patternmatcher.PatternMatcher {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatalf("reading .dockerignore: %v", err)
	}

	var patterns []string
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}

	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		t.Fatalf("parsing .dockerignore patterns: %v", err)
	}
	return matcher
}

// repoRoot returns the repository root, found by walking up to go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the working directory")
		}
		dir = parent
	}
}
