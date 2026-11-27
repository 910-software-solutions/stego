package kubernetesclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/template"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/httpapplication"
	"github.com/910-software-solutions/stego/internal/generator/oteltracing"
	"github.com/910-software-solutions/stego/internal/localtest"
)

// TestMain fills the checked module cache of localtest first and runs no test without it (ST-STD-LOCAL-R03).
func TestMain(m *testing.M) { os.Exit(localtest.MainModules(m)) }

//go:embed testdata/secret_state_test.go
var secretStateTests []byte

//go:embed testdata/secret_set_test.go
var secretSetTests []byte

//go:embed testdata/image_pull_secret_test.go
var imagePullSecretTests []byte

//go:embed testdata/route_test.go
var routeTests []byte

//go:embed testdata/tls_secret_test.go
var tlsSecretTests []byte

//go:embed testdata/readiness_test.go
var readinessTests []byte

//go:embed testdata/client_test.go
var runtimeTests []byte

//go:embed testdata/rotation_test.go
var rotationTests []byte

//go:embed testdata/watch_capacity_test.go
var watchCapacityTests []byte

//go:embed testdata/watch_set_test.go
var watchSetTests []byte

//go:embed testdata/rotation_telemetry_test.go
var rotationTelemetryTests []byte

//go:embed testdata/observation_test.go
var observationTests []byte

//go:embed testdata/pinned_admission_test.go
var pinnedAdmissionTests []byte

// The prototype is test-only until its live admission gate passes.
//
//go:embed testdata/pinned_admission.go.tmpl
var pinnedAdmissionSource string

func TestGeneratedKubernetesClient(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedKubernetesClient(t, telemetry, "") })
	}
}
func TestGeneratedOwnedResourceIdentity(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^Test(WidgetCreateConvergeAndDelete|OwnershipConflictAndInvalidDesiredState|WritesRequireStrictFieldValidation|PatchOwned)")
}
func TestGeneratedSecretState(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestSecretState")
}
func TestGeneratedOpaqueSecretSet(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestOpaqueSecretSet")
}
func TestGeneratedImagePullSecret(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestImagePull")
}
func TestGeneratedRouteAdmission(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestPassthroughRoute")
}
func TestGeneratedServerTLSSecret(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^Test(Server|Client)TLSSecret")
}
func TestGeneratedObservationHelpers(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestObservationHelpersMakeNoNetworkCall$")
}
func TestGeneratedDeploymentAvailability(t *testing.T) {
	testGeneratedKubernetesClient(t, false, "^TestDeploymentAvailability$")
}
func testGeneratedKubernetesClient(t *testing.T, telemetry bool, selected string) {
	ctx := gen.Context{ModuleName: "example.com/widget", OutDirName: "out", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application", "jwt-auth": "auth"}, AuthPackage: "example.com/widget/out/auth", ComponentConfig: map[string]any{"factory_package": "sample"}}
	if telemetry {
		ctx.PeerNamespaces["otel-tracing"] = "tracing"
	}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = "application"
	httpFiles, _, err := new(httpapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range httpFiles {
		if file.Path == "application/client/client.go" {
			files = append(files, file)
		}
	}
	files = append(files, gen.File{Path: "kubernetes/client_test.go", Content: runtimeTests})
	files = append(files, gen.File{Path: "kubernetes/secret_set_test.go", Content: secretSetTests})
	files = append(files, gen.File{Path: "kubernetes/image_pull_secret_test.go", Content: imagePullSecretTests})
	files = append(files, gen.File{Path: "kubernetes/secret_state_test.go", Content: secretStateTests})
	files = append(files, gen.File{Path: "kubernetes/readiness_test.go", Content: readinessTests})
	files = append(files, gen.File{Path: "kubernetes/route_test.go", Content: routeTests})
	files = append(files, gen.File{Path: "kubernetes/tls_secret_test.go", Content: tlsSecretTests})
	files = append(files, gen.File{Path: "kubernetes/rotation_test.go", Content: rotationTests})
	files = append(files, gen.File{Path: "kubernetes/watch_capacity_test.go", Content: watchCapacityTests})
	files = append(files, gen.File{Path: "kubernetes/watch_set_test.go", Content: watchSetTests})
	files = append(files, gen.File{Path: "kubernetes/observation_test.go", Content: observationTests})
	var module strings.Builder
	module.WriteString("module example.com/widget\ngo 1.26.11\n")
	if telemetry {
		ctx.OutputNamespace = "tracing"
		ctx.ServiceName = "widget"
		ctx.ComponentConfig = nil
		generated, wiring, err := new(oteltracing.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		files = append(files, gen.File{Path: "kubernetes/rotation_telemetry_test.go", Content: rotationTelemetryTests})
		names := make([]string, 0, len(wiring.GoModRequires))
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		module.WriteString("require (\n")
		for _, name := range names {
			fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
		}
		module.WriteString(")\n")
	} else {
		files = append(files, gen.File{Path: "kubernetes/rotation_telemetry_test.go", Content: []byte("package kubernetes\nimport (\"context\";\"testing\")\nconst rotationTelemetryEnabled=false\nfunc rotationTelemetry(t *testing.T,ctx context.Context)(context.Context,func()){return ctx,func(){}}\n")})
	}
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if telemetry {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = project
		cmd.Env = localtest.GoEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("telemetry dependencies: %v\n%s", err, output)
		}
		localtest.CheckSums(t, project)
	}
	// The generated tests take 20 to 35 seconds with their build on CI, and ran out of 30 seconds once on a busy
	// runner (PR #27, run 46049560626). Two minutes keeps the run bounded, well under the 20 minutes of the package.
	budget := "2m"
	if telemetry && os.Getenv("STEGO_KUBERNETES_LIVE_ROTATION") == "1" {
		budget = "15m"
	}
	arguments := []string{"test", "-v", "-race", "-count=1", "-timeout=" + budget}
	if selected != "" {
		arguments = append(arguments, "-run", selected)
	}
	cmd := exec.Command("go", append(arguments, "./...")...)
	cmd.Dir = project
	var rotation []string
	if !telemetry {
		rotation = append(rotation, "STEGO_KUBERNETES_LIVE_ROTATION=0")
	}
	cmd.Env = localtest.GoEnv(rotation...)
	output, runErr := cmd.CombinedOutput()
	if dir := os.Getenv("STEGO_KUBERNETES_ROTATION_ARTIFACTS"); dir != "" {
		if !telemetry {
			dir = filepath.Join(dir, "without-telemetry")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "runtime.log"), output, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runErr; err != nil {
		t.Fatalf("generated Kubernetes client: %v\n%s", err, output)
	}
}
func TestMissingHTTPClientIsRejected(t *testing.T) {
	if _, _, err := new(Generator).Generate(gen.Context{ModuleName: "example.com/widget", OutputNamespace: "kubernetes"}); err == nil {
		t.Fatal("missing transport accepted")
	}
}

// This check compiles the test-only prototype without a cluster or network.
func TestPinnedAdmissionTemplate(t *testing.T) {
	tmpl, err := template.New("prototype").Parse(pinnedAdmissionSource)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, struct{ Package string }{Package: "kubernetes"}); err != nil {
		t.Fatal(err)
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "policy.go"), code, 0600); err != nil {
		t.Fatal(err)
	}
	if artifacts := os.Getenv("STEGO_PINNED_POLICY_ARTIFACTS"); artifacts != "" {
		if err := os.Mkdir(artifacts, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "pinned_admission.go"), code, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/policy\ngo 1.26.11\n"), "types.go": []byte("package kubernetes\ntype Object map[string]any\n"), "policy_test.go": pinnedAdmissionTests} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-p=1", "-count=1", "-timeout=15s", "./...")
	command.Dir = directory
	command.Env = localtest.GoEnv()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned policy renderer: %v\n%s", err, output)
	}
}

func TestUnverifiedAdmissionPrototypeIsNotGenerated(t *testing.T) {
	t.Run("ST-SUP-RC14-OC51-T01 No generated file carries the withdrawn pinned admission renderer", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/policy", OutDirName: "out", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application"}}
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatal("Kubernetes runtime output is empty")
		}
		for _, file := range files {
			if strings.Contains(file.Path, "pinned_admission") || bytes.Contains(file.Bytes(), []byte("PinnedAdmissionPolicies")) {
				t.Fatal("unverified admission prototype reached generated output")
			}
		}
	})
}

func TestClientKeyPrefix(t *testing.T) {
	t.Run("ST-SUP-RC14-OC54-T01 Every key the client writes or selects by has the STEGO prefix", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/policy", OutDirName: "out", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application"}}
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		keyPattern := regexp.MustCompile(`stego\.[a-z0-9.-]+/[A-Za-z0-9_.-]*`)
		// The emitted client writes and selects by no STEGO key of its own: it takes each label selector from its
		// caller. The one key that the component holds, pinned-namespace-uid, is in the pinned admission prototype,
		// which the client keeps for the namespace allocation and does not emit yet.
		for _, file := range files {
			text := string(file.Content)
			if strings.Contains(text, "stego"+".dev") {
				t.Fatalf("%s holds the old key prefix", file.Path)
			}
			if keys := keyPattern.FindAllString(text, -1); len(keys) > 0 {
				for _, key := range keys {
					if !strings.HasPrefix(key, "stego.jsell-rh.github.io/") {
						t.Fatalf("%s holds the key %q, which is not under stego.jsell-rh.github.io/", file.Path, key)
					}
				}
			}
		}
		prototype := keyPattern.FindAllString(pinnedAdmissionSource, -1)
		if len(prototype) == 0 || strings.Contains(pinnedAdmissionSource, "stego"+".dev") {
			t.Fatalf("the pinned admission prototype holds the keys %v", prototype)
		}
		for _, key := range prototype {
			if !strings.HasPrefix(key, "stego.jsell-rh.github.io/") {
				t.Fatalf("the pinned admission prototype holds the key %q", key)
			}
		}
	})
}
