package registry

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
)

// ParseArchetype reads and parses an archetype YAML file.
func ParseArchetype(path string) (*Archetype, error) {
	return parser.ReadKind(path, "archetype", archetypeKind)
}

// ParseComponent reads and parses a component YAML file.
func ParseComponent(path string) (*Component, error) {
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, err
	}
	return ParseComponentFromBytes(data, path)
}

// ParseMixin reads and parses a mixin YAML file.
func ParseMixin(path string) (*Mixin, error) {
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, err
	}
	return ParseMixinFromBytes(data, path)
}

// ParseArchetypeFromBytes parses one captured registry document.
func ParseArchetypeFromBytes(data []byte, path string) (*Archetype, error) {
	return parser.ParseKind(data, path, "archetype", archetypeKind)
}

// ParseComponentFromBytes parses one captured registry document. It decodes the document strictly, and it refuses
// a need with a field that its kind does not have, naming the component, the need and the field, so that an
// environment value has no place to go (ST-DLV-INV02, ST-DLV-Q04 (A)).
func ParseComponentFromBytes(data []byte, path string) (*Component, error) {
	var component Component
	undeclared, err := parser.DecodeCollecting(data, path, &component)
	if err != nil {
		return nil, err
	}
	if len(undeclared) > 0 {
		first := undeclared[0]
		var index int
		var rest string
		if n, _ := fmt.Sscanf(first.Path, "needs[%d]", &index); n == 1 && index >= 0 && index < len(component.Needs) {
			if _, rest, _ = strings.Cut(first.Path, "]."); rest != "" && !strings.Contains(rest, ".") {
				need := component.Needs[index]
				return nil, &NeedError{Component: component.Name, Need: need.Name, Field: rest,
					Reason: fmt.Sprintf("a %s need has no field %s; a need holds only its kind, its name and the shape of its kind, and never a value of an environment such as a secret value, a host name, an address or a namespace", kindWord(need.Kind), rest)}
			}
		}
		return nil, parser.UndeclaredError(data, path, first)
	}
	if component.Kind == "" {
		return nil, &parser.ParseError{Path: path, Err: fmt.Errorf("missing required field: kind")}
	}
	if component.Kind != "component" {
		return nil, &parser.ParseError{Path: path, Err: fmt.Errorf("expected kind %q, got %q", "component", component.Kind)}
	}
	if err := checkPipelines(&component); err != nil {
		return nil, &parser.ParseError{Path: path, Err: err}
	}
	if err := checkNeeds(&component); err != nil {
		return nil, err
	}
	// The registry refuses a component that leaves out a fact of an environment variable, states a default or an
	// example for a secret, or a default for a required variable, naming the variable and the fact
	// (ST-SUP-UC01-R09).
	if err := checkEnvironment(&component); err != nil {
		return nil, err
	}
	// The registry refuses a component that does not state its place, naming it and the statement
	// (ST-SUP-UC01-R05).
	if fault := statesPlace(&component); fault != nil {
		return nil, fault
	}
	return &component, nil
}

// NeedError is a need of a component that the registry refuses: the component, the need as the component states
// it, the field at fault and why.
type NeedError struct {
	Component string
	Need      string
	Field     string
	Reason    string
}

func (e *NeedError) Error() string {
	return fmt.Sprintf("component %s: need %s: field %s: %s", e.Component, e.Need, e.Field, e.Reason)
}

// NeedKinds are the nine kinds of ST-VO53 that a need may have, in the order of the platform requirements.
var NeedKinds = []string{"secret", "configuration", "backing store", "topic", "identity issuer", "ingress", "egress", "cluster permission", "pod security"}

// needShapes gives the shape fields of each kind of need: the fields beside kind and name that a need of the kind
// must give. A cluster permission may also give api_group, which is "" for the core API group.
var needShapes = map[string][]string{
	"secret": {"keys"}, "configuration": nil, "backing store": {"engine"}, "topic": nil, "identity issuer": nil,
	"ingress": {"port"}, "egress": {"port"}, "cluster permission": {"resource", "verbs"}, "pod security": {"level"},
}

// clusterVerbs are the verbs a cluster permission may name.
var clusterVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}

var (
	// needWord is fixed text of a need name, a secret key or a backing store engine: letters, digits, '-' and
	// '_', so that it cannot hold a host name, an address or a path.
	needWord = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// needLabel is one allowed value of a setting that a need name holds: a DNS label of lower-case letters, digits
	// and '-', of 1 to 63 characters, that starts and ends with a letter or a digit (ST-SUP-INV23).
	needLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// needPlaceholder is one placeholder of a need name.
	needPlaceholder = regexp.MustCompile(`\{[^{}]*\}`)
	// apiGroup is a Kubernetes API group, such as apps or rbac.authorization.k8s.io.
	apiGroup = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*)?$`)
	// apiResource is a Kubernetes resource, such as configmaps.
	apiResource = regexp.MustCompile(`^[a-z][a-z0-9]*(/[a-z]+)?$`)
)

// maxNeedWord bounds a need name, a key, an engine and a resource.
const maxNeedWord = 63

// kindWord gives the kind of a need for a message.
func kindWord(kind string) string {
	if slices.Contains(NeedKinds, kind) {
		return kind
	}
	return strconv.Quote(kind)
}

// checkNeeds holds each need of a component to its kind's shape (ST-SUP-EN09 field Platform needs): a known kind,
// a name of fixed text and placeholders, each shape field of the kind and no field of another kind.
func checkNeeds(component *Component) error {
	seen := map[string]bool{}
	for _, need := range component.Needs {
		fault := func(field, format string, args ...any) error {
			return &NeedError{Component: component.Name, Need: need.Name, Field: field, Reason: fmt.Sprintf(format, args...)}
		}
		shape, known := needShapes[need.Kind]
		if !known {
			return fault("kind", "the kind %s is not one of %s", kindWord(need.Kind), strings.Join(NeedKinds, ", "))
		}
		if err := checkNeedName(component, need.Name); err != nil {
			return fault("name", "%s", err)
		}
		if seen[need.Name] {
			return fault("name", "the component states the need twice")
		}
		seen[need.Name] = true
		// A resource name is the name of a cluster object, so it holds the same placeholders as a need name: a
		// declaration cannot choose the object of another service (ST-SUP-INV23, #404 security review L5).
		for _, resourceName := range need.ResourceNames {
			if err := checkNeedName(component, resourceName); err != nil {
				return fault("resource_names", "the resource name %q: %s", resourceName, err)
			}
		}
		given := map[string]bool{"keys": len(need.Keys) > 0, "engine": need.Engine != "", "port": need.Port != 0,
			"api_group": need.APIGroup != "", "resource": need.Resource != "", "verbs": len(need.Verbs) > 0, "level": need.Level != "",
			"resource_names": len(need.ResourceNames) > 0}
		for _, field := range []string{"keys", "engine", "port", "api_group", "resource", "verbs", "resource_names", "level"} {
			// api_group and resource_names are the optional shape fields of a cluster permission (ST-DLV-Q14).
			ofKind := slices.Contains(shape, field) || ((field == "api_group" || field == "resource_names") && need.Kind == "cluster permission")
			if given[field] && !ofKind {
				return fault(field, "a %s need has no field %s", need.Kind, field)
			}
			if !given[field] && slices.Contains(shape, field) {
				return fault(field, "a %s need must give %s", need.Kind, field)
			}
		}
		for i, key := range need.Keys {
			if len(key) > maxNeedWord || !needWord.MatchString(key) {
				return fault("keys", "the key %q is not a key name of at most %d letters, digits, '-' or '_'", key, maxNeedWord)
			}
			if slices.Contains(need.Keys[:i], key) {
				return fault("keys", "the key %s is stated twice", key)
			}
		}
		if need.Engine != "" && (len(need.Engine) > maxNeedWord || !needWord.MatchString(need.Engine)) {
			return fault("engine", "the engine %q is not an engine name of at most %d letters, digits, '-' or '_'", need.Engine, maxNeedWord)
		}
		switch {
		case need.Access != "" && need.Kind != "topic":
			return fault("access", "a %s need has no field access; only a topic names the access read or write", need.Kind)
		case need.Access != "" && need.Access != "read" && need.Access != "write":
			return fault("access", "the access %q is not read or write", need.Access)
		}
		if err := checkNeedVariable(component, need); err != nil {
			return err
		}
		if need.Port != 0 && (need.Port < 1 || need.Port > 65535) {
			return fault("port", "the port %d is not a port number from 1 to 65535", need.Port)
		}
		if len(need.APIGroup) > 253 || !apiGroup.MatchString(need.APIGroup) {
			return fault("api_group", "the API group %q is not a Kubernetes API group", need.APIGroup)
		}
		if need.Resource != "" && (len(need.Resource) > maxNeedWord || !apiResource.MatchString(need.Resource)) {
			return fault("resource", "the resource %q is not a Kubernetes resource name", need.Resource)
		}
		for i, verb := range need.Verbs {
			if !slices.Contains(clusterVerbs, verb) {
				return fault("verbs", "the verb %q is not one of %s", verb, strings.Join(clusterVerbs, ", "))
			}
			if slices.Contains(need.Verbs[:i], verb) {
				return fault("verbs", "the verb %s is stated twice", verb)
			}
		}
		if need.Level != "" && !slices.Contains([]string{"privileged", "baseline", "restricted"}, need.Level) {
			return fault("level", "the level %q is not one of privileged, baseline, restricted", need.Level)
		}
	}
	return nil
}

// checkNeedVariable holds the variable and the value of a need (ST-SUP-EN09 field Platform needs, ST-DLV-Q26 (A)):
// only a topic or an identity issuer names a variable, which is a variable of the component that is not secret; only
// an identity issuer holds a value, which is one {settings.<key>} of a text setting of the component that is not
// secret.
func checkNeedVariable(component *Component, need Need) error {
	fault := func(field, format string, args ...any) error {
		return &NeedError{Component: component.Name, Need: need.Name, Field: field, Reason: fmt.Sprintf(format, args...)}
	}
	if need.Variable != "" {
		if need.Kind != "topic" && need.Kind != "identity issuer" {
			return fault("variable", "the %s need has no field variable; only a topic or an identity issuer names the variable that carries its value", need.Kind)
		}
		var found *EnvironmentVariable
		if component.Environment != nil {
			for i, variable := range *component.Environment {
				if variable.Name == need.Variable {
					found = &(*component.Environment)[i]
				}
			}
		}
		if found == nil {
			return fault("variable", "the variable %s is not an environment variable of the component", need.Variable)
		}
		if found.IsSecret() {
			return fault("variable", "the variable %s is secret; the value of a %s is not secret", need.Variable, need.Kind)
		}
		// A need of each subscription carries the value of the variable of that subscription, and a need of the
		// service the value of a variable of the service (decision on ST-SUP-Q204).
		if strings.Contains(need.Name, SubscriptionPlaceholder) != strings.Contains(need.Variable, SubscriptionPlaceholder) {
			return fault("variable", "the need name and its variable %s must both hold %s or neither, so that the need of each subscription carries the variable of that subscription", need.Variable, SubscriptionPlaceholder)
		}
	}
	if need.Value != "" {
		if need.Kind != "identity issuer" {
			return fault("value", "the %s need has no field value; only an identity issuer holds the issuer that the declaration fixes", need.Kind)
		}
		key, isSetting := settingOf(need.Value)
		setting, declared := component.Config[key]
		if !isSetting || !declared || setting.Secret || setting.Type != "string" {
			return fault("value", "the value %q is not one {settings.<key>} of a text setting of the component that is not secret", need.Value)
		}
	}
	return nil
}

// checkNeedName holds a need name to fixed text and the placeholders {service} and {settings.<key>}, where key is a
// text setting of the component that is not secret and whose allowed values are DNS labels alone (ST-DLV-Q04 (A),
// ST-SUP-INV23, decision on ST-SUP-Q153). So no secret, no free text and no value that differs per environment goes
// into a need name.
func checkNeedName(component *Component, name string) error {
	if name == "" {
		return fmt.Errorf("a need must give its name")
	}
	if len(name) > 4*maxNeedWord {
		return fmt.Errorf("the name is longer than %d characters", 4*maxNeedWord)
	}
	for _, placeholder := range needPlaceholder.FindAllString(name, -1) {
		inner := strings.Trim(placeholder, "{}")
		if inner == "service" {
			continue
		}
		if placeholder == SubscriptionPlaceholder {
			// A subscription name is a DNS label of the declaration, never a value of an environment (ST-SUP-INV23,
			// decision on ST-SUP-Q204).
			if !ProvidesSubscriptions(component) {
				return fmt.Errorf("the placeholder %s is only for a component that provides the port %s", placeholder, SubscriptionsPort)
			}
			continue
		}
		key, isSetting := strings.CutPrefix(inner, "settings.")
		setting, declared := component.Config[key]
		if !isSetting || !declared {
			return fmt.Errorf("the placeholder %s is not {service}, {subscription} or {settings.<key>} of a setting of the component", placeholder)
		}
		if setting.Secret {
			return fmt.Errorf("the placeholder %s names the secret setting %s; a name never holds a secret (ST-SUP-INV23)", placeholder, key)
		}
		if setting.Type != "string" || len(setting.Enum) == 0 || slices.ContainsFunc(setting.Enum, func(value string) bool { return !needLabel.MatchString(value) }) {
			return fmt.Errorf("the placeholder %s names the setting %s, which does not allow only DNS labels of lower-case letters, digits and '-'; give it an enum of such labels (ST-SUP-INV23)", placeholder, key)
		}
	}
	for _, text := range needPlaceholder.Split(name, -1) {
		if text != "" && !needWord.MatchString(text) {
			return fmt.Errorf("the fixed text %q of the name holds a character other than a letter, a digit, '-' or '_'", text)
		}
	}
	return nil
}

// ParseMixinFromBytes parses one captured registry document. It refuses a slot the mixin adds that states no phase
// or no operation that calls it, naming the mixin, the slot and the statement (ST-SUP-UC01-R05).
func ParseMixinFromBytes(data []byte, path string) (*Mixin, error) {
	mixin, err := parser.ParseKind(data, path, "mixin", mixinKind)
	if err != nil {
		return nil, err
	}
	if fault := slotsStatePlace("mixin "+mixin.Name, mixin.AddsSlots); fault != nil {
		return nil, fault
	}
	return mixin, nil
}

func archetypeKind(a *Archetype) string { return a.Kind }

func mixinKind(m *Mixin) string { return m.Kind }
