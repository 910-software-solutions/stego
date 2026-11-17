package registry

import (
	"fmt"
	"regexp"

	"github.com/jsell-rh/stego/internal/types"
)

// Port represents a named capability that a component requires or provides.
// It unmarshals from either a bare string or a mapping with a "name" key.
type Port struct {
	Name string `yaml:"name"`
}

// YAMLScalarForm marks Port as a parser.ScalarForm: the strict decoder accepts
// a port given by its bare name.
func (Port) YAMLScalarForm() {}

// UnmarshalYAML allows Port to be unmarshaled from a bare string (e.g. "- auth-provider")
// or from a mapping (e.g. "- name: auth-provider").
func (p *Port) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		p.Name = s
		return nil
	}
	type raw Port
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	*p = Port(r)
	return nil
}

// SlotDefinition represents a slot defined by a component.
type SlotDefinition struct {
	Name    string `yaml:"name"`
	Proto   string `yaml:"proto"`
	Default string `yaml:"default,omitempty"`
	// Phase is when the slot runs in a request flow, relative to the write to the store: before_store or
	// after_store (ST-SUP-EN08 field Phase).
	Phase string `yaml:"phase"`
	// Operations are the collection operations that call the slot (ST-SUP-EN08 field Operations, ST-SUP-Q134 (A)).
	Operations []string `yaml:"operations"`
}

// Archetype represents a curated component set with conventions.
type Archetype struct {
	Kind             string            `yaml:"kind"`
	Name             string            `yaml:"name"`
	Language         string            `yaml:"language"`
	Version          string            `yaml:"version"`
	Components       []string          `yaml:"components"`
	DefaultAuth      string            `yaml:"default_auth"`
	Conventions      types.Convention  `yaml:"conventions"`
	CompatibleMixins []string          `yaml:"compatible_mixins"`
	Bindings         map[string]string `yaml:"bindings,omitempty"`
	// Runtime is the one runtime library version that every service of the
	// archetype imports (ST-SUP-INV10).
	Runtime *RuntimePin `yaml:"runtime"`
	// Example is the archetype's example content (ST-SUP-EN03 field Example content): the files under its example
	// folder, which stego init writes into a new project. LoadSources reads it from the folder, not from
	// archetype.yaml. An archetype with no example folder has none.
	Example []ExampleFile `yaml:"-"`
}

// ExampleFile is one file of an archetype's example content: its path in the project, and its text with the
// placeholders ExampleService and ExampleModule.
type ExampleFile struct {
	Path string
	Text []byte
}

// The placeholders of example content: stego init puts the service name and the module path of the new project in
// their place.
const (
	ExampleService = "{{service}}"
	ExampleModule  = "{{module}}"
)

// ExampleDeclaration is the path in the project of the declaration text of example content.
const ExampleDeclaration = "service.yaml"

// Component represents a deterministic code generator with its metadata.
type Component struct {
	Kind            string                       `yaml:"kind"`
	Name            string                       `yaml:"name"`
	Version         string                       `yaml:"version"`
	Stage           types.Stage                  `yaml:"stage"`
	Sunset          string                       `yaml:"sunset,omitempty"`
	Config          map[string]types.ConfigField `yaml:"config,omitempty"`
	Requires        []Port                       `yaml:"requires"`
	Provides        []Port                       `yaml:"provides"`
	Slots           []SlotDefinition             `yaml:"slots"`
	OutputNamespace string                       `yaml:"output_namespace,omitempty"`
	// Conversions are the conversions of a retype that a database component carries out, each with the field types
	// it converts and where it is total (ST-DECL-INV62, ST-DECL-INV63).
	Conversions []types.Conversion `yaml:"conversions,omitempty"`
	// Needs are what the component needs the environment to supply, from which apply writes the platform
	// requirements of a service (ST-SUP-EN09 field Platform needs, ST-DLV-Q04 (A)).
	Needs []Need `yaml:"needs,omitempty"`
	// Pipelines are the pinned pipeline references and the trusted signer of each CI provider, which only the
	// ci-pipeline component states (ST-SUP-EN09 field Pipeline pins, ST-DLV-Q13 (A)).
	Pipelines map[string]PipelinePins `yaml:"pipelines,omitempty"`
	// Architecture is the component's architecture role, from which the architecture model places it alone
	// (ST-SUP-EN09 field Architecture role, ST-SUP-UC01-R05).
	Architecture *ArchitectureRole `yaml:"architecture"`
	// Environment is every environment variable that the component's emitted code or its runtime provider reads
	// (ST-SUP-EN09, ST-SUP-EN14, ST-SUP-UC01-R08). It is a pointer so that the registry refuses a component that
	// states no list.
	Environment *[]EnvironmentVariable `yaml:"environment"`
}

// ArchitectureRole is where a component sits in a service: its layer, its flow part, and the side of each port it
// provides or requires (ST-SUP-EN09 field Architecture role, ST-SUP-Q130 (A)).
type ArchitectureRole struct {
	// Layer is one of Layers.
	Layer string `yaml:"layer"`
	// Flow is the component's part in a request flow: one plain sentence of at most MaxFlowPart characters.
	Flow string `yaml:"flow"`
	// Ports maps each port the component provides or requires to its side, inbound or outbound.
	Ports map[string]string `yaml:"ports"`
	// Serves are the kinds of operation whose request flows start at the component, of ServedKinds (ST-GEN-Q44 (A)).
	Serves []string `yaml:"serves,omitempty"`
	// Store is the name of the store that keeps the rows of a component of the repository layer, such as
	// PostgreSQL, for a service with no component of the store layer (ST-GEN-Q44 (A)).
	Store string `yaml:"store,omitempty"`
}

// PipelinePins are the pinned references and the one trusted signer of one CI provider: the shared pipeline, the
// STEGO checks and the signer of the pipeline's images (ST-DLV-INV03, ST-DLV-INV04).
type PipelinePins struct {
	Pipeline string `yaml:"pipeline"`
	Checks   string `yaml:"checks"`
	Signer   string `yaml:"signer"`
}

// PipelineComponent is the one component that may state pipeline pins.
const PipelineComponent = "ci-pipeline"

// The CI providers of ST-VO51.
const (
	GitHubActions = "github-actions"
	Konflux       = "konflux"
)

var (
	// workflowPin is a reusable workflow at a full commit: owner/repository/.github/workflows/<file>@<40 hex>.
	workflowPin = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml@[0-9a-f]{40}$`)
	// actionPin is an action or a repository at a full commit: owner/repository[/path]@<40 hex>.
	actionPin = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*@[0-9a-f]{40}$`)
	// bundlePin is an OCI bundle at a sha256 digest.
	bundlePin = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$`)
	// signerText is one line of a keyless identity or a key reference, with no control character.
	// It holds no '$', '{' or '}', so no expression of the CI provider (ST-DLV-Q16 (A)).
	signerText = regexp.MustCompile(`^[\x21-\x23\x25-\x7a\x7c\x7e]{1,512}$`)
)

// checkPipelines refuses pipeline pins on a component other than ci-pipeline, a provider that is not one of
// ST-VO51, a reference that is not pinned (ST-DLV-SP02) and a signer that is not one line.
func checkPipelines(component *Component) error {
	if len(component.Pipelines) == 0 {
		return nil
	}
	if component.Name != PipelineComponent {
		return fmt.Errorf("component %s: only the %s component may state pipelines", component.Name, PipelineComponent)
	}
	for provider, pins := range component.Pipelines {
		var pipeline, checks *regexp.Regexp
		switch provider {
		case GitHubActions:
			pipeline, checks = workflowPin, actionPin
		case Konflux:
			pipeline, checks = bundlePin, bundlePin
		default:
			return fmt.Errorf("component %s: pipelines.%s: the provider is not %s or %s", component.Name, provider, GitHubActions, Konflux)
		}
		if !pipeline.MatchString(pins.Pipeline) {
			return fmt.Errorf("component %s: pipelines.%s.pipeline: the reference %q is not pinned by a full commit or a sha256 digest", component.Name, provider, pins.Pipeline)
		}
		if !checks.MatchString(pins.Checks) {
			return fmt.Errorf("component %s: pipelines.%s.checks: the reference %q is not pinned by a full commit or a sha256 digest", component.Name, provider, pins.Checks)
		}
		if !signerText.MatchString(pins.Signer) {
			return fmt.Errorf("component %s: pipelines.%s.signer: the trusted signer is not one line of at most 512 characters", component.Name, provider)
		}
	}
	return nil
}

// Need is one thing a component needs the environment to supply: its kind of ST-VO53, its name, and only the
// shape fields of its kind. The name is fixed text with at most the placeholders {service} and {settings.<key>},
// which apply fills from the declaration. No field holds a value of an environment, so a host name, an address, a
// namespace or a secret value has no place to go (ST-DLV-INV02).
type Need struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
	// Keys are the keys of a secret.
	Keys []string `yaml:"keys,omitempty"`
	// Engine is the engine of a backing store, such as postgres.
	Engine string `yaml:"engine,omitempty"`
	// Port is the port of an ingress or an egress destination.
	Port int `yaml:"port,omitempty"`
	// APIGroup, Resource and Verbs are a cluster permission; the core API group is "".
	APIGroup string   `yaml:"api_group,omitempty"`
	Resource string   `yaml:"resource,omitempty"`
	Verbs    []string `yaml:"verbs,omitempty"`
	// ResourceNames are the names of the objects a cluster permission is limited to; get on secrets must give them
	// (ST-DLV-Q14).
	ResourceNames []string `yaml:"resource_names,omitempty"`
	// Level is the pod security level: privileged, baseline or restricted.
	Level string `yaml:"level,omitempty"`
	// Variable is the environment variable of the component whose value is the value of a topic or an identity
	// issuer need, and Value is one {settings.<key>} that gives the issuer that the declaration fixes (ST-SUP-EN09
	// field Platform needs, ST-DLV-Q26 (A)).
	Variable string `yaml:"variable,omitempty"`
	Value    string `yaml:"value,omitempty"`
}

// Mixin adds components and slots to an archetype. A mixin with AliasOf is a deprecated alias: binding it binds
// the mixins it names instead, and it adds nothing of its own (ST-SUP-INV17). RequiresMixins are the mixins that
// must be bound with it (ST-SUP-INV16).
type Mixin struct {
	Kind           string   `yaml:"kind"`
	Name           string   `yaml:"name"`
	Version        string   `yaml:"version"`
	AliasOf        []string `yaml:"alias_of,omitempty"`
	RequiresMixins []string `yaml:"requires_mixins,omitempty"`
	// DeprecatedOn and Sunset are the dates, as YYYY-MM-DD, of the release that first ships a deprecated alias and of
	// its removal: one ga window of the registry policy after DeprecatedOn (ST-SUP-INV17, ST-SUP-Q117 (b)).
	DeprecatedOn   string           `yaml:"deprecated_on,omitempty"`
	Sunset         string           `yaml:"sunset,omitempty"`
	AddsComponents []string         `yaml:"adds_components"`
	AddsSlots      []SlotDefinition `yaml:"adds_slots"`
	Overrides      string           `yaml:"overrides"` // "none"
}

// Policy is the registry policy file policy.yaml: the months of the window of each stage, ga among them, that a
// deprecated alias stays before its sunset (ST-SUP-INV17).
type Policy struct {
	Kind    string         `yaml:"kind"`
	Windows map[string]int `yaml:"windows"`
}

// RegistrySource describes a single registry origin in .stego/config.yaml.
type RegistrySource struct {
	URL    string `yaml:"url"`
	Ref    string `yaml:"ref"`
	Path   string `yaml:"path,omitempty"`
	Vendor string `yaml:"vendor,omitempty"`
}

// RegistryConfig represents the .stego/config.yaml file.
type RegistryConfig struct {
	Registry []RegistrySource  `yaml:"registry"`
	Pins     map[string]string `yaml:"pins,omitempty"`
}
