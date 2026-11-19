package registry

import (
	"fmt"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The environment variables of a component (ST-SUP-EN14, ST-SUP-UC01-R08 to R10, owner, 2026-11-14): each component
// declares every environment variable that its emitted code or its runtime provider reads, with the facts that the
// generated reference lists, so that no variable is read that the platform does not know of.

// VariableType is the kind of value that an environment variable holds (ST-VO63).
type VariableType string

// VariableTypes are the known types of ST-VO63-C01.
var VariableTypes = []VariableType{"string", "integer", "number", "duration", "boolean", "url", "path", "address", "list"}

// ParseVariableType accepts a variable type only when it is one of the known types (ST-VO63-C01).
func ParseVariableType(text string) (VariableType, error) {
	if !slices.Contains(VariableTypes, VariableType(text)) {
		names := make([]string, len(VariableTypes))
		for i, known := range VariableTypes {
			names[i] = string(known)
		}
		return "", fmt.Errorf("the type %q is not one of %s", text, strings.Join(names, ", "))
	}
	return VariableType(text), nil
}

// VariableDescription is what an environment variable controls, in one or two sentences of ASD-STE100 Simplified
// Technical English (ST-VO64).
type VariableDescription string

// MaxVariableDescription is the most characters that a variable description may have (ST-VO64-C01).
const MaxVariableDescription = 200

// sentenceEnd is the end of one sentence: a full stop at the end of the text or before a space.
var sentenceEnd = regexp.MustCompile(`\.( |$)`)

// ParseVariableDescription accepts a description only when it is one or two sentences, each ending with a full
// stop, of at most MaxVariableDescription characters, with no line break and not empty (ST-VO64-C01).
func ParseVariableDescription(text string) (VariableDescription, error) {
	switch {
	case strings.TrimSpace(text) == "":
		return "", fmt.Errorf("the description is empty")
	case utf8.RuneCountInString(text) > MaxVariableDescription:
		return "", fmt.Errorf("the description has %d characters, more than %d", utf8.RuneCountInString(text), MaxVariableDescription)
	case strings.ContainsAny(text, "\n\r\t\v\f  "):
		return "", fmt.Errorf("the description holds a line break; give it on one line")
	case strings.TrimSpace(text) != text || !strings.HasSuffix(text, "."):
		return "", fmt.Errorf("the description does not end with a full stop")
	case strings.Contains(text, "://") || strings.Contains(strings.ToLower(text), "www."):
		// A link in the generated reference could point a reader anywhere (#344 security review L4).
		return "", fmt.Errorf("the description holds a URL; name the setting that gives the address instead")
	}
	if count := len(sentenceEnd.FindAllStringIndex(text, -1)); count > 2 {
		return "", fmt.Errorf("the description has %d sentences; give one or two", count)
	}
	return VariableDescription(text), nil
}

// EnvironmentVariable is one environment variable that a component declares (ST-SUP-EN14). Required and Secret are
// pointers so that the decoder can tell a fact left out from a fact stated as false.
type EnvironmentVariable struct {
	// Name is fixed text, or a pattern of fixed text with {service} and {settings.<key>}.
	Name        string  `yaml:"name"`
	Type        string  `yaml:"type"`
	Required    *bool   `yaml:"required"`
	Default     *string `yaml:"default,omitempty"`
	Description string  `yaml:"description"`
	Secret      *bool   `yaml:"secret"`
	// Example is one value the reader can copy, for a variable that is not secret; only the reference file shows it.
	Example *string        `yaml:"example,omitempty"`
	Allowed *AllowedValues `yaml:"allowed,omitempty"`
	// SecretSource is the secret need and the key that give the value of a secret variable, or the file of a path
	// variable (ST-SUP-EN14 field Secret source, ST-DLV-Q19), so that the Kubernetes base can name them.
	SecretSource *SecretSource `yaml:"secret_source,omitempty"`
	// ReadByMode is the one mode of the process that reads the variable, when no other mode reads it: migrate, the
	// migration job (ST-SUP-EN14 field Read by mode, ST-DLV-Q19); worker, the worker mode, which the Deployment of
	// the api mode does not wire; or api, the api mode, which the Deployment of the worker modes and the migration Job
	// do not wire (ST-DLV-UC07-R06, ST-DLV-UC07-R07).
	ReadByMode string `yaml:"read_by_mode,omitempty"`
	// OneOf is the group of variables of the component of which the process needs exactly one value, such as
	// DATABASE_URL and DATABASE_URL_FILE (ST-SUP-EN14 field One of, ST-DLV-Q09). A group has at least two variables
	// of the component, and none of them is required on its own (ST-SUP-INV24).
	OneOf string `yaml:"one_of,omitempty"`
	// ReadWith is the package of the runtime library that reads the variable, as its path under the runtime module,
	// such as service, when only that package reads it (ST-SUP-EN14 field Read with, ST-SUP-Q161). The platform
	// requirements, the reference and the Kubernetes base list the variable only when the generated service imports
	// that package.
	ReadWith string `yaml:"read_with,omitempty"`
	// RequiredWhen is, for a variable that is not required on its own, the condition in a short phrase under which the
	// service that lists it does not start without a value, such as "the service has a secret or a pii field"
	// (ST-SUP-EN14 field Required when; walkthrough 14). Only the environment variable reference shows it.
	RequiredWhen string `yaml:"required_when,omitempty"`
}

// ReadByModes are the modes that a variable may state as its one reader: the migration job, the worker mode and
// the api mode.
var ReadByModes = []string{"migrate", "worker", "api"}

// SecretSource is one key of one secret need: the need name, which may hold the placeholders of a need name, and
// the key.
type SecretSource struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// IsRequired tells whether the component fails to start when the variable has no value.
func (v EnvironmentVariable) IsRequired() bool { return v.Required != nil && *v.Required }

// IsSecret tells whether the value of the variable is a secret.
func (v EnvironmentVariable) IsSecret() bool { return v.Secret != nil && *v.Secret }

// AllowedValues are the values a component accepts for a variable: a list of words, or an inclusive range with its
// unit.
type AllowedValues struct {
	Values []string `yaml:"values,omitempty"`
	Min    *string  `yaml:"min,omitempty"`
	Max    *string  `yaml:"max,omitempty"`
	Unit   string   `yaml:"unit,omitempty"`
}

// Text gives the allowed values as one text for the reference and the platform requirements: the words in their
// order, or the range with its unit, such as "1s to 30m" or "1000 to 60000 milliseconds".
func (a *AllowedValues) Text() string {
	if a == nil {
		return ""
	}
	if len(a.Values) > 0 {
		return strings.Join(a.Values, ", ")
	}
	text := ""
	switch {
	case a.Min != nil && a.Max != nil:
		text = *a.Min + " to " + *a.Max
	case a.Min != nil:
		text = "at least " + *a.Min
	case a.Max != nil:
		text = "at most " + *a.Max
	}
	if a.Unit != "" {
		text += " " + a.Unit
	}
	return text
}

// VariableError is an environment variable of a component that the registry refuses: the component, the variable
// as the component states it, the fact at fault and why (ST-SUP-UC01-R09).
type VariableError struct {
	Component string
	Variable  string
	Fact      string
	Reason    string
}

func (e *VariableError) Error() string {
	if e.Variable == "" {
		return fmt.Sprintf("component %s: %s: %s", e.Component, e.Fact, e.Reason)
	}
	return fmt.Sprintf("component %s: environment variable %s: %s: %s", e.Component, e.Variable, e.Fact, e.Reason)
}

var (
	// variableText is the fixed text of a variable name.
	variableText = regexp.MustCompile(`^[A-Z0-9_]+$`)
	// VariableName is a resolved variable name.
	VariableName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	// settingsPlaceholder is a default that is a setting of the component.
	settingsPlaceholder = regexp.MustCompile(`^\{settings\.([a-z][a-z0-9_]*)\}$`)
	// rangeUnit is the unit of a range.
	rangeUnit = regexp.MustCompile(`^[a-z]+( [a-z]+)?$`)
)

// maxVariableValue bounds a default, an example and an allowed value. Delivering holds the same bound.
const maxVariableValue = 512

// declaresVariables is the rule of ST-SUP-INV22 that the registry holds: a component states its environment list,
// and each variable in it states its name, its type, whether it is required, its description and whether it is
// secret, each once. The release scan proves the other half, that the code reads no variable that is not declared.
func declaresVariables(component *Component) error {
	if component.Environment == nil {
		return &VariableError{Component: component.Name, Fact: "environment",
			Reason: "the component states no environment list; give each environment variable that its code reads, or environment: [] when it reads none"}
	}
	seen := map[string]bool{}
	for _, variable := range *component.Environment {
		fault := func(fact, format string, args ...any) error {
			return &VariableError{Component: component.Name, Variable: variableLabel(variable.Name), Fact: fact, Reason: fmt.Sprintf(format, args...)}
		}
		if err := checkVariableName(component, variable.Name); err != nil {
			return fault("name", "%s", err)
		}
		if seen[variable.Name] {
			return fault("name", "the component declares the variable twice")
		}
		seen[variable.Name] = true
		if variable.Type == "" {
			return fault("type", "the variable states no type; give one of %s", typeNames())
		}
		if _, err := ParseVariableType(variable.Type); err != nil {
			return fault("type", "%s", err)
		}
		if variable.Required == nil {
			return fault("required", "the variable does not state whether it is required; give required: true or false")
		}
		if variable.Secret == nil {
			return fault("secret", "the variable does not state whether its value is secret; give secret: true or false")
		}
		if _, err := ParseVariableDescription(variable.Description); err != nil {
			return fault("description", "%s; give one or two sentences of ASD-STE100 Simplified Technical English", err)
		}
		if variable.ReadByMode != "" && !slices.Contains(ReadByModes, variable.ReadByMode) {
			return fault("read_by_mode", "the mode %q is not one of %s", variable.ReadByMode, strings.Join(ReadByModes, ", "))
		}
		if variable.ReadByMode != "" && variable.IsRequired() {
			return fault("read_by_mode", "a variable that one mode reads is not required by the service")
		}
		if variable.ReadWith != "" && !runtimePackagePath.MatchString(variable.ReadWith) {
			return fault("read_with", "%q is not the path of a package under the runtime module, such as service or outbox/kafkaoutbox", variable.ReadWith)
		}
		if variable.RequiredWhen != "" && (variable.IsRequired() || !requiredWhen.MatchString(variable.RequiredWhen)) {
			return fault("required_when", "a condition of required when is a phrase of at most 120 lower-case letters, digits, spaces and the characters -_., of a variable that is not required on its own")
		}
		if variable.OneOf != "" && !groupName.MatchString(variable.OneOf) {
			return fault("one_of", "the group %q is not a name of at most 63 lower-case letters, digits and '-', such as database-url", variable.OneOf)
		}
		if variable.OneOf != "" && variable.IsRequired() {
			return fault("one_of", "the variable is required and in the group %s; a variable of a group is not required on its own, because the group needs exactly one of its variables: give required: false", variable.OneOf)
		}
		if source := variable.SecretSource; source != nil {
			if !variable.IsSecret() && variable.Type != "path" {
				return fault("secret_source", "only a secret variable, or a path variable that names the file of a secret, states a secret source")
			}
			if err := checkNeedName(component, source.Name); err != nil {
				return fault("secret_source", "%s", err)
			}
			if len(source.Key) > maxNeedWord || !needWord.MatchString(source.Key) {
				return fault("secret_source", "the key %q is not a key name of at most %d letters, digits, '-' or '_'", source.Key, maxNeedWord)
			}
		}
	}
	return checkGroups(component)
}

// runtimePackagePath is the path of a package under the runtime module: lower-case words of letters and digits,
// joined by '/'.
var runtimePackagePath = regexp.MustCompile(`^[a-z][a-z0-9]*(/[a-z][a-z0-9]*)*$`)

// checkReadWith refuses a variable whose Read with names no package of the runtime library that holds Go code, when
// the registry holds the runtime folder (ST-SUP-INV24, ST-SUP-Q161). A registry without that folder takes the runtime
// from its pin, and the form check of declaresVariables holds alone.
func (r *Registry) checkReadWith() error {
	folder := r.RuntimeFolder()
	if folder == "" {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(r.components)) {
		for _, variable := range r.components[name].Variables() {
			if variable.ReadWith == "" || holdsGoCode(filepath.Join(folder, filepath.FromSlash(variable.ReadWith))) {
				continue
			}
			return &VariableError{Component: name, Variable: variableLabel(variable.Name), Fact: "read_with",
				Reason: fmt.Sprintf("the runtime library has no package %s that holds Go code", variable.ReadWith)}
		}
	}
	return nil
}

// holdsGoCode tells whether the folder holds a Go file that is not a test file.
func holdsGoCode(folder string) bool {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			return true
		}
	}
	return false
}

// groupName is the name of a group of one of: lower-case letters, digits and '-'.
var groupName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// requiredWhen is the form of the condition of a variable that is required when it holds: a short phrase, such as
// "the service has a secret or a pii field".
var requiredWhen = regexp.MustCompile(`^[a-z0-9][a-z0-9 _.,-]{0,119}$`)

// checkGroups holds each group of one of of a component to at least two variables (ST-SUP-INV24, ST-DLV-Q09). The
// group is the component's own: the platform requirements refuse one group name that two components state.
func checkGroups(component *Component) error {
	members := map[string][]string{}
	for _, variable := range *component.Environment {
		if variable.OneOf != "" {
			members[variable.OneOf] = append(members[variable.OneOf], variable.Name)
		}
	}
	for _, group := range slices.Sorted(maps.Keys(members)) {
		if len(members[group]) < 2 {
			return &VariableError{Component: component.Name, Variable: variableLabel(members[group][0]), Fact: "one_of",
				Reason: fmt.Sprintf("the variable is the only one of the group %s; a group has at least two variables of the component, such as X and X_FILE, so give the other one the same one_of or remove one_of", group)}
		}
	}
	return nil
}

// checkSecretSources holds each secret source of each component to a secret need that a component of the registry
// states with that key (ST-DLV-Q19). The release test runs it on the shipped registry. Load does not run it,
// because a registry copy may leave out the component of a need; apply then wires no variable from that need, and
// refuses a source whose need the service states without that key.
func checkSecretSources(r *Registry) error {
	keys := map[string][]string{}
	for _, component := range r.components {
		for _, need := range component.Needs {
			if need.Kind == "secret" {
				keys[need.Name] = append(keys[need.Name], need.Keys...)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(r.components)) {
		for _, variable := range r.components[name].Variables() {
			if source := variable.SecretSource; source != nil && !slices.Contains(keys[source.Name], source.Key) {
				return &VariableError{Component: name, Variable: variable.Name, Fact: "secret_source",
					Reason: fmt.Sprintf("no component of the registry states the secret need %s with the key %s", source.Name, source.Key)}
			}
		}
	}
	return nil
}

// variableLabel names a variable in a message, with the name quoted when it is not a plain name.
func variableLabel(name string) string {
	if name != "" && VariableName.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}

func typeNames() string {
	names := make([]string, len(VariableTypes))
	for i, known := range VariableTypes {
		names[i] = string(known)
	}
	return strings.Join(names, ", ")
}

// checkVariableName holds a variable name to fixed text of capital letters, digits and '_', and the placeholders
// {service} and {settings.<key>}, where key is a text setting of the component that is not secret and whose
// allowed values are names alone (ST-SUP-EN14 field Name, ST-SUP-INV23).
func checkVariableName(component *Component, name string) error {
	if name == "" {
		return fmt.Errorf("the variable states no name")
	}
	if len(name) > 128 {
		return fmt.Errorf("the name is longer than 128 characters")
	}
	for _, placeholder := range needPlaceholder.FindAllString(name, -1) {
		inner := strings.Trim(placeholder, "{}")
		if inner == "service" {
			continue
		}
		key, isSetting := strings.CutPrefix(inner, "settings.")
		setting, declared := component.Config[key]
		if !isSetting || !declared {
			return fmt.Errorf("the placeholder %s is not {service} or {settings.<key>} of a setting of the component", placeholder)
		}
		if setting.Secret {
			return fmt.Errorf("the placeholder %s names the secret setting %s; a name never holds a secret", placeholder, key)
		}
		if setting.Type != "string" || len(setting.Enum) == 0 || slices.ContainsFunc(setting.Enum, func(value string) bool { return !variableText.MatchString(value) }) {
			return fmt.Errorf("the placeholder %s names the setting %s, which does not allow only capital letters, digits and '_'; give it an enum of such names", placeholder, key)
		}
	}
	rest := needPlaceholder.ReplaceAllString(name, "")
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("the name holds a brace that opens no placeholder")
	}
	for _, text := range needPlaceholder.Split(name, -1) {
		if text != "" && !variableText.MatchString(text) {
			return fmt.Errorf("the fixed text %q of the name holds a character other than a capital letter, a digit or '_'", text)
		}
	}
	if first := needPlaceholder.ReplaceAllString(name, "A"); !VariableName.MatchString(first) {
		return fmt.Errorf("the name does not start with a capital letter or a placeholder")
	}
	return nil
}

// secretHasNoDefault is the rule of ST-SUP-INV23: a secret variable or setting states no default and no example,
// and no name of a variable or of a need holds a secret setting (the names are held by checkVariableName and
// checkNeedName).
func secretHasNoDefault(component *Component) error {
	for _, key := range sortedSettings(component) {
		if setting := component.Config[key]; setting.Secret && setting.Default != nil {
			return &VariableError{Component: component.Name, Variable: "(setting " + key + ")", Fact: "default",
				Reason: "the setting is secret and states a default; a secret has no default"}
		}
	}
	for _, variable := range *component.Environment {
		if !variable.IsSecret() {
			if variable.Default != nil {
				if key, isSetting := settingOf(*variable.Default); isSetting && component.Config[key].Secret {
					return &VariableError{Component: component.Name, Variable: variable.Name, Fact: "default",
						Reason: fmt.Sprintf("the default is the secret setting %s; a default never holds a secret", key)}
				}
			}
			continue
		}
		if variable.Default != nil {
			return &VariableError{Component: component.Name, Variable: variable.Name, Fact: "default", Reason: "the variable is secret and states a default; a secret variable has no default"}
		}
		if variable.Example != nil {
			return &VariableError{Component: component.Name, Variable: variable.Name, Fact: "example", Reason: "the variable is secret and states an example; a secret variable has no example"}
		}
	}
	return nil
}

// sortedSettings gives the setting keys of a component in byte order.
func sortedSettings(component *Component) []string {
	keys := make([]string, 0, len(component.Config))
	for key := range component.Config {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// settingOf tells whether a default is {settings.<key>}, and gives the key.
func settingOf(value string) (string, bool) {
	match := settingsPlaceholder.FindStringSubmatch(value)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// factsAgree is the rule of ST-SUP-INV24: a required variable states no default, and a default or an example is of
// the variable's type and inside its allowed values. A default that is {settings.<key>} names a setting of the
// component that is not secret; the compiler checks its value against the type and the allowed values when apply
// resolves it (compiler.requirementsFiles, #344 security review L3).
func factsAgree(component *Component) error {
	for _, variable := range *component.Environment {
		fault := func(fact, format string, args ...any) error {
			return &VariableError{Component: component.Name, Variable: variable.Name, Fact: fact, Reason: fmt.Sprintf(format, args...)}
		}
		kind := VariableType(variable.Type)
		if variable.IsRequired() && variable.Default != nil {
			return fault("default", "the variable is required and states a default; a required variable has no default")
		}
		if err := checkAllowed(kind, variable.Allowed); err != nil {
			return fault("allowed", "%s", err)
		}
		for _, value := range []struct {
			fact  string
			value *string
		}{{"default", variable.Default}, {"example", variable.Example}} {
			if value.value == nil {
				continue
			}
			if key, isSetting := settingOf(*value.value); isSetting {
				if value.fact == "example" {
					return fault("example", "an example is fixed text, not a setting")
				}
				if _, declared := component.Config[key]; !declared {
					return fault("default", "the default names the setting %s, which the component does not have", key)
				}
				continue
			}
			if err := CheckVariableValue(kind, variable.Allowed, *value.value); err != nil {
				return fault(value.fact, "%s", err)
			}
		}
	}
	return nil
}

// checkAllowed holds the allowed values to one form: a list of distinct words, or a range of the variable's type for
// an integer, a number or a duration.
func checkAllowed(kind VariableType, allowed *AllowedValues) error {
	if allowed == nil {
		return nil
	}
	ranged := allowed.Min != nil || allowed.Max != nil
	switch {
	case len(allowed.Values) > 0 && ranged:
		return fmt.Errorf("the allowed values give both a list and a range; give one")
	case len(allowed.Values) == 0 && !ranged:
		return fmt.Errorf("the allowed values give neither a list nor a range")
	case allowed.Unit != "" && !ranged:
		return fmt.Errorf("a list of allowed values has no unit")
	case allowed.Unit != "" && !rangeUnit.MatchString(allowed.Unit):
		return fmt.Errorf("the unit %q is not one or two lower-case words", allowed.Unit)
	}
	for i, value := range allowed.Values {
		if value == "" || len(value) > maxVariableValue || strings.ContainsAny(value, ",\n\r") {
			return fmt.Errorf("the allowed value %q is not one word of at most %d characters", value, maxVariableValue)
		}
		if slices.Contains(allowed.Values[:i], value) {
			return fmt.Errorf("the allowed value %s is stated twice", value)
		}
		if kind != "list" {
			if err := checkType(kind, value); err != nil {
				return fmt.Errorf("the allowed value %s: %w", value, err)
			}
		}
	}
	if !ranged {
		return nil
	}
	if kind != "integer" && kind != "number" && kind != "duration" {
		return fmt.Errorf("a %s variable has no range; give a list of values", kind)
	}
	var bounds []float64
	for _, bound := range []*string{allowed.Min, allowed.Max} {
		if bound == nil {
			continue
		}
		number, err := numeric(kind, *bound)
		if err != nil {
			return fmt.Errorf("the bound %q: %w", *bound, err)
		}
		bounds = append(bounds, number)
	}
	if len(bounds) == 2 && bounds[0] > bounds[1] {
		return fmt.Errorf("the range starts at %s, after its end %s", *allowed.Min, *allowed.Max)
	}
	return nil
}

// CheckVariableValue tells whether a value is of the variable's type and inside its allowed values, and why not.
// It never puts the value in its message, because the caller may hold a value that it must not show.
func CheckVariableValue(kind VariableType, allowed *AllowedValues, value string) error {
	if len(value) > maxVariableValue || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("the value is not one line of at most %d characters", maxVariableValue)
	}
	if err := checkType(kind, value); err != nil {
		return err
	}
	if allowed == nil {
		return nil
	}
	if len(allowed.Values) > 0 {
		items := []string{value}
		if kind == "list" {
			items = strings.Split(value, ",")
		}
		for _, item := range items {
			if !slices.Contains(allowed.Values, strings.TrimSpace(item)) {
				return fmt.Errorf("the value is not one of the allowed values %s", allowed.Text())
			}
		}
		return nil
	}
	number, _ := numeric(kind, value)
	if allowed.Min != nil {
		if minimum, _ := numeric(kind, *allowed.Min); number < minimum {
			return fmt.Errorf("the value is less than %s", *allowed.Min)
		}
	}
	if allowed.Max != nil {
		if maximum, _ := numeric(kind, *allowed.Max); number > maximum {
			return fmt.Errorf("the value is more than %s", *allowed.Max)
		}
	}
	return nil
}

// numeric gives an integer, a number or a duration as a number, to compare it with a range.
func numeric(kind VariableType, value string) (float64, error) {
	switch kind {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		return float64(n), err
	case "number":
		return strconv.ParseFloat(value, 64)
	case "duration":
		d, err := time.ParseDuration(value)
		return float64(d), err
	}
	return 0, fmt.Errorf("a %s has no range", kind)
}

// checkType tells whether a value is of a variable type.
func checkType(kind VariableType, value string) error {
	switch kind {
	case "string":
		return nil
	case "integer":
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("the value is not an integer")
		}
	case "number":
		if n, err := strconv.ParseFloat(value, 64); err != nil || n != n || math.IsInf(n, 0) {
			return fmt.Errorf("the value is not a finite number")
		}
	case "duration":
		if d, err := time.ParseDuration(value); err != nil || d < 0 {
			return fmt.Errorf("the value is not a duration of zero or more, such as 30s or 5m")
		}
	case "boolean":
		if !slices.Contains([]string{"true", "false", "0", "1"}, value) {
			return fmt.Errorf("the value is not true, false, 0 or 1")
		}
	case "url":
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("the value is not an absolute URL")
		}
		// A URL with user info holds a credential in a value that is not secret (#353 security review L1).
		if u.User != nil {
			return fmt.Errorf("the value holds user info; give the credential as a secret")
		}
	case "path":
		if !path.IsAbs(value) {
			return fmt.Errorf("the value is not an absolute file path")
		}
	case "address":
		host, port, err := net.SplitHostPort(value)
		number, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || number < 1 || number > 65535 || strings.ContainsAny(host, " /@") {
			return fmt.Errorf("the value is not an address of a host and a port")
		}
	case "list":
		for _, item := range strings.Split(value, ",") {
			if strings.TrimSpace(item) == "" {
				return fmt.Errorf("the value is not a list of comma-separated items")
			}
		}
	default:
		return fmt.Errorf("the type %q is not known", kind)
	}
	return nil
}

// checkEnvironment holds the environment list of a component to ST-SUP-INV22 to INV24 and refuses a configuration
// need, which the platform requirements derive from the variables instead (ST-SUP-EN09 field Platform needs).
func checkEnvironment(component *Component) error {
	if err := declaresVariables(component); err != nil {
		return err
	}
	if err := secretHasNoDefault(component); err != nil {
		return err
	}
	if err := factsAgree(component); err != nil {
		return err
	}
	for _, need := range component.Needs {
		if need.Kind == "configuration" {
			return &NeedError{Component: component.Name, Need: need.Name, Field: "kind",
				Reason: "a component states no configuration need; declare the environment variable, and the platform requirements derive the need from it"}
		}
	}
	return nil
}

// Variables gives the environment list of a component, empty when it states none.
func (c *Component) Variables() []EnvironmentVariable {
	if c.Environment == nil {
		return nil
	}
	return *c.Environment
}
