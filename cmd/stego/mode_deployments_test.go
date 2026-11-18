package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModeDeploymentsOfScaffolds(t *testing.T) {
	t.Run("ST-DLV-UC07-R07-T02 stego apply writes the Deployment of the api mode and that of the worker modes for a service of the runtime data path, and one Deployment for a browser-service", func(t *testing.T) {
		// The rpc-service scaffold has an entity and a query, so it is on the runtime data path.
		project, _ := initialized(t, "rpc-service")
		signExampleFill(t, project, "rpc-service", moduleOf("rpc-service"), "validate")
		if run := stegoIn(t, project, moduleOf("rpc-service"), "apply"); run.code != 0 {
			t.Fatalf("stego apply of the rpc-service scaffold: exit %d: %s", run.code, run.stderr)
		}
		read := func(project, name string) string {
			t.Helper()
			data, err := os.ReadFile(filepath.Join(project, name))
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		api, worker := read(project, "deploy/base/deployment.yaml"), read(project, "deploy/base/worker-deployment.yaml")
		for _, want := range []string{"- --mode=api", "path: /readyz\n", "path: /livez"} {
			if !strings.Contains(api, want) {
				t.Errorf("the Deployment of the api mode does not hold %q:\n%s", want, api)
			}
		}
		for _, want := range []string{"name: greeter-worker", "- --mode=worker", "- --mode=controller", "path: /readyz/modes", "path: /livez",
			"name: STEGO_WORKER_DATABASE_URL", "name: STEGO_RELAY_DATABASE_URL"} {
			if !strings.Contains(worker, want) {
				t.Errorf("the Deployment of the worker modes does not hold %q:\n%s", want, worker)
			}
		}
		for _, refused := range []string{"name: STEGO_WORKER_DATABASE_URL", "name: STEGO_RELAY_DATABASE_URL", "name: STEGO_MODE\n"} {
			if strings.Contains(api, refused) {
				t.Errorf("the Deployment of the api mode holds %q", refused)
			}
		}
		if strings.Contains(worker, "containerPort:") || strings.Contains(worker, "name: STEGO_MODE\n") {
			t.Errorf("the Deployment of the worker modes has a container port or STEGO_MODE:\n%s", worker)
		}
		if kustomization := read(project, "deploy/base/kustomization.yaml"); !strings.Contains(kustomization, "- worker-deployment.yaml") {
			t.Errorf("the kustomization does not list the worker Deployment:\n%s", kustomization)
		}
		if run := stegoIn(t, project, moduleOf("rpc-service"), "drift"); run.code != 0 {
			t.Errorf("stego drift after apply: exit %d: %s%s", run.code, run.stdout, run.stderr)
		}

		browser, _ := initialized(t, "browser-service")
		if run := stegoIn(t, browser, moduleOf("browser-service"), "apply"); run.code != 0 {
			t.Fatalf("stego apply of the browser-service scaffold: exit %d: %s", run.code, run.stderr)
		}
		if _, err := os.Stat(filepath.Join(browser, "deploy/base/worker-deployment.yaml")); !os.IsNotExist(err) {
			t.Errorf("the browser-service base has a worker Deployment: %v", err)
		}
		if deployment := read(browser, "deploy/base/deployment.yaml"); strings.Contains(deployment, "--mode=") || !strings.Contains(deployment, "path: /readyz\n") {
			t.Errorf("the browser-service Deployment selects modes or has another readiness:\n%s", deployment)
		}
	})
}
