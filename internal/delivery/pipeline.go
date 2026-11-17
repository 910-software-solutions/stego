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

// The conditions of the two jobs of the hosted CI file (ST-DLV-Q16 (A)): a pull request runs the check job, and only a
// push to the default branch or a tag runs the release job, which pushes and signs.
const (
	checkCondition   = "github.event_name == 'pull_request'"
	releaseCondition = "github.event_name == 'push' && (github.ref == format('refs/heads/{0}', github.event.repository.default_branch) || startsWith(github.ref, 'refs/tags/'))"
)

var (
	// signerText is one line of a keyless identity or a key reference, with no space, no control character and
	// no '$', '{' or '}', so that it holds no expression of the CI provider (ST-DLV-Q16 (A)).
	signerText = regexp.MustCompile(`^[\x21-\x23\x25-\x7a\x7c\x7e]{1,512}$`)
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

// konfluxPrefetch is the prefetch-input of the Konflux build: the Go modules of the go.mod and go.sum at the root of
// the project folder, which the prefetch puts where the Containerfile reads them (PrefetchedModules).
const konfluxPrefetch = `{"type": "gomod", "path": "."}`

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
	if !serviceLabel.MatchString(service) {
		return Pipeline{}, fmt.Errorf("the service name %q is not a DNS label", service)
	}
	if !IsPinned(pins.Pipeline) || !IsPinned(pins.Checks) {
		return Pipeline{}, fmt.Errorf("the ci-pipeline pins of %s are not each pinned by a full commit or a sha256 digest", provider)
	}
	if !signerText.MatchString(pins.Signer) {
		return Pipeline{}, fmt.Errorf("the trusted signer of %s is not one line with no expression", provider)
	}
	var object map[string]any
	switch provider {
	case GitHubActions:
		with := func(publish bool) map[string]any {
			return map[string]any{"service": service, "stego-checks": pins.Checks, "trusted-signer": pins.Signer, "publish": publish}
		}
		object = map[string]any{
			"name":        "stego",
			"on":          map[string]any{"push": map[string]any{}, "pull_request": map[string]any{}},
			"permissions": map[string]any{"contents": "read"},
			"jobs": map[string]any{
				// A pull request builds and tests with read permission only, and never pushes or signs.
				"check": map[string]any{"if": checkCondition, "uses": pins.Pipeline, "permissions": map[string]any{"contents": "read"}, "with": with(false)},
				// Only a push to the default branch or a tag pushes and signs (ST-DLV-Q16 (A)). The shared pipeline
				// builds in a job with read permission only and signs in a job of its own (ST-SUP-RC25-OC03).
				"release": map[string]any{"if": releaseCondition, "uses": pins.Pipeline,
					"permissions": map[string]any{"contents": "read", "id-token": "write", "packages": "write", "attestations": "write"}, "with": with(true)},
			},
		}
	case Konflux:
		object = map[string]any{
			"apiVersion": "tekton.dev/v1",
			"kind":       "PipelineRun",
			"metadata": map[string]any{"name": service + "-on-push", "labels": map[string]any{"app.kubernetes.io/name": service},
				"annotations": map[string]any{"pipelinesascode.tekton.dev/on-event": "[push]", "pipelinesascode.tekton.dev/on-target-branch": "[main]"}},
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
					// The build of the standard pipeline reads the offline Containerfile, prefetches the Go modules
					// of go.sum, and builds with the network off (ST-DLV-UC03-R04).
					map[string]any{"name": "dockerfile", "value": ContainerFile},
					map[string]any{"name": "hermetic", "value": "true"},
					map[string]any{"name": "prefetch-input", "value": konfluxPrefetch},
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
	Name        string               `yaml:"name"`
	On          map[string]any       `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Jobs        map[string]githubJob `yaml:"jobs"`
}

// githubJob is one job of the hosted CI file: a call of the shared pipeline and nothing more.
type githubJob struct {
	If          string            `yaml:"if"`
	Uses        string            `yaml:"uses"`
	Permissions map[string]string `yaml:"permissions"`
	With        struct {
		Service       string `yaml:"service"`
		StegoChecks   string `yaml:"stego-checks"`
		TrustedSigner string `yaml:"trusted-signer"`
		Publish       bool   `yaml:"publish"`
	} `yaml:"with"`
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
		check, release := workflow.Jobs["check"], workflow.Jobs["release"]
		switch {
		case len(workflow.Jobs) != 2 || check.Uses == "" || release.Uses == "":
			return Pipeline{}, fmt.Errorf("the workflow is not the check job and the release job")
		case check.If != checkCondition || release.If != releaseCondition:
			return Pipeline{}, fmt.Errorf("the jobs do not run on a pull request and on a push to the default branch or a tag")
		case len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" || len(check.Permissions) != 1 || check.Permissions["contents"] != "read" || check.With.Publish:
			return Pipeline{}, fmt.Errorf("a pull request run may only read the contents and never publish")
		case !release.With.Publish:
			return Pipeline{}, fmt.Errorf("the release job does not publish")
		}
		for _, job := range []githubJob{check, release} {
			if !IsPinned(job.Uses) || !IsPinned(job.With.StegoChecks) || job.Uses != release.Uses || job.With.StegoChecks != release.With.StegoChecks || !signerText.MatchString(job.With.TrustedSigner) {
				return Pipeline{}, fmt.Errorf("each job does not call the one pinned pipeline with the pinned STEGO checks and a trusted signer")
			}
		}
		return Pipeline{Provider: provider, Pins: PipelinePins{Pipeline: release.Uses, Checks: release.With.StegoChecks, Signer: release.With.TrustedSigner}, File: data}, nil
	case Konflux:
		var run pipelineRun
		if err := decodeStrict(data, MaxRequirementsBytes, &run); err != nil {
			return Pipeline{}, err
		}
		bundle, checks := paramValue(run.Spec.PipelineRef.Params, "bundle"), paramValue(run.Spec.Params, "stego-checks")
		if run.APIVersion != "tekton.dev/v1" || run.Kind != "PipelineRun" || run.Spec.PipelineRef.Resolver != "bundles" || !digestPin.MatchString(bundle) || !digestPin.MatchString(checks) ||
			!signerText.MatchString(paramValue(run.Spec.Params, "trusted-signer")) ||
			run.Metadata.Annotations["pipelinesascode.tekton.dev/on-event"] != "[push]" || run.Metadata.Annotations["pipelinesascode.tekton.dev/on-target-branch"] == "" {
			return Pipeline{}, fmt.Errorf("the PipelineRun does not only reference a bundle by digest with the STEGO checks by digest")
		}
		if paramValue(run.Spec.Params, "dockerfile") != ContainerFile || paramValue(run.Spec.Params, "hermetic") != "true" || paramValue(run.Spec.Params, "prefetch-input") != konfluxPrefetch {
			return Pipeline{}, fmt.Errorf("the PipelineRun does not build the offline %s, hermetic, from the prefetched Go modules", ContainerFile)
		}
		return Pipeline{Provider: provider, Pins: PipelinePins{Pipeline: bundle, Checks: checks, Signer: paramValue(run.Spec.Params, "trusted-signer")}, File: data}, nil
	}
	return Pipeline{}, fmt.Errorf("the CI provider %q is not %s or %s", provider, GitHubActions, Konflux)
}
