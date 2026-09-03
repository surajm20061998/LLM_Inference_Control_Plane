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

package observability

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/surajm20061998/LLM_Inference_Control_Plane/internal/naming"
)

// dashboardFS holds the Grafana dashboards, compiled into the operator binary.
//
// # Why embedded rather than shipped as manifests
//
// A dashboard that has to be imported by hand is a dashboard that is out of
// date, and one shipped as a separate kustomize resource is a second artifact
// that can be deployed at a different version than the operator that produces
// the metrics it graphs. Embedding makes the dashboard and the metric names it
// queries the same build — if a panel references a series this binary does not
// emit, that is now a fact about one artifact rather than a mismatch between
// two.
//
//go:embed dashboards/*.json
var dashboardFS embed.FS

// dashboardDir is the embedded directory holding them.
const dashboardDir = "dashboards"

// Dashboard is one embedded Grafana dashboard.
type Dashboard struct {
	// Key is the ConfigMap data key, e.g. "control-plane.json".
	Key string

	// Name is the ConfigMap name, e.g. "llmcp-dashboard-control-plane".
	Name string

	// JSON is the dashboard definition.
	JSON string

	// UID is the dashboard's Grafana uid, read out of the JSON. It is what
	// makes a dashboard stable across redeploys — Grafana keys on it, so a
	// changed uid produces a duplicate dashboard rather than an update.
	UID string
}

// dashboardNamePrefix is shared by every dashboard ConfigMap so they can be
// listed and cleaned up as a set.
const dashboardNamePrefix = naming.AppName + "-dashboard-"

// Dashboards returns every embedded dashboard, ordered by key.
//
// The ordering is not cosmetic: these are applied in sequence and their names
// appear in logs, and an unordered walk of an embed.FS would make the log
// output differ between runs for no reason. It also removes any chance of the
// map-iteration nondeterminism that Server-Side Apply is intolerant of
// elsewhere in this codebase.
func Dashboards() ([]Dashboard, error) {
	entries, err := fs.ReadDir(dashboardFS, dashboardDir)
	if err != nil {
		return nil, fmt.Errorf("reading embedded dashboards: %w", err)
	}

	out := make([]Dashboard, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		body, err := fs.ReadFile(dashboardFS, path.Join(dashboardDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading dashboard %s: %w", e.Name(), err)
		}

		uid, err := dashboardUID(body)
		if err != nil {
			return nil, fmt.Errorf("dashboard %s: %w", e.Name(), err)
		}

		out = append(out, Dashboard{
			Key:  e.Name(),
			Name: dashboardNamePrefix + strings.TrimSuffix(e.Name(), ".json"),
			JSON: string(body),
			UID:  uid,
		})
	}

	slices.SortFunc(out, func(a, b Dashboard) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// dashboardUID extracts and validates the dashboard's uid.
//
// Parsing rather than trusting the file catches two mistakes at start-up
// instead of at demo time: malformed JSON, which the Grafana sidecar would
// import silently and then fail to render, and a missing uid, which makes every
// redeploy create a NEW dashboard alongside the old one until the folder is
// full of near-identical copies.
func dashboardUID(body []byte) (string, error) {
	var head struct {
		UID   string `json:"uid"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", fmt.Errorf("not valid JSON: %w", err)
	}
	if head.UID == "" {
		return "", fmt.Errorf("has no top-level \"uid\"; Grafana keys dashboards on it, " +
			"so without one every redeploy creates a duplicate instead of an update")
	}
	if head.Title == "" {
		return "", fmt.Errorf("has no top-level \"title\"")
	}
	return head.UID, nil
}

// DashboardConfigMap renders one dashboard as a ConfigMap the Grafana sidecar
// will pick up.
//
// The sidecar discovers dashboards by LABEL, watching for ConfigMaps carrying
// grafana_dashboard="1" and loading every JSON value it finds in them. Note the
// value is the string "1": writing `true` in YAML produces the label value
// "true", which does not match the sidecar's configured string and results in a
// ConfigMap that exists, looks correct, and is never read.
func DashboardConfigMap(d Dashboard, namespace string) *corev1ac.ConfigMapApplyConfiguration {
	labels := map[string]string{
		naming.LabelName:             naming.AppName,
		naming.LabelManagedBy:        naming.ManagedBy,
		naming.LabelComponent:        "dashboard",
		naming.GrafanaDashboardLabel: naming.GrafanaDashboardValue,
	}

	return corev1ac.ConfigMap(d.Name, namespace).
		WithLabels(labels).
		WithAnnotations(map[string]string{
			naming.GrafanaFolderAnnotation: naming.GrafanaFolder,
		}).
		WithData(map[string]string{d.Key: d.JSON})
}

// InstallDashboards applies every embedded dashboard into namespace.
//
// It is called once at manager start rather than from a reconcile loop, because
// dashboards belong to the OPERATOR, not to any individual ModelDeployment.
// Creating them per-resource would produce N copies of one dashboard and delete
// them all when the last ModelDeployment went away — which is exactly when
// someone is most likely to want to look at the graphs.
//
// A failure here is returned but is not fatal to the caller by design: the
// Grafana CRDs and the monitoring namespace are optional, and an operator that
// refused to start because it could not publish a dashboard would be trading a
// serving outage for a cosmetic one.
func InstallDashboards(ctx context.Context, c client.Client, namespace string) error {
	log := logf.FromContext(ctx).WithName("dashboards")

	dashboards, err := Dashboards()
	if err != nil {
		return err
	}

	for _, d := range dashboards {
		cm := DashboardConfigMap(d, namespace)
		if err := c.Apply(ctx, cm,
			client.FieldOwner(naming.FieldManager),
			client.ForceOwnership,
		); err != nil {
			return fmt.Errorf("applying dashboard %s: %w", d.Name, err)
		}
		log.Info("published dashboard", "name", d.Name, "uid", d.UID, "namespace", namespace)
	}

	return nil
}
