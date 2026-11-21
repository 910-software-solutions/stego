package fill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// Standing is where one fill stands on its way to being qualified (ST-VO45).
type Standing string

// The qualification standings.
const (
	Qualified Standing = "qualified"
	Stale     Standing = "stale"
	Unsigned  Standing = "unsigned"
	Untested  Standing = "untested"
)

// ParseStanding reads a qualification standing, which is exactly one of
// qualified, stale, unsigned or untested (ST-VO45-C01).
func ParseStanding(text string) (Standing, error) {
	switch standing := Standing(text); standing {
	case Qualified, Stale, Unsigned, Untested:
		return standing, nil
	}
	return "", fmt.Errorf("%q is not a qualification standing: one of %s, %s, %s or %s", text, Qualified, Stale, Unsigned, Untested)
}

// Current is the state of a fill and of its slot contract now, which its
// evidence must name for the fill to be qualified.
type Current struct {
	FillDigest     string
	Files          map[string]string
	ContractDigest string
	Contract       types.SlotRef
	HasTests       bool
}

// IsQualified is the predicate of ST-EXT-SP01: the fill's evidence names its
// current fill digest and its slot contract's current digest, and a key that
// SIGNERS lists signed that evidence.
func IsQualified(f *Fill, fillDigest, contractDigest string, signers Signers) bool {
	standing, _ := assess(f, Current{FillDigest: fillDigest, ContractDigest: contractDigest, HasTests: true}, signers)
	return standing == Qualified
}

// FitsBinding is the predicate of ST-EXT-SP02: the fill implements the bound
// slot of the bound component and serves the bound collection.
func FitsBinding(f *Fill, slot types.SlotRef, collection string) bool {
	return misfit(f, []types.SlotRef{slot}, compiler.FillBinding{Collection: collection}) == ""
}

// misfit names why a fill does not fit a binding of one of the slots, or
// gives "" when it fits. The binding of the fill of a command, query or
// worker names no collection, and the fill names none (decision on
// ST-EXT-Q30).
func misfit(f *Fill, slots []types.SlotRef, binding compiler.FillBinding) string {
	serves := "for the collection " + binding.Collection
	if binding.Owner != "" {
		serves = "for the " + binding.Owner
	}
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil || !slices.Contains(slots, ref) {
		names := make([]string, len(slots))
		for i, slot := range slots {
			names[i] = slot.String()
		}
		bound := strings.Join(names, " or ")
		if len(slots) == 0 {
			bound = binding.Slot
		}
		return fmt.Sprintf("fill %q implements %s, not the bound slot %s, %s", f.Name, f.Implements, bound, serves)
	}
	if f.Collection != binding.Collection {
		if binding.Collection == "" {
			return fmt.Sprintf("fill %q serves the collection %s, but a fill of the slot %s names no collection, %s", f.Name, f.Collection, ref, serves)
		}
		return fmt.Sprintf("fill %q serves the collection %s, not the bound collection %s, at the slot %s", f.Name, f.Collection, binding.Collection, ref)
	}
	return ""
}

// holdsCode tells whether the fill folder exists and holds Go code, or
// cannot be read (holdsGoCode).
func holdsCode(dir string) bool {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return false
	}
	return holdsGoCode(dir)
}

// capabilitySlot tells whether the slot is the slot of a command, a query or
// a worker, whose fill names no collection (decision on ST-EXT-Q30).
func capabilitySlot(ref types.SlotRef) bool {
	switch ref.String() {
	case commandSlot, querySlot, workerSlot:
		return true
	}
	return false
}

// nextSteps tells what to run so that a fill of the standing becomes
// qualified: stego test, then stego fill sign; only stego fill sign; or, for a
// key that SIGNERS does not list, what to list or sign with.
func nextSteps(fill string, standing Standing, reasons []string) string {
	if standing == Unsigned && slices.Contains(reasons, "SIGNERS does not list its key") {
		return fmt.Sprintf("list the signer's public key in the project's SIGNERS file, or run stego fill sign %s with a key that SIGNERS lists", fill)
	}
	if standing == Unsigned {
		return fmt.Sprintf("run stego fill sign %s", fill)
	}
	return fmt.Sprintf("run stego test, then stego fill sign %s", fill)
}

// The reasons of a fill that has no evidence and of a fill that has evidence and no sign-off.
const (
	reasonNoEvidence = "it has no evidence"
	reasonNoSignOff  = "it has no sign-off"
)

// assess gives the standing of a fill and, when it is not qualified, why.
func assess(f *Fill, current Current, signers Signers) (Standing, string) {
	if !current.HasTests {
		return Untested, "it has no test file"
	}
	if f.Evidence == nil {
		return Untested, reasonNoEvidence
	}
	var changed []string
	if f.Evidence.FillDigest != current.FillDigest {
		changed = append(changed, changedFiles(f.Evidence.Files, current.Files)...)
		if len(changed) == 0 {
			changed = append(changed, "the fill")
		}
	}
	if f.Evidence.ContractDigest != current.ContractDigest {
		contract := current.Contract.String()
		if current.Contract == (types.SlotRef{}) {
			contract = f.Implements
		}
		changed = append(changed, "the contract "+contract)
	}
	if len(changed) > 0 {
		return Stale, strings.Join(changed, ", ") + " changed after its evidence"
	}
	if f.SignOff == nil {
		return Unsigned, reasonNoSignOff
	}
	digest, err := EvidenceDigest(f)
	if err != nil {
		return Unsigned, err.Error()
	}
	if _, err := VerifySignOff(f.SignOff, digest, signers); err != nil {
		switch {
		case errors.Is(err, ErrUnlistedKey):
			return Unsigned, "SIGNERS does not list its key"
		case errors.Is(err, ErrKeyNotValid):
			return Unsigned, "the key of its sign-off is revoked or expired"
		default:
			return Unsigned, "its sign-off does not sign its evidence"
		}
	}
	return Qualified, ""
}

// changedFiles names the files that differ between the digests that the
// evidence holds and the digests now, in the order of their names.
func changedFiles(before, now map[string]string) []string {
	var names []string
	for name, digest := range now {
		if before[name] != digest {
			names = append(names, "the file "+name)
		}
	}
	for name := range before {
		if _, held := now[name]; !held {
			names = append(names, "the file "+name)
		}
	}
	sort.Strings(names)
	return names
}

// Qualification is one fill that the declaration binds, with its standing,
// whether it fits each of its bindings, and why it is not qualified or does
// not fit (ST-EXT-RM02).
type Qualification struct {
	Fill     string
	Standing Standing
	Fits     bool
	Reasons  []string
}

// ListQualification answers ST-EXT-QRY02: each fill that the declaration
// binds, with its standing and its fit, in the order of the fill names. It
// writes nothing.
//
// A fill whose evidence and sign-off are current but that breaks a fill rule
// now (declaredFillFaults: an import, a file or a call) shows as untested, with
// each fault as a reason, because stego test would record no evidence for it
// (decision on ST-EXT-Q06).
func ListQualification(check compiler.FillCheck) ([]Qualification, error) {
	rows, err := projectQualification(check, true)
	if err != nil {
		return nil, err
	}
	return NewQualificationReader(rows).Rows(), nil
}

// QualificationFaults is the compiler's compiler.FillQualifier: a fault for
// each binding of a fill that does not fit that binding or is not qualified,
// and for each fill that the declaration names, at every slot, that breaks
// the fill rules (declaredFillFaults).
// Each fault names the binding's path in the declaration and its collection
// and slot, so an agent can go to the exact binding.
func QualificationFaults(check compiler.FillCheck) ([]compiler.FillFault, error) {
	faults, err := declaredFillFaults(check)
	if err != nil {
		return nil, err
	}
	if len(check.Bindings) == 0 {
		return faults, nil
	}
	bound, err := bindingFaults(check)
	if err != nil {
		return nil, err
	}
	return append(bound, faults...), nil
}

// RevisionFaults gives a fault for each of the fills, by name, in the
// project folder dir that is not qualified as the folder holds it: its
// manifest cannot be read, it has no test file or no evidence, its files
// changed after its evidence, or a key that SIGNERS lists did not sign that
// evidence. Each fault names the fill and what to run. It reads no registry
// and no module, so stego build can run it offline on a copy of the source
// revision. The contract digest of the evidence of a fill of a command, query
// or worker must be the digest of the contract that the folder holds
// (capability, from compiler.RevisionFillCheck); the contract digest of
// another fill is taken as it is, since its contract is in the registry
// (ST-GEN-INV10).
func RevisionFaults(dir string, fills []string, capability compiler.FillCheck) ([]string, error) {
	signers, err := ReadSigners(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var faults []string
	for _, name := range fills {
		if seen[name] {
			continue
		}
		seen[name] = true
		folder := filepath.Join(dir, "fills", name)
		if _, err := fillPackageFolder(name); err != nil {
			faults = append(faults, fmt.Sprintf("fill %q: %v", name, err))
			continue
		}
		f, err := ParseFill(filepath.Join(folder, ManifestFile))
		if err != nil {
			faults = append(faults, fmt.Sprintf("fill %q is untested: %v; %s", name, err, nextSteps(name, Untested, nil)))
			continue
		}
		files, err := FileDigests(folder)
		if err != nil {
			faults = append(faults, fmt.Sprintf("fill %q is untested: %v; %s", name, err, nextSteps(name, Untested, nil)))
			continue
		}
		current := Current{FillDigest: combine(files), Files: files}
		for file := range files {
			if strings.HasSuffix(file, "_test.go") {
				current.HasTests = true
			}
		}
		// The fill of a command, query or worker must implement the slot that its binding names, as plan checks
		// (misfit); else its contract digest would be compared with no contract, or with that of another slot.
		if fault := capabilityMisfit(f, name, capability.Bindings); fault != "" {
			faults = append(faults, fault)
			continue
		}
		if f.Evidence != nil {
			current.ContractDigest = f.Evidence.ContractDigest
			if ref, err := types.ParseSlotRef(f.Implements); err == nil && capabilitySlot(ref) {
				digest, err := CapabilityContractDigest(capability, name, ref)
				if err != nil {
					faults = append(faults, fmt.Sprintf("fill %q: its contract cannot be read: %v", name, err))
					continue
				}
				if digest != f.Evidence.ContractDigest {
					faults = append(faults, fmt.Sprintf("fill %q is stale: the fill was signed against an older contract than the revision holds; %s", name, nextSteps(name, Stale, nil)))
					continue
				}
			}
		}
		if standing, reason := assess(f, current, signers); standing != Qualified {
			var reasons []string
			if reason != "" {
				reasons = append(reasons, reason)
			}
			faults = append(faults, fmt.Sprintf("fill %q is %s: %s; %s", name, standing, strings.Join(reasons, "; "), nextSteps(name, standing, reasons)))
		}
	}
	return faults, nil
}

// capabilityMisfit names why the fill does not fit a binding of a command, query or worker that names it, or gives ""
// when it fits each such binding or no such binding names it. The slot of each such binding is named after its
// component (command.command, query.query, worker.worker).
func capabilityMisfit(f *Fill, name string, bindings []compiler.FillBinding) string {
	for _, binding := range bindings {
		if binding.Fill != name || binding.Owner == "" {
			continue
		}
		if fault := misfit(f, []types.SlotRef{{Component: binding.Slot, Slot: binding.Slot}}, binding); fault != "" {
			return fault
		}
	}
	return ""
}

// fillPackageFolder refuses a fill name that is not one folder name under
// fills, such as a path.
func fillPackageFolder(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%q is not the name of a fill folder", name)
	}
	return name, nil
}

// bindingFaults gives a fault for each binding of a fill that does not fit that binding or is not qualified.
func bindingFaults(check compiler.FillCheck) ([]compiler.FillFault, error) {
	// The rule faults reach the compiler through declaredFillFaults, so the
	// binding faults do not repeat them.
	projected, err := projectQualification(check, false)
	if err != nil {
		return nil, err
	}
	rows := NewQualificationReader(projected).Rows()
	if err != nil {
		return nil, err
	}
	byFill := map[string]Qualification{}
	for _, row := range rows {
		byFill[row.Fill] = row
	}
	// The misfit of each binding, so that a standing fault does not repeat the
	// misfit of another binding of the same fill.
	misfits := make([]string, len(check.Bindings))
	misfitReasons := map[string]bool{}
	manifests := map[string]*Fill{}
	for i, binding := range check.Bindings {
		if row, ok := byFill[binding.Fill]; !ok || row.Fits {
			continue
		}
		f, seen := manifests[binding.Fill]
		if !seen {
			f, _ = ReadManifest(filepath.Join(check.ProjectDir, "fills", binding.Fill, ManifestFile), check)
			manifests[binding.Fill] = f
		}
		// A manifest that cannot be read gives the untested fault, with the reason.
		if f != nil {
			misfits[i] = misfit(f, slotsNamed(check, binding.Slot), binding)
			misfitReasons[misfits[i]] = true
		}
	}
	var faults []compiler.FillFault
	for i, binding := range check.Bindings {
		row, ok := byFill[binding.Fill]
		if !ok {
			continue
		}
		at := fmt.Sprintf("at %s (%s.%s)", binding.Path, binding.Collection, binding.Slot)
		if binding.Owner != "" {
			at = fmt.Sprintf("at %s (the fill of the %s)", binding.Path, binding.Owner)
		}
		if binding.Owner != "" && !holdsCode(filepath.Join(check.ProjectDir, "fills", binding.Fill)) {
			// A fill of a command, query or worker that holds no Go code yet cannot be built into the service, so
			// it does not stop generation, which writes the contract that the fill is written against; apply then
			// names the fill to write (CodelessFills). The check of the declaration names a fill folder that does
			// not exist.
			continue
		}
		if row.Standing != Qualified {
			var reasons []string
			for _, reason := range row.Reasons {
				if !misfitReasons[reason] {
					reasons = append(reasons, reason)
				}
			}
			faults = append(faults, compiler.FillFault{Rule: "fill-qualified", Path: binding.Path, Found: binding.Fill,
				Message:  fmt.Sprintf("fill %q is %s %s: %s; %s", binding.Fill, row.Standing, at, strings.Join(reasons, "; "), nextSteps(binding.Fill, row.Standing, row.Reasons)),
				Unproved: misfits[i] == "" && unproved(row)})
		}
		if misfits[i] != "" {
			faults = append(faults, compiler.FillFault{Rule: "fill-fits-binding", Path: binding.Path, Found: binding.Fill,
				Message: fmt.Sprintf("fill %q does not fit its binding %s: %s", binding.Fill, at, misfits[i])})
		}
	}
	return faults, nil
}

// unproved tells whether the fill of row fits its bindings and lacks only its evidence or only its sign-off, so that
// stego test, then stego fill sign, qualify it with no change to the fill (ST-DECL-UC01-R01). A stale fill, a sign-off
// that does not verify, a key that SIGNERS does not list and a fill with no test file are not unproved.
func unproved(row Qualification) bool {
	if !row.Fits || len(row.Reasons) != 1 {
		return false
	}
	switch row.Standing {
	case Untested:
		return row.Reasons[0] == reasonNoEvidence
	case Unsigned:
		return row.Reasons[0] == reasonNoSignOff
	}
	return false
}

// projectQualification builds the qualification of each bound fill from its
// manifest, its files, its slot contract and SIGNERS.
// With rules, a fill that breaks a fill rule gets each fault as a reason, and a
// qualified fill becomes untested.
func projectQualification(check compiler.FillCheck, rules bool) ([]Qualification, error) {
	signers, err := ReadSigners(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	bindings := map[string][]compiler.FillBinding{}
	var names []string
	for _, binding := range check.Bindings {
		if _, seen := bindings[binding.Fill]; !seen {
			names = append(names, binding.Fill)
		}
		bindings[binding.Fill] = append(bindings[binding.Fill], binding)
	}
	sort.Strings(names)
	var packages map[string]listedPackage
	if rules {
		if packages, err = listFills(check, names); err != nil {
			return nil, err
		}
	}
	var rows []Qualification
	for _, name := range names {
		row := Qualification{Fill: name, Fits: true}
		dir := filepath.Join(check.ProjectDir, "fills", name)
		f, err := ReadManifest(filepath.Join(dir, ManifestFile), check)
		if err != nil {
			row.Standing, row.Fits = Untested, false
			row.Reasons = append(row.Reasons, err.Error())
			rows = append(rows, row)
			continue
		}
		for _, binding := range bindings[name] {
			if reason := misfit(f, slotsNamed(check, binding.Slot), binding); reason != "" {
				row.Fits = false
				row.Reasons = append(row.Reasons, reason)
			}
		}
		current, err := CurrentState(dir, f, check)
		if err != nil {
			// A fill whose files or contract cannot be read is not qualified, and the fault names it.
			row.Standing = Untested
			row.Reasons = append(row.Reasons, fmt.Sprintf("fill %q: %v", name, err))
			rows = append(rows, row)
			continue
		}
		standing, reason := assess(f, current, signers)
		row.Standing = standing
		if reason != "" {
			row.Reasons = append(row.Reasons, reason)
		}
		if rules {
			faults, err := fillFaults(check, dir, name, slotContracts(check, name, f.Implements), packages)
			if err != nil {
				return nil, err
			}
			for _, fault := range faults {
				row.Reasons = append(row.Reasons, fault.text)
			}
			if len(faults) > 0 && row.Standing == Qualified {
				row.Standing = Untested
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// slotsNamed gives each active slot of the name.
func slotsNamed(check compiler.FillCheck, name string) []types.SlotRef {
	var refs []types.SlotRef
	for _, slot := range check.Slots {
		if slot.Ref.Slot == name {
			refs = append(refs, slot.Ref)
		}
	}
	return refs
}

// CurrentState reads the fill's files and its slot contract as they are now.
func CurrentState(dir string, f *Fill, check compiler.FillCheck) (Current, error) {
	files, err := FileDigests(dir)
	if err != nil {
		return Current{}, err
	}
	current := Current{FillDigest: combine(files), Files: files}
	for name := range files {
		if strings.HasSuffix(name, "_test.go") {
			current.HasTests = true
		}
	}
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil {
		return Current{}, err
	}
	slot, ok := findSlot(check, ref)
	if !ok {
		return Current{}, fmt.Errorf("fill %q: implements: %s is not a slot of the active component set", f.Name, f.Implements)
	}
	current.Contract = ref
	if capabilitySlot(ref) {
		if current.ContractDigest, err = CapabilityContractDigest(check, f.Name, ref); err != nil {
			return Current{}, fmt.Errorf("fill %q: %w", f.Name, err)
		}
		return current, nil
	}
	current.ContractDigest, err = ContractDigest(check.Contracts, slot.Contract)
	if err != nil {
		return Current{}, err
	}
	return current, nil
}

// findSlot finds the active slot of a reference.
func findSlot(check compiler.FillCheck, ref types.SlotRef) (compiler.FillSlot, bool) {
	for _, slot := range check.Slots {
		if slot.Ref == ref {
			return slot, true
		}
	}
	return compiler.FillSlot{}, false
}

// ReadManifest reads a fill manifest and checks it against the declaration
// and the active registry (ST-EXT-INV01): it names exactly one slot of a
// component of the active set and, for a slot of a collection, one collection
// of the declaration. A fill of the command, query or worker slot names no
// collection, as its binding is the command, query or worker that names it
// (decision on ST-EXT-Q30). A refusal names the fill and the field.
func ReadManifest(path string, check compiler.FillCheck) (*Fill, error) {
	name := filepath.Base(filepath.Dir(path))
	if err := checkOneValue(path, name); err != nil {
		return nil, err
	}
	f, err := ParseFill(path)
	if err != nil {
		return nil, fmt.Errorf("fill %q: %w", name, err)
	}
	if f.Name != "" {
		name = f.Name
	}
	if f.Implements == "" {
		return nil, fmt.Errorf("fill %q: implements: missing required field", name)
	}
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil {
		return nil, fmt.Errorf("fill %q: implements: %v", name, err)
	}
	if _, ok := findSlot(check, ref); !ok {
		return nil, fmt.Errorf("fill %q: implements: %s is not a slot of a component of the active set", name, ref)
	}
	if capabilitySlot(ref) {
		if f.Collection != "" {
			return nil, fmt.Errorf("fill %q: collection: a fill of the slot %s names no collection, as its binding is the %s that names it; remove collection: %s", name, ref, ref.Component, f.Collection)
		}
		return f, nil
	}
	if f.Collection == "" {
		return nil, fmt.Errorf("fill %q: collection: missing required field", name)
	}
	if !slices.Contains(check.Collections, f.Collection) {
		return nil, fmt.Errorf("fill %q: collection: %s is not a collection of the declaration", name, f.Collection)
	}
	return f, nil
}

// checkOneValue refuses a manifest whose implements or collection is not
// one value, such as a list of slots, and names the field. The strict reader
// refuses it too, but does not name the field.
func checkOneValue(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("fill %q: %w", name, err)
	}
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if (key == "implements" || key == "collection") && root.Content[i+1].Kind != yaml.ScalarNode {
			return fmt.Errorf("fill %q: %s: must be exactly one value, not a list or a mapping", name, key)
		}
	}
	return nil
}

// fillFolders lists the fill folders of the project, in the order of their
// names.
func fillFolders(projectDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(projectDir, "fills"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// AddedKeySignOff is a fill whose sign-off verifies against the SIGNERS file of the project and not against the
// SIGNERS file of a base, such as the merge base of the branch: its sign-off counts only because of a key that the
// change adds to SIGNERS (walkthrough 21, T-D). Signer is the identity that the SIGNERS file of the project lists for
// the key.
type AddedKeySignOff struct {
	Fill   string
	Signer string
}

// AddedKeySignOffs gives each fill that the declaration binds and that is qualified, whose sign-off does not verify
// against base for any reason, such as a key that base does not list or lists with other options, in the order of
// the fill names (ST-EXT-UC02-R02, decision ST-EXT-Q31). It writes nothing. A fill that is not qualified is not given: the checks of qualification name it.
func AddedKeySignOffs(check compiler.FillCheck, base Signers) ([]AddedKeySignOff, error) {
	signers, err := ReadSigners(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	rows, err := projectQualification(check, false)
	if err != nil {
		return nil, err
	}
	var added []AddedKeySignOff
	for _, row := range rows {
		if row.Standing != Qualified {
			continue
		}
		f, err := ReadManifest(filepath.Join(check.ProjectDir, "fills", row.Fill, ManifestFile), check)
		if err != nil {
			return nil, err
		}
		digest, err := EvidenceDigest(f)
		if err != nil {
			return nil, err
		}
		// Any refusal of the base counts, also of a key that the base lists with other options, such as
		// namespaces=, or another principal (security review of PR #485, L1).
		if _, err := VerifySignOff(f.SignOff, digest, base); err == nil {
			continue
		}
		signer, err := VerifySignOff(f.SignOff, digest, signers)
		if err != nil {
			return nil, err
		}
		added = append(added, AddedKeySignOff{Fill: row.Fill, Signer: signer})
	}
	return added, nil
}
