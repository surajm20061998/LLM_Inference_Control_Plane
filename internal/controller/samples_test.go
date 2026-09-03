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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/surajm20061998/LLM_Inference_Control_Plane/api/v1alpha1"
)

// The samples are documentation that runs. Every CEL rule in the CRD applies to
// them exactly as it does to a user's manifest, and a sample that no longer
// validates is worse than no sample: it is a copy-and-paste starting point that
// the API server rejects, at the moment someone is least equipped to debug it.
//
// This has already earned its keep once. A kubebuilder default on a field
// referenced by an "exactly one of" CEL rule makes that rule unsatisfiable —
// the field is always "present" to CEL, even when the user never wrote it — and
// the only symptom is a manifest that cannot be applied.
var _ = Describe("Shipped samples", func() {
	It("are accepted by the API server, CEL rules included", func() {
		dir := filepath.Join("..", "..", "config", "samples")
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())

		namespace := mdtNewNamespace()
		applied := 0

		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".yaml") || name == "kustomization.yaml" {
				continue
			}

			body, err := os.ReadFile(filepath.Join(dir, name))
			Expect(err).NotTo(HaveOccurred(), name)

			var md inferencev1alpha1.ModelDeployment
			Expect(yaml.Unmarshal(body, &md)).To(Succeed(), name)
			if md.Kind != "" && md.Kind != kindModelDeployment {
				continue
			}

			md.Namespace = namespace
			md.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, &md)).To(Succeed(),
				"sample %s was rejected by the API server", name)
			applied++
		}

		Expect(applied).To(BeNumerically(">=", 3),
			"expected the stub, real-engine and canary samples to be present")
	})
})
