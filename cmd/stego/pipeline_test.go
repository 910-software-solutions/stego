package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/delivery"
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

// pipelineAcceptance accepts the ci-pipeline component of pipelineRegistry, which is alpha, until its sunset: a
// declaration that names a CI provider reads its pins, so it accepts it by name (ST-DECL-EN07, sec-501 L1).
const pipelineAcceptance = "experimental_acceptances:\n  - {component: ci-pipeline, accepted_until: 2027-06-30, reason: the CI pipeline pins}\n"

// pipelineRegistry gives the registry copy of ST-DLV-TD01 with the ci-pipeline component and its test pins.
func pipelineRegistry(t *testing.T) string {
	t.Helper()
	registry := requirementsRegistry(t)
	dir := filepath.Join(registry, "components", "ci-pipeline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	component := "kind: component\nname: ci-pipeline\nversion: 0.1.0\nstage: alpha\nsunset: 2027-06-30\nconfig: {}\nrequires: []\nprovides: []\nslots: []\narchitecture: {layer: outside call, flow: \"Runs the CI of the service, out of the process.\", ports: {}}\n" +
		"pipelines:\n  github-actions: {pipeline: " + fixturePipeline + ", checks: " + fixtureChecks + ", signer: \"" + fixtureSigner + "\"}\n" +
		"  konflux: {pipeline: " + fixtureBundle + ", checks: " + fixtureChecksBundle + ", signer: \"" + fixtureChainsKeyName + "\"}\n" +
		"environment: []\n"
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
		dir := deliveryProject(t, "delivery:\n  ci: github-actions\n"+pipelineAcceptance)
		printed := mustRunInProject(t, dir, env, "apply")
		if !strings.Contains(printed, "generate: "+delivery.GitHubActionsFile) {
			t.Fatalf("the plan does not list %s:\n%s", delivery.GitHubActionsFile, printed)
		}
		workflow := read(t, dir, delivery.GitHubActionsFile)
		jobs, _ := workflow["jobs"].(map[string]any)
		if len(jobs) != 2 || jobs["check"] == nil || jobs["release"] == nil {
			t.Fatalf("the CI file has the jobs %v, want the check job and the release job", jobs)
		}
		for _, job := range jobs {
			for key := range job.(map[string]any) {
				if key != "uses" && key != "with" && key != "permissions" && key != "if" {
					t.Errorf("the job has the field %s; it only calls the shared pipeline", key)
				}
			}
			if field(job, "uses") != fixturePipeline || field(job, "with", "stego-checks") != fixtureChecks || field(job, "with", "trusted-signer") != fixtureSigner {
				t.Errorf("the job is %v, want a call of the pinned pipeline with the pinned checks and the signer of the registry", job)
			}
		}
		// A signer that holds an expression of the provider, and a service name that is not a DNS label, are refused.
		pins := delivery.PipelinePins{Pipeline: fixturePipeline, Checks: fixtureChecks, Signer: fixtureSigner}
		for _, signer := range []string{"${{github.token}}", "$(params.x)", ""} {
			bad := pins
			bad.Signer = signer
			if _, err := delivery.WritePipeline("orders", delivery.GitHubActions, bad); err == nil {
				t.Errorf("WritePipeline accepts the signer %q", signer)
			}
		}
		konflux := delivery.PipelinePins{Pipeline: fixtureBundle, Checks: fixtureChecksBundle, Signer: fixtureChainsKeyName}
		if _, err := delivery.WritePipeline("orders", delivery.Konflux, konflux); err != nil {
			t.Fatalf("WritePipeline refuses the Konflux pins of the fixture: %v", err)
		}
		if _, err := delivery.WritePipeline("-orders", delivery.Konflux, konflux); err == nil {
			t.Error("WritePipeline accepts the service name -orders")
		}
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if state.LastApplied.ProjectFiles[delivery.GitHubActionsFile] == "" {
			t.Errorf("the generation record does not list %s", delivery.GitHubActionsFile)
		}
	})

	t.Run("ST-DLV-UC06-R02-T01 A pull request run of the CI file has no write permission, and a push on a feature branch signs nothing", func(t *testing.T) {
		dir := deliveryProject(t, "delivery:\n  ci: github-actions\n"+pipelineAcceptance)
		mustRunInProject(t, dir, env, "apply")
		workflow := read(t, dir, delivery.GitHubActionsFile)
		jobs, _ := workflow["jobs"].(map[string]any)
		// The contract fake of GitHub Actions: for each event it evaluates the if condition of each job, as the
		// provider does, and gives the jobs that run with the permissions each gets. A job has no permission
		// that its permissions block does not give.
		for _, run := range []struct {
			name, event, ref string
			signs            bool
		}{
			{"a pull request", "pull_request", "refs/pull/7/merge", false},
			{"a push on a feature branch", "push", "refs/heads/feature", false},
			{"a push on the default branch", "push", "refs/heads/main", true},
			{"a push of a tag", "push", "refs/tags/v1.0.0", true},
		} {
			context := map[string]string{"github.event_name": run.event, "github.ref": run.ref, "github.event.repository.default_branch": "main"}
			signs := false
			for name, job := range jobs {
				condition, _ := field(job, "if").(string)
				runs, err := evaluateCondition(condition, context)
				if err != nil {
					t.Fatalf("job %s: %v", name, err)
				}
				if !runs {
					continue
				}
				permissions, _ := field(job, "permissions").(map[string]any)
				for permission, level := range permissions {
					if level == "write" {
						if run.event == "pull_request" {
							t.Errorf("%s runs the job %s with %s write", run.name, name, permission)
						}
						if permission == "id-token" {
							signs = true
						}
					}
				}
			}
			if signs != run.signs {
				t.Errorf("%s: the run can sign is %v, want %v", run.name, signs, run.signs)
			}
		}
	})

	t.Run("ST-DLV-UC06-R01-T02 For Konflux, applying the plan writes a PipelineRun that only references the standard pipeline bundle and the STEGO checks by digest", func(t *testing.T) {
		dir := deliveryProject(t, "delivery:\n  ci: konflux\n"+pipelineAcceptance)
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
		// The build reads the offline Containerfile, with the network off, from the prefetched Go modules
		// (ST-DLV-UC03-R04).
		if values["dockerfile"] != delivery.ContainerFile || values["hermetic"] != "true" || values["prefetch-input"] != `{"type": "gomod", "path": "."}` {
			t.Errorf("the parameters are %v, want dockerfile %s, hermetic true and the gomod prefetch of the project folder", values, delivery.ContainerFile)
		}
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := delivery.LoadPipeline(delivery.Konflux, data); err != nil {
			t.Fatalf("the written PipelineRun does not read back: %v", err)
		}
		for _, edit := range [][2]string{
			{`value: "true"`, `value: "false"`},
			{"value: Containerfile", "value: Dockerfile"},
			{`{"type": "gomod", "path": "."}`, `{"type": "gomod", "path": "other"}`},
			{"value: docker-build", "value: other-build"},
			// A second hermetic parameter after the first, which a reader that takes the last value would read.
			{"    - name: hermetic\n      value: \"true\"\n", "    - name: hermetic\n      value: \"true\"\n    - name: hermetic\n      value: \"false\"\n"},
		} {
			if !bytes.Contains(data, []byte(edit[0])) {
				t.Fatalf("the PipelineRun holds no %s:\n%s", edit[0], data)
			}
			_, err := delivery.LoadPipeline(delivery.Konflux, bytes.Replace(data, []byte(edit[0]), []byte(edit[1]), 1))
			if err == nil {
				t.Errorf("a PipelineRun with %s instead of %s reads back", edit[1], edit[0])
			} else if strings.Contains(err.Error(), "yaml:") {
				t.Errorf("the edit %q is not valid YAML, so it proves no check: %v", edit[1], err)
			}
		}
	})

	t.Run("ST-DLV-UC06-R01-T03 With no pins for the provider in the registry, applying the plan writes no CI file and says so", func(t *testing.T) {
		shipped := requirementsRegistry(t)
		dir := deliveryProject(t, "delivery:\n  ci: github-actions\n"+pipelineAcceptance)
		printed := mustRunInProject(t, dir, requirementsEnv(shipped), "apply")
		if !strings.Contains(printed, "no CI file is written") {
			t.Errorf("the plan does not say that no CI file is written:\n%s", printed)
		}
		if _, err := os.Stat(filepath.Join(dir, delivery.GitHubActionsFile)); err == nil {
			t.Error("apply writes a CI file with no pins in the registry")
		}
	})
}

// evaluateCondition evaluates the if condition of a job of GitHub Actions in the context, for the expressions that
// a CI file uses: a context value, a quoted text, format('…{0}', value), startsWith(value, text), ==, &&, || and
// parentheses. It refuses any other expression, so a condition it cannot read fails the test.
func evaluateCondition(condition string, context map[string]string) (bool, error) {
	tokens := regexp.MustCompile(`'[^']*'|\(|\)|,|==|&&|\|\||[A-Za-z_][A-Za-z0-9_.-]*`).FindAllString(condition, -1)
	if strings.Join(tokens, "") != strings.ReplaceAll(condition, " ", "") {
		return false, fmt.Errorf("the condition %q holds an expression the fake does not read", condition)
	}
	position := 0
	next := func() string {
		if position < len(tokens) {
			position++
			return tokens[position-1]
		}
		return ""
	}
	peek := func() string {
		if position < len(tokens) {
			return tokens[position]
		}
		return ""
	}
	var value func() (any, error)
	var or func() (any, error)
	value = func() (any, error) {
		token := next()
		switch {
		case token == "(":
			inner, err := or()
			if next() != ")" {
				return nil, fmt.Errorf("a parenthesis is not closed")
			}
			return inner, err
		case strings.HasPrefix(token, "'"):
			return strings.Trim(token, "'"), nil
		case token == "format" || token == "startsWith":
			if next() != "(" {
				return nil, fmt.Errorf("%s is not called", token)
			}
			first, err := value()
			if err != nil || next() != "," {
				return nil, fmt.Errorf("%s needs two arguments", token)
			}
			second, err := value()
			if err != nil || next() != ")" {
				return nil, fmt.Errorf("%s needs two arguments", token)
			}
			if token == "format" {
				return strings.ReplaceAll(first.(string), "{0}", second.(string)), nil
			}
			return strings.HasPrefix(first.(string), second.(string)), nil
		}
		known, ok := context[token]
		if !ok {
			return nil, fmt.Errorf("the context has no value %s", token)
		}
		return known, nil
	}
	equal := func() (any, error) {
		left, err := value()
		if err != nil || peek() != "==" {
			return left, err
		}
		next()
		right, err := value()
		return left == right, err
	}
	and := func() (any, error) {
		left, err := equal()
		for err == nil && peek() == "&&" {
			next()
			var right any
			right, err = equal()
			left = left == true && right == true
		}
		return left, err
	}
	or = func() (any, error) {
		left, err := and()
		for err == nil && peek() == "||" {
			next()
			var right any
			right, err = and()
			left = left == true || right == true
		}
		return left, err
	}
	result, err := or()
	if err != nil {
		return false, err
	}
	if position != len(tokens) {
		return false, fmt.Errorf("the condition %q has more after its end", condition)
	}
	runs, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("the condition %q is not true or false", condition)
	}
	return runs, nil
}
