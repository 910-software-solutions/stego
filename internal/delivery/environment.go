package delivery

import (
	"fmt"
	"maps"
	"math"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvironmentVersion is the version of the environment schema that STEGO owns (ST-DLV-TM02, ST-DLV-Q05 (A)).
const EnvironmentVersion = 1

// MaxEnvironmentBytes bounds the environment file that CheckEnvironment reads.
const MaxEnvironmentBytes = 1 << 20

// FitStatus is how the environment file meets one platform requirement: met, missing or wrong type.
type FitStatus string

// The statuses of a row of the environment fit.
const (
	Met       FitStatus = "met"
	Missing   FitStatus = "missing"
	WrongType FitStatus = "wrong type"
)

// FitRow is one row of the environment fit (ST-DLV-RM01): a requirement or a value of the environment file by its
// kind and name, its status and what is missing or wrong. It never holds a secret value.
type FitRow struct {
	Requirement string
	Status      FitStatus
	Detail      string
}

// EnvironmentFit is the environment fit of one service and one environment file (ST-DLV-RM01-TB): each platform
// requirement with its status, each value of a wrong type, and the values that no requirement names. A run builds
// it in memory and keeps nothing.
type EnvironmentFit struct {
	Service     string
	Environment string
	Rows        []FitRow
	// Unused are the values of the environment file that no platform requirement and no listed environment variable
	// names, by kind and name.
	Unused []string
	// Notices are the grants of the environment file that are wider than a requirement asks for, a less strict pod
	// security level or extra cluster verbs, and the values of the *_FILE form of a variable with a secret source,
	// which the base never wires. They do not fail the check.
	Notices []string
}

// Met tells whether the environment file meets every platform requirement and every value has its type.
func (f EnvironmentFit) Met() bool {
	for _, row := range f.Rows {
		if row.Status != Met {
			return false
		}
	}
	return true
}

// EnvironmentError is an environment file that cannot be read or does not have the shape of the environment
// schema: the line, the path of the field at fault, if one, and why.
type EnvironmentError struct {
	Line   int
	Field  string
	Reason string
}

func (e *EnvironmentError) Error() string {
	field := ""
	if e.Field != "" {
		field = " field " + e.Field + ":"
	}
	return fmt.Sprintf("line %d:%s %s", e.Line, field, e.Reason)
}

// environmentSections are the sections of the environment file, one for each requirement kind, in the order of the
// requirement kinds.
var environmentSections = []struct {
	name string
	kind RequirementKind
	// fields are the fields of an entry of the section; nil for a section whose entries are not maps.
	fields []string
}{
	{"secrets", Secret, nil},
	{"configuration", Configuration, nil},
	{"backing_stores", BackingStore, []string{"engine"}},
	{"topics", Topic, []string{"topic"}},
	{"identity_issuers", IdentityIssuer, []string{"issuer"}},
	{"ingresses", Ingress, []string{"port", "host"}},
	{"egresses", Egress, []string{"address", "port"}},
	{"cluster_permissions", ClusterPermission, []string{"api_group", "resource", "verbs"}},
	{"pod_security", PodSecurity, []string{"level"}},
}

// environmentEntry is one value of the environment file, under its kind and name. A secret entry holds its key
// names only: the decoder never keeps or looks at a secret value.
type environmentEntry struct {
	kind RequirementKind
	name string
	line int
	// keys are the key names of a secret.
	keys []string
	// fields are the fields of an entry of a map section, by name.
	fields map[string]*yaml.Node
	// fault is why the entry does not have the type of its kind, or "".
	fault string
	// empty marks a configuration value that is null or empty: it gives no value.
	empty bool
	// blank marks a configuration value that is given but is white space only: the check counts it as no value,
	// but the base gives the text as it is to the service, which reads it raw (#380 security review L2).
	blank bool
	// value is the text of a configuration value, which is never secret: a secret variable given as a configuration
	// value is of the wrong type, and the check names it without its value.
	value string
}

// environment is the environment file as the check reads it.
type environment struct {
	service     string
	environment string
	image       string
	imageLine   int
	entries     []*environmentEntry
}

// lookup gives the entry of the kind with the name, or nil.
func (e environment) lookup(kind RequirementKind, name string) *environmentEntry {
	for _, entry := range e.entries {
		if entry.kind == kind && entry.name == name {
			return entry
		}
	}
	return nil
}

// imageDigest is an image named by its repository and a sha256 digest.
var imageDigest = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`)

// readEnvironment reads an environment file through the strict decoder: one document of at most
// MaxEnvironmentBytes, with no anchor, alias or merge key, of schema version 1, with no field the schema does not
// have. It never keeps a secret value. A value of a wrong type inside an entry is kept as the entry's fault, for the
// check to name; a wrong shape above the entries is an EnvironmentError.
func readEnvironment(data []byte) (environment, error) {
	var document yaml.Node
	if err := decodeStrict(data, MaxEnvironmentBytes, &document); err != nil {
		return environment{}, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return environment{}, &EnvironmentError{Line: document.Line, Reason: "the file is not a map of the fields of the environment schema; stego env check --help and docs/environment-file.md give the schema"}
	}
	top, err := fieldsOf(document.Content[0], "")
	if err != nil {
		return environment{}, err
	}
	known := []string{"version", "service", "environment", "image", "destination"}
	for _, section := range environmentSections {
		known = append(known, section.name)
	}
	if err := onlyFields(top, known, ""); err != nil {
		return environment{}, err
	}
	for _, required := range []string{"version", "service", "environment", "image", "destination"} {
		if top.nodes[required] == nil {
			return environment{}, &EnvironmentError{Line: document.Content[0].Line, Field: required, Reason: "the field is missing"}
		}
	}
	version := top.nodes["version"]
	if version.Kind != yaml.ScalarNode || version.Tag != "!!int" {
		return environment{}, &EnvironmentError{Line: version.Line, Field: "version", Reason: "the schema version is a number"}
	}
	if number, _ := strconv.Atoi(version.Value); number != EnvironmentVersion {
		return environment{}, &EnvironmentError{Line: version.Line, Field: "version", Reason: fmt.Sprintf("the schema version is %s; this stego reads version %d", version.Value, EnvironmentVersion)}
	}
	result := environment{}
	for _, field := range []struct {
		name   string
		target *string
	}{{"service", &result.service}, {"environment", &result.environment}, {"image", &result.image}} {
		node := top.nodes[field.name]
		if !isText(node) || node.Value == "" {
			return environment{}, &EnvironmentError{Line: node.Line, Field: field.name, Reason: "the field is one text"}
		}
		*field.target = node.Value
	}
	result.imageLine = top.nodes["image"].Line
	destination := top.nodes["destination"]
	if destination.Kind != yaml.MappingNode {
		return environment{}, &EnvironmentError{Line: destination.Line, Field: "destination", Reason: "the destination is a map of cluster, namespace and overlay_path"}
	}
	parts, err := fieldsOf(destination, "destination")
	if err != nil {
		return environment{}, err
	}
	if err := onlyFields(parts, []string{"cluster", "namespace", "overlay_path"}, "destination"); err != nil {
		return environment{}, err
	}
	for _, part := range []string{"cluster", "namespace", "overlay_path"} {
		if node := parts.nodes[part]; node == nil || !isText(node) || node.Value == "" {
			return environment{}, &EnvironmentError{Line: destination.Line, Field: "destination." + part, Reason: "the field is one text"}
		}
	}
	for _, section := range environmentSections {
		node := top.nodes[section.name]
		if node == nil {
			continue
		}
		if node.Kind != yaml.MappingNode {
			return environment{}, &EnvironmentError{Line: node.Line, Field: section.name,
				Reason: fmt.Sprintf("the %s are a map of names to entries, not a %s", section.name, nodeKind(node))}
		}
		entries, err := fieldsOf(node, section.name)
		if err != nil {
			return environment{}, err
		}
		for _, name := range entries.order {
			entry, err := readEntry(section.name, section.kind, section.fields, name, entries.nodes[name])
			if err != nil {
				return environment{}, err
			}
			result.entries = append(result.entries, entry)
		}
	}
	return result, nil
}

// fields are the fields of a YAML map in their order, by name.
type fields struct {
	order []string
	nodes map[string]*yaml.Node
}

// fieldsOf reads the fields of a YAML map and refuses a key that is not one text and a key stated twice.
func fieldsOf(node *yaml.Node, path string) (fields, error) {
	result := fields{nodes: map[string]*yaml.Node{}}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if !isText(key) || key.Value == "" {
			return fields{}, &EnvironmentError{Line: key.Line, Field: path, Reason: "a key is not one text"}
		}
		if result.nodes[key.Value] != nil {
			return fields{}, &EnvironmentError{Line: key.Line, Field: join(path, key.Value), Reason: "the field is stated twice"}
		}
		result.order = append(result.order, key.Value)
		result.nodes[key.Value] = node.Content[i+1]
	}
	return result, nil
}

// onlyFields refuses a field that the environment schema does not have: an unknown shape.
func onlyFields(given fields, known []string, path string) error {
	for _, name := range given.order {
		if !slices.Contains(known, name) {
			return &EnvironmentError{Line: given.nodes[name].Line, Field: join(path, name),
				Reason: fmt.Sprintf("the environment schema has no such field; the fields are %s", strings.Join(known, ", "))}
		}
	}
	return nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func isText(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!str"
}

// readEntry reads one entry of a section. A secret is a map of its key names, whose values it skips. A configuration
// value is one value. Each other entry is a map of the fields of its kind.
func readEntry(section string, kind RequirementKind, known []string, name string, node *yaml.Node) (*environmentEntry, error) {
	entry := &environmentEntry{kind: kind, name: name, line: node.Line}
	path := section + "." + name
	switch kind {
	case Secret:
		if node.Kind != yaml.MappingNode {
			entry.fault = "a secret is a map of its key names"
			return entry, nil
		}
		keys, err := fieldsOf(node, path)
		if err != nil {
			return nil, err
		}
		// The values of the keys are not kept: the decoder sees only whether a key has a value. A key whose value is
		// null, white space only, an empty map or an empty list counts as missing.
		for _, key := range keys.order {
			if !emptyValue(keys.nodes[key]) {
				entry.keys = append(entry.keys, key)
			}
		}
		return entry, nil
	case Configuration:
		if node.Kind != yaml.ScalarNode {
			entry.fault = "a configuration value is one value, not a " + nodeKind(node)
		}
		entry.empty = emptyValue(node)
		entry.blank = node.Kind == yaml.ScalarNode && node.Tag != "!!null" && node.Value != "" && entry.empty
		if node.Kind == yaml.ScalarNode && !entry.empty {
			entry.value = node.Value
		}
		return entry, nil
	}
	if node.Kind != yaml.MappingNode {
		entry.fault = fmt.Sprintf("a %s is a map of the fields %s, not a %s", kind, strings.Join(known, ", "), nodeKind(node))
		return entry, nil
	}
	given, err := fieldsOf(node, path)
	if err != nil {
		return nil, err
	}
	if err := onlyFields(given, known, path); err != nil {
		return nil, err
	}
	entry.fields = given.nodes
	entry.fault = typeFault(given.nodes)
	return entry, nil
}

// emptyValue tells whether a value is null, a text of white space only, an empty map or an empty list, without
// keeping the value. Such a value gives the service nothing, so it counts as missing (#378 security review L3).
func emptyValue(node *yaml.Node) bool {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Tag == "!!null" || strings.TrimSpace(node.Value) == ""
	case yaml.MappingNode, yaml.SequenceNode:
		return len(node.Content) == 0
	}
	return false
}

// issuerFault gives why a value is not an issuer, an https URL with a host and no user, query or fragment, or "".
func issuerFault(value string) string {
	if u, err := url.Parse(value); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Sprintf("the issuer %s is not an https URL with a host and no user, query or fragment", value)
	}
	return ""
}

// typeFault gives why a field of an entry does not have the type that the environment schema states for it, or "".
func typeFault(given map[string]*yaml.Node) string {
	for _, name := range []string{"engine", "topic", "issuer", "host", "address", "api_group", "resource", "level"} {
		if node := given[name]; node != nil && !isText(node) {
			return fmt.Sprintf("the %s is one text", strings.ReplaceAll(name, "_", " "))
		}
	}
	for _, name := range []string{"engine", "topic", "issuer", "host", "address", "resource", "level"} {
		if node := given[name]; node != nil && node.Value == "" {
			return fmt.Sprintf("the %s is empty", name)
		}
	}
	if node := given["issuer"]; node != nil {
		if fault := issuerFault(node.Value); fault != "" {
			return fault
		}
	}
	for _, name := range []string{"host", "address"} {
		if node := given[name]; node != nil && node.Value != "" && (len(node.Value) > 253 || !hostName.MatchString(node.Value)) {
			return fmt.Sprintf("the %s %s is not a host name or an IP address; give the port in the field port, and no scheme or path", name, node.Value)
		}
	}
	if node := given["api_group"]; node != nil && (len(node.Value) > 253 || !apiGroup.MatchString(node.Value)) {
		return fmt.Sprintf("the API group %q is not a Kubernetes API group", node.Value)
	}
	if node := given["resource"]; node != nil && node.Value != "" && (len(node.Value) > maxWord || !apiResource.MatchString(node.Value)) {
		return fmt.Sprintf("the resource %q is not a Kubernetes resource name", node.Value)
	}
	if node := given["port"]; node != nil {
		number, err := strconv.Atoi(node.Value)
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" || err != nil || number < 1 || number > 65535 {
			return fmt.Sprintf("the port is %s; the environment schema states a port number from 1 to 65535", describe(node))
		}
	}
	if node := given["verbs"]; node != nil {
		if node.Kind != yaml.SequenceNode {
			return "the verbs are a list of verbs"
		}
		for _, verb := range node.Content {
			if !isText(verb) {
				return "the verbs are a list of verbs"
			}
			if !slices.Contains(clusterVerbs, verb.Value) {
				return fmt.Sprintf("the verb %q is not one of %s", verb.Value, strings.Join(clusterVerbs, ", "))
			}
		}
	}
	return ""
}

// hostName is a host name, an IPv4 address or an IPv6 address in brackets: the shape of the host of an ingress and
// the address of an egress destination, which give their port in a field of its own.
var hostName = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*)$`)

// rangeText is the text of a range of allowed values, as the platform requirements hold it: "1s to 30m", "at least
// 1" or "at most 10", with a unit of one or two words after it.
var rangeText = regexp.MustCompile(`^(?:(\S+) to (\S+)|at least (\S+)|at most (\S+))(?: [a-z]+(?: [a-z]+)?)?$`)

// allowedOf reads the allowed values of a listed variable back from their text: a range for an integer, a number or
// a duration, else a list of words.
func allowedOf(kind variableType, text string) allowedValues {
	if text == "" {
		return allowedValues{}
	}
	if kind == "integer" || kind == "number" || kind == "duration" {
		if parts := rangeText.FindStringSubmatch(text); parts != nil {
			bound := func(i int) *string {
				if parts[i] == "" {
					return nil
				}
				return &parts[i]
			}
			switch {
			case parts[1] != "":
				return allowedValues{Min: bound(1), Max: bound(2)}
			case parts[3] != "":
				return allowedValues{Min: bound(3)}
			default:
				return allowedValues{Max: bound(4)}
			}
		}
	}
	return allowedValues{Values: strings.Split(text, ", ")}
}

// valueFault tells why a configuration value is not a value of the listed variable: its type (ST-VO63), its
// allowed values or its range, and for a url an HTTPS URL with a host (ST-DLV-UC02-R02, ST-DLV-Q08); "" when it is
// one. The message names the variable and its type, never the value.
func valueFault(variable AcceptedVariable, value string) string {
	kind := variableType(variable.Type)
	if err := checkValue(kind, allowedOf(kind, variable.Allowed), value); err != nil {
		return fmt.Sprintf("the environment variable %s is of the type %s: %s", variable.Name, variable.Type, err)
	}
	if kind == "url" {
		if u, err := url.Parse(value); err != nil || u.Scheme != "https" {
			return fmt.Sprintf("the environment variable %s is of the type url: the value is not an HTTPS URL", variable.Name)
		}
	}
	return ""
}

// describe shows a value that is not secret in a message.
func describe(node *yaml.Node) string {
	if node.Kind == yaml.ScalarNode {
		if node.Tag == "!!str" {
			return "the text " + strconv.Quote(node.Value)
		}
		return node.Value
	}
	return "a " + nodeKind(node)
}

// CheckEnvironment checks an environment file against the platform requirements of a service and builds the
// environment fit (ST-DLV-DS02, ST-DLV-RM01-PJ): each requirement with its status by requirementMet, the image by its
// digest, each value of a wrong type, and the values no requirement names as unused. It reads secret names and keys
// only, never a secret value. An environment file that cannot be read, has an unknown shape or is of another service
// is an error, never a fit.
func CheckEnvironment(requirements Requirements, data []byte) (EnvironmentFit, error) {
	env, err := readEnvironment(data)
	if err != nil {
		return EnvironmentFit{}, err
	}
	if env.service != requirements.Service {
		return EnvironmentFit{}, &EnvironmentError{Line: 1, Field: "service",
			Reason: fmt.Sprintf("the environment file is of the service %s; the platform requirements are of the service %s", env.service, requirements.Service)}
	}
	fit := EnvironmentFit{Service: requirements.Service, Environment: env.environment, Unused: []string{}}
	if !imageDigest.MatchString(env.image) {
		fit.Rows = append(fit.Rows, FitRow{Requirement: "image", Status: WrongType,
			Detail: fmt.Sprintf("the image %s is not named by a sha256 digest; the environment schema states repository@sha256:<digest>", env.image)})
	}
	variables := map[string]AcceptedVariable{}
	for _, variable := range requirements.Environment {
		variables[variable.Name] = variable
	}
	named := map[string]bool{}
	for _, requirement := range requirements.Requirements {
		named[string(requirement.Kind)+" "+requirement.Name] = true
		status, detail := requirementMet(requirement, env)
		if variable, listed := variables[requirement.Name]; status == Met && requirement.Kind == Configuration && listed {
			if fault := valueFault(variable, env.lookup(Configuration, requirement.Name).value); fault != "" {
				status, detail = WrongType, fault
			}
		}
		if status == Met {
			if fault := needValueFault(requirement, env); fault != "" {
				status, detail = WrongType, fault
			}
		}
		fit.Rows = append(fit.Rows, FitRow{Requirement: string(requirement.Kind) + " " + requirement.Name, Status: status, Detail: detail})
		if status == Met {
			if notice := widerGrant(requirement, env); notice != "" {
				fit.Notices = append(fit.Notices, string(requirement.Kind)+" "+requirement.Name+": "+notice)
			}
		}
	}
	// A required environment variable that no requirement holds, a secret one, is met by a configuration value or a
	// secret of its name; the check names one that the file gives no value (ST-DLV-UC02-R06). It reads the name of a
	// secret variable only, never its value (ST-DLV-UC02-R03).
	listed, secretVariables := map[string]bool{}, map[string]bool{}
	for _, variable := range requirements.Environment {
		listed[variable.Name] = true
		secretVariables[variable.Name] = variable.Secret
		if !variable.Required || variable.RefusedAtStart || named[string(Configuration)+" "+variable.Name] {
			continue
		}
		if !variableGiven(variable, env) {
			detail := fmt.Sprintf("the environment file gives the required environment variable %s no value", variable.Name)
			if unwired(variable) {
				detail = unwiredDetail(variable)
			}
			fit.Rows = append(fit.Rows, FitRow{Requirement: "variable " + variable.Name, Status: Missing, Detail: detail})
		}
	}
	// A path variable of the api mode that names the file of a secret, such as STEGO_GRPC_TLS_CERT, is not required on
	// its own, so the check asks for no value: the Component secret-files sets the path. With no value and no such
	// Component, the api mode does not start, so the check gives a notice, never a failure (ST-SUP-Q200 (c)).
	for _, variable := range requirements.Environment {
		if variable.Required || variable.Secret || variable.Type != "path" || variable.ReadByMode != APIMode || variable.SecretSource == nil || variableGiven(variable, env) {
			continue
		}
		fit.Notices = append(fit.Notices, fmt.Sprintf("variable %s: the environment file gives it no value; the api mode does not start without the file, so give its path, "+
			"or list the Component %s in the overlay, which sets it to the key %s of the secret %s", variable.Name, SecretFilesFolder, variable.SecretSource.Key, variable.SecretSource.Name))
	}
	fit.Rows = append(fit.Rows, groupRows(requirements.Environment, env)...)
	for _, entry := range env.entries {
		if form, wired := fileFormOf(entry.name, variables); wired && (entry.kind == Configuration || entry.kind == Secret) && listed[entry.name] {
			// The base never wires the *_FILE form of a secret variable with a secret source from the environment
			// file: the key of that Secret gives the value, as the environment form, or as the file when the overlay
			// lists the Component secret-files (#375 security review L3). The value does not reach the service.
			fit.Notices = append(fit.Notices, fmt.Sprintf("%s %s: the base never wires %s from the environment file; the key %s of the secret %s gives its value, "+
				"as %s, or as the file %s when the overlay lists the Component %s; remove %s from the environment file",
				entry.kind, entry.name, entry.name, form.SecretSource.Key, form.SecretSource.Name, form.Name, entry.name, SecretFilesFolder, entry.name))
			continue
		}
		if entry.kind == Configuration && entry.blank && listed[entry.name] && !secretVariables[entry.name] && !variables[entry.name].RefusedAtStart {
			// A value of white space only of a listed variable that is not secret, required or optional: the check of
			// required variables counts it as no value, and the type check does not read it, but the service reads
			// the text as it is (#380 security review L2).
			fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType,
				Detail: fmt.Sprintf("the configuration value of %s is white space only: the check counts it as no value, but the service reads the text as it is; "+
					"give a value, or remove %s from the environment file", entry.name, entry.name)})
			continue
		}
		if named[string(entry.kind)+" "+entry.name] {
			if requirement := requirementOfKind(requirements, entry.kind, entry.name); requirement.Kind == Secret {
				for _, key := range entry.keys {
					if !slices.Contains(requirement.Keys, key) {
						fit.Unused = append(fit.Unused, fmt.Sprintf("%s %s key %s", entry.kind, entry.name, key))
					}
				}
			}
			continue
		}
		if (entry.kind == Configuration || entry.kind == Secret) && listed[entry.name] && variables[entry.name].RefusedAtStart &&
			!(entry.kind == Configuration && entry.empty && !entry.blank) && !(entry.kind == Secret && len(entry.keys) == 0) {
			// A value of a variable that the start guard refuses stops the service (ST-SUP-UC01-R10), also a value
			// of white space only: the guard refuses each value that is not "" (#380 security review L2).
			fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType,
				Detail: fmt.Sprintf("the service refuses the environment variable %s at start; give its setting through a declared variable", entry.name)})
			continue
		}
		if entry.kind == Secret && listed[entry.name] && unwired(variables[entry.name]) {
			// The base wires no secret variable with no secret source, and writes no envFrom (#378 security review L2):
			// a Secret named like the variable does not reach the service.
			fit.Notices = append(fit.Notices, fmt.Sprintf("%s %s: %s", entry.kind, entry.name, unwiredDetail(variables[entry.name])))
			continue
		}
		if (entry.kind == Configuration || entry.kind == Secret) && listed[entry.name] {
			// A value of a listed environment variable. A secret variable given as a plain configuration value is
			// of the wrong type: its value belongs in a secret (#344 security review L1).
			if entry.kind == Configuration && secretVariables[entry.name] {
				fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType,
					Detail: fmt.Sprintf("the environment variable %s is secret; give it as a secret, not as a configuration value", entry.name)})
				continue
			}
			if entry.fault != "" {
				fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType, Detail: entry.fault})
				continue
			}
			// Each configuration value of a listed variable has the variable's type, allowed values and range
			// (ST-DLV-UC02-R02). An empty value gives no value, and the check of required variables names it; a
			// value of white space only has a row of its own above.
			if entry.kind == Configuration && !entry.empty {
				if fault := valueFault(variables[entry.name], entry.value); fault != "" {
					fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType, Detail: fault})
				}
			}
			continue
		}
		fit.Unused = append(fit.Unused, fmt.Sprintf("%s %s", entry.kind, entry.name))
		if entry.fault != "" {
			fit.Rows = append(fit.Rows, FitRow{Requirement: fmt.Sprintf("%s %s", entry.kind, entry.name), Status: WrongType, Detail: entry.fault})
		}
	}
	return fit, nil
}

// groupRows checks each group of one of of the variable list against the environment file (ST-DLV-UC02-R06,
// ST-DLV-Q09): a group is met when the file gives exactly one of its variables a value, by variableGiven. The *_FILE
// form of a secret variable with a secret source is never a value of its own: the base wires that variable from the
// key of its Secret only, as the environment form, or as the file when the overlay lists the Component secret-files,
// so the key of the Secret is the one value of the two forms (#375 security review L3). It reads names and keys only,
// never a secret value.
func groupRows(list []AcceptedVariable, env environment) []FitRow {
	byName := map[string]AcceptedVariable{}
	for _, variable := range list {
		byName[variable.Name] = variable
	}
	var rows []FitRow
	groups := Groups(list)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		var given, ways []string
		for _, variable := range groups[name] {
			if _, wired := fileFormOf(variable.Name, byName); wired {
				continue
			}
			way := "a configuration value " + variable.Name
			if unwired(variable) {
				way = fmt.Sprintf("none for %s, which the base does not wire because it has no secret source", variable.Name)
			}
			if source := variable.SecretSource; source != nil {
				way = fmt.Sprintf("the key %s of the secret %s (%s)", source.Key, source.Name, variable.Name)
				if file, paired := byName[variable.Name+"_FILE"]; paired && file.OneOf == name && variable.Secret {
					way = fmt.Sprintf("the key %s of the secret %s (%s, or %s when the overlay lists the Component %s)",
						source.Key, source.Name, variable.Name, file.Name, SecretFilesFolder)
				}
			}
			ways = append(ways, way)
			if variableGiven(variable, env) {
				given = append(given, variable.Name)
			}
		}
		requirement := "group " + name
		switch {
		case len(given) == 0:
			rows = append(rows, FitRow{Requirement: requirement, Status: Missing,
				Detail: fmt.Sprintf("the environment file gives no variable of the group of one of %s a value; give exactly one of: %s", name, strings.Join(ways, "; "))})
		case len(given) > 1:
			rows = append(rows, FitRow{Requirement: requirement, Status: WrongType,
				Detail: fmt.Sprintf("the environment file gives more than one variable of the group of one of %s a value: %s; the service refuses them together, so give exactly one of: %s",
					name, strings.Join(given, ", "), strings.Join(ways, "; "))})
		default:
			rows = append(rows, FitRow{Requirement: requirement, Status: Met})
		}
	}
	return rows
}

// fileFormOf gives the environment form of a *_FILE variable when that form is a secret variable with a secret
// source in the list: the base wires neither form from the environment file, only from the key of that Secret
// (baseEnvironment, jobEnvironment).
func fileFormOf(name string, list map[string]AcceptedVariable) (AcceptedVariable, bool) {
	form, isFile := strings.CutSuffix(name, "_FILE")
	if !isFile {
		return AcceptedVariable{}, false
	}
	variable, listed := list[form]
	if !listed || !variable.Secret || variable.SecretSource == nil {
		return AcceptedVariable{}, false
	}
	return variable, true
}

// variableGiven tells whether the environment file gives a variable a value, as the base wires it (ST-DLV-UC02-R06).
// A secret variable with a secret source has a value only when the Secret of the source holds its key: the base reads
// no other Secret for it (#375 security review L2). A secret variable with no secret source never has a value: the
// base writes no entry for it and no envFrom, so no Secret reaches it (#378 security review L2). A variable that is
// not secret has a value when the configuration value of its name is not empty. It reads names and keys only, never
// a secret value.
func variableGiven(variable AcceptedVariable, env environment) bool {
	if variable.Secret {
		source := variable.SecretSource
		if source == nil {
			return false
		}
		entry := env.lookup(Secret, source.Name)
		return entry != nil && slices.Contains(entry.keys, source.Key)
	}
	entry := env.lookup(Configuration, variable.Name)
	return entry != nil && !entry.empty
}

// unwired tells whether a variable is secret with no secret source: the base writes no entry for it (baseEnvironment)
// and no envFrom, so the environment file cannot give it a value.
func unwired(variable AcceptedVariable) bool {
	return variable.Secret && variable.SecretSource == nil
}

// unwiredDetail tells why the environment file cannot give a secret variable with no secret source a value, and what
// to change.
func unwiredDetail(variable AcceptedVariable) string {
	return fmt.Sprintf("the base wires no value for the secret environment variable %s: it has no secret source, and the base writes no envFrom, "+
		"so a Secret named %s does not reach the service; state a secret source for it in the component that reads it (%s)",
		variable.Name, variable.Name, strings.Join(variable.ReadBy, ", "))
}

// requirementOfKind gives the requirement of the kind with the name.
func requirementOfKind(requirements Requirements, kind RequirementKind, name string) Requirement {
	for _, requirement := range requirements.Requirements {
		if requirement.Kind == kind && requirement.Name == name {
			return requirement
		}
	}
	return Requirement{}
}

// widerGrant gives a notice when the environment file meets a requirement with a wider grant than it asks for: a
// less strict pod security level, or cluster verbs that it does not name; else "".
func widerGrant(requirement Requirement, env environment) string {
	entry := env.lookup(requirement.Kind, requirement.Name)
	if entry == nil {
		return ""
	}
	switch requirement.Kind {
	case PodSecurity:
		if node := entry.fields["level"]; node != nil && slices.Index(podLevelOrder, node.Value) < slices.Index(podLevelOrder, requirement.Level) {
			return fmt.Sprintf("the environment admits pods of the less strict level %s; the service needs only the level %s", node.Value, requirement.Level)
		}
	case ClusterPermission:
		var extra []string
		if node := entry.fields["verbs"]; node != nil {
			for _, verb := range node.Content {
				if !slices.Contains(requirement.Verbs, verb.Value) && !slices.Contains(extra, verb.Value) {
					extra = append(extra, verb.Value)
				}
			}
		}
		if len(extra) > 0 {
			return "the environment also grants the verbs " + strings.Join(extra, ", ") + ", which the service does not need"
		}
	}
	return ""
}

// podLevelOrder ranks the pod security levels from the most open to the strictest.
var podLevelOrder = []string{"privileged", "baseline", "restricted"}

// needValueFault gives why the value that the environment file gives a topic or an identity issuer differs from
// the issuer that the requirement holds, or from the configuration value of the variable that carries it, or ""
// (ST-DLV-UC02-R02, ST-DLV-Q26 (A)). The *_FILE form of the variable is not compared, because the check reads no
// file.
func needValueFault(requirement Requirement, env environment) string {
	field := map[RequirementKind]string{Topic: "topic", IdentityIssuer: "issuer"}[requirement.Kind]
	entry := env.lookup(requirement.Kind, requirement.Name)
	if field == "" || entry == nil || entry.fields[field] == nil {
		return ""
	}
	given := entry.fields[field].Value
	if requirement.Value != "" && given != requirement.Value {
		return fmt.Sprintf("the issuer is %s; the declaration fixes the issuer %s, which the service checks each token against", given, requirement.Value)
	}
	if requirement.Variable == "" {
		return ""
	}
	if configured := env.lookup(Configuration, requirement.Variable); configured != nil && !configured.empty && !configured.blank && configured.value != given {
		return fmt.Sprintf("the %s is %s, but the configuration value of %s, which the service reads it from, is %s; give the two the same value",
			field, given, requirement.Variable, configured.value)
	}
	return ""
}

// requirementMet tells whether the environment file meets one platform requirement (ST-DLV-SP01): it supplies, under
// the requirement's name, a value of its kind with every part its shape names. When it does not, it gives the
// status and the part that is missing or wrong.
func requirementMet(requirement Requirement, env environment) (FitStatus, string) {
	entry := env.lookup(requirement.Kind, requirement.Name)
	if entry == nil {
		for _, other := range env.entries {
			if other.name == requirement.Name {
				return WrongType, fmt.Sprintf("the environment file supplies %s as a %s; the requirement is a %s", requirement.Name, other.kind, requirement.Kind)
			}
		}
		return Missing, fmt.Sprintf("the environment file supplies no %s %s", requirement.Kind, requirement.Name)
	}
	if entry.fault != "" {
		return WrongType, entry.fault
	}
	text := func(field string) (string, bool) {
		node := entry.fields[field]
		if node == nil {
			return "", false
		}
		return node.Value, true
	}
	switch requirement.Kind {
	case Configuration:
		if entry.empty {
			return Missing, "the configuration value " + requirement.Name + " is empty"
		}
	case Secret:
		var missing []string
		for _, key := range requirement.Keys {
			if !slices.Contains(entry.keys, key) {
				missing = append(missing, key)
			}
		}
		if len(missing) == 1 {
			return Missing, "the key " + missing[0] + " is missing"
		}
		if len(missing) > 1 {
			return Missing, "the keys " + strings.Join(missing, ", ") + " are missing"
		}
	case BackingStore:
		engine, given := text("engine")
		if !given {
			return Missing, "the engine is missing"
		}
		if engine != requirement.Engine {
			return WrongType, fmt.Sprintf("the engine is %s; the requirement names the engine %s", engine, requirement.Engine)
		}
	case Topic, IdentityIssuer:
		field := map[RequirementKind]string{Topic: "topic", IdentityIssuer: "issuer"}[requirement.Kind]
		if _, given := text(field); !given {
			return Missing, "the " + field + " is missing"
		}
	case Ingress, Egress:
		if requirement.Kind == Egress {
			if _, given := text("address"); !given {
				return Missing, "the address is missing"
			}
		}
		port, given := text("port")
		if !given {
			return Missing, "the port is missing"
		}
		if port != strconv.Itoa(requirement.Port) {
			return WrongType, fmt.Sprintf("the port is %s; the requirement names the port %d", port, requirement.Port)
		}
	case ClusterPermission:
		group, _ := text("api_group")
		resource, given := text("resource")
		if !given {
			return Missing, "the resource is missing"
		}
		if group != requirement.APIGroup || resource != requirement.Resource {
			return WrongType, fmt.Sprintf("the permission is for the resource %q of the API group %q; the requirement names the resource %q of the API group %q",
				resource, group, requirement.Resource, requirement.APIGroup)
		}
		var granted []string
		if node := entry.fields["verbs"]; node != nil {
			for _, verb := range node.Content {
				granted = append(granted, verb.Value)
			}
		}
		var missing []string
		for _, verb := range requirement.Verbs {
			if !slices.Contains(granted, verb) {
				missing = append(missing, verb)
			}
		}
		if len(missing) > 0 {
			return Missing, "the verbs " + strings.Join(missing, ", ") + " are not granted"
		}
	case PodSecurity:
		level, given := text("level")
		if !given {
			return Missing, "the level is missing"
		}
		admitted := slices.Index(podLevelOrder, level)
		if admitted < 0 {
			return WrongType, fmt.Sprintf("the level %s is not one of %s", level, strings.Join(podLevelOrder, ", "))
		}
		if admitted > slices.Index(podLevelOrder, requirement.Level) {
			return Missing, fmt.Sprintf("the environment admits pods of the level %s; the service needs the level %s", level, requirement.Level)
		}
	default:
		return WrongType, fmt.Sprintf("the requirement kind %q is not known to the check", requirement.Kind)
	}
	return Met, ""
}

// UnmetRequirements finds the unmet platform requirements (ST-DLV-QRY01-H): each row of the environment fit that
// the reader holds whose status is not met, in the order of the fit.
func UnmetRequirements(reader *EnvironmentReader) []FitRow {
	var unmet []FitRow
	for _, row := range reader.Fit().Rows {
		if row.Status != Met {
			unmet = append(unmet, row)
		}
	}
	return unmet
}

// variableType is a type of ST-VO63, as the platform requirements state it. The check of an environment file holds a
// value to its type with the same rules as the registry holds a default (registry.VariableType), which Delivering
// may not import (ST-STD-CODE-R03).
type variableType string

// allowedValues are the allowed values of a listed variable: a list of words, or an inclusive range of an integer, a
// number or a duration. The zero value allows each value of the type.
type allowedValues struct {
	Values   []string
	Min, Max *string
}

// maxConfigurationValue bounds a configuration value, as the registry bounds a default.
const maxConfigurationValue = 512

// checkValue tells whether a value is of the variable's type and inside its allowed values, and why not.
// It never puts the value in its message, because the caller may hold a value that it must not show.
func checkValue(kind variableType, allowed allowedValues, value string) error {
	if len(value) > maxConfigurationValue || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("the value is not one line of at most %d characters", maxConfigurationValue)
	}
	if err := checkVariableType(kind, value); err != nil {
		return err
	}
	if len(allowed.Values) > 0 {
		items := []string{value}
		if kind == "list" {
			items = strings.Split(value, ",")
		}
		for _, item := range items {
			if !slices.Contains(allowed.Values, strings.TrimSpace(item)) {
				return fmt.Errorf("the value is not one of the allowed values %s", strings.Join(allowed.Values, ", "))
			}
		}
		return nil
	}
	if allowed.Min == nil && allowed.Max == nil {
		return nil
	}
	number, err := numericValue(kind, value)
	if err != nil {
		return fmt.Errorf("the value is not a %s", kind)
	}
	// A bound that does not parse is an error, not 0 (#353 security review L3): the platform requirements give the
	// range as text, and a range that the check cannot read must not let each value pass.
	bound := func(text *string) (float64, error) {
		n, err := numericValue(kind, *text)
		if err != nil || n != n || math.IsInf(n, 0) {
			return 0, fmt.Errorf("the range bound %q of the platform requirements is not a %s", *text, kind)
		}
		return n, nil
	}
	if allowed.Min != nil {
		minimum, err := bound(allowed.Min)
		if err != nil {
			return err
		}
		if number < minimum {
			return fmt.Errorf("the value is less than %s", *allowed.Min)
		}
	}
	if allowed.Max != nil {
		maximum, err := bound(allowed.Max)
		if err != nil {
			return err
		}
		if number > maximum {
			return fmt.Errorf("the value is more than %s", *allowed.Max)
		}
	}
	return nil
}

// numericValue gives an integer, a number or a duration as a number, to compare it with a range.
func numericValue(kind variableType, value string) (float64, error) {
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

// checkVariableType tells whether a value is of a variable type.
func checkVariableType(kind variableType, value string) error {
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
