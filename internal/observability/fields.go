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

// Field keys for the unstructured prometheus-operator objects this package
// builds.
//
// Named constants rather than literals because these objects are assembled as
// nested map[string]any, where the compiler checks nothing. A typo in a key —
// "namesapce", "annotaions" — produces an object the API server accepts (the
// CRD preserves unknown fields) and prometheus-operator silently ignores. The
// symptom is an alert that never fires or a rule Prometheus never loads, with
// no error anywhere. Constants at least make the typo a build failure.
const (
	fieldAPIVersion      = "apiVersion"
	fieldKind            = "kind"
	fieldMetadata        = "metadata"
	fieldSpec            = "spec"
	fieldName            = "name"
	fieldNamespace       = "namespace"
	fieldLabels          = "labels"
	fieldAnnotations     = "annotations"
	fieldOwnerReferences = "ownerReferences"
	fieldInterval        = "interval"
	fieldExpr            = "expr"
	fieldRules           = "rules"
	fieldGroups          = "groups"
	fieldRecord          = "record"
	fieldAlert           = "alert"
	fieldFor             = "for"
	fieldPort            = "port"
	fieldPath            = "path"
	fieldScheme          = "scheme"
	fieldScrapeTimeout   = "scrapeTimeout"
	fieldHonorLabels     = "honorLabels"
)

// Alert label and annotation keys, which Alertmanager and its templates expect
// by exact name.
const (
	labelSeverity     = "severity"
	labelSLO          = "slo"
	annotationSummary = "summary"
	annotationDesc    = "description"
	annotationRunbook = "runbook_url"
)

// Severity values, matching the convention Alertmanager routing trees are
// written against.
const (
	severityCritical = "critical"
	severityWarning  = "warning"
	severityInfo     = "info"
)

// alertNoTraffic is the alert that fires when a deployment stops receiving
// requests. Named because both the generator and its test refer to it.
const alertNoTraffic = "LLMCPNoTraffic"

// runbookURL is where every alert points for context.
const runbookURL = "https://github.com/surajm20061998/LLM_Inference_Control_Plane/blob/main/docs/slo.md"
