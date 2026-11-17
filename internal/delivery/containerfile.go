package delivery

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/appbuild"
)

// ContainerFile is the path, in the project folder, of the offline Containerfile (ST-DLV-INV07). The build context
// is the project folder.
const ContainerFile = "Containerfile"

// BuilderImage is the image of the build stage: the official Go image of the Go SDK release that the application
// build pins (appbuild.GoVersion), named by the digest of its image index, so that a tag that moves does not change
// the build.
const BuilderImage = "docker.io/library/golang:1.26.11-trixie@sha256:fa340cb59ebeb1d53ac1cccfc334201d938fe5f3458003cccb767096d8e032b8"

// PrefetchedModules is the module cache that the build stage reads: the folder where the Go module prefetch of
// Konflux puts the modules (prefetch-input gomod). A local build mounts a module cache that stego build download
// wrote at the same path.
const PrefetchedModules = "/cachi2/output/deps/gomod/pkg/mod"

// ImageExecutable is the path of the executable in the image, which the container starts.
const ImageExecutable = "/service"

// ImageUser is the numeric user and group of the image, so that the restricted pod settings (runAsNonRoot) of the
// base accept it with no runAsUser.
const ImageUser = "65532:65532"

// buildPaths are the settings of the build stage that name a folder of the stage. The module cache is the
// prefetched one, and every other folder is under /tmp, so that no setting of the caller or of the image is used.
var buildPaths = map[string]string{
	"PATH": "/usr/local/go/bin:/usr/bin:/bin", "GOROOT": "/usr/local/go", "HOME": "/tmp/home", "GOPATH": "/tmp/gopath",
	"GOMODCACHE": PrefetchedModules, "GOCACHE": "/tmp/go-cache", "TMPDIR": "/tmp",
}

// containerHeader is the first lines of the Containerfile.
const containerHeader = "# The offline Containerfile of the service. stego apply writes this file from the declaration. Do not edit it.\n" +
	"# The build stage downloads no module: with the module proxy off, it reads only the Go modules at\n" +
	"# " + PrefetchedModules + ", where the Go module prefetch of Konflux puts them (prefetch-input gomod).\n" +
	"# The build holds them to go.sum (-mod=readonly), and go mod verify checks the module cache. This file does not\n" +
	"# turn off the network: build it in the hermetic mode of Konflux, or locally with --network=none. For a local\n" +
	"# build, mount a copy of the module cache folder that stego build download wrote at that path, writable,\n" +
	"# because go extracts the modules from it.\n" +
	"# The build has the settings and the arguments of stego build, with the Go SDK that it pins, so the executable is\n" +
	"# the one that the build record names. The image is scratch with the executable and the CA certificates, and it\n" +
	"# runs as the user " + ImageUser + ": it writes no file, so it runs with a read-only root file system and an\n" +
	"# emptyDir at /tmp, as the Kubernetes base sets them.\n"

// ContainerSource is what the build stage copies from the project folder: the folder of the generated output, the
// entry point of the build, whether the fills folder holds a fill, which the generated code then imports, and the
// folders of the project that go.mod replaces a module with, which the build reads as stego build does. Replacements
// are the paths of the replace directives of go.mod as it gives them; WriteDeployment makes Modules from them
// (ModuleFolders).
type ContainerSource struct {
	OutputDir    string
	Fills        bool
	Modules      []string
	Replacements []string
}

// moduleNote is the plan line for a local module that the Containerfile does not copy, with the path quoted, so
// that no text of go.mod can change the line (security review of PR #395, L1).
const moduleNote = "the Containerfile does not copy the local module %q that go.mod replaces a module with: %s, so the image build cannot read it"

// ModuleFolders gives the folders that the build stage copies for the local module replacements of go.mod, in path
// order, and a plan line for each replacement that it does not copy as its own folder. A replacement in the output
// folder, in a copied fills folder or the project folder itself needs no copy of its own. A replacement in another
// replaced folder is copied with that folder. A replacement out of the project folder, with a name that the
// Containerfile cannot hold, or that holds the output folder or the fills folder, is not copied, and the line says
// why, so that apply does not fail on a layout that stego build accepts (security review of PR #395, L2).
func ModuleFolders(replacements []string, outputDir string, fills bool) ([]string, []string) {
	copied := []string{outputDir}
	if fills {
		copied = append(copied, "fills")
	}
	within := func(folder, parent string) bool { return folder == parent || strings.HasPrefix(folder, parent+"/") }
	var candidates, notes []string
	for _, given := range replacements {
		folder := path.Clean(given)
		switch {
		case path.IsAbs(given) || strings.ContainsAny(given, "\\:") || folder == ".." || strings.HasPrefix(folder, "../"):
			notes = append(notes, fmt.Sprintf(moduleNote, given, "it is out of the project folder"))
		case folder == "." || slices.ContainsFunc(copied, func(c string) bool { return within(folder, c) }):
		case !sourceFolder.MatchString(folder):
			notes = append(notes, fmt.Sprintf(moduleNote, given, "its folder is not a relative path of letters, digits, '.', '_' and '-' that starts with a letter or a digit"))
		case slices.ContainsFunc(copied, func(c string) bool { return within(c, folder) }):
			notes = append(notes, fmt.Sprintf(moduleNote, given, "its folder holds the output folder or the fills folder, which the Containerfile copies on its own"))
		case !slices.Contains(candidates, folder):
			candidates = append(candidates, folder)
		}
	}
	slices.Sort(candidates)
	var folders []string
	for _, folder := range candidates {
		if slices.ContainsFunc(folders, func(parent string) bool { return within(folder, parent) }) {
			continue
		}
		folders = append(folders, folder)
	}
	return folders, notes
}

// sourceFolder is a folder of the project that the Containerfile names: a relative path of plain names, so that no
// text of it can change an instruction.
var sourceFolder = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// WriteContainerfile writes the offline Containerfile of the service (ST-DLV-CMD03): a build stage on the pinned Go
// image that copies the module files and the source folders, with each local module folder that go.mod replaces a
// module with, checks the Go version and the prefetched modules, and builds with the settings and the arguments of
// the application build and no module download; then a scratch stage with the executable, the CA certificates and
// a numeric user that is not root. It reads the file back with LoadContainerfile before it gives it (ST-DLV-INV07).
func WriteContainerfile(service string, source ContainerSource) ([]byte, error) {
	if !serviceLabel.MatchString(service) {
		return nil, fmt.Errorf("the service name %q is not a DNS label", service)
	}
	data, err := renderContainerfile(source)
	if err != nil {
		return nil, err
	}
	if _, err := LoadContainerfile(data); err != nil {
		return nil, fmt.Errorf("the Containerfile of %s does not read back: %w", service, err)
	}
	return data, nil
}

// renderContainerfile gives the bytes of the offline Containerfile for the source folders. It is the one text that
// WriteContainerfile writes and that LoadContainerfile accepts.
func renderContainerfile(source ContainerSource) ([]byte, error) {
	if !sourceFolder.MatchString(source.OutputDir) || slices.Contains(strings.Split(source.OutputDir, "/"), "..") {
		return nil, fmt.Errorf("the output folder %q is not a relative path of plain names", source.OutputDir)
	}
	folders := []string{source.OutputDir}
	if source.Fills {
		folders = append(folders, "fills")
	}
	for _, module := range source.Modules {
		if !sourceFolder.MatchString(module) || slices.Contains(strings.Split(module, "/"), "..") {
			return nil, fmt.Errorf("the local module folder %q that go.mod replaces a module with is not a relative path of plain names", module)
		}
		for _, folder := range folders {
			if module == folder || strings.HasPrefix(module, folder+"/") || strings.HasPrefix(folder, module+"/") {
				return nil, fmt.Errorf("the local module folder %q is in the folder %s or holds it; the Containerfile copies each folder once", module, folder)
			}
		}
		folders = append(folders, module)
	}
	var out strings.Builder
	out.WriteString(containerHeader)
	fmt.Fprintf(&out, "FROM %s AS build\nWORKDIR /src\nCOPY go.mod go.sum ./\n", BuilderImage)
	for _, folder := range folders {
		fmt.Fprintf(&out, "COPY %s/ ./%s/\n", folder, folder)
	}
	// env -i drops each setting that the image or the build host gives, the environment file of the prefetch too,
	// so the build has the settings of the build record alone.
	out.WriteString("RUN env -i")
	for _, setting := range buildSettings() {
		if strings.ContainsAny(setting, "'\\\n") {
			return nil, fmt.Errorf("the build setting %q cannot be quoted in the Containerfile", setting)
		}
		fmt.Fprintf(&out, " \\\n    '%s'", setting)
	}
	fmt.Fprintf(&out, " \\\n    /bin/sh -ec 'test \"$(go env GOVERSION)\" = %s && go mod verify && go %s'\n",
		appbuild.GoVersion, strings.Join(appbuild.BuildFlags(ImageExecutable, source.OutputDir), " "))
	fmt.Fprintf(&out, "FROM scratch\nCOPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n"+
		"COPY --from=build --chmod=0555 %s %s\nUSER %s\nENTRYPOINT [%q]\n", ImageExecutable, ImageExecutable, ImageUser, ImageExecutable)
	return []byte(out.String()), nil
}

// buildSettings gives each setting of the build stage as name=value, in name order: the fixed settings of the
// application build and the folders of the stage.
func buildSettings() []string {
	settings := appbuild.BuildEnvironment()
	maps.Copy(settings, buildPaths)
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		lines = append(lines, name+"="+settings[name])
	}
	return lines
}

// Instruction is one instruction of a Containerfile: its keyword in upper case and the rest of its logical line.
type Instruction struct {
	Keyword string
	Args    string
}

// Containerfile is a Containerfile read back: its build stage and its final stage, each with its instructions
// after its FROM.
type Containerfile struct {
	Builder string
	Build   []Instruction
	Final   []Instruction
}

// digestImage is an image named by a sha256 digest.
var digestImage = regexp.MustCompile(`^[a-z0-9./:_-]+@sha256:[0-9a-f]{64}$`)

// keywords are the instructions that the offline Containerfile may hold: no ADD, which can fetch from the network,
// and no instruction that changes the shell or the build arguments.
var keywords = []string{"FROM", "WORKDIR", "COPY", "RUN", "USER", "ENTRYPOINT"}

// LoadContainerfile reads a Containerfile back and holds it to the offline shape (ST-DLV-INV07): two stages, the
// first on the pinned Go image, which runs the application build with the module proxy off, the module checks and
// the settings of the build record, and the last on scratch, which runs nothing, sets a numeric user that is not
// root and starts the executable by its absolute path in exec form. Then it holds the file to the exact bytes that
// WriteContainerfile makes for the source folders that the build stage copies, so that no other text passes: more
// text in the RUN, a COPY with --from after another flag, a parser directive such as # syntax= or # escape=, or a
// heredoc (security review of PR #367, L1). WriteContainerfile calls it on the text it makes, before apply writes the
// file. A hand edit of the file on the disk is caught by the digest that the record keeps under project_files, as
// for each file of the base.
func LoadContainerfile(data []byte) (Containerfile, error) {
	var file Containerfile
	var stages [][]Instruction
	var froms []string
	for _, instruction := range logicalLines(string(data)) {
		if !slices.Contains(keywords, instruction.Keyword) {
			return Containerfile{}, fmt.Errorf("the instruction %s is not one of %s", instruction.Keyword, strings.Join(keywords, ", "))
		}
		if instruction.Keyword == "FROM" {
			froms = append(froms, instruction.Args)
			stages = append(stages, nil)
			continue
		}
		if len(stages) == 0 {
			return Containerfile{}, fmt.Errorf("the instruction %s comes before the first FROM", instruction.Keyword)
		}
		stages[len(stages)-1] = append(stages[len(stages)-1], instruction)
	}
	if len(froms) != 2 {
		return Containerfile{}, fmt.Errorf("the Containerfile has %d stages, not a build stage and a final stage", len(froms))
	}
	builder, alias, ok := strings.Cut(froms[0], " AS ")
	if !ok || alias != "build" || !digestImage.MatchString(builder) {
		return Containerfile{}, fmt.Errorf("the build stage is not FROM an image named by its sha256 digest AS build")
	}
	if builder != BuilderImage {
		return Containerfile{}, fmt.Errorf("the build stage is FROM %s, not the pinned Go image %s", builder, BuilderImage)
	}
	if froms[1] != "scratch" {
		return Containerfile{}, fmt.Errorf("the final stage is FROM %s, not scratch", froms[1])
	}
	file.Builder, file.Build, file.Final = builder, stages[0], stages[1]
	var runs, folders []string
	for _, instruction := range file.Build {
		switch instruction.Keyword {
		case "RUN":
			runs = append(runs, instruction.Args)
		case "COPY":
			if strings.Contains(instruction.Args, "--from") {
				return Containerfile{}, fmt.Errorf("the build stage copies from another image: %s", instruction.Args)
			}
			if instruction.Args == "go.mod go.sum ./" {
				continue
			}
			from, _, _ := strings.Cut(instruction.Args, " ")
			folders = append(folders, strings.TrimSuffix(from, "/"))
		}
	}
	if len(runs) != 1 {
		return Containerfile{}, fmt.Errorf("the build stage has %d RUN instructions, not the one build", len(runs))
	}
	if !strings.HasPrefix(runs[0], "env -i ") {
		return Containerfile{}, fmt.Errorf("the build does not start from an empty environment (env -i)")
	}
	for _, setting := range buildSettings() {
		if !strings.Contains(runs[0], "'"+setting+"'") {
			return Containerfile{}, fmt.Errorf("the build does not set %s", setting)
		}
	}
	if !strings.Contains(runs[0], "go mod verify && go build -mod=readonly -trimpath -buildvcs=false -pgo=off -p=2 -o "+ImageExecutable+" ./") {
		return Containerfile{}, fmt.Errorf("the build does not verify the modules and build with the arguments of the application build")
	}
	var user, entrypoint string
	for _, instruction := range file.Final {
		switch instruction.Keyword {
		case "RUN":
			return Containerfile{}, fmt.Errorf("the final stage runs %s; a scratch stage runs nothing", instruction.Args)
		case "COPY":
			if !strings.HasPrefix(instruction.Args, "--from=build ") {
				return Containerfile{}, fmt.Errorf("the final stage copies from the build context: %s", instruction.Args)
			}
		case "USER":
			user = instruction.Args
		case "ENTRYPOINT":
			entrypoint = instruction.Args
		}
	}
	if !nonRootUser.MatchString(user) {
		return Containerfile{}, fmt.Errorf("the final stage sets the user %q, not a numeric user and group that is not root", user)
	}
	if entrypoint != fmt.Sprintf("[%q]", ImageExecutable) || !path.IsAbs(ImageExecutable) {
		return Containerfile{}, fmt.Errorf("the final stage does not start %s in exec form", ImageExecutable)
	}
	// The source folders are the output folder, then the fills folder when the build stage copies it, then the local
	// module folders.
	if len(folders) == 0 {
		return Containerfile{}, fmt.Errorf("the build stage copies no output folder")
	}
	source := ContainerSource{OutputDir: folders[0]}
	rest := folders[1:]
	if len(rest) > 0 && rest[0] == "fills" {
		source.Fills, rest = true, rest[1:]
	}
	source.Modules = rest
	want, err := renderContainerfile(source)
	if err != nil {
		return Containerfile{}, err
	}
	if !bytes.Equal(data, want) {
		return Containerfile{}, fmt.Errorf("the Containerfile is not the one that stego apply writes for the output folder %s; run stego apply to write it again", source.OutputDir)
	}
	return file, nil
}

// nonRootUser is a numeric user and group, neither of them 0.
var nonRootUser = regexp.MustCompile(`^[1-9][0-9]*:[1-9][0-9]*$`)

// logicalLines gives the instructions of a Containerfile: comment and blank lines are dropped, and a line that
// ends with a backslash continues on the next line.
func logicalLines(text string) []Instruction {
	var instructions []Instruction
	var current strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if current.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if continued, ok := strings.CutSuffix(trimmed, "\\"); ok {
			current.WriteString(strings.TrimSpace(continued) + " ")
			continue
		}
		current.WriteString(trimmed)
		keyword, args, _ := strings.Cut(current.String(), " ")
		instructions = append(instructions, Instruction{Keyword: strings.ToUpper(keyword), Args: strings.TrimSpace(args)})
		current.Reset()
	}
	if current.Len() > 0 {
		keyword, args, _ := strings.Cut(strings.TrimSpace(current.String()), " ")
		instructions = append(instructions, Instruction{Keyword: strings.ToUpper(keyword), Args: strings.TrimSpace(args)})
	}
	return instructions
}
