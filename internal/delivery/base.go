package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// BaseFolder is the folder, in the project folder, of the Kubernetes base (ST-DLV-EN04-TB).
const BaseFolder = "deploy/base"

// The files that the Kubernetes base may hold. The kustomization file lists the others that a run writes.
const (
	KustomizationFile  = BaseFolder + "/kustomization.yaml"
	ServiceAccountFile = BaseFolder + "/service-account.yaml"
	DeploymentFile     = BaseFolder + "/deployment.yaml"
	ServiceFile        = BaseFolder + "/service.yaml"
	RoleFile           = BaseFolder + "/role.yaml"
	RoleBindingFile    = BaseFolder + "/role-binding.yaml"
)

// SecretFilesFolder is the folder, in the project folder, of the opt-in kustomize Component that gives each secret
// to the service as a mounted file in place of an environment variable (ST-DLV-Q18).
const SecretFilesFolder = "deploy/components/secret-files"

// The files of the Component secret-files: its kustomization, the patch that removes the environment entries of the
// secrets, and the patch that mounts the secrets and sets the *_FILE variables. The forms are exclusive, so the first
// patch removes each entry that the second replaces.
const (
	SecretFilesKustomization = SecretFilesFolder + "/kustomization.yaml"
	SecretFilesRemoval       = SecretFilesFolder + "/remove-env.yaml"
	SecretFilesMounts        = SecretFilesFolder + "/mount-files.yaml"
)

// SecretFilesRoot is the folder in the container under which the Component mounts each secret, one folder for each
// secret, named by the secret, with one file for each key.
const SecretFilesRoot = "/var/run/secrets/stego"

// SecretFileMode is the mode of each file that the Component mounts: 0400, read by the owner only.
const SecretFileMode = 0o400

// BaseFiles are the paths of every file that the Kubernetes base may hold, in the order of the kustomization file,
// and then the files of the Component secret-files.
var BaseFiles = []string{KustomizationFile, ServiceAccountFile, DeploymentFile, ServiceFile, RoleFile, RoleBindingFile,
	SecretFilesKustomization, SecretFilesRemoval, SecretFilesMounts}

// ConfigMapName is the name of the one ConfigMap from which the base takes each variable that is not secret
// (ST-DLV-Q18): the platform makes it, with the variable name as the key.
func ConfigMapName(service string) string {
	return service + "-config"
}

// ResourceDefaults are the default resource requests and limits of each container of the base (ST-DLV-EN04 field
// Resource defaults). An overlay may change each of them (ST-DLV-UC07-R03).
var ResourceDefaults = map[string]map[string]string{
	"requests": {"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "32Mi"},
	"limits":   {"cpu": "1", "memory": "512Mi", "ephemeral-storage": "128Mi"},
}

// baseHeader is the first lines of each file of the base.
const baseHeader = "# The Kubernetes base of the service. stego apply writes this file from the declaration and the platform\n" +
	"# requirements, with no value that differs per environment: the platform's overlay supplies them, and every\n" +
	"# overlay must set the image by digest, because the image of the base is a name that no registry serves. Do not\n" +
	"# edit it.\n"

// serviceLabel is a Kubernetes object name that the base gives each resource: a DNS label.
var serviceLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// TmpSizeLimit bounds the emptyDir at /tmp, within the ephemeral storage limit of the container.
const TmpSizeLimit = "64Mi"

// ImagePlaceholder is the image of the base: a name in the reserved domain stego.invalid, which no registry serves,
// so that a pod whose overlay sets no image digest fails to pull and never runs an image of another owner. Every
// overlay must set the image by digest.
func ImagePlaceholder(service string) string {
	return "stego.invalid/" + service + ":set-by-overlay"
}

// ImageNote is the plan line of a run that writes the base: every overlay must set the image digest.
const ImageNote = "the Kubernetes base names the image %s, which no registry serves: every overlay must set the image by digest"

// portName is a Kubernetes port name (IANA_SVC_NAME): at most 15 lower-case letters, digits and '-', with a letter.
var portName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,13}[a-z0-9])?$`)

// Base is the Kubernetes base of one service (ST-DLV-EN04): the service, the image repository with no tag or
// digest, the resource defaults, and the files the kustomization file lists.
type Base struct {
	Service string
	Image   string
	Files   map[string][]byte
}

// BaseWritten tells that the Kubernetes base of a service was written in this run (ST-DLV-EV03), so that the delivery
// object is written in the same run (ST-DLV-PO04).
type BaseWritten struct {
	Service string
	Base    string
}

// ProbeSource is the component of the active set that serves the liveness and the readiness probes: their paths and
// the environment variable whose default is their port.
type ProbeSource struct {
	Liveness     string
	Readiness    string
	PortVariable string
}

// DeploymentFiles are the files of the Kubernetes base and the delivery object of one run, and the lines that the
// plan states.
type DeploymentFiles struct {
	Files map[string][]byte
	Notes []string
}

// NoDeliveryStanza is the end of the plan line of a run whose declaration has no delivery stanza (ST-DLV-UC07-R05).
const NoDeliveryStanza = "the declaration has no delivery stanza with an argocd part"

// NoBase is the plan line of a run whose active set serves no probes: such a service is not deployed as a
// Deployment, so the run writes no Kubernetes base.
const NoBase = "no Kubernetes base is written: no component of the service serves the health probes"

// noProbePort is the plan line of a run whose platform requirements give no default port for the probes: the base
// cannot carry its probes (ST-DLV-UC07-R03), so the run writes none.
const noProbePort = "no Kubernetes base is written: the platform requirements give no default port for the probes in %s"

// WriteDelivery writes the CI file of the provider that the delivery stanza names, with the pins of the registry's
// ci-pipeline component (ST-DLV-CMD02), and then the Kubernetes base and the delivery object (WriteDeployment).
// When the stanza names no provider, or the component states no pins for it (ST-DLV-Q13 (C)), the run writes no
// CI file and says so in the plan.
func WriteDelivery(service string, requirements []byte, probes *ProbeSource, source ContainerSource, stanza *ArgoCDStanza, provider string, pins map[string]PipelinePins) (DeploymentFiles, error) {
	if provider != "" && provider != GitHubActions && provider != Konflux {
		return DeploymentFiles{}, &StanzaError{Path: "delivery.ci", Reason: fmt.Sprintf("the CI provider %q is not %s or %s", provider, GitHubActions, Konflux)}
	}
	written, err := WriteDeployment(service, requirements, probes, source, stanza)
	if err != nil {
		return DeploymentFiles{}, err
	}
	switch stated, ok := pins[provider]; {
	case provider == "":
		written.Notes = append(written.Notes, "no CI file is written: the declaration names no CI provider in its delivery stanza")
	case !ok:
		written.Notes = append(written.Notes, fmt.Sprintf("no CI file is written: the registry's ci-pipeline component states no pins for %s", provider))
	default:
		pipeline, err := WritePipeline(service, provider, stated)
		if err != nil {
			return DeploymentFiles{}, err
		}
		if written.Files == nil {
			written.Files = map[string][]byte{}
		}
		written.Files[pipeline.Path] = pipeline.File
	}
	return written, nil
}

// WriteDeployment writes the Kubernetes base of the service from its platform requirements and its probes, and the
// offline Containerfile of the image that the base runs (ST-DLV-CMD03), and then, as the base is written, the
// delivery object from the Argo CD part of the delivery stanza, or none when the declaration has no such part
// (ST-DLV-PO04).
func WriteDeployment(service string, requirements []byte, probes *ProbeSource, source ContainerSource, stanza *ArgoCDStanza) (DeploymentFiles, error) {
	if probes == nil {
		return DeploymentFiles{Notes: []string{NoBase}}, nil
	}
	base, written, err := WriteBase(service, requirements, *probes)
	if errors.Is(err, errNoProbePort) {
		return DeploymentFiles{Notes: []string{fmt.Sprintf(noProbePort, probes.PortVariable)}}, nil
	}
	if err != nil {
		return DeploymentFiles{}, err
	}
	containerfile, err := WriteContainerfile(service, source)
	if err != nil {
		return DeploymentFiles{}, err
	}
	base.Files[ContainerFile] = containerfile
	application, note, err := deliveryObjectFor(written, stanza)
	if err != nil {
		return DeploymentFiles{}, err
	}
	if application != nil {
		base.Files[ApplicationSetFile] = application.File
	}
	notes := []string{fmt.Sprintf(ImageNote, ImagePlaceholder(service))}
	if note != "" {
		notes = append(notes, note)
	}
	return DeploymentFiles{Files: base.Files, Notes: notes}, nil
}

// deliveryObjectFor is the policy Write the delivery object for a written base (ST-DLV-PO04): it hears the base of
// the service written and writes the ApplicationSet from the Argo CD part of the delivery stanza. Without that part,
// it writes none and gives the plan line that says so, with no destination, project or path (ST-DLV-UC07-R05).
func deliveryObjectFor(written BaseWritten, stanza *ArgoCDStanza) (*Application, string, error) {
	if stanza == nil {
		return nil, fmt.Sprintf("no delivery object (Argo CD ApplicationSet) is written for the base of %s: %s", written.Service, NoDeliveryStanza), nil
	}
	application, err := WriteApplication(written, *stanza)
	if err != nil {
		return nil, "", err
	}
	return &application, fmt.Sprintf(BaseNote, application.Base, application.Project), nil
}

// WriteBase writes the plain kustomize base of the service (ST-DLV-CMD03) from its platform requirements alone: a
// ServiceAccount, a Deployment with the probes and the default resources, a Service for each ingress, and a Role and
// a RoleBinding for the cluster permissions. It sets no replicas, namespace, image digest, host name, address or
// secret value (ST-DLV-INV08), and it reads the base back with LoadBase before it gives it (ST-DLV-INV09).
func WriteBase(service string, requirements []byte, probes ProbeSource) (Base, BaseWritten, error) {
	stated, err := LoadRequirements(requirements)
	if err != nil {
		return Base{}, BaseWritten{}, err
	}
	if !serviceLabel.MatchString(service) {
		return Base{}, BaseWritten{}, fmt.Errorf("the service name %q is not a Kubernetes name: a DNS label that starts and ends with a letter or a digit", service)
	}
	if stated.Service != service {
		return Base{}, BaseWritten{}, fmt.Errorf("the platform requirements are of the service %s, not %s", stated.Service, service)
	}
	port, err := probePort(stated, probes.PortVariable)
	if err != nil {
		return Base{}, BaseWritten{}, err
	}
	labels := map[string]any{"app.kubernetes.io/name": service}
	metadata := map[string]any{"name": service, "labels": labels}

	var ingress, permissions []Requirement
	for _, requirement := range stated.Requirements {
		switch requirement.Kind {
		case Ingress:
			ingress = append(ingress, requirement)
		case ClusterPermission:
			permissions = append(permissions, requirement)
		}
	}

	files := map[string][]byte{}
	resources := []string{}
	add := func(file string, object map[string]any) error {
		data, err := encodeResource(object)
		if err != nil {
			return err
		}
		files[file] = data
		resources = append(resources, path.Base(file))
		return nil
	}

	if err := add(ServiceAccountFile, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata,
		"automountServiceAccountToken": len(permissions) > 0}); err != nil {
		return Base{}, BaseWritten{}, err
	}

	container := map[string]any{
		"name":           service,
		"image":          ImagePlaceholder(service),
		"readinessProbe": httpProbe(probes.Readiness, port),
		"livenessProbe":  httpProbe(probes.Liveness, port),
		"resources":      ResourceDefaults,
	}
	var ports []any
	for _, requirement := range ingress {
		ports = append(ports, containerPort(requirement))
	}
	if len(ports) > 0 {
		container["ports"] = ports
	}
	// The base always writes the settings of the restricted Pod Security Standard and a read-only root file
	// system, whatever level the requirements state; an overlay may relax them, the base never does (ST-DLV-Q14).
	// The process writes its temporary files to an emptyDir at /tmp.
	container["securityContext"] = map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
		"readOnlyRootFilesystem": true}
	container["volumeMounts"] = []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}}
	// One environment entry, names only, for each variable that the service reads (ST-DLV-UC07-R03, ST-DLV-Q18).
	environment, switched := baseEnvironment(service, stated.Environment)
	if len(environment) > 0 {
		container["env"] = environment
	}
	pod := map[string]any{"serviceAccountName": service, "automountServiceAccountToken": len(permissions) > 0, "containers": []any{container},
		"securityContext": map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes":         []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": TmpSizeLimit}}}}
	if err := add(DeploymentFile, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata,
		"spec": map[string]any{"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod}}}); err != nil {
		return Base{}, BaseWritten{}, err
	}

	if len(ingress) > 0 {
		var servicePorts []any
		for _, requirement := range ingress {
			servicePort := map[string]any{"port": requirement.Port, "targetPort": requirement.Port, "protocol": "TCP"}
			if portName.MatchString(requirement.Name) {
				servicePort["name"] = requirement.Name
			}
			servicePorts = append(servicePorts, servicePort)
		}
		if err := add(ServiceFile, map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata,
			"spec": map[string]any{"selector": labels, "ports": servicePorts}}); err != nil {
			return Base{}, BaseWritten{}, err
		}
	}

	if len(permissions) > 0 {
		var rules []any
		for _, requirement := range permissions {
			verbs := make([]any, len(requirement.Verbs))
			for i, verb := range requirement.Verbs {
				verbs[i] = verb
			}
			if reason := sensitivePermission(requirement); reason != "" {
				return Base{}, BaseWritten{}, fmt.Errorf("the cluster permission %s: %s", requirement.Name, reason)
			}
			rule := map[string]any{"apiGroups": []any{requirement.APIGroup}, "resources": []any{requirement.Resource}, "verbs": verbs}
			if len(requirement.ResourceNames) > 0 {
				names := make([]any, len(requirement.ResourceNames))
				for i, name := range requirement.ResourceNames {
					names[i] = name
				}
				rule["resourceNames"] = names
			}
			rules = append(rules, rule)
		}
		if err := add(RoleFile, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata, "rules": rules}); err != nil {
			return Base{}, BaseWritten{}, err
		}
		// The subject names no namespace: the overlay's namespace sets it.
		if err := add(RoleBindingFile, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata,
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": service},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": service}}}); err != nil {
			return Base{}, BaseWritten{}, err
		}
	}

	kustomization := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
	data, err := encodeResource(kustomization)
	if err != nil {
		return Base{}, BaseWritten{}, err
	}
	files[KustomizationFile] = data
	if len(switched) > 0 {
		component, err := secretFilesComponent(service, switched)
		if err != nil {
			return Base{}, BaseWritten{}, err
		}
		maps.Copy(files, component)
	}
	base := Base{Service: service, Image: ImagePlaceholder(service), Files: files}
	if _, err := LoadBase(base.Files); err != nil {
		return Base{}, BaseWritten{}, fmt.Errorf("the Kubernetes base is not a plain kustomize base: %w", err)
	}
	return base, BaseWritten{Service: service, Base: BaseFolder}, nil
}

// ConsoleSinkVariable is the variable that turns the console sink of the domain probes on. The base never names
// it, so that domain observations stay off the console in each environment whose own overlay does not turn them
// on (ST-STD-OBS-R03).
const ConsoleSinkVariable = "STEGO_PROBE_CONSOLE"

// secretFile is one variable that the Component secret-files gives as a mounted file: the environment entry that
// the base writes and the Component removes, the *_FILE variable that the Component sets, and the secret and key
// of the file.
type secretFile struct {
	Removed  string
	Variable string
	Source   SecretSource
}

// baseEnvironment gives the environment entries of the container, in name order, with names only and never a value
// (ST-DLV-UC07-R03, ST-DLV-Q18), and the variables that the Component secret-files gives as files.
//
// A secret variable with a secret source takes its value from that key of that Secret, with no optional flag: the
// secret need requires each of its keys, so a missing key stops the pod before the process starts. A secret
// variable with no secret source gets no entry, because no Secret of the service holds it. The *_FILE form of a
// secret variable gets no entry, because the two forms are exclusive and the base wires the environment form. Each
// other variable takes its value from the key of its name in the ConfigMap <service>-config, optional when the
// variable is optional, so that the code keeps its default when the key is not there. The console sink of the
// domain probes gets no entry (ST-STD-OBS-R03); a variable that the start guard refuses gets none, because a value
// of it stops the service (ST-SUP-UC01-R10); and a variable that only the migration job reads gets none, so that the
// serving pod never holds the credential of the owner role (ST-DLV-Q19, least privilege).
func baseEnvironment(service string, list []AcceptedVariable) ([]any, []secretFile) {
	sourced := map[string]SecretSource{}
	for _, variable := range list {
		if variable.Secret && variable.SecretSource != nil {
			sourced[variable.Name] = *variable.SecretSource
		}
	}
	listed := map[string]bool{}
	for _, variable := range list {
		listed[variable.Name] = true
	}
	var entries []any
	var switched []secretFile
	for _, variable := range list {
		if variable.Name == ConsoleSinkVariable || variable.RefusedAtStart || variable.ReadByMode != "" {
			continue
		}
		if form, isFile := strings.CutSuffix(variable.Name, "_FILE"); isFile {
			if _, paired := sourced[form]; paired {
				continue
			}
		}
		switch {
		case variable.Secret && variable.SecretSource == nil:
			continue
		case variable.Secret:
			entries = append(entries, map[string]any{"name": variable.Name, "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": variable.SecretSource.Name, "key": variable.SecretSource.Key}}})
			if listed[variable.Name+"_FILE"] {
				switched = append(switched, secretFile{Removed: variable.Name, Variable: variable.Name + "_FILE", Source: *variable.SecretSource})
			}
			continue
		}
		reference := map[string]any{"name": ConfigMapName(service), "key": variable.Name}
		if !variable.Required {
			reference["optional"] = true
		}
		entries = append(entries, map[string]any{"name": variable.Name, "valueFrom": map[string]any{"configMapKeyRef": reference}})
		if variable.SecretSource != nil {
			// A path variable that names the file of a secret: the Component replaces its entry with the path.
			switched = append(switched, secretFile{Removed: variable.Name, Variable: variable.Name, Source: *variable.SecretSource})
		}
	}
	return entries, switched
}

// SecretFilePath is the path in the container of the file that holds one key of one secret, as the Component
// secret-files mounts it.
func SecretFilePath(source SecretSource) string {
	return SecretFilesRoot + "/" + source.Name + "/" + source.Key
}

// secretFilesComponent writes the opt-in kustomize Component secret-files (ST-DLV-Q18): a patch that removes the
// environment entry of each switched variable, and a patch that sets each *_FILE variable to the path of its file
// and mounts each secret read-only, with the mode 0400 and no subPath, so that a new value of the secret reaches
// the file in place. The forms are exclusive, so the first patch removes what the second replaces.
func secretFilesComponent(service string, switched []secretFile) (map[string][]byte, error) {
	var removed, set, mounts, volumes []any
	// The keys of each secret that a switched variable reads: the volume projects only these keys, so that no other
	// key of the Secret, such as the credential of the migration job, is a file of the pod (ST-DLV-Q19).
	keys := map[string][]any{}
	var secrets []string
	for _, file := range switched {
		removed = append(removed, map[string]any{"name": file.Removed, "$patch": "delete"})
		set = append(set, map[string]any{"name": file.Variable, "value": SecretFilePath(file.Source)})
		if _, seen := keys[file.Source.Name]; !seen {
			secrets = append(secrets, file.Source.Name)
		}
		keys[file.Source.Name] = append(keys[file.Source.Name], map[string]any{"key": file.Source.Key, "path": file.Source.Key})
	}
	for _, secret := range secrets {
		if !serviceLabel.MatchString(secret) || secret == "tmp" {
			return nil, fmt.Errorf("the secret %s is not a Kubernetes volume name: a DNS label of at most 63 characters that is not tmp; use a shorter service name", secret)
		}
		mounts = append(mounts, map[string]any{"name": secret, "mountPath": SecretFilesRoot + "/" + secret, "readOnly": true})
		volumes = append(volumes, map[string]any{"name": secret, "secret": map[string]any{"secretName": secret, "defaultMode": SecretFileMode,
			"items": keys[secret]}})
	}
	patch := func(container map[string]any, pod map[string]any) map[string]any {
		container["name"] = service
		pod["containers"] = []any{container}
		return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": service},
			"spec": map[string]any{"template": map[string]any{"spec": pod}}}
	}
	files := map[string][]byte{}
	for file, object := range map[string]map[string]any{
		SecretFilesKustomization: {"apiVersion": "kustomize.config.k8s.io/v1alpha1", "kind": "Component",
			"patches": []any{map[string]any{"path": path.Base(SecretFilesRemoval)}, map[string]any{"path": path.Base(SecretFilesMounts)}}},
		SecretFilesRemoval: patch(map[string]any{"env": removed}, map[string]any{}),
		SecretFilesMounts:  patch(map[string]any{"env": set, "volumeMounts": mounts}, map[string]any{"volumes": volumes}),
	} {
		data, err := encodeWith(secretFilesHeader, object)
		if err != nil {
			return nil, err
		}
		files[file] = data
	}
	return files, nil
}

// secretFilesHeader is the first lines of each file of the Component secret-files.
const secretFilesHeader = "# The opt-in kustomize Component secret-files of the service. stego apply writes this file from the platform\n" +
	"# requirements. An overlay that lists this folder under components gets each secret as a read-only file under\n" +
	"# " + SecretFilesRoot + " in place of an environment variable. Each file has the mode 0400: give the pod an\n" +
	"# fsGroup, as the restricted SCC of OpenShift does, and do not widen the mode. Do not edit it.\n"

// probePort is the port of the probes: the default of the variable that the probes' component states for it, from
// the environment list of the platform requirements.
func probePort(requirements Requirements, variable string) (int, error) {
	for _, accepted := range requirements.Environment {
		if accepted.Name != variable {
			continue
		}
		if accepted.Default == nil {
			return 0, fmt.Errorf("the environment variable %s states no default port for the probes", variable)
		}
		port, err := strconv.Atoi(*accepted.Default)
		if err != nil || port < 1 || port > 65535 {
			return 0, fmt.Errorf("the default of the environment variable %s is not a port", variable)
		}
		return port, nil
	}
	return 0, fmt.Errorf("%w: the platform requirements list no environment variable %s", errNoProbePort, variable)
}

// errNoProbePort is the fault of platform requirements that list no variable for the port of the probes.
var errNoProbePort = errors.New("no default port for the probes")

// httpProbe is an HTTP probe of the path on the port.
func httpProbe(path string, port int) map[string]any {
	return map[string]any{"httpGet": map[string]any{"path": path, "port": port}}
}

// containerPort is the container port of one ingress, named by the ingress when its name is a port name.
func containerPort(requirement Requirement) map[string]any {
	port := map[string]any{"containerPort": requirement.Port, "protocol": "TCP"}
	if portName.MatchString(requirement.Name) {
		port["name"] = requirement.Name
	}
	return port
}

// encodeResource gives one YAML document with the header of the base. The keys of each map are in name order, so
// the same requirements give the same bytes (ST-DLV-DS01).
func encodeResource(object map[string]any) ([]byte, error) {
	return encodeWith(baseHeader, object)
}

// encodeWith gives one YAML document with the header.
func encodeWith(header string, object map[string]any) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(header)
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(object); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// baseKinds are the kinds of resource that a Kubernetes base may hold.
var baseKinds = []string{"ServiceAccount", "Deployment", "Service", "Role", "RoleBinding"}

// LoadBase reads the files of a Kubernetes base back and holds them to plain kustomize (ST-DLV-REPO03,
// ST-DLV-INV09): each file is one of the base folder or of the Component secret-files; the kustomization file has
// only its apiVersion, its kind and its resources, so it names no generator, transformer, Helm chart or remote
// base; each resource is a file of the base folder that the base holds; each resource file is one YAML document
// with an apiVersion, a kind and a name; each environment entry of the Deployment names a key of a Secret or of a
// ConfigMap and never holds a value (ST-DLV-Q18); and the Component, when the base has one, is exactly a Component
// with its two patches of the Deployment (loadSecretFiles). The tests build the base with the kustomize library
// itself; the compiler does not carry it.
func LoadBase(files map[string][]byte) (map[string]map[string]any, error) {
	inBase := 0
	for file := range files {
		if !slices.Contains(BaseFiles, file) {
			return nil, fmt.Errorf("%s is not a file of the Kubernetes base", file)
		}
		if path.Dir(file) == BaseFolder {
			inBase++
		}
	}
	data, ok := files[KustomizationFile]
	if !ok {
		return nil, fmt.Errorf("the Kubernetes base has no %s", KustomizationFile)
	}
	var kustomization struct {
		APIVersion string   `yaml:"apiVersion"`
		Kind       string   `yaml:"kind"`
		Resources  []string `yaml:"resources"`
	}
	if err := decodeStrict(data, MaxRequirementsBytes, &kustomization); err != nil {
		return nil, fmt.Errorf("%s: %w", KustomizationFile, err)
	}
	if kustomization.APIVersion != "kustomize.config.k8s.io/v1beta1" || kustomization.Kind != "Kustomization" || len(kustomization.Resources) == 0 {
		return nil, fmt.Errorf("%s is not a kustomization that lists its resources", KustomizationFile)
	}
	resources := map[string]map[string]any{}
	for _, name := range kustomization.Resources {
		file := BaseFolder + "/" + name
		if path.Base(name) != name || name == path.Base(KustomizationFile) || !slices.Contains(BaseFiles, file) {
			return nil, fmt.Errorf("%s lists %s, which is not a file of the base folder", KustomizationFile, name)
		}
		content, held := files[file]
		if !held {
			return nil, fmt.Errorf("%s lists %s, which the base does not hold", KustomizationFile, name)
		}
		if _, twice := resources[file]; twice {
			return nil, fmt.Errorf("%s lists %s twice", KustomizationFile, name)
		}
		var resource map[string]any
		if err := decodeStrict(content, MaxRequirementsBytes, &resource); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		metadata, _ := resource["metadata"].(map[string]any)
		if resource["apiVersion"] == nil || metadata == nil || metadata["name"] == nil {
			return nil, fmt.Errorf("%s is not a Kubernetes resource with an apiVersion, a kind and a name", file)
		}
		if kind, _ := resource["kind"].(string); !slices.Contains(baseKinds, kind) {
			return nil, fmt.Errorf("%s holds the kind %v, which is not one of %s", file, resource["kind"], strings.Join(baseKinds, ", "))
		}
		if resource["kind"] == "Deployment" {
			if err := checkEnvironmentEntries(content); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
		}
		resources[file] = resource
	}
	if len(resources) != inBase-1 {
		return nil, fmt.Errorf("%s does not list each file of the base", KustomizationFile)
	}
	if err := loadSecretFiles(files); err != nil {
		return nil, err
	}
	return resources, nil
}

// keyReference is a reference to one key of a Secret or of a ConfigMap, with an optional flag that may only be true.
type keyReference struct {
	Name     string `yaml:"name"`
	Key      string `yaml:"key"`
	Optional *bool  `yaml:"optional,omitempty"`
}

// containerVariable is the one shape of an environment entry of the Deployment of a base: a name and a valueFrom
// with a key reference. The strict decoder refuses each other field, such as a value or a field reference.
type containerVariable struct {
	Name      string `yaml:"name"`
	ValueFrom *struct {
		SecretKeyRef    *keyReference `yaml:"secretKeyRef"`
		ConfigMapKeyRef *keyReference `yaml:"configMapKeyRef"`
	} `yaml:"valueFrom"`
}

// checkEnvironmentEntries holds each environment entry of the Deployment of a base to names only (ST-DLV-UC07-R02,
// ST-DLV-Q18): a variable name and one reference to a key of a Secret or of a ConfigMap, with no value and no other
// field. A secret reference has no optional flag, because the secret need requires each of its keys; a ConfigMap
// reference may be optional. No variable has two entries.
func checkEnvironmentEntries(content []byte) error {
	type container struct {
		Name    string      `yaml:"name"`
		Env     []yaml.Node `yaml:"env"`
		EnvFrom []yaml.Node `yaml:"envFrom"`
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers          []container `yaml:"containers"`
					InitContainers      []container `yaml:"initContainers"`
					EphemeralContainers []container `yaml:"ephemeralContainers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(content, &deployment); err != nil {
		return err
	}
	// The base writes no envFrom, which takes every key of a ConfigMap or a Secret, also a name that the service
	// refuses, and no environment of an init or an ephemeral container (#370 security review L2).
	pod := deployment.Spec.Template.Spec
	for _, other := range append(slices.Clone(pod.InitContainers), pod.EphemeralContainers...) {
		if len(other.Env) > 0 || len(other.EnvFrom) > 0 {
			return fmt.Errorf("the container %q is an init or an ephemeral container with an environment, which the base never writes", other.Name)
		}
	}
	for _, container := range pod.Containers {
		if len(container.EnvFrom) > 0 {
			return fmt.Errorf("the container %q takes its environment from envFrom, which the base never writes; each variable is one env entry", container.Name)
		}
		seen := map[string]bool{}
		for _, node := range container.Env {
			text, err := yaml.Marshal(&node)
			if err != nil {
				return err
			}
			var entry containerVariable
			if err := decodeStrict(text, MaxRequirementsBytes, &entry); err != nil {
				return fmt.Errorf("an environment entry has a field that the base never writes, such as a value: %w", err)
			}
			if !variableName.MatchString(entry.Name) || seen[entry.Name] {
				return fmt.Errorf("the environment entry %q is not a variable name, or is given twice", entry.Name)
			}
			seen[entry.Name] = true
			from := entry.ValueFrom
			if from == nil || (from.SecretKeyRef == nil) == (from.ConfigMapKeyRef == nil) {
				return fmt.Errorf("the environment entry %s does not take its value from one key of a Secret or of a ConfigMap", entry.Name)
			}
			reference := from.ConfigMapKeyRef
			if from.SecretKeyRef != nil {
				reference = from.SecretKeyRef
				if reference.Optional != nil {
					return fmt.Errorf("the environment entry %s takes its value from a Secret with an optional flag; the secret need requires each of its keys", entry.Name)
				}
			}
			if !serviceLabel.MatchString(reference.Name) || !shapeWord.MatchString(reference.Key) {
				return fmt.Errorf("the environment entry %s does not name a Secret or a ConfigMap and a key", entry.Name)
			}
			if reference.Optional != nil && !*reference.Optional {
				return fmt.Errorf("the environment entry %s states optional: false; a required entry states no optional flag", entry.Name)
			}
		}
	}
	return nil
}

// secretFilesKustomization is the shape of the kustomization of the Component secret-files.
type secretFilesKustomization struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Patches    []struct {
		Path string `yaml:"path"`
	} `yaml:"patches"`
}

// secretFilesPatch is the shape of each patch of the Component secret-files: a patch of the Deployment of the
// service that changes the environment entries, the volume mounts of its container and the volumes of its pod.
type secretFilesPatch struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name  string  `yaml:"name"`
						Value *string `yaml:"value"`
						Patch string  `yaml:"$patch"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret *struct {
						SecretName  string `yaml:"secretName"`
						DefaultMode *int   `yaml:"defaultMode"`
						Items       []struct {
							Key  string `yaml:"key"`
							Path string `yaml:"path"`
						} `yaml:"items"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// loadSecretFiles holds the Component secret-files, when the base has one, to its exact shape (ST-DLV-Q18): a
// kustomization of the kind Component that lists the removal patch and then the mount patch and nothing else; and
// two patches of the Deployment, each of one container. The removal patch only deletes environment entries. The
// mount patch only sets each *_FILE variable to the path of a file under SecretFilesRoot, mounts each secret
// read-only at its folder there, and adds a secret volume with the mode 0400 for each mount. No patch names a
// variable twice, and the mount patch sets no variable that the removal patch keeps.
func loadSecretFiles(files map[string][]byte) error {
	component := []string{SecretFilesKustomization, SecretFilesRemoval, SecretFilesMounts}
	held := 0
	for _, file := range component {
		if _, ok := files[file]; ok {
			held++
		}
	}
	if held == 0 {
		return nil
	}
	if held != len(component) {
		return fmt.Errorf("the Component %s does not hold each of its files", SecretFilesFolder)
	}
	var kustomization secretFilesKustomization
	if err := decodeStrict(files[SecretFilesKustomization], MaxRequirementsBytes, &kustomization); err != nil {
		return fmt.Errorf("%s: %w", SecretFilesKustomization, err)
	}
	if kustomization.APIVersion != "kustomize.config.k8s.io/v1alpha1" || kustomization.Kind != "Component" || len(kustomization.Patches) != 2 ||
		kustomization.Patches[0].Path != path.Base(SecretFilesRemoval) || kustomization.Patches[1].Path != path.Base(SecretFilesMounts) {
		return fmt.Errorf("%s is not a Component that lists %s and then %s", SecretFilesKustomization, path.Base(SecretFilesRemoval), path.Base(SecretFilesMounts))
	}
	var patches [2]secretFilesPatch
	for i, file := range []string{SecretFilesRemoval, SecretFilesMounts} {
		if err := decodeStrict(files[file], MaxRequirementsBytes, &patches[i]); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		patch := patches[i]
		if patch.APIVersion != "apps/v1" || patch.Kind != "Deployment" || !serviceLabel.MatchString(patch.Metadata.Name) || len(patch.Spec.Template.Spec.Containers) != 1 ||
			patch.Spec.Template.Spec.Containers[0].Name != patch.Metadata.Name {
			return fmt.Errorf("%s is not a patch of the one container of the Deployment of the service", file)
		}
	}
	if patches[0].Metadata.Name != patches[1].Metadata.Name {
		return fmt.Errorf("the patches of %s are not of one Deployment", SecretFilesFolder)
	}
	removal, mount := patches[0].Spec.Template.Spec, patches[1].Spec.Template.Spec
	removed := map[string]bool{}
	if len(removal.Volumes) > 0 || len(removal.Containers[0].VolumeMounts) > 0 || len(removal.Containers[0].Env) == 0 {
		return fmt.Errorf("%s does more than remove environment entries", SecretFilesRemoval)
	}
	for _, entry := range removal.Containers[0].Env {
		if entry.Patch != "delete" || entry.Value != nil || !variableName.MatchString(entry.Name) || removed[entry.Name] {
			return fmt.Errorf("%s does more than remove each environment entry once: %q", SecretFilesRemoval, entry.Name)
		}
		removed[entry.Name] = true
	}
	volumes := map[string]bool{}
	// projected holds each key that a volume projects, as secret/key; each must be the file of one set variable.
	projected := map[string]bool{}
	for _, volume := range mount.Volumes {
		if !serviceLabel.MatchString(volume.Name) || volume.Name == "tmp" || volumes[volume.Name] || volume.Secret == nil || volume.Secret.SecretName != volume.Name ||
			volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != SecretFileMode || len(volume.Secret.Items) == 0 {
			return fmt.Errorf("%s holds the volume %q, which is not one secret volume of the mode 0400 named by its secret, with its keys", SecretFilesMounts, volume.Name)
		}
		for _, item := range volume.Secret.Items {
			if item.Path != item.Key || !shapeWord.MatchString(item.Key) || projected[volume.Name+"/"+item.Key] {
				return fmt.Errorf("%s projects %q of %s, which is not one key at the path of its name", SecretFilesMounts, item.Key, volume.Name)
			}
			projected[volume.Name+"/"+item.Key] = true
		}
		volumes[volume.Name] = true
	}
	mounted := map[string]bool{}
	for _, volumeMount := range mount.Containers[0].VolumeMounts {
		if !volumes[volumeMount.Name] || mounted[volumeMount.Name] || volumeMount.MountPath != SecretFilesRoot+"/"+volumeMount.Name || !volumeMount.ReadOnly {
			return fmt.Errorf("%s mounts %q, which is not one read-only secret volume at its folder under %s", SecretFilesMounts, volumeMount.Name, SecretFilesRoot)
		}
		mounted[volumeMount.Name] = true
	}
	if len(mounted) != len(volumes) || len(mount.Containers[0].Env) == 0 {
		return fmt.Errorf("%s does not mount each volume it adds, or sets no variable", SecretFilesMounts)
	}
	set, used := map[string]bool{}, map[string]bool{}
	for _, entry := range mount.Containers[0].Env {
		if entry.Patch != "" || entry.Value == nil || !variableName.MatchString(entry.Name) || set[entry.Name] {
			return fmt.Errorf("%s does more than set each file variable once: %q", SecretFilesMounts, entry.Name)
		}
		folder, key := path.Split(*entry.Value)
		if !mounted[path.Base(folder)] || path.Clean(folder) != SecretFilesRoot+"/"+path.Base(folder) || !shapeWord.MatchString(key) ||
			!projected[path.Base(folder)+"/"+key] {
			return fmt.Errorf("%s sets %s to a path that is not a projected key of a mounted secret", SecretFilesMounts, entry.Name)
		}
		set[entry.Name] = true
		used[path.Base(folder)+"/"+key] = true
	}
	for key := range projected {
		if !used[key] {
			return fmt.Errorf("%s projects the key %s, which no variable that it sets reads", SecretFilesMounts, key)
		}
	}
	for name := range set {
		// The mount patch sets the *_FILE form of a removed secret variable, or a removed path variable itself: a
		// path variable that the base wires from the ConfigMap must be removed first, or the two entries merge.
		if !removed[name] && !removed[strings.TrimSuffix(name, "_FILE")] {
			return fmt.Errorf("%s sets %s, but %s does not remove the environment form", SecretFilesMounts, name, SecretFilesRemoval)
		}
	}
	return nil
}
