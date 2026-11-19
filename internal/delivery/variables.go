package delivery

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The environment variable list of the platform requirements (ST-DLV-EN08, ST-DLV-UC01-R06 to R09, owner,
// 2026-11-14): every environment variable that the service accepts, from the components of the active set and the
// configuration fields of the declaration, derived once on each apply. The platform requirements hold the list, and
// the reference deploy/environment-variables.md is rendered from the same list.

// Variable is one environment variable as a component or the declaration states it: its name may hold the
// placeholders {service} and {settings.<key>}, and its default may be {settings.<key>}.
type Variable struct {
	Name        string
	Type        string
	Required    bool
	Default     *string
	Description string
	Secret      bool
	// Example is one value the reader can copy, for a variable that is not secret; only the reference shows it.
	Example *string
	// Allowed are the allowed values as one text, such as "otlp, none" or "1s to 30m".
	Allowed string
	// SecretSource is the secret need and the key that give the value of a secret variable, or the file of a path
	// variable; its name may hold the placeholders of a need name (ST-DLV-Q19).
	SecretSource *SecretSource
	// RefusedAtStart marks a variable that a library reads and that the start guard of the service refuses when it
	// has a value, such as PGHOST (ST-SUP-UC01-R10): the service never takes it.
	RefusedAtStart bool
	// ReadByMode is the one mode that reads the variable, migrate for the migration job, or "" (ST-DLV-Q19).
	ReadByMode string
	// OneOf is the group of variables of the component of which the process needs exactly one value, such as
	// DATABASE_URL and DATABASE_URL_FILE, or "" (ST-SUP-EN14 field One of, ST-DLV-Q09).
	OneOf string
	// RequiredWhen is the condition under which the service does not start without a value, for a variable that is
	// not required on its own, or "" (ST-SUP-EN14 field Required when). Only the reference shows it.
	RequiredWhen string
}

// SecretSource is one key of one secret need: the need name and the key (ST-DLV-EN08 field Secret source,
// ST-DLV-Q19). The Kubernetes base names the Secret and the key of a secret variable from it.
type SecretSource struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// DeclaredVariables are the environment variables that the declaration gives one group of a component, such as the
// configuration fields of one group of runtime-configuration (ST-SUP-RC22-OC09). Their names hold no placeholder.
type DeclaredVariables struct {
	Group     string
	Variables []Variable
}

// AcceptedVariable is one environment variable that the service accepts, as the platform requirements list it
// (ST-DLV-EN08). A secret variable holds its name, readers, type, required flag, description and secret flag only
// (ST-DLV-INV12).
type AcceptedVariable struct {
	Name string `yaml:"name"`
	// ReadBy are the components that read the variable, in name order, or runtime-configuration with the group.
	ReadBy      []string `yaml:"read_by"`
	Type        string   `yaml:"type"`
	Required    bool     `yaml:"required"`
	Default     *string  `yaml:"default,omitempty"`
	Description string   `yaml:"description"`
	Secret      bool     `yaml:"secret"`
	Allowed     string   `yaml:"allowed,omitempty"`
	// SecretSource is the secret need and the key that give the value, or the file of a path variable, when the
	// service states that need (ST-DLV-Q19).
	SecretSource *SecretSource `yaml:"secret_source,omitempty"`
	// RefusedAtStart marks a variable that the start guard refuses when it has a value (ST-SUP-UC01-R10): the base
	// never wires it, and the environment check refuses a value for it.
	RefusedAtStart bool `yaml:"refused_at_start,omitempty"`
	// ReadByMode is the one mode of the process that reads the variable, migrate for the migration job, when no other
	// mode reads it. The serving Deployment of the base never wires such a variable (ST-DLV-Q19).
	ReadByMode string `yaml:"read_by_mode,omitempty"`
	// OneOf is the group of variables of which the service needs exactly one value (ST-DLV-EN08 field One of,
	// ST-DLV-Q09): the environment check fails when the environment file gives no variable of the group a value, or
	// more than one.
	OneOf string `yaml:"one_of,omitempty"`
	// example is the example of the first reader, for the reference alone: the platform requirements never hold
	// it, because an example is often a host name (ST-DLV-INV02).
	example *string
	// requiredWhen is the condition of Variable.RequiredWhen of the first reader, for the reference alone: the
	// platform requirements, the environment check and the base take the variable as not required (ST-DLV-UC01-R07).
	requiredWhen string
}

// VariableError is an environment variable that the platform requirements cannot list: the sources that state it,
// the variable and why. A conflict names both sources (ST-DLV-UC01-R09).
type VariableError struct {
	Sources  []string
	Variable string
	Reason   string
}

func (e *VariableError) Error() string {
	return fmt.Sprintf("%s: environment variable %s: %s", strings.Join(e.Sources, " and "), e.Variable, e.Reason)
}

// variableTypes are the types of ST-VO63.
var variableTypes = []string{"string", "integer", "number", "duration", "boolean", "url", "path", "address", "list"}

var (
	// variableName is a resolved variable name.
	variableName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	// settingDefault is a default that is a setting of the component.
	settingDefault = regexp.MustCompile(`^\{settings\.([a-z][a-z0-9_]*)\}$`)
	// sentenceStop is the end of one sentence of a description.
	sentenceStop = regexp.MustCompile(`\.( |$)`)
)

// maxVariableValue bounds a default and an allowed value text, as the registry does.
const maxVariableValue = 512

// source names a source of a variable in a message: a component, or runtime-configuration with its group.
func source(component, group string) string {
	if group == "" {
		return "component " + component
	}
	return fmt.Sprintf("component %s group %s", component, group)
}

// reader names a reader of a variable in the list.
func reader(component, group string) string {
	if group == "" {
		return component
	}
	return fmt.Sprintf("%s (%s)", component, group)
}

// deriveVariables derives the environment variable list of the service from the variables that its components
// state and that the declaration gives them, in name order (ST-DLV-DS01, ST-DLV-INV11). It fills each name and
// each setting default, refuses a variable whose facts break their rules, and refuses one name that two sources
// state with a different type, required flag, default, secret flag or allowed values (ST-DLV-UC01-R09). The
// description and the example of a variable that several sources state are those of the first reader in name order.
func deriveVariables(service string, components []ComponentNeeds) ([]AcceptedVariable, error) {
	type stated struct {
		variable AcceptedVariable
		source   string
		// first is the first reader in name order, whose description and example the list shows.
		first string
	}
	byName := map[string]*stated{}
	var names []string
	add := func(component, group string, settings map[string]string, variable Variable) error {
		src := source(component, group)
		accepted, err := acceptedOf(service, settings, variable)
		if err != nil {
			return &VariableError{Sources: []string{src}, Variable: variable.Name, Reason: err.Error()}
		}
		accepted.ReadBy = []string{reader(component, group)}
		previous, seen := byName[accepted.Name]
		if !seen {
			byName[accepted.Name] = &stated{variable: accepted, source: src, first: accepted.ReadBy[0]}
			names = append(names, accepted.Name)
			return nil
		}
		if slices.Contains(previous.variable.ReadBy, accepted.ReadBy[0]) {
			return &VariableError{Sources: []string{src}, Variable: accepted.Name, Reason: "the source states the variable twice"}
		}
		if fact := conflict(previous.variable, accepted); fact != "" {
			// The two sources in byte order, so that the message is the same whatever order they load in.
			both := []string{previous.source, src}
			slices.Sort(both)
			return &VariableError{Sources: both, Variable: accepted.Name,
				Reason: fmt.Sprintf("the sources state the variable with a different %s; one variable has one %s", fact, fact)}
		}
		previous.variable.ReadBy = append(previous.variable.ReadBy, accepted.ReadBy[0])
		// A source that one reader states and another does not is the source of the variable.
		if previous.variable.SecretSource == nil {
			previous.variable.SecretSource = accepted.SecretSource
		}
		// The description and the example of the first reader in name order.
		if accepted.ReadBy[0] < previous.first {
			previous.first = accepted.ReadBy[0]
			previous.variable.Description, previous.variable.example = accepted.Description, accepted.example
			previous.variable.requiredWhen = accepted.requiredWhen
		}
		return nil
	}
	for _, component := range components {
		for _, variable := range component.Variables {
			if err := add(component.Component, "", component.Settings, variable); err != nil {
				return nil, err
			}
		}
		for _, group := range component.Declared {
			for _, variable := range group.Variables {
				if strings.ContainsAny(variable.Name, "{}") || (variable.Default != nil && strings.ContainsAny(*variable.Default, "{}")) {
					return nil, &VariableError{Sources: []string{source(component.Component, group.Group)}, Variable: variable.Name,
						Reason: "a variable that the declaration gives holds no placeholder"}
				}
				if err := add(component.Component, group.Group, nil, variable); err != nil {
					return nil, err
				}
			}
		}
	}
	slices.Sort(names)
	list := make([]AcceptedVariable, 0, len(names))
	for _, name := range names {
		variable := byName[name].variable
		slices.Sort(variable.ReadBy)
		list = append(list, variable)
	}
	if err := checkGroups(list); err != nil {
		return nil, err
	}
	return list, nil
}

// groupName is the name of a group of one of: lower-case letters, digits and '-'.
var groupName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Groups gives the variables of each group of one of of the list, by group, in name order.
func Groups(list []AcceptedVariable) map[string][]AcceptedVariable {
	groups := map[string][]AcceptedVariable{}
	for _, variable := range list {
		if variable.OneOf != "" {
			groups[variable.OneOf] = append(groups[variable.OneOf], variable)
		}
	}
	return groups
}

// checkGroups holds each group of one of of the list to its rules (ST-SUP-INV24, ST-DLV-Q09): at least two
// variables, which the same components read, so that the group of one component never takes a variable of another.
// checkAccepted holds the name of the group and that no variable of a group is required on its own.
func checkGroups(list []AcceptedVariable) error {
	groups := Groups(list)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		members := groups[name]
		if len(members) < 2 {
			return &VariableError{Sources: members[0].ReadBy, Variable: members[0].Name,
				Reason: fmt.Sprintf("the variable is the only one of the group of one of %s; a group has at least two variables, such as X and X_FILE", name)}
		}
		for _, member := range members[1:] {
			if !slices.Equal(member.ReadBy, members[0].ReadBy) {
				return &VariableError{Sources: append(slices.Clone(members[0].ReadBy), member.ReadBy...), Variable: member.Name,
					Reason: fmt.Sprintf("the group of one of %s holds %s, read by %s, and %s, read by %s; the variables of a group are of one component, so give the group of each component its own name",
						name, members[0].Name, strings.Join(members[0].ReadBy, ", "), member.Name, strings.Join(member.ReadBy, ", "))}
			}
		}
	}
	return nil
}

// conflict gives the first fact on which two statements of one variable differ, or "".
func conflict(a, b AcceptedVariable) string {
	switch {
	case a.Type != b.Type:
		return "type"
	case a.Required != b.Required:
		return "required flag"
	case a.Secret != b.Secret:
		return "secret flag"
	case (a.Default == nil) != (b.Default == nil) || (a.Default != nil && *a.Default != *b.Default):
		return "default"
	case a.Allowed != b.Allowed:
		return "allowed values"
	case a.SecretSource != nil && b.SecretSource != nil && *a.SecretSource != *b.SecretSource:
		return "secret source"
	case a.RefusedAtStart != b.RefusedAtStart:
		return "refused at start flag"
	case a.ReadByMode != b.ReadByMode:
		return "mode that reads it"
	case a.OneOf != b.OneOf:
		return "group of one of"
	}
	return ""
}

// acceptedOf fills the name and a setting default of one variable and checks the entry it gives. A secret variable
// keeps no default, no example and no allowed values (ST-DLV-INV12, ST-DLV-UC01-R08).
func acceptedOf(service string, settings map[string]string, variable Variable) (AcceptedVariable, error) {
	name, err := fillName(strings.ToUpper(strings.ReplaceAll(service, "-", "_")), settings, variable.Name)
	if err != nil {
		return AcceptedVariable{}, err
	}
	accepted := AcceptedVariable{Name: name, Type: variable.Type, Required: variable.Required, Description: variable.Description,
		Secret: variable.Secret, Allowed: variable.Allowed, RefusedAtStart: variable.RefusedAtStart,
		ReadByMode: variable.ReadByMode, OneOf: variable.OneOf, requiredWhen: variable.RequiredWhen}
	if source := variable.SecretSource; source != nil {
		if !variable.Secret && variable.Type != "path" {
			return AcceptedVariable{}, fmt.Errorf("only a secret variable, or a path variable that names the file of a secret, states a secret source")
		}
		secretName, err := fillName(service, settings, source.Name)
		if err != nil {
			return AcceptedVariable{}, fmt.Errorf("the secret source: %w", err)
		}
		accepted.SecretSource = &SecretSource{Name: secretName, Key: source.Key}
	}
	if !variable.Secret {
		accepted.example = variable.Example
		if variable.Default != nil {
			value := *variable.Default
			if match := settingDefault.FindStringSubmatch(value); match != nil {
				given, ok := settings[match[1]]
				if !ok || given == "" {
					// A setting that the declaration does not give leaves the variable with no default.
					value = ""
				} else {
					value = given
				}
			}
			if value != "" {
				accepted.Default = &value
			}
		}
	} else if variable.Default != nil || variable.Example != nil {
		return AcceptedVariable{}, fmt.Errorf("the variable is secret and states a default or an example; a secret variable has none")
	}
	if variable.Secret {
		accepted.Allowed = ""
	}
	if err := checkAccepted(accepted); err != nil {
		return AcceptedVariable{}, err
	}
	return accepted, nil
}

// checkAccepted holds one entry of the list to its rules: a variable name, a known type, a description of one or
// two sentences, no default for a required or a secret variable, and no allowed values for a secret one.
func checkAccepted(v AcceptedVariable) error {
	if !variableName.MatchString(v.Name) {
		return fmt.Errorf("the name %q is not a variable name of capital letters, digits and '_' of at most 128 characters", v.Name)
	}
	if !slices.Contains(variableTypes, v.Type) {
		return fmt.Errorf("the type %q is not one of %s", v.Type, strings.Join(variableTypes, ", "))
	}
	if err := checkDescription(v.Description); err != nil {
		return err
	}
	if v.Default != nil && (v.Required || v.Secret) {
		return fmt.Errorf("a required or a secret variable has no default")
	}
	if v.Default != nil && (len(*v.Default) > maxVariableValue || strings.ContainsAny(*v.Default, "\n\r")) {
		return fmt.Errorf("the default is not one line of at most %d characters", maxVariableValue)
	}
	if v.ReadByMode != "" && ((v.ReadByMode != MigrateMode && v.ReadByMode != WorkerMode && v.ReadByMode != APIMode) || v.Required) {
		return fmt.Errorf("the mode that reads the variable is migrate, worker or api, and such a variable is not required")
	}
	if v.OneOf != "" && !groupName.MatchString(v.OneOf) {
		return fmt.Errorf("the group of one of %q is not a name of at most 63 lower-case letters, digits and '-'", v.OneOf)
	}
	if v.OneOf != "" && v.Required {
		return fmt.Errorf("the variable is required and in the group of one of %s; a variable of a group is not required on its own, because the group needs exactly one of its variables", v.OneOf)
	}
	if v.RefusedAtStart && v.Required {
		return fmt.Errorf("a variable that the service refuses at start is not required")
	}
	if v.Secret && v.Allowed != "" {
		return fmt.Errorf("a secret variable lists no allowed values")
	}
	if len(v.Allowed) > maxVariableValue || strings.ContainsAny(v.Allowed, "\n\r") {
		return fmt.Errorf("the allowed values are not one line of at most %d characters", maxVariableValue)
	}
	return nil
}

// checkDescription holds a description to one or two sentences of at most 200 characters on one line (ST-VO64-C01).
func checkDescription(text string) error {
	switch {
	case strings.TrimSpace(text) == "" || strings.TrimSpace(text) != text:
		return fmt.Errorf("the description is empty or starts or ends with a space")
	case utf8.RuneCountInString(text) > 200:
		return fmt.Errorf("the description has more than 200 characters")
	case strings.ContainsAny(text, "\n\r\t\v\f  "):
		return fmt.Errorf("the description holds a line break")
	case !strings.HasSuffix(text, "."):
		return fmt.Errorf("the description does not end with a full stop")
	case len(sentenceStop.FindAllStringIndex(text, -1)) > 2:
		return fmt.Errorf("the description has more than two sentences")
	case strings.Contains(text, "://") || strings.Contains(strings.ToLower(text), "www."):
		return fmt.Errorf("the description holds a URL")
	}
	return nil
}

// configurationNeeds gives one configuration requirement for each required variable of the list that is not secret,
// with the components that read it (ST-DLV-INV11): the environment file must give each such variable its value.
func configurationNeeds(list []AcceptedVariable) []Requirement {
	var needs []Requirement
	for _, variable := range list {
		if !variable.Required || variable.Secret || variable.RefusedAtStart {
			continue
		}
		components := make([]string, 0, len(variable.ReadBy))
		for _, name := range variable.ReadBy {
			component, _, _ := strings.Cut(name, " (")
			if !slices.Contains(components, component) {
				components = append(components, component)
			}
		}
		needs = append(needs, Requirement{Kind: Configuration, Name: variable.Name, Components: components})
	}
	return needs
}
