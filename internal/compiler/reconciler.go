package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
)

// FileAction describes what will happen to a generated file.
type FileAction string

const (
	ActionGenerate  FileAction = "generate"
	ActionUpdate    FileAction = "update"
	ActionUnchanged FileAction = "unchanged"
	ActionDelete    FileAction = "delete"
)

// PlannedFile represents a single file in the plan.
type PlannedFile struct {
	Path   string
	Action FileAction
}

// EntityChange describes a change to an entity's fields between applies.
type EntityChange struct {
	Entity   string
	Added    []string // "name (type)" format
	Removed  []string // "name (type)" format
	Modified []string // "name (old_type → new_type)" or "name (type)" format
}

// Plan represents the computed changeset between desired and current state.
type Plan struct {
	// Files lists every file that would be generated, along with its action.
	Files []PlannedFile

	// EntityChanges lists entity field changes detected between the current
	// service.yaml and the previous apply.
	EntityChanges []EntityChange

	// GeneratedFiles holds the actual file contents ready to write.
	GeneratedFiles []gen.File

	// NewState is the state that will be written after a successful apply.
	NewState *State

	// StateChanged includes input and ownership changes with unchanged output.
	StateChanged bool

	// HandEdited lists, in path order, each generated file that the record
	// lists and that is missing or no longer matches its recorded hash
	// (ST-GEN-SP01). Apply refuses such a plan unless ReplaceHandEdits is set.
	HandEdited []string
	// Restored lists, in path order, each generated file that the record tracks and that is missing: it holds no
	// hand edit, so the run writes it again with no flag, and the plan names it as restored (ST-GEN-Q46).
	Restored []string

	// ReplaceHandEdits states that the author lets this apply replace the
	// hand-edited files (stego apply --replace-hand-edits).
	ReplaceHandEdits bool

	// Notes are the lines that the plan states besides its files, such as that the run writes no delivery object
	// because the declaration has no delivery stanza (ST-DLV-UC07-R05).
	Notes []string

	// saved is the saved plan that apply carries out, when it is given one.
	saved *SavedPlan

	inputManifestSHA string
	registry         *registry.Registry
	requirements     map[string]string
	// runtime is the archetype's runtime pin when generated code imports the
	// runtime library, and nil otherwise.
	runtime    *registry.RuntimePin
	snapshots  map[string]fileSnapshot
	projectDir string
	outDir     string
}

// HasChanges returns true if the plan includes any generate, update, delete,
// or entity changes.
func (p *Plan) HasChanges() bool {
	if p.StateChanged {
		return true
	}
	return p.hasOutputChanges()
}

func (p *Plan) hasOutputChanges() bool {
	if len(p.EntityChanges) > 0 {
		return true
	}
	for _, f := range p.Files {
		if f.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// hasCodeChanges tells whether the plan changes a file that comes from the declaration, which module resolution
// waits for (ST-BLD-UC01-R03). The offline Containerfile is left out: it also copies the local module folders that
// go.mod replaces a module with, so a replace directive that the service author adds before stego deps changes it,
// with no change of the declaration. The next apply writes it.
func (p *Plan) hasCodeChanges() bool {
	if len(p.EntityChanges) > 0 {
		return true
	}
	for _, f := range p.Files {
		if f.Action != ActionUnchanged && f.Path != ContainerFile {
			return true
		}
	}
	return false
}

// ReconcilerInput gathers everything needed to compute a plan.
type ReconcilerInput struct {
	// ProjectDir is the root of the project (where service.yaml lives).
	ProjectDir string

	// RegistryDir is the path to the registry directory.
	RegistryDir string
	// RegistryDirs selects all composed sources. If empty, RegistryDir is used.
	RegistryDirs []string

	// Generators maps component name to its Generator implementation.
	Generators map[string]gen.Generator

	// GoVersion is the Go version for go.mod (e.g. "1.22").
	GoVersion string

	// ModuleName is the Go module path (e.g. "github.com/myorg/user-service").
	ModuleName string

	// RegistrySHA is the supplied registry reference, which can be a local
	// label. Captured registry content has a separate SHA-256 identity.
	RegistrySHA string

	// OutDir is the output directory for generated files. Defaults to
	// filepath.Join(ProjectDir, "out") if empty.
	OutDir string

	// ReadFill reads the collection that each fill named in a slot binding
	// serves. Validation needs it when a slot binding names a fill whose
	// fill.yaml exists; cmd/stego gives fill.ReadCollection.
	ReadFill FillReader
	// ReadFillSlot reads the slot that each fill named by a command, a query or a worker implements;
	// cmd/stego gives fill.ReadImplements.
	ReadFillSlot FillSlotReader

	// QualifyFills checks each fill that a slot binding names: that it fits
	// its binding and that it is qualified. Validate, plan and apply stop on
	// each fault it gives. cmd/stego gives fill.QualificationFaults.
	QualifyFills FillQualifier

	// WriteRequirements writes the platform requirements from the needs of the active set; cmd/stego gives
	// delivery.WriteRequirements. Without it a run writes no platform requirements.
	WriteRequirements RequirementsWriter

	// WriteDeployment writes the Kubernetes base from the platform requirements of the run and the probes of the
	// active set; cmd/stego gives delivery.WriteDeployment. Without it, or without platform requirements, a run
	// writes no Kubernetes base.
	WriteDeployment DeploymentWriter

	// CheckOnly marks a plan that stego validate makes only to read the pins it uses. Such a plan is never
	// written, so Reconcile does not refuse it for a part of the declaration that no component can generate yet.
	CheckOnly bool

	// WarnUnproved makes checking report the fault of an unproved fill (FillFault.Unproved) as a warning that names
	// stego test and stego fill sign, not as a fault. Only stego validate sets it, and Reconcile refuses it on a plan
	// that is not CheckOnly, so no plan that is written, and no binary, holds a fill with no evidence or no sign-off
	// (ST-DECL-UC01-R01, ST-GEN-INV10).
	WarnUnproved bool
}

// boundMixins gives the mixins that a declaration binds, with each alias replaced by the mixins it names, each once
// and in the order of the declaration (ST-SUP-INV17). The registry holds no alias of an alias. It refuses a mixin
// that the registry does not hold, and a bound mixin whose required mixin is not bound too (ST-SUP-INV16).
func boundMixins(reg *registry.Registry, declared []string) ([]string, error) {
	var mixins []string
	add := func(name string) {
		if !slices.Contains(mixins, name) {
			mixins = append(mixins, name)
		}
	}
	for _, name := range declared {
		mixin := reg.Mixin(name)
		if mixin == nil {
			return nil, fmt.Errorf("mixin %q not found in registry", name)
		}
		if len(mixin.AliasOf) == 0 {
			add(name)
			continue
		}
		for _, target := range mixin.AliasOf {
			if resolved := reg.Mixin(target); resolved == nil || len(resolved.AliasOf) > 0 {
				return nil, fmt.Errorf("mixin %q is an alias of %q, which is no mixin of the registry", name, target)
			}
			add(target)
		}
	}
	if err := reg.RequiredMixins(mixins); err != nil {
		return nil, err
	}
	return mixins, nil
}

// FillSlot is one slot of the active component set, or of a mixin of the
// declaration, with the path of its contract in the registry.
type FillSlot struct {
	Ref      types.SlotRef
	Contract string
}

// FillBinding is one fill that a slot binding names, with the slot and the
// collection of the binding, and where the declaration names the fill, such
// as slots[1].gate[0].
//
// The fill of a command, a query or a worker names no collection: its binding is the one command, query or worker
// that names it (decision on ST-EXT-Q30). Such a binding has the slot command, query or worker, no collection and the
// owner, such as command place-order, with the Go name that generation gives the command, query or worker in its
// contract package, such as PlaceOrder.
type FillBinding struct {
	Fill       string
	Slot       string
	Collection string
	Path       string
	Owner      string
	GoName     string
}

// FillFault is one fault of a bound fill at one binding: the rule it breaks,
// the binding's path in the declaration, the fill it names and the fault in a
// sentence.
type FillFault struct {
	Rule    string
	Path    string
	Found   string
	Message string
	// Unproved marks the fault of a fill that fits its binding and has its test file, and that has no evidence or
	// no sign-off only: stego test, then stego fill sign, qualify it. stego validate reports such a fault as a
	// warning (ReconcilerInput.WarnUnproved); plan, apply, build and deps refuse it (ST-GEN-INV10).
	Unproved bool
}

// FillContracts reads a slot contract and the files it imports from the
// active registry.
type FillContracts interface {
	ReadFile(name string) ([]byte, error)
	ReadProtoImport(name string) ([]byte, error)
}

// FillCheck is what a check of the fills needs from the declaration and the
// active registry: each active slot with its contract, each fill that a slot
// binding names, the declared collections, and the registry's contracts.
// Its slots and bindings hold the command, query and worker slots and the binding of the fill of each command,
// query and worker, so that these fills are qualified as every fill is (decision on ST-EXT-Q30).
type FillCheck struct {
	ProjectDir  string
	Slots       []FillSlot
	Bindings    []FillBinding
	Collections []string
	Contracts   FillContracts
	// Module is the Go module path of the service, whose package out/slots
	// holds the slot contracts.
	Module string
	// ModFile is the module file that the check lists the fills with in place of the project's go.mod, such as the
	// staged go.mod of stego deps, with the go.sum beside it; empty for the project's own go.mod and go.sum.
	ModFile string
	// RuntimeProxy stages a module proxy folder that serves the archetype's runtime pin from the registry's runtime
	// folder under stage, beside a copy of go.mod and go.sum there, and gives its path, as stego deps does; "" when the
	// registry does not hold the pinned version. Nil when the archetype pins no runtime.
	RuntimeProxy func(stage string) (string, error)
	// FillModules are the modules that the declaration lets fills import.
	FillModules []string
	// CommandFills are the fills that the commands of the declaration name, with the import path of the command
	// slot contract, whose import closure the qualifier checks (ST-SUP-RC32-OC03).
	CommandFills []CommandFill
	// QueryContract is the import path of the query slot contract package, out/queries, which a fill of the query
	// slot imports (ST-EXT-INV05); empty when the service has no query component.
	QueryContract string
	// QueryFills are the fills that the queries of the declaration name when the query component is active, whose
	// import closure the qualifier checks (ST-SUP-RC33-OC03).
	QueryFills []QueryFill
	// WorkerFills are the fills that the workers of the declaration name, with WorkerContract, the import path of
	// the worker slot contract package when a component named worker is active, so that worker fills are held to
	// the fill rules as every fill is (ST-SUP-RC35-OC03).
	WorkerFills    []QueryFill
	WorkerContract string
	// Planned maps the absolute path of each generated Go file and go.mod that apply would write or change to its
	// planned content, so that stego test lists and tests the fill of a command, query or worker against the
	// contract that apply writes, also before that apply (go -overlay); nil when the fills are listed against the
	// project as it is.
	Planned map[string][]byte
	// CapabilityContract gives the generated Go files of the contract package of the command, query or worker
	// component, by their paths in the project, as apply writes them: the contract that the fill of each of its
	// commands, queries or workers imports, whose digest is the contract digest of that fill (ST-EXT-DS01). Nil when
	// no declaration is read.
	CapabilityContract func(component string) (map[string][]byte, error)
	// contractFailure gives the error of the plan that CapabilityContract makes, nil when that plan was not made or
	// was made, so that validation gives that error once at the top level and not inside each fill's fault.
	contractFailure func() error
	// ContractRuntime gives, by the import path of a command, query or worker slot contract, the packages of the
	// runtime library that its generator can write into that contract, which a fill of the slot may import
	// (ST-EXT-INV05). The generator gives them, so the fill check does not depend on a generated out/ folder.
	ContractRuntime map[string][]string
}

// PlannedGoFiles gives each generated Go file and go.mod of the plan whose content differs from the file in the
// project, or that the project does not hold, by its absolute path, with its planned content. A fill of a command,
// query or worker is tested against them (FillCheck.Planned).
func PlannedGoFiles(plan *Plan, projectDir, outDir string) (map[string][]byte, error) {
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return nil, err
	}
	planned := map[string][]byte{}
	for _, file := range plan.GeneratedFiles {
		name := projectFilePath(relative, file.Path)
		if !strings.HasSuffix(name, ".go") && filepath.ToSlash(name) != "go.mod" {
			continue
		}
		if err := gen.ValidatePath(filepath.ToSlash(name)); err != nil {
			return nil, err
		}
		data := file.Bytes()
		path := filepath.Join(projectDir, filepath.FromSlash(name))
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			continue
		}
		planned[path] = data
	}
	return planned, nil
}

// TargetBuildSettings are the Go settings of the platform that a generated service is built for: Linux on amd64 at
// the v1 level, with no cgo, no experiment and no Go environment file. The application build sets them, and the
// fill check lists a fill's packages with them, so both see the same files and imports, whatever the host's settings.
func TargetBuildSettings() map[string]string {
	return map[string]string{
		"GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1",
		"CGO_ENABLED": "0", "GOEXPERIMENT": "", "GOFIPS140": "off",
	}
}

// QueryFill is the one fill of one query or one worker: the fill, the query or worker and where the declaration
// names it, such as queries[0].fill or workers[0].fill.
type QueryFill struct {
	Fill  string
	Query string
	Path  string
}

// CommandFill is the one fill of one command: the fill, the command, where the declaration names it, such as
// commands[0].fill, and the import path of the command slot contract package that the fill may import.
type CommandFill struct {
	Fill     string
	Command  string
	Path     string
	Contract string
}

// FillQualifier gives a fault for each binding of a fill that does not fit
// that binding or is not qualified. The compiler owns this port and Extending's
// fill package implements it (ST-STD-CODE-R03).
type FillQualifier func(check FillCheck) ([]FillFault, error)

// FillReader reads the fill manifest (fill.yaml) at path and gives the
// collection the fill serves. The compiler owns this port and Extending's
// fill package implements it, because Generating and Declaring may not import
// Extending (ST-STD-CODE-R03).
type FillReader func(path string) (collection string, err error)

// FillSlotReader reads the fill manifest (fill.yaml) at path and gives the slot the fill implements, as
// component.slot. The compiler owns this port and Extending's fill package implements it (ST-STD-CODE-R03).
type FillSlotReader func(path string) (slot string, err error)

// checkBuiltFolder refuses, before plan, apply and deps, a folder out that holds a file that go build would put in
// the binary and that the generated Go files do not show, such as a .s or a .syso file, or a link: stego build
// refuses it (CheckGeneratedTree), so the runs before it refuse it too, and the author learns of it at the first one
// (walkthrough 8 and 9, rank 3). It checks out, the one folder that stego build checks. A project with no out
// folder, as a fresh clone, has nothing to refuse; any other error of reading out is a fault (sec-431 INFO-3). A
// linked out is refused here, as stego build refuses it.
func checkBuiltFolder(projectDir string) error {
	if _, err := os.Lstat(filepath.Join(projectDir, generatedFolder)); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("the generated folder %s cannot be read: %w", generatedFolder, err)
	}
	if err := CheckGeneratedTree(projectDir, generatedFolder); err != nil {
		return fmt.Errorf("%w; remove it, as plan, apply and deps refuse it too", err)
	}
	return nil
}

// ValidationFailure is the error of Reconcile when validation refuses the declaration: it holds the faults, so that
// the check of the fills, which makes a plan for the contract of the fills, gives each fault once at the top level.
type ValidationFailure struct {
	Result *ValidationResult
}

func (f *ValidationFailure) Error() string {
	return "service validation failed:\n" + FormatValidation(f.Result)
}

// Reconcile computes a plan by loading the service declaration, resolving the
// archetype, running all component generators, and assembling shared files.
// The plan can then be inspected (plan) or applied (Apply).
func Reconcile(input ReconcilerInput) (*Plan, error) {
	if input.WarnUnproved && !input.CheckOnly {
		return nil, errors.New("a plan that is written cannot take a fill with no evidence or no sign-off: run stego test, then stego fill sign")
	}
	if err := checkPendingProject(input.ProjectDir); err != nil {
		return nil, err
	}
	if !input.CheckOnly {
		if err := checkBuiltFolder(input.ProjectDir); err != nil {
			return nil, err
		}
	}
	source, err := loadCompilationSource(input)
	if err != nil {
		return nil, err
	}
	validation, err := validateSource(input, source)
	if err != nil {
		return nil, err
	}
	if validation.HasErrors() {
		return nil, &ValidationFailure{Result: validation}
	}
	if !input.CheckOnly {
		if err := authorizationGeneratorMissing(source.Service, source.Registry, input.Generators); err != nil {
			return nil, err
		}
		if err := selectedComponentMissing(source.Service, source.Registry); err != nil {
			return nil, err
		}
		if err := unbuiltSchemaChanges(source.Service); err != nil {
			return nil, err
		}
		// Checking accepts a declared invariant; generation refuses it until a generator emits it.
		if err := unemittedInvariants(source.Service.Invariants); err != nil {
			return nil, err
		}
	}
	serviceData, svcDecl, reg := source.ServiceData, source.Service, source.Registry
	existingState, inputSnapshots, moduleData, err := captureProjectInputs(input.ProjectDir, serviceData)
	if err != nil {
		return nil, err
	}
	resolved := source.Resolved
	components, componentNames := resolved.Components, resolved.Names
	outDir, outDirName, slotsPackage := resolved.OutDir, resolved.OutDirName, resolved.SlotsPackage
	for name, snapshot := range resolved.InputSnapshots {
		if before, exists := inputSnapshots[name]; exists && before != snapshot {
			return nil, fmt.Errorf("input %q changed during compilation", name)
		}
		inputSnapshots[name] = snapshot
	}

	// Every generated file may import the runtime library only at the
	// archetype's pin (ST-SUP-INV10); the registry refuses an archetype
	// without one.
	archetype := reg.Archetype(svcDecl.Archetype)
	if archetype == nil || archetype.Runtime == nil {
		return nil, fmt.Errorf("archetype %q pins no runtime library", svcDecl.Archetype)
	}
	pin := *archetype.Runtime
	if err := checkPinMove(existingState, archetype); err != nil {
		return nil, err
	}
	importsRuntime := false

	// Run all component generators in archetype-declared order.
	var allFiles []gen.File
	var wirings []ComponentWiring

	for _, compName := range componentNames {
		generator := input.Generators[compName]

		ctx := resolved.Contexts[compName]
		if resolved.RuntimeData && slices.Contains(authProviders([]string{compName}, components), compName) {
			// The verifier of the runtime library takes the place of the auth provider on the runtime data path,
			// with its declared settings (ST-SUP-RP13); its own package would be dead code.
			continue
		}

		files, wiring, err := generator.Generate(ctx)
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", compName, err)
		}
		if err := requireUnchangedGoModels(compName, generator, ctx, resolved); err != nil {
			return nil, err
		}
		if declared, ok := resolved.HTTPRoutes[compName]; ok {
			actual, err := gen.WiringHTTPRoutes(wiring)
			if err != nil {
				return nil, fmt.Errorf("generator %q HTTP routes: %w", compName, err)
			}
			if !slices.Equal(declared, actual) {
				return nil, fmt.Errorf("generator %q HTTP routes differ from its preflight declaration", compName)
			}
		}

		if err := validateGeneratorNamespace(compName, components, files); err != nil {
			return nil, err
		}
		uses, err := runtimeImports("component "+compName, files, pin)
		if err != nil {
			return nil, err
		}
		importsRuntime = importsRuntime || uses
		var wired []string
		if wiring != nil {
			wired = wiring.Imports
		}
		for _, path := range wired {
			uses, err := runtimeImport(path, pin)
			if err != nil {
				return nil, fmt.Errorf("component %s wires the import %s into main.go: %w", compName, path, err)
			}
			importsRuntime = importsRuntime || uses
		}

		allFiles = append(allFiles, files...)
		wirings = append(wirings, ComponentWiring{Name: compName, Wiring: wiring})
	}

	// On the runtime data path the compiler wires the runtime library into main.go (ST-SUP-RC01-OC41).
	if resolved.RuntimeData {
		files, wiring, err := runtimeWiring(runtimeWiringInput{ModuleName: input.ModuleName, OutDirName: outDirName, Service: svcDecl,
			ServiceData: serviceData, Components: components, Names: componentNames, Wirings: wirings, QueryStreams: resolved.QueryStreams,
			Generators: input.Generators, Contexts: resolved.Contexts})
		if err != nil {
			return nil, err
		}
		uses, err := runtimeImports("the runtime wiring", files, pin)
		if err != nil {
			return nil, err
		}
		importsRuntime = importsRuntime || uses
		allFiles = append(allFiles, files...)
		wirings = append(wirings, wiring)
	}

	// Generate slot interfaces and operators if slots are configured.
	slotFiles, err := generateSlotFiles(slotsPackage, components, svcDecl, reg)
	if err != nil {
		return nil, fmt.Errorf("generating slot files: %w", err)
	}
	allFiles = append(allFiles, slotFiles...)
	uses, err := runtimeImports("the slot generator", slotFiles, pin)
	if err != nil {
		return nil, err
	}
	importsRuntime = importsRuntime || uses
	var runtime *registry.RuntimePin
	if importsRuntime {
		runtime = &pin
	}

	// Validate that slot binding collections are in the collections list. Slot
	// operators are injected into handler constructors, which only exist for
	// collections. A slot binding referencing a non-existent collection would
	// produce an unused variable in the generated main.go — a Go compile error.
	if err := validateSlotCollectionsDefined(svcDecl.Slots, svcDecl.Collections); err != nil {
		return nil, err
	}

	// Assemble shared files (main.go, go.mod).
	assemblerInput := AssemblerInput{
		ModuleName:   input.ModuleName,
		ServiceName:  svcDecl.Name,
		GoVersion:    input.GoVersion,
		Wirings:      wirings,
		SlotBindings: svcDecl.Slots,
		SlotsPackage: slotsPackage,
		OutDirName:   outDirName,
		Runtime:      runtime,
	}
	sharedFiles, err := Assemble(assemblerInput)
	if err != nil {
		return nil, fmt.Errorf("assembling shared files: %w", err)
	}
	for i, file := range sharedFiles {
		if file.Path == "go.mod" {
			sharedFiles[i], err = mergeCapturedModule(moduleData, inputSnapshots["go.mod"].Exists, file)
			if err != nil {
				return nil, err
			}
		}
	}
	if runtime != nil {
		sum, err := mergeProjectSums(input.ProjectDir, inputSnapshots["go.sum"], *runtime)
		if err != nil {
			return nil, err
		}
		sharedFiles = append(sharedFiles, sum)
	}
	allFiles = append(allFiles, sharedFiles...)

	// The platform requirements and the architecture model are the files of the run out of the generated output; no
	// generator may take their paths (ST-DLV-UC01-R05, ST-GEN-UC08).
	for _, file := range allFiles {
		if isProjectGeneratedFile(file.Path) {
			return nil, fmt.Errorf("the generated file %s takes a path that apply keeps for its own project files", file.Path)
		}
	}
	var requirements []byte
	if input.WriteRequirements != nil {
		files, err := requirementsFiles(input.WriteRequirements, svcDecl, componentNames, components, input.Generators, resolved.Contexts, runtimePackages(allFiles, pin))
		if err != nil {
			return nil, err
		}
		requirements = files[0].Content
		allFiles = append(allFiles, files...)
	}
	var notes []string
	if input.WriteDeployment != nil && requirements != nil {
		var pipelines map[string]registry.PipelinePins
		if component := reg.Component(registry.PipelineComponent); component != nil {
			pipelines = component.Pipelines
		}
		var moduleFile []byte
		for _, file := range allFiles {
			if file.Path == "go.mod" {
				moduleFile = file.Content
			}
		}
		modules, err := localModuleReplacements(moduleFile)
		if err != nil {
			return nil, fmt.Errorf("reading the local module replacements of go.mod: %w", err)
		}
		// A binary of the runtime data path selects its modes, so the base runs them as the declared modes need
		// (ST-DLV-UC07-R07); another binary has no modes.
		var modes []string
		if resolved.RuntimeData {
			modes = declaredModes(svcDecl.Modes)
		}
		files, written, err := deploymentFiles(input.WriteDeployment, svcDecl, componentNames, input.Generators, resolved.Contexts, requirements, pipelines, outDirName, resolved.HoldsFills, modules, schemaDigest(allFiles, runtime), modes)
		if err != nil {
			return nil, err
		}
		allFiles = append(allFiles, files...)
		notes = written
	}
	if note := releaseIdentityNote(svcDecl); note != "" {
		notes = append(notes, note)
	}
	// The architecture model comes from the declaration, the registry parts and the wiring of this run alone, and
	// a component or slot that does not state its place stops the run before any file is written (ST-GEN-UC08-R05).
	architecture, err := architectureFiles(input, svcDecl, reg, archetype, componentNames, components, wirings, requirements)
	if err != nil {
		return nil, err
	}
	allFiles = append(allFiles, architecture...)

	// Validate no duplicate file paths across all sources (generators, slots,
	// assembler). A duplicate means one generator's output would silently
	// overwrite another's.
	if err := validateUniqueFilePaths(allFiles); err != nil {
		return nil, err
	}

	manifest, err := newInputManifest(input, outDirName, resolved.HoldsFills, inputSnapshots)
	if err != nil {
		return nil, err
	}
	// Compute plan by comparing generated files against existing state.
	pins, err := componentPins(components, reg)
	if err != nil {
		return nil, err
	}
	plan, err := computePlan(allFiles, existingState, serviceData, svcDecl.Name, svcDecl.Entities, pins, outDir, input.ProjectDir, input.RegistrySHA, reg.ContentHash(), manifest)
	if err != nil {
		return nil, err
	}
	if err := bindPlanInputs(plan, input.ProjectDir, inputSnapshots); err != nil {
		return nil, err
	}
	plan.inputManifestSHA = manifest.SHA256
	plan.registry = reg
	plan.Notes = notes
	if err := plan.verifyRegistry(); err != nil {
		return nil, err
	}
	plan.requirements, err = moduleRequirements(wirings, runtime)
	if err != nil {
		return nil, err
	}
	plan.runtime = runtime
	plan.NewState.LastApplied.Archetype = &ArchetypeState{Name: archetype.Name, Version: archetype.Version, Runtime: &pin, ImportsRuntime: importsRuntime}
	if plan.StateChanged, err = stateDiffers(existingState, plan.NewState); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply writes all generated files to disk under outDir (or projectDir for
// project-root files like go.mod), removes orphaned files, and saves the
// new state.
func Apply(plan *Plan, projectDir, outDir string) error {
	return applyWithFault(plan, projectDir, outDir, stopHook)
}

func applyWithFault(plan *Plan, projectDir, outDir string, fault applyFault) error {
	if plan == nil || plan.NewState == nil {
		return fmt.Errorf("apply requires a complete plan")
	}
	if outDir == "" {
		outDir = filepath.Join(projectDir, "out")
	}
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return err
	}
	if err := validateStatePaths(plan.NewState); err != nil {
		return err
	}
	// Check every path before the first write or deletion, including old state
	// entries that no longer occur in the generated file list.
	for _, file := range plan.GeneratedFiles {
		if err := gen.ValidatePath(file.Path); err != nil {
			return err
		}
	}
	for _, file := range plan.Files {
		if err := gen.ValidatePath(file.Path); err != nil {
			return err
		}
	}
	project, err := os.OpenRoot(projectDir)
	if err != nil {
		return fmt.Errorf("opening project root: %w", err)
	}
	defer project.Close()
	if err := pendingTransaction(project); err != nil {
		return err
	}
	if err := checkFilePath(project, relative); err != nil {
		return err
	}
	statePath := filepath.Join(".stego", "state.yaml")
	if err := checkFileTarget(project, statePath); err != nil {
		return err
	}
	for _, file := range plan.GeneratedFiles {
		if err := checkFileTarget(project, projectFilePath(relative, file.Path)); err != nil {
			return err
		}
	}
	for _, file := range plan.Files {
		if err := checkFileTarget(project, projectFilePath(relative, file.Path)); err != nil {
			return err
		}
	}
	if err := verifyPlanLocation(plan, projectDir, outDir); err != nil {
		return err
	}
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	if err := refuseHandEdits(plan); err != nil {
		return err
	}
	lock, err := lockProject(project)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pendingTransaction(project); err != nil {
		return err
	}
	// Another apply can finish between the first snapshot check and the lock.
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	if err := plan.verifyRegistry(); err != nil {
		return err
	}
	if plan.saved != nil {
		if err := plan.saved.StillApplies(plan); err != nil {
			return err
		}
	}
	if err := commitTransaction(project, projectDir, plan, relative, fault); err != nil {
		return err
	}
	return removeSavedPlan(project)
}

// isProjectRootFile returns true for files that should be placed at the project
// root. The application owns its module and checksum files.
func isProjectRootFile(filePath string) bool {
	return filePath == "go.mod" || filePath == "go.sum"
}

// RequirementsFile is the path, in the project folder, of the platform requirements that apply writes: the path of
// delivery.RequirementsFile (ST-DLV-EN01-TB), which Generating may not import.
const RequirementsFile = "deploy/platform-requirements.yaml"

// isProjectGeneratedFile tells if a generated file is placed at its path in the project folder, out of the
// generated output, and owned by STEGO whole: the record keeps its digest under project_files, so that apply refuses
// a hand edit of it and drift names one (ST-DLV-UC01-R05).
func isProjectGeneratedFile(filePath string) bool {
	return filePath == RequirementsFile || filePath == ReferenceFile || filePath == ArchitectureFile || filePath == ArchitectureViewFile ||
		slices.Contains(BaseFiles, filePath) || filePath == DeliveryObjectFile || filePath == ContainerFile || isPipelineFile(filePath)
}

// ContainerFile is the path, in the project folder, of the offline Containerfile that apply writes with the
// Kubernetes base: the path of delivery.ContainerFile (ST-DLV-INV07), which Generating may not import.
const ContainerFile = "Containerfile"

// isPipelineFile tells if a path is the CI file of a provider: .github/workflows/stego.yml, or
// .tekton/<service>-push.yaml (ST-DLV-EN03-TB, the paths of delivery.PipelineFile).
func isPipelineFile(filePath string) bool {
	name, tekton := strings.CutPrefix(filePath, ".tekton/")
	return filePath == ".github/workflows/stego.yml" || (tekton && strings.HasSuffix(name, "-push.yaml") && !strings.Contains(name, "/"))
}

// DeliveryObjectFile is the path, in the project folder, of the Argo CD ApplicationSet: the path of
// delivery.ApplicationSetFile (ST-DLV-EN05-TB).
const DeliveryObjectFile = "deploy/argocd/applicationset.yaml"

// BaseFiles are the paths, in the project folder, of every file that the Kubernetes base may hold, with the opt-in
// Components secret-files (ST-DLV-Q18) and fs-group: the paths of delivery.BaseFiles (ST-DLV-EN04-TB), which Generating may not
// import. A run writes the files that the platform
// requirements need, and apply removes a file of the base that a run no longer writes.
var BaseFiles = []string{"deploy/base/kustomization.yaml", "deploy/base/service-account.yaml", "deploy/base/deployment.yaml",
	"deploy/base/worker-service-account.yaml", "deploy/base/worker-deployment.yaml", "deploy/base/service.yaml", "deploy/base/role.yaml", "deploy/base/role-binding.yaml", "deploy/base/migration-job.yaml",
	"deploy/components/secret-files/kustomization.yaml", "deploy/components/secret-files/remove-env.yaml",
	"deploy/components/secret-files/mount-files.yaml", "deploy/components/secret-files/remove-worker-env.yaml",
	"deploy/components/secret-files/mount-worker-files.yaml", "deploy/components/secret-files/remove-job-env.yaml",
	"deploy/components/secret-files/mount-job-files.yaml", "deploy/components/fs-group/kustomization.yaml",
	"deploy/components/fs-group/deployment-fs-group.yaml", "deploy/components/fs-group/worker-deployment-fs-group.yaml",
	"deploy/components/fs-group/job-fs-group.yaml"}

// DeploymentWriter writes the Kubernetes base of the service from the platform requirements of the run and the
// probes of the active set, and then the delivery object or the plan line that says why the run writes none
// (ST-DLV-CMD03, ST-DLV-PO04). The compiler owns this port and Delivering's delivery package implements it,
// because Generating may not import Delivering (ST-STD-CODE-R03).
type DeploymentWriter func(DeploymentInput) (DeploymentFiles, error)

// DeploymentInput is what the Kubernetes base is written from: the service name, the platform requirements file
// of the run, and the probes of the component of the active set that serves them, if one does.
type DeploymentInput struct {
	Service      string
	Requirements []byte
	Probes       *gen.Probes
	// Delivery is the delivery stanza of the declaration, or nil (ST-DLV-Q12 (A)).
	Delivery *types.Delivery
	// Pipelines are the pins of each CI provider that the registry's ci-pipeline component states, or nil
	// (ST-DLV-INV04).
	Pipelines map[string]registry.PipelinePins
	// OutputDir is the folder, in the project, of the generated output, which is the entry point of the build, and
	// HoldsFills tells whether the fills folder holds a fill, which the generated code then imports: the offline
	// Containerfile copies both (ST-DLV-INV07).
	OutputDir  string
	HoldsFills bool
	// LocalModules are the paths, as go.mod gives them, that go.mod replaces a module with (a replace directive
	// with no version), which the offline Containerfile copies when they are folders of the project
	// (localModuleReplacements).
	LocalModules []string
	// Release is the digest of the schema of the release, in hex (schemaDigest), which names the migration Job of
	// the base (ST-DLV-UC07-R06).
	Release string
	// Modes are the declared modes of a service whose binary selects its modes, all three when the declaration
	// states none, or nil for a binary with no mode selection. With the api mode and a background mode, the base
	// runs them in two Deployments (ST-DLV-UC07-R07).
	Modes []string
}

// DeploymentFiles are the files of the Kubernetes base and of the delivery object, and the lines of the plan.
type DeploymentFiles struct {
	Files []gen.File
	Notes []string
}

// deploymentFiles writes the Kubernetes base of the run from its platform requirements and the probes that a
// component of the active set serves, in the order of the active set (ST-DLV-INV08). It refuses a file out of the
// paths of the base.
func deploymentFiles(write DeploymentWriter, svcDecl *types.ServiceDeclaration, names []string, generators map[string]gen.Generator,
	contexts map[string]gen.Context, requirements []byte, pipelines map[string]registry.PipelinePins, outDirName string, holdsFills bool, modules []string, release string, modes []string) ([]gen.File, []string, error) {
	var probes *gen.Probes
	for _, name := range names {
		declarer, ok := generators[name].(gen.ProbeDeclarer)
		if !ok {
			continue
		}
		served, err := declarer.Probes(contexts[name])
		if err != nil {
			return nil, nil, fmt.Errorf("reading the probes that %s serves: %w", name, err)
		}
		if probes != nil {
			return nil, nil, fmt.Errorf("two components of the active set serve the probes of the Kubernetes base; %s is the second", name)
		}
		probes = &served
	}
	written, err := write(DeploymentInput{Service: svcDecl.Name, Requirements: requirements, Probes: probes, Delivery: svcDecl.Delivery, Pipelines: pipelines,
		OutputDir: filepath.ToSlash(outDirName), HoldsFills: holdsFills, LocalModules: modules, Release: release, Modes: modes})
	if err != nil {
		return nil, nil, fmt.Errorf("writing the Kubernetes base: %w", err)
	}
	for _, file := range written.Files {
		if !slices.Contains(BaseFiles, file.Path) && file.Path != DeliveryObjectFile && file.Path != ContainerFile && !isPipelineFile(file.Path) {
			return nil, nil, fmt.Errorf("the Kubernetes base writes %s, which is not a file of the base or the delivery object", file.Path)
		}
	}
	return written.Files, written.Notes, nil
}

// schemaDigest is the digest of the schema version of a run, which names the migration Job of the base
// (ST-DLV-UC07-R06): the SHA-256, in hex, of the path and the content of each schema file of the run in path order
// (isSchemaFile), and of the runtime pin, whose module gives the migration texts of the outbox, the audit record and
// the authorization tables. A release that changes the schema gets a Job of a new name. The module files and the
// other generated code are not in it, so stego deps, which writes go.sum, and a fix that changes no schema keep the
// name, and the same inputs give the same digest.
func schemaDigest(files []gen.File, runtime *registry.RuntimePin) string {
	var schema []gen.File
	for _, file := range files {
		if isSchemaFile(file.Path) {
			schema = append(schema, file)
		}
	}
	slices.SortFunc(schema, func(a, b gen.File) int { return strings.Compare(a.Path, b.Path) })
	sum := sha256.New()
	if runtime != nil {
		fmt.Fprintf(sum, "runtime %s %s %s\n", runtime.Module, runtime.Version, runtime.Sum)
	}
	for _, file := range schema {
		fmt.Fprintf(sum, "%d:%s\n%d:", len(file.Path), file.Path, len(file.Content))
		sum.Write(file.Content)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// isSchemaFile tells whether a generated file states the schema: a migration of a folder named migrations, the
// schema manifest, which also names the last migration of the release, or the session table SQL or a file of the
// schema package of a browser-service (browser/schema.sql, browser/schema), whose mode migrate makes and checks it. Without the last, the
// migration Job of a browser-service took the digest of no file in every release (walkthrough 7, B6).
func isSchemaFile(name string) bool {
	return slices.Contains(strings.Split(path.Dir(name), "/"), "migrations") || path.Base(name) == "schema_manifest.go" ||
		name == "browser/schema.sql" || path.Dir(name) == "browser/schema"
}

// ReferenceFile is the path, in the project folder, of the environment variable reference that apply writes: the
// path of delivery.ReferenceFile (ST-DLV-UC01-R07).
const ReferenceFile = "deploy/environment-variables.md"

// architectureFiles derives the architecture model of the run and renders its view (ST-GEN-CMD05): from the declaration, the registry
// parts of the active set and the bound mixins, the wiring, the platform requirements of the run and the declared
// authorization schema.
func architectureFiles(input ReconcilerInput, svcDecl *types.ServiceDeclaration, reg *registry.Registry, archetype *registry.Archetype,
	names []string, components map[string]*registry.Component, wirings []ComponentWiring, requirements []byte) ([]gen.File, error) {
	mixinNames, err := boundMixins(reg, svcDecl.Mixins)
	if err != nil {
		return nil, err
	}
	mixins := make([]*registry.Mixin, 0, len(mixinNames))
	for _, name := range mixinNames {
		mixins = append(mixins, reg.Mixin(name))
	}
	schema, err := declaration.ReadAuthzSchema(svcDecl, os.DirFS(input.ProjectDir))
	if err != nil {
		return nil, fmt.Errorf("reading the authorization schema for the architecture model: %w", err)
	}
	return WriteArchitecture(ArchitectureInput{Service: svcDecl, Archetype: archetype, Components: components, Names: names, Mixins: mixins,
		Wirings: wirings, Requirements: requirements, Schema: schema})
}

// RequirementsWriter writes the platform requirements of the service from the needs and the environment variables
// that its components state, with the environment variable reference rendered from the same list (ST-DLV-CMD01).
// The compiler owns this port and Delivering's delivery package implements it, because Generating may not import
// Delivering (ST-STD-CODE-R03).
type RequirementsWriter func(service string, components []ComponentNeeds) (RequirementsFiles, error)

// RequirementsFiles are the bytes of the platform requirements and of the environment variable reference.
type RequirementsFiles struct {
	Requirements []byte
	Reference    []byte
}

// ComponentNeeds is one component of the active set with the needs and the environment variables it states, the
// variables that the declaration gives it, and its settings as the declaration and the registry give them. A
// setting marked secret is left out, so that no output can carry its value (ST-SUP-INV23).
type ComponentNeeds struct {
	Component string
	Needs     []registry.Need
	Variables []registry.EnvironmentVariable
	Declared  []gen.DeclaredVariables
	Settings  map[string]any
}

// requirementsFiles writes the platform requirements and the environment variable reference of the active set, in
// the order of the active set (ST-DLV-DS01). The derivation reads the declaration and the registry alone, never the
// generated code or a fill. A component whose generator gives variables from its declaration entries, such as
// runtime-configuration, gives them through gen.VariableDeclarer.
func requirementsFiles(write RequirementsWriter, svcDecl *types.ServiceDeclaration, names []string, components map[string]*registry.Component,
	generators map[string]gen.Generator, contexts map[string]gen.Context, imported map[string]bool) ([]gen.File, error) {
	needs := make([]ComponentNeeds, 0, len(names))
	for _, name := range names {
		component := components[name]
		settings := resolveComponentConfig(component, svcDecl)
		for key, field := range component.Config {
			if field.Secret {
				delete(settings, key)
			}
		}
		if err := checkSettingDefaults(component, settings); err != nil {
			return nil, err
		}
		variables := readVariables(declaredModeVariables(component.Variables(), svcDecl.Modes), imported)
		componentNeeds := readNeeds(component.Needs, component.Variables(), variables)
		variables, componentNeeds = perSubscription(variables, componentNeeds, svcDecl.Subscriptions)
		stated := ComponentNeeds{Component: name, Needs: componentNeeds, Variables: variables, Settings: settings}
		if declarer, ok := generators[name].(gen.VariableDeclarer); ok {
			declared, err := declarer.DeclaredVariables(contexts[name])
			if err != nil {
				return nil, fmt.Errorf("reading the environment variables that the declaration gives %s: %w", name, err)
			}
			stated.Declared = declared
		}
		needs = append(needs, stated)
	}
	data, err := write(svcDecl.Name, needs)
	if err != nil {
		return nil, fmt.Errorf("writing the platform requirements %s: %w", RequirementsFile, err)
	}
	return []gen.File{{Path: RequirementsFile, Content: data.Requirements}, {Path: ReferenceFile, Content: data.Reference}}, nil
}

// perSubscription gives the variables and the needs of a component with each one whose name holds
// registry.SubscriptionPlaceholder listed once for each declared subscription, in the order of the declaration, and
// the others unchanged (ST-SUP-EN14 field Name, decision on ST-SUP-Q204): in a variable name, the variable of a need
// and the secret source of a variable, the subscription name in capital letters with '_' in place of '-', such as
// STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC; in a group of one of and a need name, the name as the declaration gives
// it. A service with no subscription lists no such variable or need.
func perSubscription(variables []registry.EnvironmentVariable, needs []registry.Need, subscriptions []types.Subscription) ([]registry.EnvironmentVariable, []registry.Need) {
	const placeholder = registry.SubscriptionPlaceholder
	upper := func(name string) string { return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) }
	var outVariables []registry.EnvironmentVariable
	for _, variable := range variables {
		if !strings.Contains(variable.Name, placeholder) {
			outVariables = append(outVariables, variable)
			continue
		}
		for _, subscription := range subscriptions {
			v := variable
			v.Name = strings.ReplaceAll(variable.Name, placeholder, upper(subscription.Name))
			v.OneOf = strings.ReplaceAll(variable.OneOf, placeholder, subscription.Name)
			if variable.SecretSource != nil {
				source := *variable.SecretSource
				source.Name = strings.ReplaceAll(source.Name, placeholder, subscription.Name)
				v.SecretSource = &source
			}
			outVariables = append(outVariables, v)
		}
	}
	var outNeeds []registry.Need
	for _, need := range needs {
		if !strings.Contains(need.Name, placeholder) {
			outNeeds = append(outNeeds, need)
			continue
		}
		for _, subscription := range subscriptions {
			n := need
			n.Name = strings.ReplaceAll(need.Name, placeholder, subscription.Name)
			n.Variable = strings.ReplaceAll(need.Variable, placeholder, upper(subscription.Name))
			outNeeds = append(outNeeds, n)
		}
	}
	return outVariables, outNeeds
}

// readVariables gives the variables that the generated service reads: each one whose Read with names no package, or
// a package of the runtime library that the generated code imports (ST-SUP-Q161, ST-DLV-UC01-R06). A browser-service
// that imports no package of the runtime is not asked for the connection strings that only the runtime reads. A
// variable that only a client program of the component reads, such as APPLY_CONFIG of the generated CLI, is never
// one that the service reads, so no server pod gets it (Read by client; walkthrough 17, W17-D2).
func readVariables(variables []registry.EnvironmentVariable, imported map[string]bool) []registry.EnvironmentVariable {
	return slices.DeleteFunc(slices.Clone(variables), func(variable registry.EnvironmentVariable) bool {
		return variable.ReadByClient || (variable.ReadWith != "" && !imported[variable.ReadWith])
	})
}

// readNeeds gives the needs of a component that the generated service has: each need, but a secret need that only
// variables that the service does not read name as their secret source, such as the key ring of the secret fields of a
// service with none (ST-SUP-RC32-OC21).
func readNeeds(needs []registry.Need, all, read []registry.EnvironmentVariable) []registry.Need {
	named := func(list []registry.EnvironmentVariable, need string) bool {
		return slices.ContainsFunc(list, func(v registry.EnvironmentVariable) bool {
			return v.SecretSource != nil && v.SecretSource.Name == need
		})
	}
	return slices.DeleteFunc(slices.Clone(needs), func(n registry.Need) bool {
		return named(all, n.Name) && !named(read, n.Name)
	})
}

// checkSettingDefaults holds each default of a variable that is {settings.<key>} to the variable's type and allowed
// values, with the value that the declaration gives the setting (ST-SUP-INV24). The message never holds the value.
func checkSettingDefaults(component *registry.Component, settings map[string]any) error {
	for _, variable := range component.Variables() {
		if variable.Default == nil {
			continue
		}
		key, ok := strings.CutPrefix(*variable.Default, "{settings.")
		if !ok || !strings.HasSuffix(key, "}") {
			continue
		}
		key = strings.TrimSuffix(key, "}")
		value, given := settings[key]
		if !given {
			continue
		}
		var text string
		switch value := value.(type) {
		case string:
			text = value
		case int, int64, uint64, float64, bool:
			text = fmt.Sprint(value)
		default:
			return fmt.Errorf("component %s: environment variable %s: default: the setting %s is not one value", component.Name, variable.Name, key)
		}
		if text == "" {
			continue
		}
		if err := registry.CheckVariableValue(registry.VariableType(variable.Type), variable.Allowed, text); err != nil {
			return fmt.Errorf("component %s: environment variable %s: default: the value of the setting %s: %w", component.Name, variable.Name, key, err)
		}
	}
	return nil
}

// collectComponentNames assembles the ordered list of component names from the
// archetype, default_auth, and any mixin-added components.
func collectComponentNames(archetype *registry.Archetype, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]string, error) {
	// Collect the baseline component set from the archetype, default_auth,
	// and mixins. Override-component replacement is handled by ports.Resolve().
	seen := make(map[string]bool)
	var names []string

	for _, name := range archetype.Components {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	if archetype.DefaultAuth != "" {
		if !seen[archetype.DefaultAuth] {
			seen[archetype.DefaultAuth] = true
			names = append(names, archetype.DefaultAuth)
		}
	}

	// Validate declared mixins against archetype's compatible_mixins constraint.
	// Distinguish nil (no field declared — any mixin accepted) from empty
	// (compatible_mixins: [] — no mixins are compatible).
	// An alias is expanded first, so each mixin it names must be compatible with the archetype too.
	mixins, err := boundMixins(reg, svcDecl.Mixins)
	if err != nil {
		return nil, err
	}
	if archetype.CompatibleMixins != nil && len(mixins) > 0 {
		compatible := make(map[string]bool, len(archetype.CompatibleMixins))
		for _, m := range archetype.CompatibleMixins {
			compatible[m] = true
		}
		for _, mixinName := range mixins {
			if !compatible[mixinName] {
				return nil, fmt.Errorf("mixin %q is not compatible with archetype %q (compatible mixins: %v)", mixinName, archetype.Name, archetype.CompatibleMixins)
			}
		}
	}

	for _, mixinName := range mixins {
		mixin := reg.Mixin(mixinName)
		if mixin == nil {
			return nil, fmt.Errorf("mixin %q not found in registry", mixinName)
		}
		if err := reg.CheckExtension(archetype, mixin); err != nil {
			return nil, err
		}
		for _, compName := range mixin.AddsComponents {
			if !seen[compName] {
				seen[compName] = true
				names = append(names, compName)
			}
		}
	}

	// The infrastructure components that the declared settings select, which the registry holds
	// (ST-GEN-INV37). A selected component is not part of the archetype or a mixin.
	for _, s := range selectComponents(svcDecl) {
		if !seen[s.Component] && reg.Component(s.Component) != nil {
			seen[s.Component] = true
			names = append(names, s.Component)
		}
	}

	return names, nil
}

// resolveComponentConfig merges a component's default config values with any
// service-level overrides for that component.
func resolveComponentConfig(comp *registry.Component, svcDecl *types.ServiceDeclaration) map[string]any {
	config := make(map[string]any)

	for key, field := range comp.Config {
		if field.Default != nil {
			config[key] = field.Default
		}
	}

	if overrides, ok := svcDecl.Overrides[comp.Name]; ok {
		if m, ok := overrides.(map[string]any); ok {
			for k, v := range m {
				config[k] = v
			}
		}
	}

	return config
}

// generateSlotFiles generates Go interface and operator files for all slots
// used by the service declaration.
func generateSlotFiles(slotsPackage string, components map[string]*registry.Component, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]gen.File, error) {
	if slotsPackage == "" {
		return nil, nil
	}

	pkgName := filepath.Base(slotsPackage)

	// The package holds the contract of every slot of the active component
	// set and of the declaration's mixins, bound or not, so that a fill of any
	// of them compiles before it is bound (ST-EXT-INV05).
	slotNames := make(map[string]bool)
	for _, sb := range svcDecl.Slots {
		slotNames[sb.Slot] = true
	}
	for _, comp := range components {
		for _, sd := range comp.Slots {
			slotNames[sd.Name] = true
		}
	}
	mixins, _ := boundMixins(reg, svcDecl.Mixins)
	for _, mixinName := range mixins {
		if mixin := reg.Mixin(mixinName); mixin != nil {
			for _, sd := range mixin.AddsSlots {
				slotNames[sd.Name] = true
			}
		}
	}

	// Find the slot definitions and their owning components or mixins.
	type slotInfo struct {
		definition registry.SlotDefinition
		// protoDir is the directory containing the slots/ subdirectory for this
		// slot's proto file, relative to the captured registry root.
		protoDir string
	}
	// One slot name has one definition: a second definition by another component, or by a mixin, is refused, so the
	// contract that a fill is generated against never depends on the order of a map (sec-500 M1).
	slotDefs := make(map[string]slotInfo)
	for _, compName := range sortedKeys(components) {
		comp := components[compName]
		for _, sd := range comp.Slots {
			if slotNames[sd.Name] {
				if first, exists := slotDefs[sd.Name]; exists {
					return nil, fmt.Errorf("slot %q is defined twice, in %s and in %s; one slot name has one contract", sd.Name, first.protoDir, filepath.Join("components", comp.Name))
				}
				slotDefs[sd.Name] = slotInfo{
					definition: sd,
					protoDir:   filepath.Join("components", comp.Name),
				}
			}
		}
	}
	// Also search mixin adds_slots for slot definitions not found in components.
	for _, mixinName := range mixins {
		mixin := reg.Mixin(mixinName)
		if mixin == nil {
			continue
		}
		for _, sd := range mixin.AddsSlots {
			if slotNames[sd.Name] {
				if first, exists := slotDefs[sd.Name]; exists {
					return nil, fmt.Errorf("slot %q is defined twice, in %s and in %s; one slot name has one contract", sd.Name, first.protoDir, filepath.Join("mixins", mixin.Name))
				}
				slotDefs[sd.Name] = slotInfo{
					definition: sd,
					protoDir:   filepath.Join("mixins", mixin.Name),
				}
			}
		}
	}

	// Sort slot names for deterministic output.
	sortedSlots := make([]string, 0, len(slotNames))
	for name := range slotNames {
		sortedSlots = append(sortedSlots, name)
	}
	sort.Strings(sortedSlots)

	var files []gen.File

	// Track types emitted across all slot files to avoid duplicate
	// declarations when multiple slots share imported types (e.g. SlotResult).
	emittedTypes := make(map[string]bool)

	for _, slotName := range sortedSlots {
		info, ok := slotDefs[slotName]
		if !ok {
			return nil, fmt.Errorf("slot %q not defined by any component or mixin", slotName)
		}

		protoPath := filepath.Join(info.protoDir, "slots", slotName+".proto")

		protoData, err := reg.ReadFile(filepath.ToSlash(protoPath))
		if err != nil {
			return nil, fmt.Errorf("opening proto for slot %q: %w", slotName, err)
		}
		parsed, err := slot.ParseProto(bytes.NewReader(protoData))
		if err != nil {
			return nil, fmt.Errorf("parsing proto for slot %q: %w", slotName, err)
		}

		var imports []*slot.ProtoFile
		for _, name := range parsed.Imports {
			data, err := reg.ReadProtoImport(name)
			if err != nil {
				return nil, fmt.Errorf("slot %q import %q: %w", slotName, name, err)
			}
			parsedImport, err := slot.ParseProto(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("slot %q import %q: %w", slotName, name, err)
			}
			imports = append(imports, parsedImport)
		}

		// Generate interface file, excluding types already emitted by a
		// previous slot file in the same package.
		ifaceFile, err := slot.GenerateInterfaceExcluding(
			filepath.Join(slotsPackage, slotName+".go"),
			pkgName,
			parsed,
			imports,
			emittedTypes,
		)
		if err != nil {
			return nil, fmt.Errorf("generating interface for slot %q: %w", slotName, err)
		}
		files = append(files, ifaceFile)

		// Record all types that were referenced (and thus emitted) by this
		// slot file so subsequent files skip them.
		allMessages := make(map[string]slot.Message)
		for _, imp := range imports {
			for _, msg := range imp.Messages {
				allMessages[imp.Package+"."+msg.Name] = msg
				allMessages[msg.Name] = msg
			}
		}
		for _, msg := range parsed.Messages {
			allMessages[parsed.Package+"."+msg.Name] = msg
			allMessages[msg.Name] = msg
		}
		for typeName := range slot.CollectAllReferencedTypes(parsed, allMessages) {
			emittedTypes[typeName] = true
		}

		// Generate operators file.
		opsFile, err := slot.GenerateOperators(
			filepath.Join(slotsPackage, slotName+"_operators.go"),
			pkgName,
			parsed,
		)
		if err != nil {
			return nil, fmt.Errorf("generating operators for slot %q: %w", slotName, err)
		}
		files = append(files, opsFile)
	}

	return files, nil
}

// refuseHandEdits holds ST-GEN-UC03-R03: apply names every hand-edited
// generated file and writes nothing, unless the author lets it replace them.
func refuseHandEdits(plan *Plan) error {
	if len(plan.HandEdited) == 0 || plan.ReplaceHandEdits {
		return nil
	}
	return fmt.Errorf("apply would replace %d hand-edited generated file(s), and writes nothing: %s; restore them, or run stego apply --replace-hand-edits to replace them", len(plan.HandEdited), strings.Join(plan.HandEdited, ", "))
}

// componentPins gives each component of the active set with its version and
// its component digest, from the registry inputs that the run captured.
func componentPins(components map[string]*registry.Component, reg *registry.Registry) (map[string]ComponentState, error) {
	files := reg.InputFiles()
	pins := make(map[string]ComponentState, len(components))
	for name, component := range components {
		digest, err := ComponentDigest(files, name)
		if err != nil {
			return nil, err
		}
		pins[name] = ComponentState{Version: component.Version, SHA: digest}
	}
	return pins, nil
}

// validateGeneratorNamespace holds ST-GEN-INV07: a component that generates
// files declares an output namespace, and every file is inside it. A file
// outside it is refused by the rule of ST-SUP-INV02, which names each file and
// the component of the active set whose namespace it is in.
func validateGeneratorNamespace(name string, components map[string]*registry.Component, files []gen.File) error {
	component := components[name]
	if component == nil {
		return fmt.Errorf("generator %q has no component in the active set", name)
	}
	if len(files) == 0 {
		return nil
	}
	if component.OutputNamespace == "" {
		return fmt.Errorf("generator %q produced %d file(s) but component declares no output_namespace — "+
			"all components that generate files must declare an output_namespace", name, len(files))
	}
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	if err := registry.WritesOwnNamespace(name, components, paths); err != nil {
		return fmt.Errorf("generator %q: %w", name, err)
	}
	// A component that emits a Go library at the top of its namespace is imported by the namespace, so the last
	// element of the namespace is a usable package name: no keyword, no blank and no main (ST-SUP-INV13, sec-500
	// L2). Each library generator checks it in its preflight too; this holds a generator that does not.
	for _, file := range files {
		if strings.HasSuffix(file.Path, ".go") && !strings.HasSuffix(file.Path, "_test.go") && path.Dir(file.Path) == component.OutputNamespace {
			if err := gen.ValidateGoLibraryName(path.Base(component.OutputNamespace)); err != nil {
				return fmt.Errorf("generator %q emits the Go library %s: %w", name, file.Path, err)
			}
			break
		}
	}
	return nil
}

// computePlan compares generated files against existing state to determine
// which files need to be written and what entity changes occurred.
func computePlan(
	generatedFiles []gen.File,
	existingState *State,
	serviceData []byte,
	serviceName string,
	entities []types.Entity,
	pins map[string]ComponentState,
	outDir string,
	projectDir string,
	registrySHA string,
	registryContentHash string,
	manifest *InputManifest,
) (*Plan, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return nil, err
	}
	snapshots := make(map[string]fileSnapshot)
	serviceHash := HashBytes(serviceData)

	existingHashes := make(map[string]string)
	if existingState.LastApplied != nil && existingState.LastApplied.Files != nil {
		existingHashes = existingState.LastApplied.Files
	}
	existingProject := make(map[string]string)
	if existingState.LastApplied != nil && existingState.LastApplied.ProjectFiles != nil {
		existingProject = existingState.LastApplied.ProjectFiles
	}

	var planned []PlannedFile
	newFileHashes := make(map[string]string)
	var newProjectHashes map[string]string
	var handEdited, restored []string

	for _, f := range generatedFiles {
		content := f.Bytes()
		if len(content) > maxTrackedFileBytes {
			return nil, fmt.Errorf("generated file %s exceeds the %d-byte tracking limit", f.Path, maxTrackedFileBytes)
		}
		hash := HashBytes(content)
		// The application owns go.mod. Go tools can update it after apply.
		if isProjectGeneratedFile(f.Path) {
			if newProjectHashes == nil {
				newProjectHashes = make(map[string]string)
			}
			newProjectHashes[f.Path] = hash
		} else if !isProjectRootFile(f.Path) {
			newFileHashes[f.Path] = hash
		}

		name := projectFilePath(relative, f.Path)
		_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
		if err != nil {
			return nil, err
		}
		snapshots[name] = snapshot
		if _, tracked := existingProject[f.Path]; isProjectGeneratedFile(f.Path) && !tracked && snapshot.Exists && snapshot.Hash != hash {
			// A file that the record does not track yet is the author's: apply does not replace it unasked.
			handEdited = append(handEdited, f.Path)
		}
		action := ActionUpdate
		if !snapshot.Exists {
			action = ActionGenerate
		} else if snapshot.Hash == hash {
			action = ActionUnchanged
		}
		planned = append(planned, PlannedFile{Path: f.Path, Action: action})
		if isProjectRootFile(f.Path) && action != ActionUnchanged && manifest != nil {
			// The run writes the module file that the generated code builds
			// with, so the record names it as the run leaves it, and a second
			// apply from the same inputs changes nothing (ST-GEN-UC04-R01).
			mode := defaultOutputMode()
			if snapshot.Exists {
				mode = snapshot.Mode.Perm()
			}
			manifest.Files[name] = InputFile{Exists: true, SHA256: hash, Mode: uint32(mode)}
			manifest.SHA256 = manifest.digest()
		}
	}

	// Detect orphaned files: tracked in previous state but no longer generated.
	for path := range existingHashes {
		if isProjectRootFile(path) {
			continue // Transfer ownership from older state without deletion.
		}
		if _, stillGenerated := newFileHashes[path]; !stillGenerated {
			name := projectFilePath(relative, path)
			_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
			if err != nil {
				return nil, err
			}
			snapshots[name] = snapshot
			planned = append(planned, PlannedFile{Path: path, Action: ActionDelete})
		}
	}
	for path := range existingProject {
		if _, stillGenerated := newProjectHashes[path]; !stillGenerated {
			_, snapshot, err := readSnapshot(root, path, maxTrackedFileBytes, false)
			if err != nil {
				return nil, err
			}
			snapshots[path] = snapshot
			planned = append(planned, PlannedFile{Path: path, Action: ActionDelete})
		}
	}

	tracked := make(map[string]string, len(existingHashes)+len(existingProject))
	maps.Copy(tracked, existingHashes)
	maps.Copy(tracked, existingProject)
	for path, recorded := range tracked {
		if isProjectRootFile(path) {
			continue
		}
		name := projectFilePath(relative, path)
		snapshot, read := snapshots[name]
		if !read {
			var err error
			if _, snapshot, err = readSnapshot(root, name, maxTrackedFileBytes, false); err != nil {
				return nil, err
			}
		}
		// A missing generated file holds no hand edit, so the run writes it again with no flag; only a file that
		// is there with other bytes is a hand edit (ST-GEN-Q46, ST-GEN-CLI01).
		_, generated := newFileHashes[path]
		if _, project := newProjectHashes[path]; project {
			generated = true
		}
		switch {
		case !snapshot.Exists && generated:
			restored = append(restored, path)
		case snapshot.Exists && snapshot.Hash != recorded:
			handEdited = append(handEdited, path)
		}
	}
	sort.Strings(handEdited)
	sort.Strings(restored)

	sort.Slice(planned, func(i, j int) bool {
		return planned[i].Path < planned[j].Path
	})
	if err := validateFileLayout(sortedKeys(snapshots)); err != nil {
		return nil, err
	}

	// Compute entity field changes.
	entityChanges := computeEntityChanges(entities, existingState)

	// Build new entity snapshot for state, capturing field types and hashes
	// for change detection.
	entitySnapshot := make(map[string][]EntityFieldState, len(entities))
	for _, e := range entities {
		var fields []EntityFieldState
		for _, f := range e.Fields {
			fields = append(fields, EntityFieldState{
				Name: f.Name,
				Type: string(f.Type),
				Hash: fieldHash(f),
			})
		}
		entitySnapshot[e.Name] = fields
	}

	compilerBuild := buildidentity.Current()
	newState := &State{
		FormatVersion: StateFormatVersion,
		LastApplied: &AppliedState{
			Inputs:                manifest,
			CompilerBuild:         &compilerBuild,
			ServiceName:           serviceName,
			ServiceHash:           serviceHash,
			RegistrySHA:           registrySHA,
			RegistryContentSHA256: registryContentHash,
			Components:            pins,
			Entities:              entitySnapshot,
			Files:                 newFileHashes,
			ProjectFiles:          newProjectHashes,
		},
	}

	oldStateData, err := yaml.Marshal(existingState)
	if err != nil {
		return nil, err
	}
	newStateData, err := yaml.Marshal(newState)
	if err != nil {
		return nil, err
	}
	projectPath, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	outputPath, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	return &Plan{
		HandEdited:   handEdited,
		Restored:     restored,
		StateChanged: string(oldStateData) != string(newStateData),
		snapshots:    snapshots, projectDir: projectPath, outDir: outputPath,
		Files:          planned,
		EntityChanges:  entityChanges,
		GeneratedFiles: generatedFiles,
		NewState:       newState,
	}, nil
}

// fieldDescriptor formats a field name and type for plan display.
func fieldDescriptor(name, typ string) string {
	return fmt.Sprintf("%s (%s)", name, typ)
}

// fieldHash computes a SHA-256 hash of a serialized field definition,
// capturing type, constraints, and all attributes for change detection.
func fieldHash(f types.Field) string {
	data, _ := yaml.Marshal(f)
	return HashBytes(data)
}

// computeEntityChanges diffs current entities against the previous apply's
// entity snapshot to find added, removed, and modified fields.
func computeEntityChanges(entities []types.Entity, existingState *State) []EntityChange {
	if existingState.LastApplied == nil || existingState.LastApplied.Entities == nil {
		return nil
	}

	oldEntities := existingState.LastApplied.Entities

	// Build a set of current entity names for deletion detection.
	currentNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		currentNames[e.Name] = true
	}

	var changes []EntityChange

	// Detect additions, modifications, and field removals in current entities.
	for _, e := range entities {
		oldFields, existed := oldEntities[e.Name]
		if !existed {
			// Entire entity is new — all fields are additions.
			var added []string
			for _, f := range e.Fields {
				added = append(added, fieldDescriptor(f.Name, string(f.Type)))
			}
			if len(added) > 0 {
				changes = append(changes, EntityChange{
					Entity: e.Name,
					Added:  added,
				})
			}
			continue
		}

		// Build lookup from old field name to its snapshot.
		oldByName := make(map[string]EntityFieldState, len(oldFields))
		for _, f := range oldFields {
			oldByName[f.Name] = f
		}

		newSet := make(map[string]bool)
		var added, modified []string
		for _, f := range e.Fields {
			newSet[f.Name] = true
			oldField, wasPresent := oldByName[f.Name]
			if !wasPresent {
				added = append(added, fieldDescriptor(f.Name, string(f.Type)))
				continue
			}
			// Field exists in both — check for type or constraint changes via hash.
			newHash := fieldHash(f)
			if newHash != oldField.Hash {
				if string(f.Type) != oldField.Type {
					modified = append(modified, fieldDescriptor(f.Name, oldField.Type+" → "+string(f.Type)))
				} else {
					modified = append(modified, fieldDescriptor(f.Name, string(f.Type)))
				}
			}
		}

		var removed []string
		for _, f := range oldFields {
			if !newSet[f.Name] {
				removed = append(removed, fieldDescriptor(f.Name, f.Type))
			}
		}

		if len(added) > 0 || len(removed) > 0 || len(modified) > 0 {
			changes = append(changes, EntityChange{
				Entity:   e.Name,
				Added:    added,
				Removed:  removed,
				Modified: modified,
			})
		}
	}

	// Detect deleted entities: present in old state but absent from current.
	// Collect and sort deleted entity names for deterministic output.
	var deletedNames []string
	for entityName := range oldEntities {
		if !currentNames[entityName] {
			deletedNames = append(deletedNames, entityName)
		}
	}
	sort.Strings(deletedNames)
	for _, entityName := range deletedNames {
		oldFields := oldEntities[entityName]
		var removed []string
		for _, f := range oldFields {
			removed = append(removed, fieldDescriptor(f.Name, f.Type))
		}
		changes = append(changes, EntityChange{
			Entity:  entityName,
			Removed: removed,
		})
	}

	return changes
}

// FormatPlan produces a human-readable summary of the plan.
func FormatPlan(plan *Plan) string {
	if !plan.HasChanges() {
		return "No changes. Infrastructure is up-to-date."
	}

	var result strings.Builder

	// Show entity field changes if any.
	if len(plan.EntityChanges) > 0 {
		result.WriteString("Changes detected in service.yaml:\n")
		for _, ec := range plan.EntityChanges {
			fmt.Fprintf(&result, "  entities.%s:\n", ec.Entity)
			for _, f := range ec.Added {
				fmt.Fprintf(&result, "    + field: %s\n", f)
			}
			for _, f := range ec.Modified {
				fmt.Fprintf(&result, "    ~ field: %s\n", f)
			}
			for _, f := range ec.Removed {
				fmt.Fprintf(&result, "    - field: %s\n", f)
			}
		}
		result.WriteString("\n")
	}

	var sb strings.Builder
	var generateCount, updateCount, deleteCount, unchangedCount int

	for _, f := range plan.Files {
		switch f.Action {
		case ActionGenerate:
			generateCount++
			if slices.Contains(plan.Restored, f.Path) {
				fmt.Fprintf(&sb, "  restore:  %s (missing; written again)\n", f.Path)
			} else {
				fmt.Fprintf(&sb, "  generate: %s\n", f.Path)
			}
		case ActionUpdate:
			updateCount++
			fmt.Fprintf(&sb, "  update:   %s\n", f.Path)
		case ActionDelete:
			deleteCount++
			fmt.Fprintf(&sb, "  delete:   %s\n", f.Path)
		case ActionUnchanged:
			unchangedCount++
		}
	}

	result.WriteString("Plan:\n")
	result.WriteString(sb.String())
	for _, note := range plan.Notes {
		fmt.Fprintf(&result, "  note:     %s\n", note)
	}
	if plan.StateChanged {
		result.WriteString("  update:   compiler state\n")
	}
	if unchangedCount > 0 {
		fmt.Fprintf(&result, "  unchanged: %d files\n", unchangedCount)
	}
	fmt.Fprintf(&result, "\nSummary: %d to generate, %d to update, %d to delete, %d unchanged\n",
		generateCount, updateCount, deleteCount, unchangedCount)

	return result.String()
}

// WorkersDigest gives the workers digest of the worker names, as the runtime's tenant.WorkersDigest gives it: the
// lower-case hex SHA-256 of the names in byte order, each followed by a line feed (ST-SUP-RC35-OC13).
func WorkersDigest(names []string) string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sum := sha256.New()
	for _, name := range sorted {
		sum.Write([]byte(name + "\n"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// releaseIdentityNote is the plan line of a service with tenant provisioning: the workers digest that the migration
// job of the release writes in the release identity row, which fences the reconcile of the worker tuples
// (ST-SUP-RC35-OC13). A service with no provisioning has none.
func releaseIdentityNote(service *types.ServiceDeclaration) string {
	if service == nil || service.Tenancy == nil || service.Tenancy.Provisioning == nil {
		return ""
	}
	names := make([]string, len(service.Workers))
	for i, w := range service.Workers {
		names[i] = w.Name
	}
	slices.Sort(names)
	return fmt.Sprintf("release identity: workers digest %s of the workers [%s]; the migration job writes it, and only a controller of the same release reconciles the worker tuples", WorkersDigest(names), strings.Join(names, ", "))
}

// validateSlotCollectionsDefined checks that every slot binding with a non-empty
// Collection field references a collection that exists. Slot operators are
// injected into handler constructors, which are only generated for collections.
// A binding referencing a non-existent collection would produce an unused
// variable — a Go compile error.
func validateSlotCollectionsDefined(slots []types.SlotDeclaration, collections []types.Collection) error {
	if len(slots) == 0 {
		return nil
	}

	// Build set of collection names.
	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
	}

	for _, sb := range slots {
		if sb.Collection == "" {
			continue
		}
		if !collectionNames[sb.Collection] {
			var names []string
			for _, c := range collections {
				names = append(names, c.Name)
			}
			return fmt.Errorf("slot binding %q references collection %q but %q is not defined — "+
				"slot operators can only be wired to defined collections (available: %v)",
				sb.Slot, sb.Collection, sb.Collection, names)
		}
	}
	return nil
}

// applyConventionOverrides returns a copy of the archetype conventions with any
// service-level convention overrides applied. Currently supports:
//   - cors: "disabled" → sets CORS to "" (suppresses CORS middleware generation)
//   - cors: "enabled"  → keeps CORS as "enabled" (explicit opt-in, same as default)
func applyConventionOverrides(base types.Convention, overrides map[string]any) types.Convention {
	conv := base
	if corsVal, ok := overrides["cors"]; ok {
		if strVal, ok := corsVal.(string); ok {
			switch strVal {
			case "disabled":
				conv.CORS = ""
			case "enabled":
				conv.CORS = "enabled"
			}
		}
	}
	return conv
}

// validateUniqueFilePaths checks that no two files in the collection share the
// same output path. Duplicate paths mean one source's output would silently
// overwrite another's.
func validateUniqueFilePaths(files []gen.File) error {
	seen := make(map[string]bool, len(files))
	var duplicates []string
	var layout []string
	for _, f := range files {
		if err := gen.ValidatePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			duplicates = append(duplicates, f.Path)
		}
		seen[f.Path] = true
		name := "out/" + f.Path
		if isProjectRootFile(f.Path) {
			name = f.Path
		}
		layout = append(layout, name)
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return fmt.Errorf("duplicate generated file paths: %s", strings.Join(duplicates, ", "))
	}
	return validateFileLayout(layout)
}
