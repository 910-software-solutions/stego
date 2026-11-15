// Package delivery is Delivering: what a service needs from its platform and how it reaches it.
package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RequirementKind is the sort of thing a service needs its environment to supply (ST-VO53).
type RequirementKind string

// The nine requirement kinds, in the order of the platform requirements.
const (
	Secret            RequirementKind = "secret"
	Configuration     RequirementKind = "configuration"
	BackingStore      RequirementKind = "backing store"
	Topic             RequirementKind = "topic"
	IdentityIssuer    RequirementKind = "identity issuer"
	Ingress           RequirementKind = "ingress"
	Egress            RequirementKind = "egress"
	ClusterPermission RequirementKind = "cluster permission"
	PodSecurity       RequirementKind = "pod security"
)

// RequirementKinds are the nine kinds, in the order of the platform requirements.
var RequirementKinds = []RequirementKind{Secret, Configuration, BackingStore, Topic, IdentityIssuer, Ingress, Egress, ClusterPermission, PodSecurity}

// ParseRequirementKind accepts a requirement kind only when it is one of the nine known kinds (ST-VO53-C01).
func ParseRequirementKind(text string) (RequirementKind, error) {
	kind := RequirementKind(text)
	if !slices.Contains(RequirementKinds, kind) {
		names := make([]string, len(RequirementKinds))
		for i, known := range RequirementKinds {
			names[i] = string(known)
		}
		return "", fmt.Errorf("the requirement kind %q is not one of %s", text, strings.Join(names, ", "))
	}
	return kind, nil
}

// UnmarshalYAML reads a requirement kind and refuses one that is not known.
func (k *RequirementKind) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("line %d: a requirement kind is text", node.Line)
	}
	kind, err := ParseRequirementKind(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	*k = kind
	return nil
}

// Need is one need that a component states, as the registry holds it: its name may hold the placeholders
// {service} and {settings.<key>}.
type Need struct {
	Kind     string
	Name     string
	Keys     []string
	Engine   string
	Port     int
	APIGroup string
	Resource string
	Verbs    []string
	Level    string
}

// ComponentNeeds is one component of the service with the needs it states and its settings as the declaration and
// the registry give them.
type ComponentNeeds struct {
	Component string
	Needs     []Need
	Settings  map[string]string
}

// Requirement is one thing the environment must supply: its kind, its name, the shape of its kind and the
// components that need it (ST-DLV-EN02). It holds no value of an environment (ST-DLV-INV02).
type Requirement struct {
	Kind     RequirementKind `yaml:"kind"`
	Name     string          `yaml:"name"`
	Keys     []string        `yaml:"keys,omitempty"`
	Engine   string          `yaml:"engine,omitempty"`
	Port     int             `yaml:"port,omitempty"`
	APIGroup string          `yaml:"api_group,omitempty"`
	Resource string          `yaml:"resource,omitempty"`
	Verbs    []string        `yaml:"verbs,omitempty"`
	Level    string          `yaml:"level,omitempty"`
	// Components are the components that state the need, by name.
	Components []string `yaml:"components"`
}

// Requirements are the platform requirements of one service (ST-DLV-EN01): the file
// deploy/platform-requirements.yaml.
type Requirements struct {
	// Version is the version of the format, 1.
	Version      int           `yaml:"version"`
	Service      string        `yaml:"service"`
	Requirements []Requirement `yaml:"requirements"`
}

// RequirementsFile is the path, in the project folder, of the platform requirements (ST-DLV-EN01-TB).
const RequirementsFile = "deploy/platform-requirements.yaml"

// RequirementsVersion is the version of the format of the platform requirements.
const RequirementsVersion = 1

// MaxRequirementsBytes bounds the platform requirements that LoadRequirements reads.
const MaxRequirementsBytes = 1 << 20

var (
	// requirementName is a resolved requirement name: letters, digits, '-' and '_', so that it cannot hold a host
	// name, an address, a path or a URL.
	requirementName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	// shapeWord is a key, an engine or a resource.
	shapeWord = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// placeholder is one placeholder of a need name.
	placeholder = regexp.MustCompile(`\{[^{}]*\}`)
	// apiGroup is a Kubernetes API group, "" for the core group.
	apiGroup = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*)?$`)
	// apiResource is a Kubernetes resource, such as configmaps.
	apiResource = regexp.MustCompile(`^[a-z][a-z0-9]*(/[a-z]+)?$`)
)

// maxWord bounds a requirement name, a key, an engine and a resource.
const maxWord = 63

var (
	clusterVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	podLevels    = []string{"privileged", "baseline", "restricted"}
)

// RequirementError is a need that the platform requirements cannot hold: the components that state it, the need,
// the field at fault, if one, and why.
type RequirementError struct {
	Components []string
	Need       string
	Field      string
	Reason     string
}

func (e *RequirementError) Error() string {
	field := ""
	if e.Field != "" {
		field = " field " + e.Field + ":"
	}
	return fmt.Sprintf("component %s: need %s:%s %s", strings.Join(e.Components, " and component "), e.Need, field, e.Reason)
}

// DeriveRequirements derives the platform requirements of the service from the needs its components state alone,
// in a fixed order, so that the same declaration and registry always give the same requirements (ST-DLV-DS01). It
// fills each name from the service name and the settings, refuses a need whose filled name or shape holds a value
// of an environment (ST-DLV-INV02), and refuses one need that two components state with different kinds or shapes
// (ST-DLV-UC01-R04). A need that two components state alike is one requirement with both components.
func DeriveRequirements(service string, components []ComponentNeeds) (Requirements, error) {
	byName := map[string]*Requirement{}
	for _, component := range components {
		for _, need := range component.Needs {
			requirement, err := requirementOf(service, component, need)
			if err != nil {
				return Requirements{}, err
			}
			previous, stated := byName[requirement.Name]
			if !stated {
				byName[requirement.Name] = &requirement
				continue
			}
			if slices.Contains(previous.Components, component.Component) {
				return Requirements{}, &RequirementError{Components: []string{component.Component}, Need: requirement.Name, Reason: "the component states the need twice"}
			}
			// The two statements in the order of their components' names, so that the message is the same
			// whatever order the components load in.
			first, second := *previous, requirement
			first.Components = previous.Components[:1]
			if second.Components[0] < first.Components[0] {
				first, second = second, first
			}
			both := []string{first.Components[0], second.Components[0]}
			if first.Kind != second.Kind {
				return Requirements{}, &RequirementError{Components: both, Need: requirement.Name,
					Reason: fmt.Sprintf("the components state the need with different kinds, %s and %s; one need has one kind", first.Kind, second.Kind)}
			}
			if shape(first) != shape(second) {
				return Requirements{}, &RequirementError{Components: both, Need: requirement.Name,
					Reason: fmt.Sprintf("the components state the %s with different shapes, %s and %s; one need has one shape", requirement.Kind, shape(first), shape(second))}
			}
			previous.Components = append(previous.Components, component.Component)
		}
	}
	result := Requirements{Version: RequirementsVersion, Service: service, Requirements: []Requirement{}}
	for _, requirement := range byName {
		slices.Sort(requirement.Components)
		result.Requirements = append(result.Requirements, *requirement)
	}
	slices.SortFunc(result.Requirements, func(a, b Requirement) int {
		if order := slices.Index(RequirementKinds, a.Kind) - slices.Index(RequirementKinds, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

// shape gives the shape fields of a requirement as one text, to compare two statements of one need.
func shape(r Requirement) string {
	var parts []string
	if len(r.Keys) > 0 {
		parts = append(parts, "keys "+strings.Join(r.Keys, ", "))
	}
	if r.Engine != "" {
		parts = append(parts, "engine "+r.Engine)
	}
	if r.Port != 0 {
		parts = append(parts, "port "+strconv.Itoa(r.Port))
	}
	if r.APIGroup != "" {
		parts = append(parts, "API group "+r.APIGroup)
	}
	if r.Resource != "" {
		parts = append(parts, "resource "+r.Resource)
	}
	if len(r.Verbs) > 0 {
		parts = append(parts, "verbs "+strings.Join(r.Verbs, ", "))
	}
	if r.Level != "" {
		parts = append(parts, "level "+r.Level)
	}
	if len(parts) == 0 {
		return "no shape"
	}
	return strings.Join(parts, "; ")
}

// requirementOf fills the name of one need of a component and checks the requirement it gives.
func requirementOf(service string, component ComponentNeeds, need Need) (Requirement, error) {
	fault := func(name, field, format string, args ...any) error {
		return &RequirementError{Components: []string{component.Component}, Need: name, Field: field, Reason: fmt.Sprintf(format, args...)}
	}
	name, err := fillName(service, component.Settings, need.Name)
	if err != nil {
		return Requirement{}, fault(need.Name, "name", "%s", err)
	}
	if len(name) > maxWord || !requirementName.MatchString(name) {
		return Requirement{}, fault(name, "name", "the name is not a name of at most %d letters, digits, '-' or '_', so it can hold a value of an environment, such as a host name or an address", maxWord)
	}
	kind, err := ParseRequirementKind(need.Kind)
	if err != nil {
		return Requirement{}, fault(name, "kind", "%s", err)
	}
	requirement := Requirement{Kind: kind, Name: name, Keys: slices.Sorted(slices.Values(need.Keys)), Engine: need.Engine, Port: need.Port,
		APIGroup: need.APIGroup, Resource: need.Resource, Verbs: slices.Sorted(slices.Values(need.Verbs)), Level: need.Level, Components: []string{component.Component}}
	if field, reason := checkShape(requirement); field != "" {
		return Requirement{}, fault(name, field, "%s", reason)
	}
	return requirement, nil
}

// fillName fills the placeholders {service} and {settings.<key>} of a need name from the service name and the
// settings of the component.
func fillName(service string, settings map[string]string, name string) (string, error) {
	var failure error
	filled := placeholder.ReplaceAllStringFunc(name, func(text string) string {
		inner := strings.Trim(text, "{}")
		if inner == "service" {
			return service
		}
		if key, ok := strings.CutPrefix(inner, "settings."); ok {
			if value, given := settings[key]; given {
				return value
			}
			failure = errors.Join(failure, fmt.Errorf("the declaration gives no value for the setting %s", key))
			return text
		}
		failure = errors.Join(failure, fmt.Errorf("the placeholder %s is not {service} or {settings.<key>}", text))
		return text
	})
	return filled, failure
}

// checkShape holds a requirement to the shape of its kind (ST-SUP-EN09 field Platform needs): each shape field of
// its kind, no field of another kind, and no field that can hold a value of an environment. It gives the field at
// fault and why, or "" when the shape is sound.
func checkShape(r Requirement) (string, string) {
	given := map[string]bool{"keys": len(r.Keys) > 0, "engine": r.Engine != "", "port": r.Port != 0, "api_group": r.APIGroup != "",
		"resource": r.Resource != "", "verbs": len(r.Verbs) > 0, "level": r.Level != ""}
	shapes := map[RequirementKind][]string{Secret: {"keys"}, BackingStore: {"engine"}, Ingress: {"port"}, Egress: {"port"},
		ClusterPermission: {"resource", "verbs"}, PodSecurity: {"level"}}
	for _, field := range []string{"keys", "engine", "port", "api_group", "resource", "verbs", "level"} {
		ofKind := slices.Contains(shapes[r.Kind], field) || (field == "api_group" && r.Kind == ClusterPermission)
		if given[field] && !ofKind {
			return field, fmt.Sprintf("a %s need has no field %s", r.Kind, field)
		}
		if !given[field] && slices.Contains(shapes[r.Kind], field) {
			return field, fmt.Sprintf("a %s need must give %s", r.Kind, field)
		}
	}
	for i, key := range r.Keys {
		if len(key) > maxWord || !shapeWord.MatchString(key) {
			return "keys", fmt.Sprintf("the key %q is not a key name of at most %d letters, digits, '-' or '_'", key, maxWord)
		}
		if i > 0 && r.Keys[i-1] == key {
			return "keys", fmt.Sprintf("the key %s is stated twice", key)
		}
	}
	if r.Engine != "" && (len(r.Engine) > maxWord || !shapeWord.MatchString(r.Engine)) {
		return "engine", fmt.Sprintf("the engine %q is not an engine name of at most %d letters, digits, '-' or '_'", r.Engine, maxWord)
	}
	if r.Port < 0 || r.Port > 65535 {
		return "port", fmt.Sprintf("the port %d is not a port number from 1 to 65535", r.Port)
	}
	if len(r.APIGroup) > 253 || !apiGroup.MatchString(r.APIGroup) {
		return "api_group", fmt.Sprintf("the API group %q is not a Kubernetes API group", r.APIGroup)
	}
	if r.Resource != "" && (len(r.Resource) > maxWord || !apiResource.MatchString(r.Resource)) {
		return "resource", fmt.Sprintf("the resource %q is not a Kubernetes resource name", r.Resource)
	}
	for i, verb := range r.Verbs {
		if !slices.Contains(clusterVerbs, verb) {
			return "verbs", fmt.Sprintf("the verb %q is not one of %s", verb, strings.Join(clusterVerbs, ", "))
		}
		if i > 0 && r.Verbs[i-1] == verb {
			return "verbs", fmt.Sprintf("the verb %s is stated twice", verb)
		}
	}
	if r.Level != "" && !slices.Contains(podLevels, r.Level) {
		return "level", fmt.Sprintf("the level %q is not one of %s", r.Level, strings.Join(podLevels, ", "))
	}
	return "", ""
}

// requirementsHeader opens the platform requirements file.
const requirementsHeader = "# The platform requirements of the service: what its environment must supply. stego apply writes this file\n" +
	"# from the declaration and the needs of its components. Do not edit it: change the declaration or the registry.\n"

// WriteRequirements writes the platform requirements of the service during a run, from the declaration and its
// components' needs (ST-DLV-CMD01): the bytes of deploy/platform-requirements.yaml, which the run adds to its plan.
func WriteRequirements(service string, components []ComponentNeeds) ([]byte, error) {
	requirements, err := DeriveRequirements(service, components)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(requirementsHeader)
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(requirements); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// LoadRequirements reads the platform requirements back through the strict decoder (ST-DLV-REPO01): one document
// of at most MaxRequirementsBytes, with no alias and no field the format does not have, of format version 1, whose
// every requirement is of a known kind and holds the shape of its kind.
func LoadRequirements(data []byte) (Requirements, error) {
	var document yaml.Node
	if err := decodeStrict(data, MaxRequirementsBytes, &document); err != nil {
		return Requirements{}, fmt.Errorf("platform requirements: %w", err)
	}
	if err := checkRequirementsFields(&document); err != nil {
		return Requirements{}, fmt.Errorf("platform requirements: %w", err)
	}
	var requirements Requirements
	if err := decodeStrict(data, MaxRequirementsBytes, &requirements); err != nil {
		return Requirements{}, fmt.Errorf("platform requirements: %w", err)
	}
	if requirements.Version != RequirementsVersion {
		return Requirements{}, fmt.Errorf("platform requirements: field version: the format version is %d; this stego reads version %d", requirements.Version, RequirementsVersion)
	}
	if requirements.Service == "" {
		return Requirements{}, fmt.Errorf("platform requirements: the file names no service")
	}
	seen := map[string]bool{}
	for _, requirement := range requirements.Requirements {
		if len(requirement.Name) > maxWord || !requirementName.MatchString(requirement.Name) {
			return Requirements{}, fmt.Errorf("platform requirements: the requirement name %q is not a name of at most %d letters, digits, '-' or '_'", requirement.Name, maxWord)
		}
		if seen[requirement.Name] {
			return Requirements{}, fmt.Errorf("platform requirements: the requirement %s is stated twice", requirement.Name)
		}
		seen[requirement.Name] = true
		if len(requirement.Components) == 0 {
			return Requirements{}, fmt.Errorf("platform requirements: the requirement %s names no component", requirement.Name)
		}
		sorted := requirement
		sorted.Keys = slices.Sorted(slices.Values(requirement.Keys))
		sorted.Verbs = slices.Sorted(slices.Values(requirement.Verbs))
		if field, reason := checkShape(sorted); field != "" {
			return Requirements{}, fmt.Errorf("platform requirements: requirement %s: field %s: %s", requirement.Name, field, reason)
		}
	}
	return requirements, nil
}

// checkRequirementsFields holds the top fields of the platform requirements to the node kinds of the format, so
// that a refusal names the field that is wrong: version and service are each one value, and requirements a list.
func checkRequirementsFields(document *yaml.Node) error {
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: the file is not a map of the fields version, service and requirements", document.Line)
	}
	top := document.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, value := top.Content[i], top.Content[i+1]
		switch key.Value {
		case "version", "service":
			if value.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: field %s: the %s is one value", value.Line, key.Value, key.Value)
			}
		case "requirements":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("line %d: field requirements: the requirements are a list, not a %s", value.Line, nodeKind(value))
			}
		}
	}
	return nil
}

// nodeKind names the kind of a YAML node in a message.
func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "map"
	case yaml.SequenceNode:
		return "list"
	default:
		return "single value"
	}
}

// maxDepth bounds the nesting of a document that decodeStrict reads.
const maxDepth = 16

// decodeStrict decodes one YAML document of at most limit bytes into target. It refuses a second document, an
// anchor, an alias, a merge key, a nesting deeper than maxDepth and a field that target does not have.
func decodeStrict(data []byte, limit int, target any) error {
	if len(data) > limit {
		return fmt.Errorf("the file is larger than %d bytes", limit)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the file is empty")
		}
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the file holds more than one document")
	}
	if err := checkNode(&root, 0); err != nil {
		return err
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	return strict.Decode(target)
}

// checkNode refuses an anchor, an alias, a merge key and a deep nesting in a document.
func checkNode(node *yaml.Node, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("line %d: the document is nested deeper than %d levels", node.Line, maxDepth)
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("line %d: the document holds an anchor or an alias", node.Line)
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Tag == "!!merge" {
				return fmt.Errorf("line %d: the document holds a merge key", node.Content[i].Line)
			}
		}
	}
	for _, child := range node.Content {
		if err := checkNode(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}
