package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"testing"
)

// The output contracts of the rendered manifests (ST-SUP-RC15). Each test renders with the generated render
// command, as an operator does, for the service, the worker queue and the RPC process records, and reads the
// document that render writes: render changes the manifest template after it runs, so a test of the template alone
// proves nothing of what is applied.

// renderedWorkloads gives the document that render writes for each workload, by its name.
func renderedWorkloads(t *testing.T) map[string][]map[string]any {
	t.Helper()
	base := []string{"--image", "registry.example.test/team/widget@sha256:" + strings.Repeat("a", 64), "--namespace", "test", "--fs-group", "10001"}
	documents := map[string][]map[string]any{}
	for name, extra := range map[string][]string{"service": nil, "worker queue": {"--worker", "queue"}, "rpc records": {"--rpc-process", "records"}} {
		var output bytes.Buffer
		if err := render(append(append([]string{}, base...), extra...), &output); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		var document struct{ Items []map[string]any }
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatalf("render %s wrote no JSON document: %v", name, err)
		}
		documents[name] = document.Items
	}
	return documents
}

// field reads a nested field of a decoded object by the keys given, or nil.
func field(value any, keys ...string) any {
	for _, key := range keys {
		object, _ := value.(map[string]any)
		value = object[key]
	}
	return value
}

// list reads a field as a list, or nil.
func list(value any, keys ...string) []any {
	items, _ := field(value, keys...).([]any)
	return items
}

// kind gives the items of a kind of one rendered document.
func kind(items []map[string]any, name string) []map[string]any {
	var found []map[string]any
	for _, item := range items {
		if item["kind"] == name {
			found = append(found, item)
		}
	}
	return found
}

// oneDeployment gives the one Deployment of a rendered document, and fails the test unless it holds exactly one.
func oneDeployment(t *testing.T, workload string, items []map[string]any) map[string]any {
	t.Helper()
	deployments := kind(items, "Deployment")
	if len(deployments) != 1 {
		t.Fatalf("%s renders %d Deployments, want one", workload, len(deployments))
	}
	return deployments[0]
}

// policies gives the NetworkPolicies of a rendered document, and fails the test unless it holds at least one.
func policies(t *testing.T, workload string, items []map[string]any) []map[string]any {
	t.Helper()
	found := kind(items, "NetworkPolicy")
	if len(found) == 0 {
		t.Fatalf("%s renders no NetworkPolicy", workload)
	}
	return found
}

// containers gives every container of a Pod spec, the init containers too.
func containers(pod any) []any {
	return append(append([]any{}, list(pod, "initContainers")...), list(pod, "containers")...)
}

func TestRenderedContracts(t *testing.T) {
	documents := renderedWorkloads(t)

	t.Run("ST-SUP-RC15-OC01-T03 Every rendered container refuses privilege escalation, drops every capability and runs read-only and non-root", func(t *testing.T) {
		for workload, items := range documents {
			pod := field(oneDeployment(t, workload, items), "spec", "template", "spec")
			if field(pod, "securityContext", "runAsNonRoot") != true {
				t.Fatalf("%s: the Pod does not run as non-root", workload)
			}
			all := containers(pod)
			if len(all) == 0 {
				t.Fatalf("%s: the Pod holds no container", workload)
			}
			for _, container := range all {
				name := field(container, "name")
				security := field(container, "securityContext")
				drop := list(security, "capabilities", "drop")
				if field(security, "allowPrivilegeEscalation") != false || field(security, "readOnlyRootFilesystem") != true || len(drop) != 1 || drop[0] != "ALL" {
					t.Fatalf("%s: the container %v has the security context %v", workload, name, security)
				}
				if field(security, "runAsNonRoot") == false || field(security, "runAsUser") == float64(0) || field(security, "privileged") == true {
					t.Fatalf("%s: the container %v may run as root or privileged: %v", workload, name, security)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC02-T01 Every emitted Pod selects the RuntimeDefault seccomp profile", func(t *testing.T) {
		for workload, items := range documents {
			pod := field(oneDeployment(t, workload, items), "spec", "template", "spec")
			if field(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
				t.Fatalf("%s: the Pod does not select RuntimeDefault", workload)
			}
			for _, container := range containers(pod) {
				if profile := field(container, "securityContext", "seccompProfile"); profile != nil && field(profile, "type") != "RuntimeDefault" {
					t.Fatalf("%s: the container %v overrides the seccomp profile with %v", workload, field(container, "name"), profile)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC05-T01 The startup and liveness probes use /livez", func(t *testing.T) {
		container := list(oneDeployment(t, "service", documents["service"]), "spec", "template", "spec", "containers")[0]
		for _, probe := range []string{"startupProbe", "livenessProbe"} {
			if got := field(container, probe, "httpGet", "path"); got != "/livez" {
				t.Fatalf("the %s path is %v, want /livez", probe, got)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC05-T02 The readiness probe uses /readyz", func(t *testing.T) {
		container := list(oneDeployment(t, "service", documents["service"]), "spec", "template", "spec", "containers")[0]
		if got := field(container, "readinessProbe", "httpGet", "path"); got != "/readyz" {
			t.Fatalf("the readiness probe path is %v, want /readyz", got)
		}
	})

	t.Run("ST-SUP-RC15-OC06-T01 The NetworkPolicy declares both Ingress and Egress for every workload Pod", func(t *testing.T) {
		for workload, items := range documents {
			deployment := oneDeployment(t, workload, items)
			labels, _ := field(deployment, "spec", "template", "metadata", "labels").(map[string]any)
			selected := false
			for _, policy := range policies(t, workload, items) {
				types := list(policy, "spec", "policyTypes")
				match, _ := field(policy, "spec", "podSelector", "matchLabels").(map[string]any)
				covers := len(match) > 0
				for key, value := range match {
					covers = covers && labels[key] == value
				}
				selected = selected || (len(types) == 2 && types[0] == "Ingress" && types[1] == "Egress" && covers)
			}
			if !selected {
				t.Fatalf("%s: no NetworkPolicy with Ingress and Egress selects the Pods", workload)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC07-T01 DNS egress goes only to the declared resolver namespace and port, over UDP and TCP", func(t *testing.T) {
		for workload, items := range documents {
			for _, policy := range policies(t, workload, items) {
				dns := 0
				for _, rule := range list(policy, "spec", "egress") {
					// A DNS rule is one whose peer selects a namespace by its name, or whose port is 53 or 5353 on any
					// protocol.
					resolver := false
					for _, peer := range list(rule, "to") {
						if field(peer, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != nil && field(peer, "podSelector") == nil {
							resolver = true
						}
					}
					ports := map[any]any{}
					for _, port := range list(rule, "ports") {
						if number := field(port, "port"); number == float64(53) || number == float64(5353) {
							resolver = true
						}
						ports[field(port, "protocol")] = field(port, "port")
					}
					if !resolver {
						continue
					}
					dns++
					to := list(rule, "to")
					if len(to) != 1 || field(to[0], "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "openshift-dns" ||
						len(list(rule, "ports")) != 2 || ports["UDP"] != float64(5353) || ports["TCP"] != float64(5353) {
						t.Fatalf("%s: the DNS rule %v is not to openshift-dns on 5353 over UDP and TCP only", workload, rule)
					}
				}
				if dns != 1 {
					t.Fatalf("%s: %d DNS rules, want one", workload, dns)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC37-T01 The service container sets STEGO_HTTP_REQUIRE_TLS to 1", func(t *testing.T) {
		container := list(oneDeployment(t, "service", documents["service"]), "spec", "template", "spec", "containers")[0]
		for _, variable := range list(container, "env") {
			if field(variable, "name") == "STEGO_HTTP_REQUIRE_TLS" {
				if field(variable, "value") != "1" {
					t.Fatalf("STEGO_HTTP_REQUIRE_TLS is %v", field(variable, "value"))
				}
				return
			}
		}
		t.Fatal("the service container does not set STEGO_HTTP_REQUIRE_TLS")
	})

	t.Run("ST-SUP-RC15-OC37-T02 The TLS settings point at the mounted key pair", func(t *testing.T) {
		deployment := oneDeployment(t, "service", documents["service"])
		container := list(deployment, "spec", "template", "spec", "containers")[0]
		env := map[any]any{}
		for _, variable := range list(container, "env") {
			env[field(variable, "name")] = field(variable, "value")
		}
		secrets := map[any]any{}
		for _, volume := range list(deployment, "spec", "template", "spec", "volumes") {
			secrets[field(volume, "name")] = field(volume, "secret", "secretName")
		}
		mounted := ""
		for _, mount := range list(container, "volumeMounts") {
			if secrets[field(mount, "name")] == "widget-files" {
				mounted, _ = field(mount, "mountPath").(string)
			}
		}
		if mounted == "" || env["STEGO_HTTP_TLS_CERT"] != path.Join(mounted, "tls.crt") || env["STEGO_HTTP_TLS_KEY"] != path.Join(mounted, "tls.key") {
			t.Fatalf("the TLS settings %v and %v do not name the key pair under the mount %q of the service Secret", env["STEGO_HTTP_TLS_CERT"], env["STEGO_HTTP_TLS_KEY"], mounted)
		}
	})

	t.Run("ST-SUP-RC15-OC39-T03 The emitted network policy uses no Technology Preview resource", func(t *testing.T) {
		for workload, items := range documents {
			for _, item := range items {
				version, name := fmt.Sprint(item["apiVersion"]), fmt.Sprint(item["kind"])
				switch name {
				case "AdminNetworkPolicy", "BaselineAdminNetworkPolicy", "EgressFirewall", "DNSNameResolver":
					t.Fatalf("%s renders the Technology Preview resource %s", workload, name)
				case "NetworkPolicy":
					if version != "networking.k8s.io/v1" {
						t.Fatalf("%s renders a NetworkPolicy of %s", workload, version)
					}
				}
				if strings.Contains(version, "policy.networking.k8s.io") || strings.Contains(version, "openshift.io") {
					t.Fatalf("%s renders %s %s", workload, version, name)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC39-T01 An egress destination given as a DNS name is refused", func(t *testing.T) {
		base := []string{"--image", "registry.example.test/team/widget@sha256:" + strings.Repeat("a", 64), "--namespace", "test", "--worker", "remote"}
		for _, host := range []string{"db.example.com:5432", "private-kube.invalid:443"} {
			var output bytes.Buffer
			err := render(append(append([]string{}, base...), "--egress", "provider="+host), &output)
			if err == nil || output.Len() != 0 {
				t.Fatalf("the destination %s rendered a manifest", host)
			}
			if name, _, _ := strings.Cut(host, ":"); strings.Contains(err.Error(), name) {
				t.Fatalf("the refusal of %s echoes the host: %v", host, err)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC39-T02 An egress destination given as an IP address and port is rendered as one ipBlock rule", func(t *testing.T) {
		var output bytes.Buffer
		args := []string{"--image", "registry.example.test/team/widget@sha256:" + strings.Repeat("a", 64), "--namespace", "test", "--worker", "remote", "--egress", "provider=192.0.2.10:5432"}
		if err := render(args, &output); err != nil {
			t.Fatal(err)
		}
		var document struct{ Items []map[string]any }
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, policy := range policies(t, "worker remote", document.Items) {
			for _, rule := range list(policy, "spec", "egress") {
				to := list(rule, "to")
				if len(to) == 0 || field(to[0], "ipBlock") == nil {
					continue
				}
				found++
				ports := list(rule, "ports")
				if len(to) != 1 || field(to[0], "ipBlock", "cidr") != "192.0.2.10/32" || len(ports) != 1 || field(ports[0], "protocol") != "TCP" || field(ports[0], "port") != float64(5432) {
					t.Fatalf("the destination rule is %v, want one ipBlock 192.0.2.10/32 on TCP 5432", rule)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%d ipBlock rules, want one", found)
		}
	})
}
