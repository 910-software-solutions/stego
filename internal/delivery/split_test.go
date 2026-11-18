package delivery

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// workerRequirements gives the platform requirements of ST-DLV-TD02, in which postgres-adapter states the connection
// of the worker role as read by the worker mode alone.
func workerRequirements(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "environment", "platform-requirements.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"STEGO_WORKER_DATABASE_URL", "STEGO_WORKER_DATABASE_URL_FILE"} {
		at := strings.Index(string(data), "  - name: "+name+"\n")
		end := strings.Index(string(data)[at+1:], "  - name: ")
		if at < 0 || end < 0 || !strings.Contains(string(data)[at:at+1+end], "    read_by_mode: worker\n") {
			t.Fatalf("the fixture does not state %s as read by the worker mode", name)
		}
	}
	return data
}

// container gives the one container of a Deployment, and the pod labels.
func container(t *testing.T, deployment map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	template := deployment["spec"].(map[string]any)["template"].(map[string]any)
	labels := template["metadata"].(map[string]any)["labels"].(map[string]any)
	containers := template["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("the Deployment has %d containers", len(containers))
	}
	return containers[0].(map[string]any), labels
}

// variables gives the names of the environment entries of a container.
func variables(c map[string]any) []string {
	var names []string
	entries, _ := c["env"].([]any)
	for _, entry := range entries {
		names = append(names, entry.(map[string]any)["name"].(string))
	}
	return names
}

func readinessOf(c map[string]any) string {
	return c["readinessProbe"].(map[string]any)["httpGet"].(map[string]any)["path"].(string)
}

func TestModeDeployments(t *testing.T) {
	t.Run("ST-DLV-UC07-R07-T01 The api mode and the background modes run in their own Deployments, each with its probes and the variables of its modes", func(t *testing.T) {
		requirements := workerRequirements(t)
		base, _, err := WriteBase("orders", testRelease, requirements, healthCheckProbes, []string{"api", "worker", "controller"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the split base: %v", err)
		}
		project := writeProject(t, base.Files)
		digest := "sha256:" + strings.Repeat("ab", 32)
		built := resourcesOf(t, buildOverlay(t, project, "overlay", "images: [{name: stego.invalid/orders, newName: registry.example/orders, digest: "+digest+"}]\n"+
			"components: [../deploy/components/secret-files, ../deploy/components/fs-group]\n"))
		deployments := map[string]map[string]any{}
		for _, deployment := range built["Deployment"] {
			deployments[deployment["metadata"].(map[string]any)["name"].(string)] = deployment
		}
		if len(deployments) != 2 || deployments["orders"] == nil || deployments["orders-worker"] == nil {
			t.Fatalf("the build holds the Deployments %v, want orders and orders-worker", slices.Sorted(func(yield func(string) bool) {
				for name := range deployments {
					if !yield(name) {
						return
					}
				}
			}))
		}
		api, apiLabels := container(t, deployments["orders"])
		worker, workerLabels := container(t, deployments["orders-worker"])

		// The probes and the modes of each Deployment.
		if readinessOf(api) != "/readyz" || readinessOf(worker) != WorkerReadiness {
			t.Errorf("the readiness probes are %s and %s, want /readyz and %s", readinessOf(api), readinessOf(worker), WorkerReadiness)
		}
		for _, c := range []map[string]any{api, worker} {
			if c["livenessProbe"].(map[string]any)["httpGet"].(map[string]any)["path"] != "/livez" {
				t.Error("a Deployment has a liveness probe other than /livez")
			}
			if c["image"] != "registry.example/orders@"+digest {
				t.Errorf("the overlay did not set the image of a Deployment: %v", c["image"])
			}
		}
		if args := api["args"].([]any); !slices.Equal(args, []any{"--mode=api"}) {
			t.Errorf("the api Deployment runs %v", args)
		}
		if args := worker["args"].([]any); !slices.Equal(args, []any{"--mode=worker", "--mode=controller"}) {
			t.Errorf("the worker Deployment runs %v", args)
		}
		if _, ports := worker["ports"]; ports {
			t.Error("the worker Deployment has a container port")
		}

		// The environment entries of each side: the api Deployment holds no variable of the worker mode, and neither
		// holds STEGO_MODE, which the args replace; the Component secret-files patched both.
		apiNames, workerNames := variables(api), variables(worker)
		for _, name := range []string{"STEGO_WORKER_DATABASE_URL", "STEGO_WORKER_DATABASE_URL_FILE", "STEGO_MODE", "STEGO_MIGRATION_DATABASE_URL"} {
			if slices.Contains(apiNames, name) {
				t.Errorf("the api Deployment holds %s", name)
			}
		}
		for _, name := range []string{"STEGO_MODE", "STEGO_MIGRATION_DATABASE_URL"} {
			if slices.Contains(workerNames, name) {
				t.Errorf("the worker Deployment holds %s", name)
			}
		}
		if !slices.Contains(workerNames, "STEGO_WORKER_DATABASE_URL_FILE") || slices.Contains(workerNames, "STEGO_WORKER_DATABASE_URL") {
			t.Errorf("the Component secret-files did not give the worker Deployment its worker credential as a file: %v", workerNames)
		}
		for _, name := range []string{"DATABASE_URL_FILE", "PORT"} {
			if !slices.Contains(apiNames, name) || !slices.Contains(workerNames, name) {
				t.Errorf("a Deployment lacks %s, which every mode reads", name)
			}
		}

		// No selector takes a pod of the other Deployment.
		selector := deployments["orders"]["spec"].(map[string]any)["selector"].(map[string]any)["matchLabels"].(map[string]any)
		for key, value := range selector {
			if workerLabels[key] == value && len(selector) == 1 {
				t.Errorf("the selector of the api Deployment takes the worker pods (%s=%v)", key, value)
			}
		}
		for _, service := range built["Service"] {
			for key, value := range service["spec"].(map[string]any)["selector"].(map[string]any) {
				if workerLabels[key] != value && apiLabels[key] == value {
					continue
				}
				if workerLabels[key] == value {
					t.Errorf("the Service selects the worker pods (%s=%v)", key, value)
				}
			}
		}
		for _, job := range built["Job"] {
			jobContainer := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			if slices.Contains(variables(jobContainer), "STEGO_WORKER_DATABASE_URL_FILE") {
				t.Error("the migration Job holds the worker credential")
			}
		}

		// One side alone gets one Deployment with the readiness of that side and every variable of its modes.
		for _, test := range []struct {
			modes     []string
			readiness string
			worker    bool
		}{
			{[]string{"api"}, "/readyz", false},
			{[]string{"worker", "controller"}, WorkerReadiness, true},
			{nil, "/readyz", true},
		} {
			one, _, err := WriteBase("orders", testRelease, requirements, healthCheckProbes, test.modes)
			if err != nil {
				t.Fatal(err)
			}
			if _, split := one.Files[WorkerDeploymentFile]; split {
				t.Errorf("modes %v: the base splits the Deployment", test.modes)
			}
			resources, err := LoadBase(one.Files)
			if err != nil {
				t.Fatalf("modes %v: %v", test.modes, err)
			}
			c, _ := container(t, resources[DeploymentFile])
			if readinessOf(c) != test.readiness || c["args"] != nil || !slices.Contains(variables(c), "STEGO_MODE") {
				t.Errorf("modes %v: readiness %s, args %v, want %s, no args and the entry of STEGO_MODE", test.modes, readinessOf(c), c["args"], test.readiness)
			}
			held := slices.Contains(variables(c), "STEGO_WORKER_DATABASE_URL") || slices.Contains(variables(c), "STEGO_WORKER_DATABASE_URL_FILE")
			if held != test.worker {
				t.Errorf("modes %v: the entry of the worker credential is %v, want %v", test.modes, !test.worker, test.worker)
			}
		}

		// A worker Deployment name that is not a DNS label is refused.
		long := strings.Repeat("o", 57)
		renamed := []byte(strings.Replace(string(requirements), "service: orders\n", "service: "+long+"\n", 1))
		if _, _, err := WriteBase(long, testRelease, renamed, healthCheckProbes, []string{"api", "worker"}); err == nil || !strings.Contains(err.Error(), "worker Deployment") {
			t.Errorf("a worker Deployment name of more than 63 characters was not refused for its name: %v", err)
		}
		// LoadBase refuses a worker Deployment with another name.
		changed := map[string][]byte{}
		for name, data := range base.Files {
			changed[name] = data
		}
		changed[WorkerDeploymentFile] = []byte(strings.Replace(string(base.Files[WorkerDeploymentFile]), "orders-worker", "other", -1))
		if _, err := LoadBase(changed); err == nil {
			t.Error("LoadBase accepts a worker Deployment with another name")
		}
	})

	t.Run("ST-DLV-UC07-R07-T03 A variable of the api mode is only in the Deployment of the api mode", func(t *testing.T) {
		// The fixture of ST-DLV-TD02 states the CORS settings of rest-api as read by the api mode alone, as the
		// shipped component states them.
		requirements := string(workerRequirements(t))
		api := []string{"CORS_ALLOWED_HEADERS", "CORS_ALLOWED_METHODS", "CORS_ALLOWED_ORIGINS"}
		for _, name := range api {
			at := strings.Index(requirements, "  - name: "+name+"\n")
			end := strings.Index(requirements[at+1:], "  - name: ")
			if at < 0 || end < 0 || !strings.Contains(requirements[at:at+1+end], "    read_by_mode: api\n") {
				t.Fatalf("the fixture does not state %s as read by the api mode", name)
			}
		}
		for _, modes := range [][]string{{"api", "worker", "controller"}, {"worker", "controller"}, {"api"}, nil} {
			base, _, err := WriteBase("orders", testRelease, []byte(requirements), healthCheckProbes, modes)
			if err != nil {
				t.Fatalf("modes %v: %v", modes, err)
			}
			resources, err := LoadBase(base.Files)
			if err != nil {
				t.Fatalf("modes %v: %v", modes, err)
			}
			held := func(file string) []string {
				var in []string
				c, _ := container(t, resources[file])
				for _, name := range api {
					if slices.Contains(variables(c), name) {
						in = append(in, name)
					}
				}
				return in
			}
			// The Deployment of a side with the api mode, or of no mode selection, holds each; the worker Deployment
			// and the migration Job hold none.
			wantAPI := modes == nil || slices.Contains(modes, "api")
			want := 0
			if wantAPI {
				want = len(api)
			}
			if got := held(DeploymentFile); len(got) != want {
				t.Errorf("modes %v: the Deployment %s holds %v of the variables of the api mode, want %d of them", modes, DeploymentFile, got, want)
			}
			if _, split := resources[WorkerDeploymentFile]; split {
				if got := held(WorkerDeploymentFile); len(got) != 0 {
					t.Errorf("modes %v: the worker Deployment holds %v", modes, got)
				}
			}
			if _, job := resources[MigrationJobFile]; !job {
				t.Fatalf("modes %v: the base has no migration Job", modes)
			}
			job := resources[MigrationJobFile]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			for _, name := range api {
				if slices.Contains(variables(job), name) {
					t.Errorf("modes %v: the migration Job holds %s", modes, name)
				}
			}
		}
	})

	t.Run("ST-DLV-UC07-R07-T04 The worker Deployment runs as its own ServiceAccount, and the Role binds the ServiceAccount of each Deployment", func(t *testing.T) {
		// The fixture of ST-DLV-TD02 with one cluster permission, so that the base holds a Role and a RoleBinding.
		requirements := strings.Replace(string(workerRequirements(t)), "requirements:\n",
			"requirements:\n  - kind: cluster permission\n    name: orders-maps\n    resource: configmaps\n    verbs: [get]\n    components: [kubernetes-client]\n", 1)
		base, _, err := WriteBase("orders", testRelease, []byte(requirements), healthCheckProbes, []string{"api", "worker", "controller"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the split base: %v", err)
		}
		built := resourcesOf(t, buildOverlay(t, writeProject(t, base.Files), "overlay", "namespace: shop\n"))
		accounts := map[string]bool{}
		for _, account := range built["ServiceAccount"] {
			accounts[account["metadata"].(map[string]any)["name"].(string)] = true
		}
		if len(accounts) != 2 || !accounts["orders"] || !accounts["orders-worker"] {
			t.Errorf("the build holds the ServiceAccounts %v, want orders and orders-worker", accounts)
		}
		for _, deployment := range built["Deployment"] {
			name := deployment["metadata"].(map[string]any)["name"].(string)
			pod := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			if pod["serviceAccountName"] != name {
				t.Errorf("the Deployment %s runs as the ServiceAccount %v, want %s", name, pod["serviceAccountName"], name)
			}
		}
		if len(built["RoleBinding"]) != 1 {
			t.Fatalf("the build holds %d RoleBindings, want 1", len(built["RoleBinding"]))
		}
		var subjects []string
		for _, subject := range built["RoleBinding"][0]["subjects"].([]any) {
			subject := subject.(map[string]any)
			subjects = append(subjects, subject["name"].(string)+"/"+subject["namespace"].(string))
		}
		if !slices.Equal(subjects, []string{"orders/shop", "orders-worker/shop"}) {
			t.Errorf("the RoleBinding binds %v, want orders and orders-worker in the namespace of the overlay", subjects)
		}

		// One Deployment alone runs as the ServiceAccount of the service, and the base holds no worker ServiceAccount.
		for _, modes := range [][]string{{"api"}, {"worker", "controller"}, nil} {
			one, _, err := WriteBase("orders", testRelease, []byte(requirements), healthCheckProbes, modes)
			if err != nil {
				t.Fatal(err)
			}
			if _, held := one.Files[WorkerServiceAccountFile]; held {
				t.Errorf("modes %v: the base holds a worker ServiceAccount", modes)
			}
		}

		// LoadBase refuses a worker ServiceAccount of another name, and one in a base with no worker Deployment.
		changed := maps.Clone(base.Files)
		changed[WorkerServiceAccountFile] = []byte(strings.Replace(string(base.Files[WorkerServiceAccountFile]), "\n  name: orders-worker\n", "\n  name: other\n", 1))
		if _, err := LoadBase(changed); err == nil {
			t.Error("LoadBase accepts a worker ServiceAccount with another name")
		}
		one, _, err := WriteBase("orders", testRelease, []byte(requirements), healthCheckProbes, []string{"api"})
		if err != nil {
			t.Fatal(err)
		}
		alone := maps.Clone(one.Files)
		alone[WorkerServiceAccountFile] = base.Files[WorkerServiceAccountFile]
		alone[KustomizationFile] = []byte(strings.Replace(string(one.Files[KustomizationFile]), "- service-account.yaml\n", "- service-account.yaml\n  - worker-service-account.yaml\n", 1))
		if _, err := LoadBase(alone); err == nil {
			t.Error("LoadBase accepts a worker ServiceAccount in a base with no worker Deployment")
		}
	})

	t.Run("ST-DLV-UC07-R07-T05 LoadBase holds each Deployment to its own ServiceAccount, and the RoleBinding to exactly those", func(t *testing.T) {
		requirements := strings.Replace(string(workerRequirements(t)), "requirements:\n",
			"requirements:\n  - kind: cluster permission\n    name: orders-maps\n    resource: configmaps\n    verbs: [get]\n    components: [kubernetes-client]\n", 1)
		base, _, err := WriteBase("orders", testRelease, []byte(requirements), healthCheckProbes, []string{"api", "worker", "controller"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the split base: %v", err)
		}
		changed := func(file, from, to string) map[string][]byte {
			t.Helper()
			files := maps.Clone(base.Files)
			if !strings.Contains(string(files[file]), from) {
				t.Fatalf("%s does not hold %q", file, from)
			}
			files[file] = []byte(strings.Replace(string(files[file]), from, to, 1))
			return files
		}
		noWorkerAccount := changed(KustomizationFile, "  - worker-service-account.yaml\n", "")
		delete(noWorkerAccount, WorkerServiceAccountFile)
		for name, files := range map[string]map[string][]byte{
			"no worker ServiceAccount":                        noWorkerAccount,
			"a worker Deployment that runs as the service":    changed(WorkerDeploymentFile, "serviceAccountName: orders-worker", "serviceAccountName: orders"),
			"an api Deployment that runs as default":          changed(DeploymentFile, "serviceAccountName: orders", "serviceAccountName: default"),
			"a ServiceAccount of the service of another name": changed(ServiceAccountFile, "\n  name: orders\n", "\n  name: other\n"),
			"a RoleBinding with no worker subject":            changed(RoleBindingFile, "  - kind: ServiceAccount\n    name: orders-worker\n", ""),
			"a RoleBinding with another subject":              changed(RoleBindingFile, "name: orders-worker\n", "name: orders-worker\n  - kind: ServiceAccount\n    name: default\n"),
			"a RoleBinding subject with a namespace":          changed(RoleBindingFile, "name: orders-worker\n", "name: orders-worker\n    namespace: other\n"),
		} {
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a base with %s", name)
			}
		}
	})
}
