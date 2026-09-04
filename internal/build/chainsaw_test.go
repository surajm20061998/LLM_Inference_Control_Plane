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

package build

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Chainsaw suites cannot be RUN without a cluster, which makes them the one
// tier where an authoring mistake can sit undetected for a long time.
// `chainsaw lint` validates their schema; these tests check the semantics
// that a schema cannot express, and each exists because the mistake was actually
// made.

// TestEveryChainsawScriptDeclaresBash guards a portability trap that is
// invisible on macOS.
//
// Chainsaw runs script steps with `sh -c`. On an Ubuntu runner /bin/sh is
// DASH, which does not implement `set -o pipefail` and aborts the whole step
// with "Illegal option". On macOS /bin/sh is bash in POSIX mode, where the same
// script runs fine — so every one of these steps passed locally and every one
// would have failed in CI.
func TestEveryChainsawScriptDeclaresBash(t *testing.T) {
	t.Parallel()

	suites := chainsawSuites(t)
	checked := 0

	for _, suite := range suites {
		var doc any
		if err := yaml.Unmarshal(suite.body, &doc); err != nil {
			t.Fatalf("%s: %v", suite.path, err)
		}

		for _, script := range findKey(doc, "script") {
			m, ok := script.(map[string]any)
			if !ok {
				continue
			}
			checked++

			shell, _ := m["shell"].(string)
			content, _ := m["content"].(string)

			if shell != "bash" {
				t.Errorf("%s: a script step does not declare `shell: bash` (got %q).\n"+
					"    Chainsaw defaults to `sh -c`, which is dash on Ubuntu runners.\n"+
					"    Script begins: %s",
					suite.path, shell, firstLine(content))
			}
		}
	}

	if checked == 0 {
		t.Fatal("no script steps were found; either the suites have none or the scan is broken")
	}
	t.Logf("checked %d script steps across %d suites", checked, len(suites))
}

// TestChainsawScriptsDoNotUseQuietGrepInPipelines guards a pipefail trap.
//
// `grep -q` exits immediately after its first match. When it is downstream of
// kubectl (or another producer) in a pipefail-enabled script, that closes the
// pipe while the producer may still be writing. The producer then exits with
// SIGPIPE (141), turning a successful assertion into a failed Chainsaw step.
// Capture the producer's complete output first, then run grep on a here-string.
func TestChainsawScriptsDoNotUseQuietGrepInPipelines(t *testing.T) {
	t.Parallel()

	quietGrepPipeline := regexp.MustCompile(`(?m)\|(?:[^\n|]*\|)?\s*grep\s+-q(?:\s|$)`)
	checked := 0

	for _, suite := range chainsawSuites(t) {
		var doc any
		if err := yaml.Unmarshal(suite.body, &doc); err != nil {
			t.Fatalf("%s: %v", suite.path, err)
		}

		for _, script := range findKey(doc, "script") {
			m, ok := script.(map[string]any)
			if !ok {
				continue
			}
			content, _ := m["content"].(string)
			checked++
			if quietGrepPipeline.MatchString(content) {
				t.Errorf("%s pipes into `grep -q`; under pipefail an early match can give the producer SIGPIPE (exit 141). Capture the output first and grep a here-string", suite.path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no script steps were found; either the suites have none or the scan is broken")
	}
}

// TestChainsawConditionsAreLookedUpNotIndexed guards the assertion trap.
//
// Chainsaw compares arrays ELEMENT BY ELEMENT, BY INDEX. So this:
//
//	status:
//	  conditions:
//	    - type: Ready
//	      status: "True"
//
// does not mean "the Ready condition is True". It means "conditions[0] is Ready
// and True" — and this operator's conditions[0] is SpecValid, always. The
// assertion fails for a reason that has nothing to do with what it was written
// to check, and the failure message points at the wrong field.
//
// The correct form is a JMESPath lookup, which is also order-independent:
//
//	(conditions[?type == 'Ready'] | [0].status): "True"
func TestChainsawConditionsAreLookedUpNotIndexed(t *testing.T) {
	t.Parallel()

	for _, suite := range chainsawFiles(t) {
		var doc any
		if err := yaml.Unmarshal(suite.body, &doc); err != nil {
			t.Fatalf("%s: %v", suite.path, err)
		}

		for _, conditions := range findKey(doc, "conditions") {
			list, ok := conditions.([]any)
			if !ok {
				continue
			}
			for _, elem := range list {
				m, ok := elem.(map[string]any)
				if !ok {
					continue
				}
				if _, hasType := m["type"]; hasType {
					t.Errorf(
						"%s asserts on conditions as a positional LIST.\n"+
							"    Chainsaw matches arrays by index, and conditions[0] is whatever the\n"+
							"    controller set first — SpecValid here, never Ready.\n"+
							"    Use a JMESPath lookup instead:\n"+
							"        (conditions[?type == 'Ready'] | [0].status): \"True\"",
						suite.path)
				}
			}
		}
	}
}

// TestChainsawExpressionKeysAreFullyParenthesized guards the trap that the
// lookup form above walks straight into.
//
// Chainsaw decides whether a map key is a JMESPath EXPRESSION or a literal
// FIELD NAME with one regexp, `^\((?:(\w+);)?(.+)\)$` — the key must open
// with "(" and CLOSE with ")". So this, which reads perfectly naturally:
//
//	(conditions[?type == 'Ready'])[0].status: "True"
//
// is not an expression at all. The trailing "[0].status" sits outside the
// parentheses, the regexp does not match, and Chainsaw looks for a field
// literally named `(conditions[?type == 'Ready'])[0].status`. It reports
//
//	status.(conditions[?type == 'Ready'])[0].status: Required value: field not
//	found in the input object
//
// which reads like the CONTROLLER failed to set the condition, and sends you
// debugging the operator instead of the assertion. The whole path has to live
// inside the parentheses:
//
//	(conditions[?type == 'Ready'] | [0].status): "True"
//
// The pipe is load-bearing too. `[?...]` opens a JMESPath PROJECTION, and a
// following `[0]` applies to each projected element rather than to the list —
// so `(conditions[?type == 'Ready'][0].status)` indexes a map by 0 and fails
// with "types are not comparable". `|` closes the projection first.
func TestChainsawExpressionKeysAreFullyParenthesized(t *testing.T) {
	t.Parallel()

	// The same shapes Chainsaw's own expression parser recognises: a
	// parenthesised expression, or a backslash-escaped literal.
	expression := regexp.MustCompile(`^\((?:\w+;)?.+\)$`)
	escaped := regexp.MustCompile(`^\\.+\\$`)

	checked := 0

	for _, suite := range chainsawFiles(t) {
		var doc any
		if err := yaml.Unmarshal(suite.body, &doc); err != nil {
			t.Fatalf("%s: %v", suite.path, err)
		}

		for _, key := range mapKeys(doc) {
			// Strip the foreach and binding decorations Chainsaw peels off
			// before it looks for parentheses.
			stripped := key
			if m := foreachPrefix.FindStringSubmatch(stripped); m != nil {
				stripped = m[1]
			}
			if m := bindingSuffix.FindStringSubmatch(stripped); m != nil {
				stripped = m[1]
			}

			if !strings.ContainsAny(stripped, "()[]?|") {
				continue // an ordinary field name
			}
			checked++

			if expression.MatchString(stripped) || escaped.MatchString(stripped) {
				continue
			}

			t.Errorf(
				"%s uses the key %q, which Chainsaw reads as a LITERAL FIELD NAME.\n"+
					"    An expression key must open with \"(\" and close with \")\" — the whole\n"+
					"    path, not just the lookup. The assertion does not fail loudly; it\n"+
					"    reports \"field not found in the input object\" and looks like the\n"+
					"    controller never set the field.\n"+
					"    Write it as:\n"+
					"        (conditions[?type == 'Ready'] | [0].status): \"True\"",
				suite.path, key)
		}
	}

	if checked == 0 {
		t.Fatal("no expression-shaped keys were found; either the suites have none or the scan is broken")
	}
	t.Logf("checked %d expression-shaped keys", checked)
}

// Mirrors the decorations Chainsaw's expression parser strips before it decides
// whether what is left is a parenthesised expression.
var (
	foreachPrefix = regexp.MustCompile(`^~(?:\w+)?\.(.*)`)
	bindingSuffix = regexp.MustCompile(`(.*)\s*->\s*\w+$`)
)

// mapKeys walks a decoded YAML tree and returns every map key in it.
func mapKeys(node any) []string {
	var out []string
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			out = append(out, k)
			out = append(out, mapKeys(child)...)
		}
	case []any:
		for _, child := range v {
			out = append(out, mapKeys(child)...)
		}
	}
	return out
}

// suiteFile is one parsed Chainsaw YAML document.
type suiteFile struct {
	path string
	body []byte
}

// chainsawSuites returns the chainsaw-test.yaml of every suite.
func chainsawSuites(t *testing.T) []suiteFile {
	t.Helper()
	return readGlob(t, filepath.Join(repoRoot(t), "test", "chainsaw", "*", "chainsaw-test.yaml"))
}

// chainsawFiles returns every YAML file under test/chainsaw, because assertions
// also live in standalone assert-*.yaml files.
func chainsawFiles(t *testing.T) []suiteFile {
	t.Helper()
	return readGlob(t, filepath.Join(repoRoot(t), "test", "chainsaw", "*", "*.yaml"))
}

func readGlob(t *testing.T, pattern string) []suiteFile {
	t.Helper()

	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no files matched %s", pattern)
	}

	root := repoRoot(t)
	out := make([]suiteFile, 0, len(paths))
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, suiteFile{path: filepath.ToSlash(rel), body: body})
	}
	return out
}

// findKey walks a decoded YAML tree and returns every value stored under key.
func findKey(node any, key string) []any {
	var out []any
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if k == key {
				out = append(out, child)
			}
			out = append(out, findKey(child, key)...)
		}
	case []any:
		for _, child := range v {
			out = append(out, findKey(child, key)...)
		}
	}
	return out
}

// firstLine returns the first non-blank line of a script, for error messages.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "(empty)"
}
