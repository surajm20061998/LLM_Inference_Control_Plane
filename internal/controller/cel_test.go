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

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestNoDefaultOnAFieldInsideAHasRule is a STRUCTURAL guard over the generated
// CRD, and it exists because this project shipped the bug it describes.
//
// # The bug
//
// A `+kubebuilder:default` on a field that a CEL rule tests with `has()` makes
// that rule unsatisfiable. The API server applies the default before validation
// runs, so the field is ALWAYS present, and `has(self.x)` is always true.
//
// An "at most one of x and y" rule written as
//
//	!(has(self.x) && has(self.y))
//
// therefore rejects any object that sets y — because the API server helpfully
// set x for them. The user did not write x. The error message says they did.
//
// It cost a rejected sample to find in Sprint 4, and only because a test
// applies the shipped samples against the real CRD. Nothing about the Go source
// looks wrong; the collision only exists in the generated schema.
//
// # The guard
//
// Walk every `x-kubernetes-validations` rule in the generated CRD, extract every
// `has(self.FIELD)`, and check that FIELD does not carry a `default` at the
// schema node the rule is attached to. That is mechanical, complete, and needs
// no fixture to stay in step with the API.
func TestNoDefaultOnAFieldInsideAHasRule(t *testing.T) {
	t.Parallel()

	schemas := loadCRDSchemas(t)
	if len(schemas) == 0 {
		t.Fatal("no CRD schemas were loaded; the check is not checking anything")
	}

	checked := 0
	for path, node := range schemas {
		rules, ok := node["x-kubernetes-validations"].([]any)
		if !ok {
			continue
		}

		props, _ := node["properties"].(map[string]any)

		for _, raw := range rules {
			rule, _ := raw.(map[string]any)
			expr, _ := rule["rule"].(string)
			if expr == "" {
				continue
			}

			for _, field := range hasReferences(expr) {
				checked++

				prop, exists := props[field].(map[string]any)
				if !exists {
					// A rule referencing a field that is not a sibling property
					// is either a typo or a reference into a nested object. Both
					// are worth surfacing.
					t.Errorf("%s: rule %q calls has(self.%s), but %s is not a property at that level",
						path, expr, field, field)
					continue
				}

				if _, hasDefault := prop["default"]; hasDefault {
					t.Errorf(
						"%s: field %q has a default AND is tested with has() in rule %q.\n"+
							"    The API server applies the default before validation, so has() is "+
							"always true and the rule can never be satisfied.\n"+
							"    Fix by removing the kubebuilder default and resolving it in Go, or "+
							"by rewriting the rule not to use has().",
						path, field, expr)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no has() references were found in any CEL rule; either the API has none " +
			"(in which case delete this test) or the extraction is broken")
	}
	t.Logf("checked %d has() references across the generated CRD", checked)
}

// hasSelfPattern matches has(self.field) with or without surrounding spaces.
var hasSelfPattern = regexp.MustCompile(`has\(\s*self\.([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

// hasReferences returns the field names a CEL rule tests with has(self.x).
func hasReferences(expr string) []string {
	matches := hasSelfPattern.FindAllStringSubmatch(expr, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// loadCRDSchemas returns every object schema node in the generated CRDs, keyed
// by a readable path.
func loadCRDSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()

	dir := filepath.Join("..", "..", "config", "crd", "bases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	out := map[string]map[string]any{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		var crd map[string]any
		if err := yaml.Unmarshal(body, &crd); err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}

		spec, _ := crd["spec"].(map[string]any)
		versions, _ := spec["versions"].([]any)
		for _, v := range versions {
			version, _ := v.(map[string]any)
			name, _ := version["name"].(string)
			schema, _ := version["schema"].(map[string]any)
			root, _ := schema["openAPIV3Schema"].(map[string]any)
			if root == nil {
				continue
			}
			collectSchemas(entry.Name()+"/"+name, root, out)
		}
	}

	return out
}

// collectSchemas walks a schema tree, recording every node.
func collectSchemas(path string, node map[string]any, out map[string]map[string]any) {
	out[path] = node

	if props, ok := node["properties"].(map[string]any); ok {
		for name, raw := range props {
			if child, ok := raw.(map[string]any); ok {
				collectSchemas(path+"."+name, child, out)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		collectSchemas(path+"[]", items, out)
	}
	if addl, ok := node["additionalProperties"].(map[string]any); ok {
		collectSchemas(path+"{}", addl, out)
	}
}

// TestEveryCELRuleCarriesAMessage checks that no rule falls back to the API
// server's generated text.
//
// The default rejection message quotes the CEL expression verbatim. For a rule
// like `[has(self.image), has(self.huggingFace), has(self.persistentVolumeClaim)]
// .filter(x, x).size() == 1` that is technically accurate and completely
// useless to whoever pasted a manifest — and a validation error is read at the
// moment someone is least equipped to decode one.
func TestEveryCELRuleCarriesAMessage(t *testing.T) {
	t.Parallel()

	schemas := loadCRDSchemas(t)
	found := 0

	for path, node := range schemas {
		rules, ok := node["x-kubernetes-validations"].([]any)
		if !ok {
			continue
		}
		for _, raw := range rules {
			rule, _ := raw.(map[string]any)
			expr, _ := rule["rule"].(string)
			msg, _ := rule["message"].(string)
			found++

			if strings.TrimSpace(msg) == "" {
				t.Errorf("%s: rule %q has no message; the API server would quote the raw "+
					"CEL expression at whoever pasted the manifest", path, expr)
			}
		}
	}

	if found == 0 {
		t.Fatal("no CEL rules were found at all")
	}
	t.Logf("checked %d CEL rules", found)
}
