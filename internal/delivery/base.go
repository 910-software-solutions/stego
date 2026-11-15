package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
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
	"# requirements, with no value that differs per environment: the platform's overlay supplies them. Do not edit it.\n"

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
	return DeploymentFiles{Files: base.Files, Notes: []string{deliveryObjectFor(written)}}, nil
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
// secret value (ST-DLV-INV08), and it builds the base with plain kustomize before it gives it (ST-DLV-INV09).
func WriteBase(service string, requirements []byte, probes ProbeSource) (Base, BaseWritten, error) {
	stated, err := LoadRequirements(requirements)
	if err != nil {
		return Base{}, BaseWritten{}, err
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
	restricted := false
	for _, requirement := range stated.Requirements {
		switch requirement.Kind {
		case Ingress:
			ingress = append(ingress, requirement)
		case ClusterPermission:
			permissions = append(permissions, requirement)
		case PodSecurity:
			restricted = restricted || requirement.Level == "restricted"
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
		"image":          service,
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
	pod := map[string]any{"serviceAccountName": service, "automountServiceAccountToken": len(permissions) > 0, "containers": []any{container}}
	if restricted {
		// The settings that the restricted Pod Security Standard asks of a pod and its containers.
		pod["securityContext"] = map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
		container["securityContext"] = map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	}
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
			rules = append(rules, map[string]any{"apiGroups": []any{requirement.APIGroup}, "resources": []any{requirement.Resource}, "verbs": verbs})
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
	base := Base{Service: service, Image: service, Files: files}
	if _, err := LoadBase(base.Files); err != nil {
		return Base{}, BaseWritten{}, fmt.Errorf("the Kubernetes base does not build with plain kustomize: %w", err)
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

// LoadBase builds the files of a Kubernetes base with the kustomize library compiled into the compiler, with
// plugins off and in a file system that holds the base alone, so that no remote base, path out of the base or
// generator program can build (ST-DLV-REPO03, ST-DLV-INV09). It gives the built resources as one YAML stream.
func LoadBase(files map[string][]byte) ([]byte, error) {
	fs := filesys.MakeFsInMemory()
	root := "/" + BaseFolder
	for file, data := range files {
		if path.Dir(file) != BaseFolder || !slices.Contains(BaseFiles, file) {
			return nil, fmt.Errorf("%s is not a file of the Kubernetes base", file)
		}
		if err := fs.WriteFile("/"+file, data); err != nil {
			return nil, err
		}
	}
	if _, ok := files[KustomizationFile]; !ok {
		return nil, fmt.Errorf("the Kubernetes base has no %s", KustomizationFile)
	}
	options := krusty.MakeDefaultOptions()
	options.PluginConfig = types.DisabledPluginConfig()
	options.LoadRestrictions = types.LoadRestrictionsRootOnly
	built, err := krusty.MakeKustomizer(options).Run(fs, root)
	if err != nil {
		return nil, err
	}
	return built.AsYaml()
}
