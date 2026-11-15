package delivery

import (
	"strings"
	"testing"
)

// testStanza is the Argo CD part of the delivery stanza of ST-DLV-TD01.
var testStanza = ArgoCDStanza{Project: "orders", SourceRepo: "https://git.example/orders.git", SourcePath: BaseFolder,
	EnvironmentRepo: "https://git.example/platform.git", EnvironmentRevision: "main", EnvironmentPath: "environments/orders/*.yaml"}

func TestDeliveryObject(t *testing.T) {
	t.Run("ST-DLV-INV10-T The delivery object takes its cluster, namespace and overlay from the environment files, never from the declaration", func(t *testing.T) {
		application, err := WriteApplication(BaseWritten{Service: "orders", Base: BaseFolder}, testStanza)
		if err != nil {
			t.Fatal(err)
		}
		text := string(application.File)
		for _, want := range []string{"server: '{{.destination.cluster}}'", "namespace: '{{.destination.namespace}}'", "path: '{{.destination.overlay_path}}'", "project: orders"} {
			if !strings.Contains(text, want) {
				t.Errorf("the ApplicationSet does not hold %s:\n%s", want, text)
			}
		}
		for name, edit := range map[string][2]string{
			"a literal cluster":          {"server: '{{.destination.cluster}}'", "server: https://kubernetes.default.svc"},
			"a literal namespace":        {"namespace: '{{.destination.namespace}}'", "namespace: kube-system"},
			"a literal overlay":          {"path: '{{.destination.overlay_path}}'", "path: overlays/prod"},
			"no missingkey=error":        {"- missingkey=error", "- missingkey=default"},
			"no Go templates":            {"goTemplate: true", "goTemplate: false"},
			"the project default":        {"project: orders", "project: default"},
			"an overlay of another repo": {"        repoURL: https://git.example/platform.git\n        targetRevision", "        repoURL: https://git.example/other.git\n        targetRevision"},
			"a sync policy":              {"      project: orders\n", "      project: orders\n      syncPolicy: {automated: {prune: true}}\n"},
		} {
			if !strings.Contains(text, edit[0]) {
				t.Fatalf("the test edit for %s finds no %q in:\n%s", name, edit[0], text)
			}
			if _, err := LoadApplicationSet([]byte(strings.Replace(text, edit[0], edit[1], 1))); err == nil {
				t.Errorf("LoadApplicationSet accepts an ApplicationSet with %s", name)
			}
		}
		for name, stanza := range map[string]ArgoCDStanza{
			"the project default":         func() ArgoCDStanza { s := testStanza; s.Project = "default"; return s }(),
			"a host that starts with '-'": func() ArgoCDStanza { s := testStanza; s.EnvironmentRepo = "ssh://git@-oproxy/p.git"; return s }(),
		} {
			if _, err := WriteApplication(BaseWritten{Service: "orders", Base: BaseFolder}, stanza); err == nil {
				t.Errorf("WriteApplication accepts a stanza with %s", name)
			}
		}
	})
}
