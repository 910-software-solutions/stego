package delivery

import (
	"bytes"
	"errors"
	"fmt"
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

// BaseFiles are the paths of every file that the Kubernetes base may hold, in the order of the kustomization file.
var BaseFiles = []string{KustomizationFile, ServiceAccountFile, DeploymentFile, ServiceFile, RoleFile, RoleBindingFile}

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
const NoDeliveryStanza = "the declaration has no delivery stanza"

// NoBase is the plan line of a run whose active set serves no probes: such a service is not deployed as a
// Deployment, so the run writes no Kubernetes base.
const NoBase = "no Kubernetes base is written: no component of the service serves the health probes"

// noProbePort is the plan line of a run whose platform requirements give no default port for the probes: the base
// cannot carry its probes (ST-DLV-UC07-R03), so the run writes none.
const noProbePort = "no Kubernetes base is written: the platform requirements give no default port for the probes in %s"

// WriteDeployment writes the Kubernetes base of the service from its platform requirements and its probes
// (ST-DLV-CMD03), and then, as the base is written, the delivery object (ST-DLV-PO04). A declaration has no
// delivery stanza (ST-DLV-Q12), so the run writes no delivery object and says so in the plan (ST-DLV-UC07-R05).
func WriteDeployment(service string, requirements []byte, probes *ProbeSource) (DeploymentFiles, error) {
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
	return DeploymentFiles{Files: base.Files, Notes: []string{fmt.Sprintf(ImageNote, ImagePlaceholder(service)), deliveryObjectFor(written)}}, nil
}

// deliveryObjectFor is the policy Write the delivery object for a written base (ST-DLV-PO04): it hears the base of
// the service written and states the delivery object of the run. No declaration has a delivery stanza yet
// (ST-DLV-Q12), so it writes none and gives the plan line that says so, with no destination, project or path.
func deliveryObjectFor(written BaseWritten) string {
	return fmt.Sprintf("no delivery object (Argo CD ApplicationSet) is written for the base of %s: %s", written.Service, NoDeliveryStanza)
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
	base := Base{Service: service, Image: ImagePlaceholder(service), Files: files}
	if _, err := LoadBase(base.Files); err != nil {
		return Base{}, BaseWritten{}, fmt.Errorf("the Kubernetes base is not a plain kustomize base: %w", err)
	}
	return base, BaseWritten{Service: service, Base: BaseFolder}, nil
}

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
	var out bytes.Buffer
	out.WriteString(baseHeader)
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
// ST-DLV-INV09): each file is one of the base folder; the kustomization file has only its apiVersion, its kind and
// its resources, so it names no generator, transformer, Helm chart or remote base; each resource is a file of the
// base folder that the base holds; and each resource file is one YAML document with an apiVersion, a kind and a
// name. The tests build the base with the kustomize library itself; the compiler does not carry it.
func LoadBase(files map[string][]byte) (map[string]map[string]any, error) {
	for file := range files {
		if path.Dir(file) != BaseFolder || !slices.Contains(BaseFiles, file) {
			return nil, fmt.Errorf("%s is not a file of the Kubernetes base", file)
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
		resources[file] = resource
	}
	if len(resources) != len(files)-1 {
		return nil, fmt.Errorf("%s does not list each file of the base", KustomizationFile)
	}
	return resources, nil
}
