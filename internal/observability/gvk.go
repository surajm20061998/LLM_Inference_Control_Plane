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

// Package observability renders everything the operator publishes for
// Prometheus and Grafana: the ServiceMonitor that gets a ModelDeployment
// scraped, and the dashboards that make the result legible.
//
// # Why prometheus-operator types are unstructured here
//
// The obvious approach is to import github.com/prometheus-operator/
// prometheus-operator/pkg/apis/monitoring/v1 and build a typed ServiceMonitor.
// This package deliberately does not, for two reasons.
//
// The first is dependency weight: that module pulls a large transitive graph
// into a controller that needs exactly one object kind from it, and every
// Kubernetes minor bump then has to be reconciled across two API surfaces
// instead of one.
//
// The second is the one that actually matters. Registering the type in the
// scheme invites watching it, and a manager asked to watch a kind whose CRD is
// absent fails at Start() with meta.NoKindMatchError — permanently, because the
// RESTMapper backing an informer is not re-resolved when the CRD appears later.
// Building the object as unstructured makes it structurally impossible for the
// operator to acquire a hard dependency on an optional CRD: it is applied and
// never read back.
package observability

import "k8s.io/apimachinery/pkg/runtime/schema"

// The prometheus-operator API surface this operator writes to.
const (
	MonitoringGroup   = "monitoring.coreos.com"
	MonitoringVersion = "v1"

	KindServiceMonitor = "ServiceMonitor"
	KindPrometheusRule = "PrometheusRule"
)

// ServiceMonitorGVK identifies the kind the discovery probe looks for before
// the controller attempts to create one.
var ServiceMonitorGVK = schema.GroupVersionKind{
	Group:   MonitoringGroup,
	Version: MonitoringVersion,
	Kind:    KindServiceMonitor,
}

// PrometheusRuleGVK is the recording- and alerting-rule kind. Declared here
// alongside its sibling so both probes read from one place.
var PrometheusRuleGVK = schema.GroupVersionKind{
	Group:   MonitoringGroup,
	Version: MonitoringVersion,
	Kind:    KindPrometheusRule,
}
