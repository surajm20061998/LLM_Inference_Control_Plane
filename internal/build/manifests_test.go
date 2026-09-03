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
	"strings"
	"testing"
)

// optionalCRDGroups are API groups this project treats as OPTIONAL: the
// operator probes for them at runtime and degrades gracefully when they are
// absent.
//
// A manifest overlay that a fresh cluster is expected to apply must not
// reference them, because `kubectl apply` has no such graceful path — it fails
// the whole apply with:
//
//	no matches for kind "ServiceMonitor" in version "monitoring.coreos.com/v1"
var optionalCRDGroups = []string{
	"monitoring.coreos.com",
}

// baseOverlays are the overlays a cluster with nothing pre-installed must be
// able to apply. config/monitoring is deliberately absent: it is applied by
// `make monitoring-install`, after Helm has created the CRDs.
var baseOverlays = []string{
	"config/default",
	"config/dev",
	"config/crd",
	"config/rbac",
}

// TestBaseOverlaysDoNotRequireOptionalCRDs is the regression test for a bug
// that made the documented quick start impossible.
//
// config/dev contained a ServiceMonitor, so `kubectl apply` of it failed on any
// cluster without prometheus-operator — and the README tells you to run
// `make dev-deploy` BEFORE `make monitoring-install`. The operator itself
// handles the same situation correctly, probing for the CRD and reporting
// MetricsRegistered instead of failing; the manifests had simply not been held
// to the same standard.
//
// It reads the YAML directly rather than shelling out to kustomize, so it needs
// no downloaded binary and runs in the ordinary unit job.
func TestBaseOverlaysDoNotRequireOptionalCRDs(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	checked := 0

	for _, overlay := range baseOverlays {
		dir := filepath.Join(root, overlay)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", overlay, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			rel := overlay + "/" + entry.Name()

			body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			checked++

			for _, group := range optionalCRDGroups {
				// Match the apiVersion line specifically. A prose mention of
				// the group in a comment is fine and, in these files,
				// deliberate — the comments explain why the resources live
				// elsewhere.
				for line := range strings.SplitSeq(string(body), "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "#") {
						continue
					}
					if strings.HasPrefix(trimmed, "apiVersion:") && strings.Contains(trimmed, group) {
						t.Errorf(
							"%s declares a resource in the optional API group %q.\n"+
								"    `kubectl apply` of this overlay fails outright on a cluster without\n"+
								"    that CRD — it has no graceful path the way the operator does.\n"+
								"    Move the resource to config/monitoring, which is applied by\n"+
								"    `make monitoring-install` after the CRDs exist.",
							rel, group)
					}
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no overlay YAML was inspected; the scan is broken")
	}
	t.Logf("inspected %d manifest files across %d overlays", checked, len(baseOverlays))
}
