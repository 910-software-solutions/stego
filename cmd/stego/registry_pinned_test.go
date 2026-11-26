package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// pinnedSources gives a project of the declaration of ST-DECL-TD02 whose registry is the shipped registry, split
// into two Git sources pinned in .stego/config.yaml by their full commits: the common source holds every part but
// the mixin go-sdk, and the local source holds that mixin. It gives the project and both repositories.
func pinnedSources(t *testing.T) (project, common, local string) {
	t.Helper()
	shipped := filepath.Join(t.TempDir(), "shipped")
	if err := shippedRegistryInto(shipped); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	if err := filepath.WalkDir(shipped, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(shipped, path)
		files[filepath.ToSlash(name)] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commonFiles, localFiles := map[string]string{}, map[string]string{}
	for name, data := range files {
		if strings.HasPrefix(name, "mixins/go-sdk/") {
			localFiles[name] = data
		} else {
			commonFiles[name] = data
		}
	}
	common, commonSHA := registryRepository(t, "common-registry", commonFiles)
	local, localSHA := registryRepository(t, "local-registry", localFiles)
	project = declaringProject(t)
	putFile(t, project, "service.yaml", td02)
	registryProject(t, project, "", pinned(common, commonSHA), pinned(local, localSHA))
	return project, common, local
}

// committedDigest gives the content digest of the registry, as registry.registryDigest computes it, of the files of
// the repositories at their HEAD commits, in the order given: each name once, sorted, with its length, its size and
// its SHA-256. It reads the committed files, not the working copies.
func committedDigest(t *testing.T, repositories ...string) string {
	t.Helper()
	type entry struct {
		size int64
		hash [32]byte
	}
	captured := map[string]entry{}
	for _, repository := range repositories {
		for _, name := range strings.Split(git(t, repository, "ls-tree", "-r", "--name-only", "HEAD"), "\n") {
			if name == "" || !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".proto") || strings.Contains(name, "/example/")) {
				continue
			}
			blob := exec.Command("git", "cat-file", "blob", "HEAD:"+name)
			blob.Dir = repository
			raw, err := blob.Output()
			if err != nil {
				t.Fatalf("git cat-file %s: %v", name, err)
			}
			data := string(raw)
			captured[name] = entry{int64(len(data)), sha256.Sum256([]byte(data))}
		}
	}
	digest := sha256.New()
	io.WriteString(digest, "stego-registry-content-v1\x00")
	var length [8]byte
	for _, name := range slices.Sorted(maps.Keys(captured)) {
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		digest.Write(length[:])
		io.WriteString(digest, name)
		binary.BigEndian.PutUint64(length[:], uint64(captured[name].size))
		digest.Write(length[:])
		hash := captured[name].hash
		digest.Write(hash[:])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// recordedDigest gives the registry content digest that the record of the project holds.
func recordedDigest(t *testing.T, project string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, ".stego", "state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		LastApplied struct {
			Digest string `yaml:"registry_content_sha256"`
		} `yaml:"last_applied"`
	}
	if err := yaml.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.LastApplied.Digest
}

// sourceState is the bytes, the mode and the time of change of each file of a folder, and its git status.
func sourceState(t *testing.T, folder string) map[string]string {
	t.Helper()
	// The Git metadata too, apart from the index that git status refreshes: every ref, the local configuration, and
	// the list of objects with their sizes (review of #505, L2).
	state := map[string]string{"git status": git(t, folder, "status", "--porcelain", "--ignored"),
		"git refs":    git(t, folder, "for-each-ref", "--format=%(refname) %(objectname)"),
		"git config":  git(t, folder, "config", "--local", "--list"),
		"git objects": git(t, folder, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")}
	if err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The Git metadata is not a registry file; git status itself refreshes its index.
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data := ""
		if entry.Type().IsRegular() {
			bytes, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			data = string(bytes)
		}
		state[path] = info.Mode().String() + " " + info.ModTime().Format(time.RFC3339Nano) + " " + data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRegistryReadAtPinnedContent(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-A1-SR01-T01 A run reads the registry at the pinned commit, not at a later commit", func(t *testing.T) {
		project, common, local := pinnedSources(t)
		pinnedDigest := committedDigest(t, common, local)
		// After the pin: a later commit that changes rest-crud, and a change in the working copy.
		archetype := filepath.Join(common, "archetypes", "rest-crud", "archetype.yaml")
		data, err := os.ReadFile(archetype)
		if err != nil {
			t.Fatal(err)
		}
		later := strings.Replace(string(data), "  cors: enabled\n", "  cors: disabled\n", 1)
		if later == string(data) {
			t.Fatal("the later commit does not change rest-crud")
		}
		commitRegistry(t, common, map[string]string{"archetypes/rest-crud/archetype.yaml": later})
		if err := os.WriteFile(filepath.Join(common, "components", "rest-api", "component.yaml"), []byte("kind: broken\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
			t.Fatalf("stego apply: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		if got := recordedDigest(t, project); got != pinnedDigest {
			t.Fatalf("the record holds the content digest %s, want %s of the pinned commits", got, pinnedDigest)
		}
	})

	t.Run("ST-SUP-A1-SR01-T02 A run writes nothing to the registry sources", func(t *testing.T) {
		project, common, local := pinnedSources(t)
		before := []map[string]string{sourceState(t, common), sourceState(t, local)}
		for _, command := range [][]string{{"validate"}, {"plan"}, {"apply"}, {"registry", "compose"}} {
			if run := stegoIn(t, project, nil, command...); run.code != 0 {
				t.Fatalf("stego %v: exit %d\n%s%s", command, run.code, run.stdout, run.stderr)
			}
		}
		for i, folder := range []string{common, local} {
			after := sourceState(t, folder)
			for name, state := range before[i] {
				if after[name] != state {
					t.Fatalf("%s changed in %s: %.200q, was %.200q", name, folder, after[name], state)
				}
			}
			for name := range after {
				if _, ok := before[i][name]; !ok {
					t.Fatalf("a run added %s to %s", name, folder)
				}
			}
		}
	})

	t.Run("ST-SUP-A1-SR01-T03 The recorded content digest is the digest of what the run read", func(t *testing.T) {
		project, common, local := pinnedSources(t)
		if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
			t.Fatalf("stego apply: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		first := recordedDigest(t, project)
		if want := committedDigest(t, common, local); first != want {
			t.Fatalf("the record holds %s, want %s", first, want)
		}
		// A later pin of the local source, at a commit that adds a file, moves the recorded digest to the digest of
		// the files at the new commits.
		localSHA := commitRegistry(t, local, map[string]string{"mixins/go-sdk/README.md": "The go-sdk mixin.\n", "mixins/go-sdk/notes.yaml": "kind: notes\n"})
		registryProject(t, project, "", pinned(common, git(t, common, "rev-parse", "HEAD")), pinned(local, localSHA))
		if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
			t.Fatalf("stego apply at the new pin: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		second := recordedDigest(t, project)
		if want := committedDigest(t, common, local); second != want || second == first {
			t.Fatalf("at the new pin the record holds %s, want %s, which differs from %s", second, want, first)
		}
	})
}
