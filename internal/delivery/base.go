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
	// WorkerDeploymentFile holds the Deployment <service>-worker of the worker and controller modes, when the base
	// splits the modes (ST-DLV-UC07-R07).
	WorkerDeploymentFile = BaseFolder + "/worker-deployment.yaml"
	// WorkerServiceAccountFile holds the ServiceAccount <service>-worker of the worker Deployment, so that the pods of
	// the background modes do not run as the identity of the api mode (security review of PR #424).
	WorkerServiceAccountFile = BaseFolder + "/worker-service-account.yaml"
	ServiceFile              = BaseFolder + "/service.yaml"
	RoleFile                 = BaseFolder + "/role.yaml"
	RoleBindingFile          = BaseFolder + "/role-binding.yaml"
	MigrationJobFile         = BaseFolder + "/migration-job.yaml"
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
	// The two patches of the migration Job, of the same shapes as those of the Deployment (ST-DLV-Q20).
	SecretFilesJobRemoval = SecretFilesFolder + "/remove-job-env.yaml"
	SecretFilesJobMounts  = SecretFilesFolder + "/mount-job-files.yaml"
	// The two patches of the worker Deployment, of the same shapes as those of the Deployment (ST-DLV-UC07-R07).
	SecretFilesWorkerRemoval = SecretFilesFolder + "/remove-worker-env.yaml"
	SecretFilesWorkerMounts  = SecretFilesFolder + "/mount-worker-files.yaml"
)

// FSGroupFolder is the folder, in the project folder, of the second opt-in kustomize Component, which sets the pod
// fsGroup of the Deployment and of the migration Job to FSGroup, so that the process reads the files of the Component
// secret-files on a cluster that does not set an fsGroup itself. The restricted-v2 SCC of OpenShift gives each
// namespace its own fsGroup and refuses a pod that sets another one, so the base sets none, and an overlay on
// OpenShift never lists this Component.
const FSGroupFolder = "deploy/components/fs-group"

// The files of the Component fs-group: its kustomization, the patch of the Deployment and, when the base has a
// migration Job, the patch of the Job.
const (
	FSGroupKustomization = FSGroupFolder + "/kustomization.yaml"
	FSGroupDeployment    = FSGroupFolder + "/deployment-fs-group.yaml"
	FSGroupJob           = FSGroupFolder + "/job-fs-group.yaml"
	FSGroupWorker        = FSGroupFolder + "/worker-deployment-fs-group.yaml"
)

// FSGroup is the fsGroup that the Component fs-group sets: the user and the group of the image (WriteContainerfile).
const FSGroup = 65532

// SecretFilesRoot is the folder in the container under which the Component mounts each secret, one folder for each
// secret, named by the secret, with one file for each key.
const SecretFilesRoot = "/var/run/secrets/stego"

// SecretFileMode is the mode of each file that the Component mounts: 0400, read by the owner only.
const SecretFileMode = 0o400

// BaseFiles are the paths of every file that the Kubernetes base may hold, in the order of the kustomization file,
// and then the files of the Component secret-files and of the Component fs-group.
var BaseFiles = []string{KustomizationFile, ServiceAccountFile, DeploymentFile, WorkerServiceAccountFile, WorkerDeploymentFile, ServiceFile, RoleFile, RoleBindingFile, MigrationJobFile,
	SecretFilesKustomization, SecretFilesRemoval, SecretFilesMounts, SecretFilesWorkerRemoval, SecretFilesWorkerMounts, SecretFilesJobRemoval, SecretFilesJobMounts,
	FSGroupKustomization, FSGroupDeployment, FSGroupWorker, FSGroupJob}

// MigrateMode is the mode of the migration job: the argument of the service binary in the migration Job, and the
// value of read_by_mode of a variable that only that mode reads (ST-DLV-Q19, ST-DLV-Q20).
const MigrateMode = "migrate"

// WorkerMode is the value of read_by_mode of a variable that only the worker mode reads, such as the connection of
// the worker role or a Kafka setting: the Deployment of the api mode wires none of them (ST-DLV-UC07-R07).
const WorkerMode = "worker"

// APIMode is the value of read_by_mode of a variable that only the api mode reads, such as the CORS settings of the
// rest-api component: the Deployment of the worker modes and the migration Job wire none of them (ST-DLV-UC07-R06,
// ST-DLV-UC07-R07).
const APIMode = "api"

// ModeVariable selects the modes of a process when no mode option is given. A Deployment of a split base selects its
// modes with the option --mode in its args, so it wires no entry of this variable.
const ModeVariable = "STEGO_MODE"

// WorkerReadiness is the readiness probe of the Deployment that runs only the worker and controller modes: the
// readiness of the modes of the service runtime (ST-DLV-UC07-R03, ST-SUP-RP09-LC05).
const WorkerReadiness = "/readyz/modes"

// The sides of the modes of a Deployment, which give its environment entries: every mode, the api mode alone, or
// the worker and controller modes alone.
const (
	allModes   = ""
	apiSide    = "api"
	workerSide = "worker"
)

// workerName is the name of the Deployment of the worker and controller modes of a split base.
func workerName(service string) string { return service + "-worker" }

// modeSides tells which Deployments the declared modes need (ST-DLV-UC07-R07): with the api mode and a background
// mode, two; else one, with the readiness of its side. No modes means a binary with no mode selection: one
// Deployment with the probes of health-check.
func modeSides(modes []string) (split, backgroundOnly bool) {
	api := slices.Contains(modes, "api")
	background := slices.Contains(modes, "worker") || slices.Contains(modes, "controller")
	return api && background, !api && background
}

// modeArgs gives the args of a Deployment of a split base: one --mode option for each of its declared modes.
func modeArgs(modes []string, side string) []any {
	var args []any
	for _, mode := range modes {
		if (side == apiSide) == (mode == "api") {
			args = append(args, "--mode="+mode)
		}
	}
	return args
}

// OwnerSecret is the name of the Secret of the credential of the owner role, which only the migration Job
// references (ST-DLV-Q20, ST-DLV-INV13).
func OwnerSecret(service string) string {
	return service + "-database-owner"
}

// MigrationKeyFiles are the path variables of a secret file that the migrate mode reads besides its own variables:
// the preshared key of SpiceDB, to which the migration job writes the authorization schema (ReadMigrationSettings of
// the service runtime). The Job gets no path of another secret file, so the Component mounts no other key in it.
//
// Each has the secret need and the key of its file, with {service}: LoadBase holds the mount patch of the Job to them
// and to the key url of the owner Secret alone (ST-DLV-INV13).
var MigrationKeyFiles = map[string]SecretSource{"STEGO_SPICEDB_KEY_FILE": {Name: "{service}-spicedb", Key: "preshared-key"}}

// migrationKeyFile gives the Secret and the key of a key file of MigrationKeyFiles for the service, and whether the
// variable is one.
func migrationKeyFile(service, variable string) (SecretSource, bool) {
	source, ok := MigrationKeyFiles[variable]
	if !ok {
		return SecretSource{}, false
	}
	return SecretSource{Name: strings.ReplaceAll(source.Name, "{service}", service), Key: source.Key}, true
}

// The settings of the migration Job (ST-DLV-UC07-R06). The job runs its schema steps within 10 minutes and its
// backfills within one hour (MigrationTimeout and BackfillTimeout of the service runtime), so two hours hold one run
// and its retries. A finished Job stays ten minutes and then Kubernetes deletes it, so that the next plain apply of a
// new image makes it again and does not fail on its pod template; its logs stay in the logging of the cluster, and
// Argo CD deletes it by its hook policy.
const (
	MigrationBackoffLimit = 2
	MigrationDeadline     = 7200
	MigrationTimeToLive   = 600
)

// releaseSuffixLength is the length of the suffix of the name of the migration Job: the first hex digits of the
// digest of the release.
const releaseSuffixLength = 10

// releaseDigest is the digest of a release that names its migration Job: lower-case hex digits.
var releaseDigest = regexp.MustCompile(`^[0-9a-f]{10,}$`)

// MigrationJobName is the name of the migration Job of a release: the service, -migrate- and the first ten hex
// digits of the digest of the schema of the release (its migrations, its schema manifest and its runtime pin). A new
// apply cannot change the pod template of a Job, so each release that changes the schema gets a Job of a new name. Kubernetes also writes the name as the value of
// the label job-name on each pod of the Job, so the name is a DNS label of at most 63 characters.
func MigrationJobName(service, release string) (string, error) {
	if !releaseDigest.MatchString(release) {
		return "", fmt.Errorf("the digest of the release %q is not ten or more lower-case hex digits", release)
	}
	name := service + "-migrate-" + release[:releaseSuffixLength]
	if len(name) > 63 || !serviceLabel.MatchString(name) {
		return "", fmt.Errorf("the name of the migration Job %s has %d characters, more than the 63 of a Kubernetes label value; use a service name of at most %d characters",
			name, len(name), 63-len("-migrate-")-releaseSuffixLength)
	}
	return name, nil
}

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

// objectName is the name of a Secret or a ConfigMap: a DNS subdomain of lower-case letters, digits, '-' and '.', of
// at most maxObjectName characters.
var objectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// maxObjectName bounds the name of a Secret or a ConfigMap.
const maxObjectName = 253

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
//
// release is the digest of the schema of the release, in hex, which names the migration Job (MigrationJobName).
func WriteDelivery(service, release string, requirements []byte, probes *ProbeSource, modes []string, source ContainerSource, stanza *ArgoCDStanza, provider string, pins map[string]PipelinePins) (DeploymentFiles, error) {
	if provider != "" && provider != GitHubActions && provider != Konflux {
		return DeploymentFiles{}, &StanzaError{Path: "delivery.ci", Reason: fmt.Sprintf("the CI provider %q is not %s or %s", provider, GitHubActions, Konflux)}
	}
	written, err := WriteDeployment(service, release, requirements, probes, modes, source, stanza)
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
func WriteDeployment(service, release string, requirements []byte, probes *ProbeSource, modes []string, source ContainerSource, stanza *ArgoCDStanza) (DeploymentFiles, error) {
	if probes == nil {
		return DeploymentFiles{Notes: []string{NoBase}}, nil
	}
	base, written, err := WriteBase(service, release, requirements, *probes, modes)
	if errors.Is(err, errNoProbePort) {
		return DeploymentFiles{Notes: []string{fmt.Sprintf(noProbePort, probes.PortVariable)}}, nil
	}
	if err != nil {
		return DeploymentFiles{}, err
	}
	modules, moduleNotes := ModuleFolders(source.Replacements, source.OutputDir, source.Fills)
	source.Modules = modules
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
	notes = append(notes, moduleNotes...)
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
// secret value (ST-DLV-INV08), and it reads the base back with LoadBase before it gives it (ST-DLV-INV09). When the
// migrate mode alone reads a variable of the service, the base also holds the migration Job of the release, named
// from the digest release (migrationJob, ST-DLV-UC07-R06).
func WriteBase(service, release string, requirements []byte, probes ProbeSource, modes []string) (Base, BaseWritten, error) {
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

	split, backgroundOnly := modeSides(modes)
	readiness := probes.Readiness
	if backgroundOnly {
		readiness = WorkerReadiness
	}
	// workload gives the container and the pod of one Deployment: its readiness, its args, its ports and its
	// environment entries of its side of the modes. The base always writes the settings of the restricted Pod
	// Security Standard and a read-only root file system, whatever level the requirements state; an overlay may
	// relax them, the base never does (ST-DLV-Q14). The process writes its temporary files to an emptyDir at /tmp.
	workload := func(side, account, readiness string, ports []any) (map[string]any, []secretFile) {
		container := map[string]any{
			"name":           service,
			"image":          ImagePlaceholder(service),
			"readinessProbe": httpProbe(readiness, port),
			"livenessProbe":  httpProbe(probes.Liveness, port),
			"resources":      ResourceDefaults,
		}
		if split {
			container["args"] = modeArgs(modes, side)
		}
		if len(ports) > 0 {
			container["ports"] = ports
		}
		container["securityContext"] = map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
			"readOnlyRootFilesystem": true}
		container["volumeMounts"] = []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}}
		// One environment entry, names only, for each variable that the modes of the Deployment read
		// (ST-DLV-UC07-R03, ST-DLV-UC07-R07, ST-DLV-Q18).
		environment, switched := baseEnvironment(service, stated.Environment, side, split)
		if len(environment) > 0 {
			container["env"] = environment
		}
		return map[string]any{"serviceAccountName": account, "automountServiceAccountToken": len(permissions) > 0, "containers": []any{container},
			"securityContext": map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"volumes":         []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": TmpSizeLimit}}}}, switched
	}
	var ports []any
	for _, requirement := range ingress {
		ports = append(ports, containerPort(requirement))
	}
	// A Deployment that runs only the api mode wires no variable of the worker mode, and one that runs only the
	// worker and controller modes wires no variable of the api mode.
	side := allModes
	if split || (slices.Contains(modes, "api") && !backgroundOnly && len(modes) == 1) {
		side = apiSide
	}
	if backgroundOnly {
		side = workerSide
	}
	pod, switched := workload(side, service, readiness, ports)
	if err := add(DeploymentFile, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata,
		"spec": map[string]any{"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod}}}); err != nil {
		return Base{}, BaseWritten{}, err
	}
	// The Deployment of the worker and controller modes (ST-DLV-UC07-R07): readiness /readyz/modes, no port that a
	// Service selects, and labels with another name, so that neither the selector of the Deployment of the api mode,
	// which never changes, nor that of the Service takes one of its pods.
	var workerSwitched []secretFile
	worker := ""
	if split {
		worker = workerName(service)
		if !serviceLabel.MatchString(worker) {
			return Base{}, BaseWritten{}, fmt.Errorf("the worker Deployment %s is not a Kubernetes name: a DNS label of at most 63 characters; use a service name of at most 56 characters", worker)
		}
		workerLabels := map[string]any{nameLabel: worker, partOfLabel: service, componentLabel: workerSide}
		// The worker Deployment runs as its own ServiceAccount, of the name of the Deployment: a token or an audit
		// line of a worker pod never names the identity of the api mode, and a platform can grant each side apart.
		if err := add(WorkerServiceAccountFile, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": worker, "labels": workerLabels}, "automountServiceAccountToken": len(permissions) > 0}); err != nil {
			return Base{}, BaseWritten{}, err
		}
		var workerPod map[string]any
		workerPod, workerSwitched = workload(workerSide, worker, WorkerReadiness, nil)
		if err := add(WorkerDeploymentFile, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": worker, "labels": workerLabels},
			"spec": map[string]any{"selector": map[string]any{"matchLabels": workerLabels},
				"template": map[string]any{"metadata": map[string]any{"labels": workerLabels}, "spec": workerPod}}}); err != nil {
			return Base{}, BaseWritten{}, err
		}
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
		// The subject names no namespace: the overlay's namespace sets it. A cluster permission states no mode, so
		// the Role binds the ServiceAccount of each Deployment.
		subjects := []any{map[string]any{"kind": "ServiceAccount", "name": service}}
		if worker != "" {
			subjects = append(subjects, map[string]any{"kind": "ServiceAccount", "name": worker})
		}
		if err := add(RoleBindingFile, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata,
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": service},
			"subjects": subjects}); err != nil {
			return Base{}, BaseWritten{}, err
		}
	}

	var jobSwitched []secretFile
	jobName := ""
	if migrates(stated.Environment) {
		if jobName, err = MigrationJobName(service, release); err != nil {
			return Base{}, BaseWritten{}, err
		}
		var job map[string]any
		job, jobSwitched = migrationJob(service, jobName, stated.Environment)
		if err := add(MigrationJobFile, job); err != nil {
			return Base{}, BaseWritten{}, err
		}
	}

	kustomization := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
	data, err := encodeResource(kustomization)
	if err != nil {
		return Base{}, BaseWritten{}, err
	}
	files[KustomizationFile] = data
	if len(switched) > 0 || len(workerSwitched) > 0 || len(jobSwitched) > 0 {
		component, err := secretFilesComponent(service, worker, jobName, switched, workerSwitched, jobSwitched)
		if err != nil {
			return Base{}, BaseWritten{}, err
		}
		maps.Copy(files, component)
		fsGroup, err := fsGroupComponent(service, worker, jobName)
		if err != nil {
			return Base{}, BaseWritten{}, err
		}
		maps.Copy(files, fsGroup)
	}
	base := Base{Service: service, Image: ImagePlaceholder(service), Files: files}
	if _, err := LoadBase(base.Files); err != nil {
		return Base{}, BaseWritten{}, fmt.Errorf("the Kubernetes base is not a plain kustomize base: %w", err)
	}
	return base, BaseWritten{Service: service, Base: BaseFolder}, nil
}

// migrates tells whether the migrate mode alone reads a variable of the service: then the service has a migration
// job, and the base writes its Job.
func migrates(list []AcceptedVariable) bool {
	for _, variable := range list {
		if variable.ReadByMode == MigrateMode {
			return true
		}
	}
	return false
}

// The labels of the migration Job and its pods. The Job has the name of the service, so that one stable selector,
// app.kubernetes.io/component=migration,app.kubernetes.io/name=<service>, finds it for kubectl delete. Its pods have
// no app.kubernetes.io/name, so that the selector of the Deployment and of the Service never takes a pod of the Job.
const (
	nameLabel      = "app.kubernetes.io/name"
	partOfLabel    = "app.kubernetes.io/part-of"
	componentLabel = "app.kubernetes.io/component"
	migrationLabel = "migration"
)

// The Argo CD annotations of the migration Job: it runs as a PreSync hook in the sync wave 0, before Argo CD syncs
// the Deployment, so that an overlay can give its Secrets and its ConfigMap an earlier PreSync wave, such as -1; a
// new sync deletes the Job of the sync before, and a Job that passed is deleted. Plain Kubernetes ignores them.
const (
	HookAnnotation       = "argocd.argoproj.io/hook"
	HookDeleteAnnotation = "argocd.argoproj.io/hook-delete-policy"
	SyncWaveAnnotation   = "argocd.argoproj.io/sync-wave"
	HookPhase            = "PreSync"
	HookDeletePolicy     = "BeforeHookCreation,HookSucceeded"
	MigrationSyncWave    = "0"
)

// migrationJob writes the migration Job of the release (ST-DLV-UC07-R06, ST-DLV-Q20): a batch/v1 Job that runs the
// service binary with the argument migrate, as a PreSync hook of Argo CD, with a retry limit, a time limit and a time
// to live, the restricted pod security and the resource defaults of the Deployment, and no service account token. It
// runs as the default service account of the namespace, because Argo CD runs the hook before it syncs the service
// account of the base. It gives the environment entries of jobEnvironment, and the variables that the Component
// secret-files gives the Job as files.
func migrationJob(service, name string, list []AcceptedVariable) (map[string]any, []secretFile) {
	labels := map[string]any{partOfLabel: service, componentLabel: migrationLabel}
	jobLabels := map[string]any{nameLabel: service, partOfLabel: service, componentLabel: migrationLabel}
	container := map[string]any{
		"name":      service,
		"image":     ImagePlaceholder(service),
		"args":      []any{MigrateMode},
		"resources": ResourceDefaults,
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
			"readOnlyRootFilesystem": true},
		"volumeMounts": []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}},
	}
	environment, switched := jobEnvironment(service, list)
	if len(environment) > 0 {
		container["env"] = environment
	}
	pod := map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "containers": []any{container},
		"securityContext": map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes":         []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": TmpSizeLimit}}}}
	return map[string]any{"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "labels": jobLabels,
			"annotations": map[string]any{HookAnnotation: HookPhase, HookDeleteAnnotation: HookDeletePolicy, SyncWaveAnnotation: MigrationSyncWave}},
		"spec": map[string]any{"backoffLimit": MigrationBackoffLimit, "activeDeadlineSeconds": MigrationDeadline,
			"ttlSecondsAfterFinished": MigrationTimeToLive,
			"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod}}}, switched
}

// jobEnvironment gives the environment entries of the migration Job, in name order, with names only, and the
// variables that the Component secret-files gives the Job as files. A secret variable gets an entry only when the
// migrate mode alone reads it, so that the Job never holds the credential of a serving role; each other variable
// that is not secret takes its value from the ConfigMap as in the Deployment, except a path variable of a secret file
// that is not one of MigrationKeyFiles, so that the Job holds no path of a secret file that it does not read. The
// console sink, the variables that the start guard refuses and the *_FILE form of a sourced secret variable get no
// entry, as in the Deployment.
func jobEnvironment(service string, list []AcceptedVariable) ([]any, []secretFile) {
	sourced := map[string]bool{}
	listed := map[string]bool{}
	for _, variable := range list {
		listed[variable.Name] = true
		if variable.Secret && variable.SecretSource != nil {
			sourced[variable.Name] = true
		}
	}
	var entries []any
	var switched []secretFile
	for _, variable := range list {
		// A variable that only the worker or the api mode reads gets no entry: the migrate mode does not read it.
		if variable.Name == ConsoleSinkVariable || variable.RefusedAtStart || variable.ReadByMode == WorkerMode || variable.ReadByMode == APIMode {
			continue
		}
		if form, isFile := strings.CutSuffix(variable.Name, "_FILE"); isFile && sourced[form] {
			continue
		}
		switch {
		case variable.Secret && (variable.SecretSource == nil || variable.ReadByMode != MigrateMode):
			continue
		case variable.Secret:
			entries = append(entries, map[string]any{"name": variable.Name, "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": variable.SecretSource.Name, "key": variable.SecretSource.Key}}})
			if listed[variable.Name+"_FILE"] {
				switched = append(switched, secretFile{Removed: variable.Name, Variable: variable.Name + "_FILE", Source: *variable.SecretSource})
			}
			continue
		case variable.SecretSource != nil:
			// A key file that the migrate mode reads, from its own Secret and key alone.
			if source, ok := migrationKeyFile(service, variable.Name); !ok || source != *variable.SecretSource {
				continue
			}
		}
		reference := map[string]any{"name": ConfigMapName(service), "key": variable.Name}
		if !variable.Required {
			reference["optional"] = true
		}
		entries = append(entries, map[string]any{"name": variable.Name, "valueFrom": map[string]any{"configMapKeyRef": reference}})
		if variable.SecretSource != nil {
			switched = append(switched, secretFile{Removed: variable.Name, Variable: variable.Name, Source: *variable.SecretSource})
		}
	}
	return entries, switched
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
//
// side is the side of the modes of the Deployment (ST-DLV-UC07-R07): every mode, which wires the variables of the
// worker mode too; the api mode alone, which wires none of them; or the worker and controller modes alone. A
// Deployment of a split base selects its modes in its args, so it wires no entry of STEGO_MODE.
func baseEnvironment(service string, list []AcceptedVariable, side string, split bool) ([]any, []secretFile) {
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
		if variable.Name == ConsoleSinkVariable || variable.RefusedAtStart || variable.ReadByMode == MigrateMode ||
			(variable.ReadByMode == WorkerMode && side == apiSide) || (variable.ReadByMode == APIMode && side == workerSide) ||
			(split && variable.Name == ModeVariable) {
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

// secretFilesComponent writes the opt-in kustomize Component secret-files (ST-DLV-Q18): for the Deployment, and for
// the migration Job job when the base has one (ST-DLV-Q20), a patch that removes the environment entry of each
// switched variable, and a patch that sets each *_FILE variable to the path of its file and mounts each secret
// read-only, with the mode 0400, no subPath and only the keys that the switched variables read, so that a new value
// of the secret reaches the file in place. The forms are exclusive, so the first patch removes what the second
// replaces.
func secretFilesComponent(service, worker, job string, switched, workerSwitched, jobSwitched []secretFile) (map[string][]byte, error) {
	files := map[string][]byte{}
	var patches []any
	for _, target := range []struct {
		kind, apiVersion, name, removal, mounts string
		switched                                []secretFile
	}{
		{"Deployment", "apps/v1", service, SecretFilesRemoval, SecretFilesMounts, switched},
		{"Deployment", "apps/v1", worker, SecretFilesWorkerRemoval, SecretFilesWorkerMounts, workerSwitched},
		{"Job", "batch/v1", job, SecretFilesJobRemoval, SecretFilesJobMounts, jobSwitched},
	} {
		if len(target.switched) == 0 {
			continue
		}
		removal, mounts, err := secretFilesPatches(service, target.apiVersion, target.kind, target.name, target.switched)
		if err != nil {
			return nil, err
		}
		files[target.removal], files[target.mounts] = removal, mounts
		patches = append(patches, map[string]any{"path": path.Base(target.removal)}, map[string]any{"path": path.Base(target.mounts)})
	}
	data, err := encodeWith(secretFilesHeader, map[string]any{"apiVersion": "kustomize.config.k8s.io/v1alpha1", "kind": "Component", "patches": patches})
	if err != nil {
		return nil, err
	}
	files[SecretFilesKustomization] = data
	return files, nil
}

// secretFilesPatches writes the removal patch and the mount patch of one workload of the Component secret-files.
func secretFilesPatches(service, apiVersion, kind, name string, switched []secretFile) ([]byte, []byte, error) {
	var removed, set, mounts, volumes []any
	// The keys of each secret that a switched variable reads: the volume projects only these keys, so that no other
	// key of the Secret is a file of the pod (ST-DLV-Q19).
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
			return nil, nil, fmt.Errorf("the secret %s is not a Kubernetes volume name: a DNS label of at most 63 characters that is not tmp; use a shorter service name", secret)
		}
		mounts = append(mounts, map[string]any{"name": secret, "mountPath": SecretFilesRoot + "/" + secret, "readOnly": true})
		volumes = append(volumes, map[string]any{"name": secret, "secret": map[string]any{"secretName": secret, "defaultMode": SecretFileMode,
			"items": keys[secret]}})
	}
	patch := func(container map[string]any, pod map[string]any) map[string]any {
		container["name"] = service
		pod["containers"] = []any{container}
		return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name},
			"spec": map[string]any{"template": map[string]any{"spec": pod}}}
	}
	removal, err := encodeWith(secretFilesHeader, patch(map[string]any{"env": removed}, map[string]any{}))
	if err != nil {
		return nil, nil, err
	}
	mount, err := encodeWith(secretFilesHeader, patch(map[string]any{"env": set, "volumeMounts": mounts}, map[string]any{"volumes": volumes}))
	if err != nil {
		return nil, nil, err
	}
	return removal, mount, nil
}

// fsGroupComponent writes the opt-in kustomize Component fs-group: a patch that sets the pod fsGroup of the
// Deployment to FSGroup and, when the base has the migration Job job, the same patch of the Job. Kubernetes then
// gives the files of the Component secret-files, which have the mode 0400, to that group, so that the process, which
// is not root, reads them. The base writes it next to the Component secret-files, for clusters other than OpenShift.
func fsGroupComponent(service, worker, job string) (map[string][]byte, error) {
	files := map[string][]byte{}
	var patches []any
	for _, target := range []struct{ kind, apiVersion, name, file string }{
		{"Deployment", "apps/v1", service, FSGroupDeployment},
		{"Deployment", "apps/v1", worker, FSGroupWorker},
		{"Job", "batch/v1", job, FSGroupJob},
	} {
		if target.name == "" {
			continue
		}
		data, err := encodeWith(fsGroupHeader, map[string]any{"apiVersion": target.apiVersion, "kind": target.kind, "metadata": map[string]any{"name": target.name},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"securityContext": map[string]any{"fsGroup": FSGroup}}}}})
		if err != nil {
			return nil, err
		}
		files[target.file] = data
		patches = append(patches, map[string]any{"path": path.Base(target.file)})
	}
	data, err := encodeWith(fsGroupHeader, map[string]any{"apiVersion": "kustomize.config.k8s.io/v1alpha1", "kind": "Component", "patches": patches})
	if err != nil {
		return nil, err
	}
	files[FSGroupKustomization] = data
	return files, nil
}

// fsGroupHeader is the first lines of each file of the Component fs-group.
const fsGroupHeader = "# The opt-in kustomize Component fs-group of the service. stego apply writes this file next to the Component\n" +
	"# secret-files. It sets the pod fsGroup 65532 of the Deployment and of the migration Job, so that the process reads\n" +
	"# the secret files of the mode 0400. List it only on a cluster that sets no fsGroup itself. Never list it on\n" +
	"# OpenShift: the restricted-v2 SCC gives each namespace its own fsGroup and refuses a pod that sets another one.\n" +
	"# Do not edit it.\n"

// secretFilesHeader is the first lines of each file of the Component secret-files.
const secretFilesHeader = "# The opt-in kustomize Component secret-files of the service. stego apply writes this file from the platform\n" +
	"# requirements. An overlay that lists this folder under components gets each secret as a read-only file under\n" +
	"# " + SecretFilesRoot + " in place of an environment variable. Each file has the mode 0400: give the pod an\n" +
	"# fsGroup, as the restricted SCC of OpenShift does, or list the Component fs-group on another cluster, and do not\n" +
	"# widen the mode. Do not edit it.\n"

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
var baseKinds = []string{"ServiceAccount", "Deployment", "Service", "Role", "RoleBinding", "Job"}

// LoadBase reads the files of a Kubernetes base back and holds them to plain kustomize (ST-DLV-REPO03,
// ST-DLV-INV09): each file is one of the base folder or of the Component secret-files; the kustomization file has
// only its apiVersion, its kind and its resources, so it names no generator, transformer, Helm chart or remote
// base; each resource is a file of the base folder that the base holds; each resource file is one YAML document
// with an apiVersion, a kind and a name; each environment entry of the Deployment names a key of a Secret or of a
// ConfigMap and never holds a value (ST-DLV-Q18); the Deployment never references the Secret of the owner
// credential, and the migration Job, when the base has one, has its exact shape and references no other Secret
// (checkMigrationJob, ST-DLV-INV13); and the Component, when the base has one, is exactly a Component with its two
// patches of the Deployment and, with a Job, its two patches of the Job (loadSecretFiles). The tests build the base
// with the kustomize library itself; the compiler does not carry it.
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
	var service, worker, job string
	var servingSecrets []string
	var jobContent []byte
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
		switch resource["kind"] {
		case "Deployment":
			switch {
			case file == DeploymentFile && service == "":
				service, _ = metadata["name"].(string)
			case file == WorkerDeploymentFile && worker == "":
				worker, _ = metadata["name"].(string)
			default:
				return nil, fmt.Errorf("%s holds a second Deployment, or a Deployment out of %s and %s", file, DeploymentFile, WorkerDeploymentFile)
			}
			references, _, err := checkEnvironmentEntries(content)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			for _, reference := range references {
				if !slices.Contains(servingSecrets, reference.Name) {
					servingSecrets = append(servingSecrets, reference.Name)
				}
			}
			if err := checkDeploymentVolumes(content); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
		case "Job":
			if job != "" || file != MigrationJobFile {
				return nil, fmt.Errorf("%s holds a second Job, or a Job out of %s", file, MigrationJobFile)
			}
			job, _ = metadata["name"].(string)
			jobContent = content
		}
		resources[file] = resource
	}
	if len(resources) != inBase-1 {
		return nil, fmt.Errorf("%s does not list each file of the base", KustomizationFile)
	}
	if service == "" {
		return nil, fmt.Errorf("the Kubernetes base holds no Deployment in %s", DeploymentFile)
	}
	if worker != "" && worker != workerName(service) {
		return nil, fmt.Errorf("%s holds the Deployment %s, not %s", WorkerDeploymentFile, worker, workerName(service))
	}
	// The ServiceAccount of the worker Deployment is of its name, and only a base with a worker Deployment holds it.
	if account, held := resources[WorkerServiceAccountFile]; held {
		metadata, _ := account["metadata"].(map[string]any)
		if account["kind"] != "ServiceAccount" || worker == "" || metadata["name"] != workerName(service) {
			return nil, fmt.Errorf("%s holds no ServiceAccount %s of the worker Deployment", WorkerServiceAccountFile, workerName(service))
		}
	}
	// The serving Deployment never references the Secret of the owner credential (ST-DLV-INV13).
	for _, secret := range servingSecrets {
		if isOwnerSecret(service, secret) {
			return nil, fmt.Errorf("%s takes a variable from the Secret %s, the credential of the owner role, which only the migration Job may reference (ST-DLV-INV13)", DeploymentFile, secret)
		}
	}
	if job != "" {
		if err := checkMigrationJob(jobContent, service, job); err != nil {
			return nil, fmt.Errorf("%s: %w", MigrationJobFile, err)
		}
	}
	if err := loadSecretFiles(files, service, worker, job, servingSecrets); err != nil {
		return nil, err
	}
	if err := loadFSGroup(files, service, worker, job); err != nil {
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
// reference may be optional. No variable has two entries. It gives the references to the keys of the Secrets, and
// the names of the ConfigMaps, that the entries take, each once.
func checkEnvironmentEntries(content []byte) (secrets []keyReference, configMaps []string, err error) {
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
		return nil, nil, err
	}
	// The base writes no envFrom, which takes every key of a ConfigMap or a Secret, also a name that the service
	// refuses, and no environment of an init or an ephemeral container (#370 security review L2).
	pod := deployment.Spec.Template.Spec
	for _, other := range append(slices.Clone(pod.InitContainers), pod.EphemeralContainers...) {
		if len(other.Env) > 0 || len(other.EnvFrom) > 0 {
			return nil, nil, fmt.Errorf("the container %q is an init or an ephemeral container with an environment, which the base never writes", other.Name)
		}
	}
	for _, container := range pod.Containers {
		if len(container.EnvFrom) > 0 {
			return nil, nil, fmt.Errorf("the container %q takes its environment from envFrom, which the base never writes; each variable is one env entry", container.Name)
		}
		seen := map[string]bool{}
		for _, node := range container.Env {
			text, err := yaml.Marshal(&node)
			if err != nil {
				return nil, nil, err
			}
			var entry containerVariable
			if err := decodeStrict(text, MaxRequirementsBytes, &entry); err != nil {
				return nil, nil, fmt.Errorf("an environment entry has a field that the base never writes, such as a value: %w", err)
			}
			if !variableName.MatchString(entry.Name) || seen[entry.Name] {
				return nil, nil, fmt.Errorf("the environment entry %q is not a variable name, or is given twice", entry.Name)
			}
			seen[entry.Name] = true
			from := entry.ValueFrom
			if from == nil || (from.SecretKeyRef == nil) == (from.ConfigMapKeyRef == nil) {
				return nil, nil, fmt.Errorf("the environment entry %s does not take its value from one key of a Secret or of a ConfigMap", entry.Name)
			}
			reference := from.ConfigMapKeyRef
			if from.SecretKeyRef != nil {
				reference = from.SecretKeyRef
				if reference.Optional != nil {
					return nil, nil, fmt.Errorf("the environment entry %s takes its value from a Secret with an optional flag; the secret need requires each of its keys", entry.Name)
				}
			}
			// A Secret and a ConfigMap have a DNS subdomain name of at most 253 characters, not a DNS label: the
			// ConfigMap <service>-config of a service name of 57 or more characters is longer than 63 (#370 security
			// re-review L1). A Secret that the Component secret-files mounts is also a volume name, which
			// secretFilesPatches holds to a DNS label.
			if len(reference.Name) > maxObjectName || !objectName.MatchString(reference.Name) || !shapeWord.MatchString(reference.Key) {
				return nil, nil, fmt.Errorf("the environment entry %s does not name a Secret or a ConfigMap, a DNS subdomain of at most %d characters, and a key: %q, key %q",
					entry.Name, maxObjectName, reference.Name, reference.Key)
			}
			if reference.Optional != nil && !*reference.Optional {
				return nil, nil, fmt.Errorf("the environment entry %s states optional: false; a required entry states no optional flag", entry.Name)
			}
			if from.SecretKeyRef != nil {
				secrets = append(secrets, *reference)
			}
			if from.ConfigMapKeyRef != nil && !slices.Contains(configMaps, reference.Name) {
				configMaps = append(configMaps, reference.Name)
			}
		}
	}
	return secrets, configMaps, nil
}

// checkDeploymentVolumes holds the volumes of the pod of the Deployment of a base to the one emptyDir tmp, which the
// base writes (#373 security review M1): a secret, projected, CSI or hostPath volume in the base itself is refused,
// so that no volume of the base mounts the owner Secret or a file of the node. The Component secret-files adds the
// secret volumes, under the checks of loadPatchPair.
func checkDeploymentVolumes(content []byte) error {
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []yaml.Node `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(content, &deployment); err != nil {
		return err
	}
	volumes := deployment.Spec.Template.Spec.Volumes
	for _, node := range volumes {
		text, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		var volume struct {
			Name     string `yaml:"name"`
			EmptyDir *struct {
				SizeLimit string `yaml:"sizeLimit"`
			} `yaml:"emptyDir"`
		}
		if err := decodeStrict(text, MaxRequirementsBytes, &volume); err != nil || volume.Name != "tmp" || volume.EmptyDir == nil {
			return fmt.Errorf("the pod holds a volume that is not the emptyDir tmp, such as a secret, projected, CSI or hostPath volume; the Component secret-files adds the secret volumes (ST-DLV-INV13): %s", strings.TrimSpace(string(text)))
		}
	}
	if len(volumes) != 1 {
		return fmt.Errorf("the pod holds %d volumes, not the one emptyDir tmp", len(volumes))
	}
	return nil
}

// isOwnerSecret tells whether a Secret is the Secret of the owner credential: the one of the service, or any whose
// name ends as one, so that a renamed copy is refused too.
func isOwnerSecret(service, secret string) bool {
	return secret == OwnerSecret(service) || strings.HasSuffix(secret, "-database-owner")
}

// migrationJobShape is the one shape of the migration Job of a base. The strict decoder refuses each other field,
// such as a service account, an fsGroup, an init container, an envFrom or a volume of a secret.
type migrationJobShape struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		BackoffLimit            *int `yaml:"backoffLimit"`
		ActiveDeadlineSeconds   *int `yaml:"activeDeadlineSeconds"`
		TTLSecondsAfterFinished *int `yaml:"ttlSecondsAfterFinished"`
		Template                struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				RestartPolicy                string `yaml:"restartPolicy"`
				AutomountServiceAccountToken *bool  `yaml:"automountServiceAccountToken"`
				SecurityContext              struct {
					RunAsNonRoot   *bool `yaml:"runAsNonRoot"`
					SeccompProfile struct {
						Type string `yaml:"type"`
					} `yaml:"seccompProfile"`
				} `yaml:"securityContext"`
				Containers []struct {
					Name      string   `yaml:"name"`
					Image     string   `yaml:"image"`
					Args      []string `yaml:"args"`
					Resources struct {
						Requests map[string]string `yaml:"requests"`
						Limits   map[string]string `yaml:"limits"`
					} `yaml:"resources"`
					SecurityContext struct {
						AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
						Capabilities             struct {
							Drop []string `yaml:"drop"`
						} `yaml:"capabilities"`
						ReadOnlyRootFilesystem *bool `yaml:"readOnlyRootFilesystem"`
					} `yaml:"securityContext"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
					Env []yaml.Node `yaml:"env"`
				} `yaml:"containers"`
				Volumes []struct {
					Name     string `yaml:"name"`
					EmptyDir *struct {
						SizeLimit string `yaml:"sizeLimit"`
					} `yaml:"emptyDir"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// checkMigrationJob holds the migration Job of a base to its exact shape (ST-DLV-UC07-R06, ST-DLV-INV13): a batch/v1
// Job of the name of the release with the labels of the migration and the service, pods with no
// app.kubernetes.io/name, the Argo CD hook annotations with the sync wave 0, a retry limit, a time limit and a time to live; a pod that never restarts in place, with no service
// account token, that runs as non-root with the seccomp profile RuntimeDefault and no fsGroup, and has only the
// emptyDir at /tmp; and one container of the service that runs the binary with migrate alone, with no privilege
// escalation, every capability dropped and a read-only root file system. Its environment entries name only keys,
// from the Secret of the owner credential and from the ConfigMap of the service alone.
func checkMigrationJob(content []byte, service, name string) error {
	var job migrationJobShape
	if err := decodeStrict(content, MaxRequirementsBytes, &job); err != nil {
		return fmt.Errorf("the migration Job has a field that the base never writes: %w", err)
	}
	labels := map[string]string{partOfLabel: service, componentLabel: migrationLabel}
	jobLabels := map[string]string{nameLabel: service, partOfLabel: service, componentLabel: migrationLabel}
	annotations := map[string]string{HookAnnotation: HookPhase, HookDeleteAnnotation: HookDeletePolicy, SyncWaveAnnotation: MigrationSyncWave}
	spec, pod := job.Spec, job.Spec.Template.Spec
	switch {
	case job.APIVersion != "batch/v1" || job.Kind != "Job" || !strings.HasPrefix(name, service+"-migrate-") || len(name) > 63 || !serviceLabel.MatchString(name):
		return fmt.Errorf("the Job %q is not the batch/v1 migration Job <service>-migrate-<release> of %s", name, service)
	case !maps.Equal(job.Metadata.Labels, jobLabels) || !maps.Equal(spec.Template.Metadata.Labels, labels):
		return fmt.Errorf("the Job %s does not have exactly the labels %s, %s and %s: %s, or its pods not exactly the last two, with no %s",
			name, nameLabel, partOfLabel, componentLabel, migrationLabel, nameLabel)
	case !maps.Equal(job.Metadata.Annotations, annotations):
		return fmt.Errorf("the Job %s does not have exactly the annotations %s: %s, %s: %s and %s: %q", name, HookAnnotation, HookPhase,
			HookDeleteAnnotation, HookDeletePolicy, SyncWaveAnnotation, MigrationSyncWave)
	case spec.BackoffLimit == nil || *spec.BackoffLimit < 0 || spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds <= 0 ||
		spec.TTLSecondsAfterFinished == nil || *spec.TTLSecondsAfterFinished <= 0:
		return fmt.Errorf("the Job %s does not set backoffLimit, activeDeadlineSeconds and ttlSecondsAfterFinished", name)
	case pod.RestartPolicy != "Never" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken:
		return fmt.Errorf("the pod of the Job %s does not have restartPolicy Never and no service account token", name)
	case pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault":
		return fmt.Errorf("the pod of the Job %s does not run as non-root with the seccomp profile RuntimeDefault", name)
	case len(pod.Volumes) != 1 || pod.Volumes[0].Name != "tmp" || pod.Volumes[0].EmptyDir == nil:
		return fmt.Errorf("the pod of the Job %s has a volume other than the emptyDir tmp", name)
	case len(pod.Containers) != 1:
		return fmt.Errorf("the pod of the Job %s has more than one container", name)
	}
	container := pod.Containers[0]
	security := container.SecurityContext
	switch {
	case container.Name != service || !slices.Equal(container.Args, []string{MigrateMode}):
		return fmt.Errorf("the container of the Job %s is not %s with the one argument %s", name, service, MigrateMode)
	case security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || !slices.Equal(security.Capabilities.Drop, []string{"ALL"}) ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem:
		return fmt.Errorf("the container of the Job %s does not drop ALL capabilities with no privilege escalation and a read-only root file system", name)
	case len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != "tmp" || container.VolumeMounts[0].MountPath != "/tmp":
		return fmt.Errorf("the container of the Job %s mounts more than the emptyDir tmp at /tmp", name)
	}
	// The Job has bounded resources, as the Deployment has (#373 security review L1).
	for _, resource := range []string{"cpu", "memory"} {
		if container.Resources.Requests[resource] == "" || container.Resources.Limits[resource] == "" {
			return fmt.Errorf("the container of the Job %s does not set a request and a limit of %s", name, resource)
		}
	}
	secrets, configMaps, err := checkEnvironmentEntries(content)
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		if secret.Name != OwnerSecret(service) || secret.Key != "url" {
			return fmt.Errorf("the Job %s takes a variable from the key %s of the Secret %s; it may take one from the key url of %s alone (ST-DLV-INV13)",
				name, secret.Key, secret.Name, OwnerSecret(service))
		}
	}
	for _, configMap := range configMaps {
		if configMap != ConfigMapName(service) {
			return fmt.Errorf("the Job %s takes a variable from the ConfigMap %s; it may take one from %s alone", name, configMap, ConfigMapName(service))
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
// kustomization of the kind Component that lists, in this order, the removal patch and then the mount patch of the
// Deployment and, when the base has a migration Job, of the Job, and nothing else; each patch is of the one
// container of its workload (loadPatchPair). The Deployment patches never mount the Secret of the owner credential,
// and the Job patches never mount a Secret that the Deployment takes a variable from (ST-DLV-INV13).
func loadSecretFiles(files map[string][]byte, service, worker, job string, servingSecrets []string) error {
	component := []string{SecretFilesKustomization, SecretFilesRemoval, SecretFilesMounts, SecretFilesWorkerRemoval, SecretFilesWorkerMounts, SecretFilesJobRemoval, SecretFilesJobMounts}
	held := map[string]bool{}
	for _, file := range component {
		if _, ok := files[file]; ok {
			held[file] = true
		}
	}
	if len(held) == 0 {
		return nil
	}
	pairs := []struct {
		removal, mounts, apiVersion, kind, name string
	}{
		{SecretFilesRemoval, SecretFilesMounts, "apps/v1", "Deployment", service},
		{SecretFilesWorkerRemoval, SecretFilesWorkerMounts, "apps/v1", "Deployment", worker},
		{SecretFilesJobRemoval, SecretFilesJobMounts, "batch/v1", "Job", job},
	}
	var listed []string
	for _, pair := range pairs {
		if held[pair.removal] != held[pair.mounts] {
			return fmt.Errorf("the Component %s holds one patch of the %s and not the other", SecretFilesFolder, pair.kind)
		}
		if held[pair.removal] {
			listed = append(listed, path.Base(pair.removal), path.Base(pair.mounts))
		}
	}
	if !held[SecretFilesKustomization] || len(listed) == 0 {
		return fmt.Errorf("the Component %s does not hold each of its files", SecretFilesFolder)
	}
	if held[SecretFilesJobRemoval] && job == "" {
		return fmt.Errorf("the Component %s patches a migration Job, but the base has none", SecretFilesFolder)
	}
	if held[SecretFilesWorkerRemoval] && worker == "" {
		return fmt.Errorf("the Component %s patches a worker Deployment, but the base has none", SecretFilesFolder)
	}
	var kustomization secretFilesKustomization
	if err := decodeStrict(files[SecretFilesKustomization], MaxRequirementsBytes, &kustomization); err != nil {
		return fmt.Errorf("%s: %w", SecretFilesKustomization, err)
	}
	var paths []string
	for _, patch := range kustomization.Patches {
		paths = append(paths, patch.Path)
	}
	if kustomization.APIVersion != "kustomize.config.k8s.io/v1alpha1" || kustomization.Kind != "Component" || !slices.Equal(paths, listed) {
		return fmt.Errorf("%s is not a Component that lists %s, in this order", SecretFilesKustomization, strings.Join(listed, ", "))
	}
	for _, pair := range pairs {
		if !held[pair.removal] {
			continue
		}
		mounted, set, err := loadPatchPair(files, pair.removal, pair.mounts, pair.apiVersion, pair.kind, pair.name, service)
		if err != nil {
			return err
		}
		// The Job mounts the key url of the owner Secret for the *_FILE form of its own credential, and the Secret
		// and key of each key file of MigrationKeyFiles, and nothing else (#373 security review M2).
		for variable, source := range set {
			if pair.kind != "Job" {
				continue
			}
			want, keyFile := migrationKeyFile(service, variable)
			if !keyFile {
				want = SecretSource{Name: OwnerSecret(service), Key: "url"}
			}
			if source != want || (!keyFile && !strings.HasSuffix(variable, "_FILE")) {
				return fmt.Errorf("%s gives the Job %s from the key %s of the Secret %s; the Job may mount only the key url of %s and the key files that the migrate mode reads (ST-DLV-INV13)",
					pair.mounts, variable, source.Key, source.Name, OwnerSecret(service))
			}
		}
		for _, secret := range mounted {
			if pair.kind == "Deployment" && isOwnerSecret(service, secret) {
				return fmt.Errorf("%s mounts the Secret %s, the credential of the owner role, in the Deployment (ST-DLV-INV13)", pair.mounts, secret)
			}
			if pair.kind == "Job" && slices.Contains(servingSecrets, secret) {
				return fmt.Errorf("%s mounts the Secret %s, which the Deployment takes a variable from, in the migration Job (ST-DLV-INV13)", pair.mounts, secret)
			}
		}
	}
	return nil
}

// fsGroupPatch is the one shape of a patch of the Component fs-group: a patch of one workload that sets the fsGroup
// of its pod and nothing else. The strict decoder refuses each other field, such as a runAsUser, a supplemental
// group, a container or a volume.
type fsGroupPatch struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext struct {
					FSGroup *int `yaml:"fsGroup"`
				} `yaml:"securityContext"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// loadFSGroup holds the Component fs-group, when the base has one, to its exact shape: the base holds it exactly when
// it holds the Component secret-files; its kustomization is a Component that lists, in this order, the patch of the
// Deployment and, when the base has a migration Job, the patch of the Job, and nothing else; and each patch only sets
// the pod fsGroup of its workload to FSGroup.
func loadFSGroup(files map[string][]byte, service, worker, job string) error {
	_, secretFiles := files[SecretFilesKustomization]
	held := map[string]bool{}
	for _, file := range []string{FSGroupKustomization, FSGroupDeployment, FSGroupWorker, FSGroupJob} {
		if _, ok := files[file]; ok {
			held[file] = true
		}
	}
	if len(held) == 0 && !secretFiles {
		return nil
	}
	if !secretFiles {
		return fmt.Errorf("the base holds the Component %s but not the Component %s; the base writes the two together", FSGroupFolder, SecretFilesFolder)
	}
	type target struct{ file, apiVersion, kind, name string }
	want := []target{{FSGroupDeployment, "apps/v1", "Deployment", service}}
	if worker != "" {
		want = append(want, target{FSGroupWorker, "apps/v1", "Deployment", worker})
	}
	if job != "" {
		want = append(want, target{FSGroupJob, "batch/v1", "Job", job})
	}
	var listed []string
	for _, patch := range want {
		listed = append(listed, path.Base(patch.file))
	}
	if !held[FSGroupKustomization] || len(held) != len(want)+1 || !held[FSGroupDeployment] || (worker != "" && !held[FSGroupWorker]) || (job != "" && !held[FSGroupJob]) {
		return fmt.Errorf("the Component %s does not hold exactly its kustomization and the patches %s", FSGroupFolder, strings.Join(listed, ", "))
	}
	var kustomization secretFilesKustomization
	if err := decodeStrict(files[FSGroupKustomization], MaxRequirementsBytes, &kustomization); err != nil {
		return fmt.Errorf("%s: %w", FSGroupKustomization, err)
	}
	var paths []string
	for _, patch := range kustomization.Patches {
		paths = append(paths, patch.Path)
	}
	if kustomization.APIVersion != "kustomize.config.k8s.io/v1alpha1" || kustomization.Kind != "Component" || !slices.Equal(paths, listed) {
		return fmt.Errorf("%s is not a Component that lists %s, in this order", FSGroupKustomization, strings.Join(listed, ", "))
	}
	for _, patch := range want {
		var shape fsGroupPatch
		if err := decodeStrict(files[patch.file], MaxRequirementsBytes, &shape); err != nil {
			return fmt.Errorf("%s: the patch does more than set the fsGroup of the pod: %w", patch.file, err)
		}
		group := shape.Spec.Template.Spec.SecurityContext.FSGroup
		if shape.APIVersion != patch.apiVersion || shape.Kind != patch.kind || shape.Metadata.Name != patch.name || group == nil || *group != FSGroup {
			return fmt.Errorf("%s is not a patch of the %s %s %s that sets the pod fsGroup %d", patch.file, patch.apiVersion, patch.kind, patch.name, FSGroup)
		}
	}
	return nil
}

// loadPatchPair holds the removal patch and the mount patch of one workload of the Component secret-files to their
// exact shape: two patches of the workload name of the kind, each of its one container, the service. The removal
// patch only deletes environment entries. The mount patch only sets each *_FILE variable to the path of a file under
// SecretFilesRoot, mounts each secret read-only at its folder there, and adds a secret volume with the mode 0400 for
// each mount, with only the keys that a set variable reads. No patch names a variable twice, and the mount patch
// sets no variable that the removal patch keeps. It gives the mounted secrets, and the Secret and key of each set
// variable.
func loadPatchPair(files map[string][]byte, removalFile, mountsFile, apiVersion, kind, name, service string) ([]string, map[string]SecretSource, error) {
	var patches [2]secretFilesPatch
	for i, file := range []string{removalFile, mountsFile} {
		if err := decodeStrict(files[file], MaxRequirementsBytes, &patches[i]); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", file, err)
		}
		patch := patches[i]
		if patch.APIVersion != apiVersion || patch.Kind != kind || patch.Metadata.Name != name || len(patch.Spec.Template.Spec.Containers) != 1 ||
			patch.Spec.Template.Spec.Containers[0].Name != service {
			return nil, nil, fmt.Errorf("%s is not a patch of the one container of the %s %s", file, kind, name)
		}
	}
	removal, mount := patches[0].Spec.Template.Spec, patches[1].Spec.Template.Spec
	removed := map[string]bool{}
	if len(removal.Volumes) > 0 || len(removal.Containers[0].VolumeMounts) > 0 || len(removal.Containers[0].Env) == 0 {
		return nil, nil, fmt.Errorf("%s does more than remove environment entries", removalFile)
	}
	for _, entry := range removal.Containers[0].Env {
		if entry.Patch != "delete" || entry.Value != nil || !variableName.MatchString(entry.Name) || removed[entry.Name] {
			return nil, nil, fmt.Errorf("%s does more than remove each environment entry once: %q", removalFile, entry.Name)
		}
		removed[entry.Name] = true
	}
	volumes := map[string]bool{}
	var secrets []string
	// projected holds each key that a volume projects, as secret/key; each must be the file of one set variable.
	projected := map[string]bool{}
	for _, volume := range mount.Volumes {
		if !serviceLabel.MatchString(volume.Name) || volume.Name == "tmp" || volumes[volume.Name] || volume.Secret == nil || volume.Secret.SecretName != volume.Name ||
			volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != SecretFileMode || len(volume.Secret.Items) == 0 {
			return nil, nil, fmt.Errorf("%s holds the volume %q, which is not one secret volume of the mode 0400 named by its secret, with its keys", mountsFile, volume.Name)
		}
		for _, item := range volume.Secret.Items {
			if item.Path != item.Key || !shapeWord.MatchString(item.Key) || projected[volume.Name+"/"+item.Key] {
				return nil, nil, fmt.Errorf("%s projects %q of %s, which is not one key at the path of its name", mountsFile, item.Key, volume.Name)
			}
			projected[volume.Name+"/"+item.Key] = true
		}
		volumes[volume.Name] = true
		secrets = append(secrets, volume.Name)
	}
	mounted := map[string]bool{}
	for _, volumeMount := range mount.Containers[0].VolumeMounts {
		if !volumes[volumeMount.Name] || mounted[volumeMount.Name] || volumeMount.MountPath != SecretFilesRoot+"/"+volumeMount.Name || !volumeMount.ReadOnly {
			return nil, nil, fmt.Errorf("%s mounts %q, which is not one read-only secret volume at its folder under %s", mountsFile, volumeMount.Name, SecretFilesRoot)
		}
		mounted[volumeMount.Name] = true
	}
	if len(mounted) != len(volumes) || len(mount.Containers[0].Env) == 0 {
		return nil, nil, fmt.Errorf("%s does not mount each volume it adds, or sets no variable", mountsFile)
	}
	set, used := map[string]bool{}, map[string]bool{}
	sources := map[string]SecretSource{}
	for _, entry := range mount.Containers[0].Env {
		if entry.Patch != "" || entry.Value == nil || !variableName.MatchString(entry.Name) || set[entry.Name] {
			return nil, nil, fmt.Errorf("%s does more than set each file variable once: %q", mountsFile, entry.Name)
		}
		folder, key := path.Split(*entry.Value)
		if !mounted[path.Base(folder)] || path.Clean(folder) != SecretFilesRoot+"/"+path.Base(folder) || !shapeWord.MatchString(key) ||
			!projected[path.Base(folder)+"/"+key] {
			return nil, nil, fmt.Errorf("%s sets %s to a path that is not a projected key of a mounted secret", mountsFile, entry.Name)
		}
		set[entry.Name] = true
		sources[entry.Name] = SecretSource{Name: path.Base(folder), Key: key}
		used[path.Base(folder)+"/"+key] = true
	}
	for key := range projected {
		if !used[key] {
			return nil, nil, fmt.Errorf("%s projects the key %s, which no variable that it sets reads", mountsFile, key)
		}
	}
	for name := range set {
		// The mount patch sets the *_FILE form of a removed secret variable, or a removed path variable itself: a
		// path variable that the base wires from the ConfigMap must be removed first, or the two entries merge.
		if !removed[name] && !removed[strings.TrimSuffix(name, "_FILE")] {
			return nil, nil, fmt.Errorf("%s sets %s, but %s does not remove the environment form", mountsFile, name, removalFile)
		}
	}
	return secrets, sources, nil
}
