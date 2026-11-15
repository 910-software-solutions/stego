package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/delivery"
)

// These tests prove that apply writes the CI file of a service for the provider that its delivery stanza names
// (ST-DLV-UC06), on the service of ST-DLV-TD01. The shipped registry states no pins yet (ST-DLV-Q13 (C)), so the
// fixture registry adds the ci-pipeline component with test pins.

// The test pins of the fixture registry of ST-DLV-TD01.
const (
	fixturePipeline      = "platform/pipelines/.github/workflows/stego.yml@0123456789abcdef0123456789abcdef01234567"
	fixtureChecks        = "platform/stego-checks@89abcdef0123456789abcdef0123456789abcdef"
	fixtureSigner        = "https://github.com/platform/pipelines/.github/workflows/stego.yml@refs/heads/main"
	fixtureBundle        = "quay.io/konflux-ci/pipelines/docker-build@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixtureChecksBundle  = "quay.io/stego/checks@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	fixtureChainsKeyName = "k8s://openshift-pipelines/signing-secrets"
)

// pipelineRegistry gives the registry copy of ST-DLV-TD01 with the ci-pipeline component and its test pins.
func pipelineRegistry(t *testing.T) string {
	t.Helper()
	registry := requirementsRegistry(t)
	dir := filepath.Join(registry, "components", "ci-pipeline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	component := "kind: component\nname: ci-pipeline\nversion: 0.1.0\nstage: alpha\nsunset: 2027-06-30\nconfig: {}\nrequires: []\nprovides: []\nslots: []\nenvironment: []\narchitecture: {layer: outside call, flow: \"Runs the CI of the service, out of the process.\", ports: {}}\n" +
		"pipelines:\n  github-actions: {pipeline: " + fixturePipeline + ", checks: " + fixtureChecks + ", signer: \"" + fixtureSigner + "\"}\n" +
		"  konflux: {pipeline: " + fixtureBundle + ", checks: " + fixtureChecksBundle + ", signer: \"" + fixtureChainsKeyName + "\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "component.yaml"), []byte(component), 0o644); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestPipelineApply(t *testing.T) {
	registry := pipelineRegistry(t)
	env := requirementsEnv(registry)
	read := func(t *testing.T, dir, file string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("apply writes no %s: %v", file, err)
		}
		var object map[string]any
		if err := yaml.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		return object
	}

	t.Run("ST-DLV-UC06-R01-T01 For the hosted CI provider, applying the plan writes a CI file that only calls the shared pipeline by full commit and the STEGO checks by pin", func(t *testing.T) {
		dir := deliveryProject(t, "delivery:\n  ci: github-actions\n")
		printed := mustRunInProject(t, dir, env, "apply")
		if !strings.Contains(printed, "generate: "+delivery.GitHubActionsFile) {
			t.Fatalf("the plan does not list %s:\n%s", delivery.GitHubActionsFile, printed)
		}
		workflow := read(t, dir, delivery.GitHubActionsFile)
		jobs, _ := workflow["jobs"].(map[string]any)
		if len(jobs) != 1 {
			t.Fatalf("the CI file has the jobs %v, want one", jobs)
		}
		for _, job := range jobs {
			for key := range job.(map[string]any) {
				if key != "uses" && key != "with" && key != "permissions" {
					t.Errorf("the job has the field %s; it only calls the shared pipeline", key)
				}
			}
			if field(job, "uses") != fixturePipeline || field(job, "with", "stego-checks") != fixtureChecks || field(job, "with", "trusted-signer") != fixtureSigner {
				t.Errorf("the job is %v, want a call of the pinned pipeline with the pinned checks and the signer of the registry", job)
			}
		}
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if state.LastApplied.ProjectFiles[delivery.GitHubActionsFile] == "" {
			t.Errorf("the generation record does not list %s", delivery.GitHubActionsFile)
		}
	})

	t.Run("ST-DLV-UC06-R01-T02 For Konflux, applying the plan writes a PipelineRun that only references the standard pipeline bundle and the STEGO checks by digest", func(t *testing.T) {
		dir := deliveryProject(t, "delivery:\n  ci: konflux\n")
		mustRunInProject(t, dir, env, "apply")
		file := delivery.PipelineFile(delivery.Konflux, "orders")
		run := read(t, dir, file)
		// The contract fake of Tekton: the PipelineRun of tekton.dev/v1 resolves its pipeline through the bundles
		// resolver alone, with no inline pipeline, task or script.
		if run["apiVersion"] != "tekton.dev/v1" || run["kind"] != "PipelineRun" {
			t.Fatalf("the file is %v %v, want a tekton.dev/v1 PipelineRun", run["apiVersion"], run["kind"])
		}
		spec, _ := run["spec"].(map[string]any)
		for key := range spec {
			if key != "pipelineRef" && key != "params" {
				t.Errorf("the PipelineRun has spec.%s; it only references the bundle", key)
			}
		}
		if field(spec, "pipelineRef", "resolver") != "bundles" {
			t.Errorf("the pipeline is not resolved from a bundle: %v", field(spec, "pipelineRef"))
		}
		values := map[string]any{}
		for _, params := range []any{field(spec, "pipelineRef", "params"), field(spec, "params")} {
			list, _ := params.([]any)
			for _, param := range list {
				values[field(param, "name").(string)] = field(param, "value")
			}
		}
		if values["bundle"] != fixtureBundle || values["stego-checks"] != fixtureChecksBundle || values["trusted-signer"] != fixtureChainsKeyName {
			t.Errorf("the parameters are %v, want the bundle, the checks bundle and the Chains key of the registry", values)
		}
	})

	t.Run("ST-DLV-UC06-R01-T03 With no pins for the provider in the registry, applying the plan writes no CI file and says so", func(t *testing.T) {
		shipped := requirementsRegistry(t)
		dir := deliveryProject(t, "delivery:\n  ci: github-actions\n")
		printed := mustRunInProject(t, dir, requirementsEnv(shipped), "apply")
		if !strings.Contains(printed, "no CI file is written") {
			t.Errorf("the plan does not say that no CI file is written:\n%s", printed)
		}
		if _, err := os.Stat(filepath.Join(dir, delivery.GitHubActionsFile)); err == nil {
			t.Error("apply writes a CI file with no pins in the registry")
		}
	})
}
