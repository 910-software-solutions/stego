package delivery

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ApplicationSetFile is the path, in the project folder, of the delivery object (ST-DLV-EN05-TB).
const ApplicationSetFile = "deploy/argocd/applicationset.yaml"

// applicationHeader is the first lines of the delivery object.
const applicationHeader = "# The delivery object of the service. stego apply writes this file from the delivery stanza of the declaration.\n" +
	"# Each environment file of the platform gives the cluster, the namespace and the overlay. Do not edit it.\n"

// ArgoCDStanza is the Argo CD part of the delivery stanza of the declaration (ST-DLV-Q12 (A)): the project, the
// address of the service's repository with the path of the base in it, and the repository, the revision and the
// path pattern of the platform's environment files.
type ArgoCDStanza struct {
	Project             string
	SourceRepo          string
	SourcePath          string
	EnvironmentRepo     string
	EnvironmentRevision string
	EnvironmentPath     string
}

// Application is the delivery object of one service (ST-DLV-EN05): the Argo CD ApplicationSet that the platform's
// environment files feed.
type Application struct {
	Service string
	Project string
	Base    string
	File    []byte
}

var (
	// argoProject is an Argo CD project name: a DNS label.
	argoProject = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// repoAddress is the address of a Git repository: https or ssh, with no query, fragment, space or user
	// information other than the git user of an ssh address.
	repoAddress = regexp.MustCompile(`^(https://[A-Za-z0-9.-]+(:[0-9]+)?|ssh://git@[A-Za-z0-9.-]+(:[0-9]+)?|git@[A-Za-z0-9.-]+:)[A-Za-z0-9._/~-]+$`)
	// revisionName is a branch, a tag or a commit.
	revisionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// filePattern is a relative path pattern of the platform's environment files.
	filePattern = regexp.MustCompile(`^[A-Za-z0-9._*/-]+\.(yaml|yml|json)$`)
)

// StanzaError is a field of the delivery stanza that apply cannot write a delivery object from: its path in the
// declaration and why.
type StanzaError struct {
	Path   string
	Reason string
}

func (e *StanzaError) Error() string {
	return fmt.Sprintf("the declaration field %s: %s", e.Path, e.Reason)
}

// relativeClean tells if a path is relative, clean and stays in its repository.
func relativeClean(name string) bool {
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

// checkStanza refuses an Argo CD stanza whose fields cannot name what they must, naming the path of each field.
func checkStanza(stanza ArgoCDStanza) error {
	switch {
	case !argoProject.MatchString(stanza.Project):
		return &StanzaError{Path: "delivery.argocd.project", Reason: "an Argo CD project is a DNS label"}
	case !repoAddress.MatchString(stanza.SourceRepo):
		return &StanzaError{Path: "delivery.argocd.source.repo", Reason: "the address of the service's repository is an https or ssh Git address"}
	case !relativeClean(stanza.SourcePath) || (stanza.SourcePath != BaseFolder && !strings.HasSuffix(stanza.SourcePath, "/"+BaseFolder)):
		return &StanzaError{Path: "delivery.argocd.source.path", Reason: "the path of the base is a clean relative path that ends in " + BaseFolder}
	case !repoAddress.MatchString(stanza.EnvironmentRepo):
		return &StanzaError{Path: "delivery.argocd.environments.repo", Reason: "the address of the platform's repository is an https or ssh Git address"}
	case !revisionName.MatchString(stanza.EnvironmentRevision):
		return &StanzaError{Path: "delivery.argocd.environments.revision", Reason: "the revision is a branch, a tag or a commit"}
	case !relativeClean(stanza.EnvironmentPath) || !filePattern.MatchString(stanza.EnvironmentPath):
		return &StanzaError{Path: "delivery.argocd.environments.path", Reason: "the path of the environment files is a clean relative pattern of YAML or JSON files"}
	}
	return nil
}

// WriteApplication writes the Argo CD ApplicationSet of the service from the delivery stanza (ST-DLV-CMD04), as the
// base is written (ST-DLV-PO04). Its template names the service, the project and the base; its files generator
// lists the platform's environment files, and the destination cluster, the namespace and the overlay path of each
// Application are parameters of that generator, never values of the declaration (ST-DLV-INV10, ST-DLV-UC07-R04).
// The Application builds the overlay of its environment file from the platform's repository; the overlay builds
// on the base, which the second source names.
func WriteApplication(written BaseWritten, stanza ArgoCDStanza) (Application, error) {
	if err := checkStanza(stanza); err != nil {
		return Application{}, err
	}
	object := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "ApplicationSet",
		"metadata":   map[string]any{"name": written.Service, "labels": map[string]any{"app.kubernetes.io/name": written.Service}},
		"spec": map[string]any{
			"goTemplate":        true,
			"goTemplateOptions": []any{"missingkey=error"},
			"generators": []any{map[string]any{"git": map[string]any{
				"repoURL":  stanza.EnvironmentRepo,
				"revision": stanza.EnvironmentRevision,
				"files":    []any{map[string]any{"path": stanza.EnvironmentPath}},
			}}},
			"template": map[string]any{
				"metadata": map[string]any{
					"name":        written.Service + "-{{.environment}}",
					"labels":      map[string]any{"app.kubernetes.io/name": written.Service},
					"annotations": map[string]any{"stego.dev/base": stanza.SourceRepo + "//" + stanza.SourcePath},
				},
				"spec": map[string]any{
					"project": stanza.Project,
					"sources": []any{
						map[string]any{"repoURL": stanza.EnvironmentRepo, "targetRevision": stanza.EnvironmentRevision, "path": "{{.destination.overlay_path}}"},
						map[string]any{"repoURL": stanza.SourceRepo, "targetRevision": "HEAD", "ref": "base"},
					},
					"destination": map[string]any{"name": "{{.destination.cluster}}", "namespace": "{{.destination.namespace}}"},
				},
			},
		},
	}
	data, err := encodeWith(applicationHeader, object)
	if err != nil {
		return Application{}, err
	}
	return Application{Service: written.Service, Project: stanza.Project, Base: stanza.SourcePath, File: data}, nil
}
