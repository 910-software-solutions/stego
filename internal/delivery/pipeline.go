package delivery

import (
	"fmt"
	"regexp"
)

// The CI files of the providers, in the project folder (ST-DLV-EN03-TB).
const (
	GitHubActionsFile = ".github/workflows/stego.yml"
	konfluxFolder     = ".tekton/"
)

// The CI providers of ST-VO51.
const (
	GitHubActions = "github-actions"
	Konflux       = "konflux"
)

// PipelinePins are the pinned references and the trusted signer of one provider, as the registry's ci-pipeline
// component states them (ST-DLV-INV04).
type PipelinePins struct {
	Pipeline string
	Checks   string
	Signer   string
}

// Pipeline is the delivery pipeline of one service (ST-DLV-EN03): the provider, the pinned pipeline, the pinned
// STEGO checks, the trusted signer, and the CI file at its path.
type Pipeline struct {
	Provider string
	Pins     PipelinePins
	Path     string
	File     []byte
}

// pipelineHeader is the first lines of the CI file.
const pipelineHeader = "# The CI file of the service. stego apply writes this file from the delivery stanza of the declaration and the\n" +
	"# pins of the registry's ci-pipeline component. It holds one pinned call and nothing more. Do not edit it.\n"

var (
	// commitPin is a reference pinned by a full 40-character commit (ST-DLV-SP02).
	commitPin = regexp.MustCompile(`@[0-9a-f]{40}$`)
	// digestPin is a reference pinned by a sha256 OCI digest (ST-DLV-SP02).
	digestPin = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// PipelineFile gives the path of the CI file of the provider for the service.
func PipelineFile(provider, service string) string {
	if provider == Konflux {
		return konfluxFolder + service + "-push.yaml"
	}
	return GitHubActionsFile
}

// IsPinned tells if a pipeline reference names a full 40-character commit or a sha256 digest (ST-DLV-SP02).
func IsPinned(reference string) bool {
	return commitPin.MatchString(reference) || digestPin.MatchString(reference)
}

// WritePipeline writes the CI file of the service for the provider that its delivery stanza names, with the pins
// of the registry (ST-DLV-CMD02): on the hosted CI provider one job that calls the platform's shared pipeline at
// its commit and passes the STEGO checks and the trusted signer; on Konflux one PipelineRun that references the
// standard pipeline bundle by digest and passes the STEGO checks bundle and the signer. It holds no step, task or
// script of its own (ST-DLV-INV03), and it refuses a reference that is not pinned.
func WritePipeline(service, provider string, pins PipelinePins) (Pipeline, error) {
	if !IsPinned(pins.Pipeline) || !IsPinned(pins.Checks) {
		return Pipeline{}, fmt.Errorf("the ci-pipeline pins of %s are not each pinned by a full commit or a sha256 digest", provider)
	}
	var object map[string]any
	switch provider {
	case GitHubActions:
		object = map[string]any{
			"name":        "stego",
			"on":          map[string]any{"push": map[string]any{}, "pull_request": map[string]any{}},
			"permissions": map[string]any{"contents": "read"},
			"jobs": map[string]any{"stego": map[string]any{
				"uses": pins.Pipeline,
				// The shared pipeline builds in a job with read permission only and signs in a job of its own
				// (ST-SUP-RC25-OC03); the call grants what its signing job needs and nothing more.
				"permissions": map[string]any{"contents": "read", "id-token": "write", "packages": "write", "attestations": "write"},
				"with":        map[string]any{"service": service, "stego-checks": pins.Checks, "trusted-signer": pins.Signer},
			}},
		}
	case Konflux:
		object = map[string]any{
			"apiVersion": "tekton.dev/v1",
			"kind":       "PipelineRun",
			"metadata": map[string]any{"name": service + "-on-push", "labels": map[string]any{"app.kubernetes.io/name": service},
				"annotations": map[string]any{"pipelinesascode.tekton.dev/on-event": "[push]"}},
			"spec": map[string]any{
				"pipelineRef": map[string]any{"resolver": "bundles", "params": []any{
					map[string]any{"name": "bundle", "value": pins.Pipeline},
					map[string]any{"name": "name", "value": "docker-build"},
					map[string]any{"name": "kind", "value": "pipeline"},
				}},
				"params": []any{
					map[string]any{"name": "service", "value": service},
					map[string]any{"name": "stego-checks", "value": pins.Checks},
					map[string]any{"name": "trusted-signer", "value": pins.Signer},
				},
			},
		}
	default:
		return Pipeline{}, fmt.Errorf("the CI provider %q is not %s or %s", provider, GitHubActions, Konflux)
	}
	data, err := encodeWith(pipelineHeader, object)
	if err != nil {
		return Pipeline{}, err
	}
	pipeline, err := LoadPipeline(provider, data)
	if err != nil {
		return Pipeline{}, fmt.Errorf("the CI file of %s does not read back: %w", service, err)
	}
	pipeline.Path = PipelineFile(provider, service)
	return pipeline, nil
}

// githubWorkflow is the shape of the CI file that STEGO writes for the hosted CI provider, and nothing more: a
// step, a runs-on or a second job is not in it, so the strict decoder refuses it.
type githubWorkflow struct {
	Name        string            `yaml:"name"`
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Uses        string            `yaml:"uses"`
		Permissions map[string]string `yaml:"permissions"`
		With        struct {
			Service       string `yaml:"service"`
			StegoChecks   string `yaml:"stego-checks"`
			TrustedSigner string `yaml:"trusted-signer"`
		} `yaml:"with"`
	} `yaml:"jobs"`
}

// tektonParam is one parameter of a PipelineRun or of its bundle resolver.
type tektonParam struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// pipelineRun is the shape of the PipelineRun that STEGO writes for Konflux, and nothing more: an inline pipeline,
// task or script is not in it, so the strict decoder refuses it.
type pipelineRun struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		PipelineRef struct {
			Resolver string        `yaml:"resolver"`
			Params   []tektonParam `yaml:"params"`
		} `yaml:"pipelineRef"`
		Params []tektonParam `yaml:"params"`
	} `yaml:"spec"`
}

// paramValue gives the value of the named parameter.
func paramValue(params []tektonParam, name string) string {
	for _, param := range params {
		if param.Name == name {
			return param.Value
		}
	}
	return ""
}

// LoadPipeline reads a CI file back through the strict decoder (ST-DLV-REPO02), in the shape that STEGO writes for
// the provider: one job that only calls the pinned shared pipeline, or one PipelineRun that only references the
// pinned bundle, each passing the pinned STEGO checks (ST-DLV-INV03).
func LoadPipeline(provider string, data []byte) (Pipeline, error) {
	switch provider {
	case GitHubActions:
		var workflow githubWorkflow
		if err := decodeStrict(data, MaxRequirementsBytes, &workflow); err != nil {
			return Pipeline{}, err
		}
		job, ok := workflow.Jobs["stego"]
		if len(workflow.Jobs) != 1 || !ok || !IsPinned(job.Uses) || !IsPinned(job.With.StegoChecks) {
			return Pipeline{}, fmt.Errorf("the workflow is not one job that calls a pinned pipeline with the pinned STEGO checks")
		}
		return Pipeline{Provider: provider, Pins: PipelinePins{Pipeline: job.Uses, Checks: job.With.StegoChecks, Signer: job.With.TrustedSigner}, File: data}, nil
	case Konflux:
		var run pipelineRun
		if err := decodeStrict(data, MaxRequirementsBytes, &run); err != nil {
			return Pipeline{}, err
		}
		bundle, checks := paramValue(run.Spec.PipelineRef.Params, "bundle"), paramValue(run.Spec.Params, "stego-checks")
		if run.APIVersion != "tekton.dev/v1" || run.Kind != "PipelineRun" || run.Spec.PipelineRef.Resolver != "bundles" || !digestPin.MatchString(bundle) || !digestPin.MatchString(checks) {
			return Pipeline{}, fmt.Errorf("the PipelineRun does not only reference a bundle by digest with the STEGO checks by digest")
		}
		return Pipeline{Provider: provider, Pins: PipelinePins{Pipeline: bundle, Checks: checks, Signer: paramValue(run.Spec.Params, "trusted-signer")}, File: data}, nil
	}
	return Pipeline{}, fmt.Errorf("the CI provider %q is not %s or %s", provider, GitHubActions, Konflux)
}
