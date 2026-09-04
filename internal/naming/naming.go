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

// Package naming owns every name, label, selector, annotation and port the
// operator produces. It is deliberately the only place these strings appear.
//
// Several of the values here are effectively permanent:
//
//   - SelectorLabels becomes the child Deployment's spec.selector, which the
//     API server treats as IMMUTABLE. Adding a label to it later requires
//     deleting and recreating every Deployment in every cluster.
//   - FieldManager identifies this controller's ownership in every applied
//     object's managedFields. Renaming it strands that ownership and Server-Side
//     Apply silently stops converging.
//   - Object names are symmetric from day one ("<md>-primary" / "<md>-canary")
//     so that promoting a canary never has to rename anything.
//
// Treat changes to this file as an API break.
package naming

import (
	"fmt"
	"hash/fnv"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/util/rand"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

const (
	// Domain is the operator's DNS domain. It prefixes every custom label and
	// annotation key and forms the CRD's API group (inference.<Domain>).
	Domain = "llmcp.io"

	// AppName is the value of the app.kubernetes.io/name label on every owned
	// object, and the prefix of every metric this project emits.
	AppName = "llmcp"

	// ManagedBy is the value of the app.kubernetes.io/managed-by label.
	ManagedBy = "llmcp-controller"

	// FieldManager is the Server-Side Apply field manager for every object the
	// controller applies. PERMANENT: see the package comment.
	FieldManager = "llmcp-controller"
)

// Well-known Kubernetes recommended labels.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
)

// Operator-specific labels.
const (
	// LabelModelDeployment carries the owning ModelDeployment's name.
	// Part of the immutable Deployment selector.
	LabelModelDeployment = Domain + "/model-deployment"

	// LabelVariant is "primary" or "canary".
	// Part of the immutable Deployment selector.
	LabelVariant = Domain + "/variant"

	// LabelRevision carries the pod-template revision hash.
	//
	// NEVER put this in a selector. It changes on every spec change, and
	// spec.selector is immutable — including it would make every rollout a
	// delete-and-recreate.
	LabelRevision = Domain + "/revision"
)

// Annotations read from the ModelDeployment by the controller.
const (
	// AnnoPromote, set to "true", promotes a canary that is waiting for manual
	// approval. Consumed in a later sprint; reserved here so the key is stable.
	AnnoPromote = Domain + "/promote"

	// AnnoAbort, set to "true", aborts an in-flight canary and rolls back.
	AnnoAbort = Domain + "/abort"
)

// Component label values.
const (
	ComponentInferenceServer = "inference-server"
	ComponentRouter          = "router"
)

// ShimContainerName is the name of the metrics sidecar in every pod template.
//
// Like the engine container's name this is stable API: `kubectl logs -c shim`
// appears in runbooks, and Server-Side Apply matches containers by name, so a
// rename would orphan the old container rather than update it.
const ShimContainerName = "shim"

// GrafanaDashboardLabel is the label the Grafana sidecar watches ConfigMaps
// for. Its VALUE is the string "1", not the boolean true: the sidecar compares
// the label value against a configured string, and YAML's `true` serialises to
// a label value of "true", which does not match.
const (
	GrafanaDashboardLabel = "grafana_dashboard"
	GrafanaDashboardValue = "1"

	// GrafanaFolderAnnotation places dashboards in a named Grafana folder
	// rather than scattering them at the root.
	GrafanaFolderAnnotation = "grafana_folder"
	GrafanaFolder           = "LLM Inference"
)

// Ports. Fixed in Sprint 1 so that nothing downstream has to move.
//
// The engine listens on EnginePort. Once the metrics shim is introduced it
// fronts the engine on ShimPort and takes over the "http" container port name;
// the Service's own port and port NAME never change, because ServiceMonitor
// selects endpoints by port name and renaming one silently breaks scraping.
const (
	// ServicePort is the port the ModelDeployment's Service listens on.
	ServicePort int32 = 8080

	// EnginePort is the container port the inference engine serves on.
	EnginePort int32 = 8000

	// ShimPort is the container port the metrics shim serves proxied traffic on.
	ShimPort int32 = 8080

	// ShimMetricsPort is where the shim exposes Prometheus metrics.
	//
	// A separate port, not a path on ShimPort, and the Service that fronts it
	// is a different Service from the serving one. Sharing a listener would
	// publish /metrics to every client that can reach the inference endpoint,
	// and — worse for this project specifically — would let a client bypass the
	// proxy path whose measurements are the point of the sidecar.
	ShimMetricsPort int32 = 9090
)

// Port names. Stable forever: ServiceMonitor selects endpoints by port NAME,
// and renaming one silently stops scraping with no error anywhere.
const (
	// PortNameHTTP is the client-facing OpenAI-compatible port. It is the shim
	// when the shim is enabled and the engine when it is not, which is exactly
	// why the Service targets it by name rather than by number.
	PortNameHTTP = "http"

	// PortNameEngine is the engine's own port. When the shim is enabled this is
	// reachable only inside the pod and through the headless metrics Service —
	// never through the serving Service, or clients could bypass the proxy that
	// produces every measurement this project makes decisions on.
	PortNameEngine = "engine"

	// PortNameMetrics is the shim's Prometheus endpoint.
	PortNameMetrics = "metrics"
)

// HTTP paths the shim serves on its own behalf, alongside the proxied engine
// surface. They are under /llmcp/ so they cannot collide with an engine route,
// present or future.
const (
	// ShimHealthPath proxies the engine's health endpoint. It is what the
	// shim's READINESS probe targets, so the pod is only Ready when both the
	// shim is listening and the engine behind it has loaded its model.
	ShimHealthPath = "/llmcp/healthz"

	// ShimLivePath reports only that the shim's own listener is up, without
	// touching the engine.
	//
	// The distinction is load-bearing for a native sidecar. The kubelet starts
	// the main container only after the sidecar's STARTUP probe passes, so a
	// startup probe that proxied to the engine would wait for a container that
	// has not been started yet — a deadlock that presents as a pod stuck in Init
	// forever. Startup targets this path; readiness targets ShimHealthPath.
	ShimLivePath = "/llmcp/livez"

	// ShimMetricsPath is the Prometheus scrape path, served on the metrics
	// listener only.
	ShimMetricsPath = "/metrics"
)

// maxNameLen is the longest name we will generate. Kubernetes allows 253
// characters for a Deployment's metadata.name, but the Deployment controller
// appends a pod-template-hash to derive ReplicaSet and Pod names, and label
// VALUES (which these names become, via app.kubernetes.io/instance) are capped
// at 63. 63 is therefore the real limit.
const maxNameLen = 63

// CommonLabels are applied to every object the controller owns. They are
// metadata only: none of them appear in an immutable selector, so this set can
// grow safely.
func CommonLabels(mdName string) map[string]string {
	return map[string]string{
		LabelName:            AppName,
		LabelInstance:        mdName,
		LabelManagedBy:       ManagedBy,
		LabelComponent:       ComponentInferenceServer,
		LabelModelDeployment: mdName,
	}
}

// SelectorLabels is the child Deployment's spec.selector.matchLabels and the
// minimal label set its pods must carry.
//
// IMMUTABLE. It contains exactly these two labels, forever. See the package
// comment before considering a change.
func SelectorLabels(mdName string, variant inferencev1alpha1.Variant) map[string]string {
	return map[string]string{
		LabelModelDeployment: mdName,
		LabelVariant:         string(variant),
	}
}

// PodLabels is the full label set stamped on pods: the immutable selector plus
// descriptive metadata. The revision hash is included so that pods can be
// attributed to a revision by queries and dashboards — but note it is
// deliberately absent from SelectorLabels.
func PodLabels(mdName string, variant inferencev1alpha1.Variant, revision string) map[string]string {
	labels := CommonLabels(mdName)
	maps.Copy(labels, SelectorLabels(mdName, variant))
	if revision != "" {
		labels[LabelRevision] = revision
	}
	return labels
}

// ServiceSelector spans BOTH variants, so a single Service load-balances across
// primary and canary pods. Serialized, this is also status.selector — which an
// HPA uses to find the pods whose metrics it averages, so it must match the
// whole fleet or the HPA divides by the wrong pod count.
func ServiceSelector(mdName string) map[string]string {
	return map[string]string{
		LabelModelDeployment: mdName,
	}
}

// VariantServiceSelector selects a single variant. Used by the per-variant
// Services that metric analysis scrapes separately.
func VariantServiceSelector(mdName string, variant inferencev1alpha1.Variant) map[string]string {
	return SelectorLabels(mdName, variant)
}

// PrimaryDeployment is the Deployment name for the stable variant.
func PrimaryDeployment(mdName string) string {
	return withSuffix(mdName, "-primary")
}

// CanaryDeployment is the Deployment name for the canary variant.
func CanaryDeployment(mdName string) string {
	return withSuffix(mdName, "-canary")
}

// DeploymentFor returns the Deployment name for a variant.
func DeploymentFor(mdName string, variant inferencev1alpha1.Variant) string {
	switch variant {
	case inferencev1alpha1.VariantCanary:
		return CanaryDeployment(mdName)
	default:
		return PrimaryDeployment(mdName)
	}
}

// Service is the stable Service name. It equals the ModelDeployment name so
// that the in-cluster endpoint is predictable: http://<name>.<ns>.svc:8080/v1
func Service(mdName string) string {
	return withSuffix(mdName, "")
}

// Endpoint is the OpenAI-compatible base URL published in status.
//
// port is spec.serving.port, and 0 means "unset", which resolves to ServicePort
// — the same rule buildService applies when it renders the Service. Taking the
// port as an argument rather than assuming the constant is the whole point:
// status.endpoint is the address this operator TELLS people to use, so a
// deployment that moved its port off the default published a URL on which every
// client connection is refused.
func Endpoint(mdName, namespace string, port int32) string {
	if port == 0 {
		port = ServicePort
	}
	return fmt.Sprintf("http://%s.%s.svc:%d/v1", Service(mdName), namespace, port)
}

// MetricsService is the headless Service that Prometheus scrapes.
//
// It is deliberately a SECOND Service rather than extra ports on the serving
// one. Two reasons, both practical: the engine's own /metrics has to be
// reachable for the diagnostics scrape, and exposing the engine's port on the
// serving Service would let any in-cluster client address the engine directly,
// skipping the shim — at which point that traffic contributes to no metric and
// the canary analysis is quietly reasoning about a subset of the load. Headless
// (clusterIP: None) because a scraper wants individual pod addresses, not one
// load-balanced VIP that lands on a different pod every scrape.
func MetricsService(mdName string) string {
	return withSuffix(mdName, "-metrics")
}

// ServiceMonitor is the prometheus-operator ServiceMonitor name.
func ServiceMonitor(mdName string) string {
	return withSuffix(mdName, "")
}

// PrometheusRule is the recording- and alerting-rule object's name.
//
// Suffixed rather than sharing the ServiceMonitor's name, even though they are
// different kinds and could collide safely. Two reasons: `kubectl get
// prometheusrules` is legible without cross-referencing, and prometheus-operator
// derives the generated rule FILE name from this, so a distinct name keeps the
// two apart in Prometheus's own config directory.
func PrometheusRule(mdName string) string {
	return withSuffix(mdName, "-slo")
}

// ControllerRevision names a revision history entry.
func ControllerRevision(mdName, revision string) string {
	return withSuffix(mdName, "-"+revision)
}

// withSuffix appends suffix to name, keeping the result a valid DNS-1123 label
// of at most 63 characters.
//
// When name+suffix would overflow, the NAME is truncated and a short hash of
// the full original is inserted, so that two long names sharing a prefix cannot
// collide. The suffix is preserved in preference to the name — it is what
// distinguishes "-primary" from "-canary", and losing it would collapse two
// Deployments onto one name.
func withSuffix(name, suffix string) string {
	if len(name)+len(suffix) <= maxNameLen {
		return name + suffix
	}

	h := fnv.New32a()
	// Hash the full original so distinct inputs yield distinct outputs even
	// when their truncated prefixes are identical.
	_, _ = h.Write([]byte(name))
	digest := rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
	if len(digest) > 8 {
		digest = digest[:8]
	}

	// Reserve room for at least one character of name, the separator, and the
	// digest. A suffix longer than that budget must itself be truncated:
	// emitting an over-long name would be rejected by the API server at
	// admission, and for a revision object that means no history is recorded
	// and a later rollback has nothing to roll back to. A shortened suffix is
	// the lesser failure, and the digest still guarantees uniqueness.
	maxSuffix := maxNameLen - len(digest) - 2
	if len(suffix) > maxSuffix {
		suffix = suffix[:maxSuffix]
	}

	keep := min(maxNameLen-len(suffix)-len(digest)-1, len(name))
	keep = max(keep, 1)

	// Truncating the suffix can leave a trailing "-", which is not a valid
	// DNS-1123 label ending.
	return strings.TrimRight(name[:keep]+"-"+digest+suffix, "-")
}
