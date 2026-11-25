package assure

import (
	"encoding/json"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The proofs of the pattern ST-PAT-GO-CODE in one domain: its rules R01 (each hand-written technical record fills
// exactly one slot), R02 (each fill serves an essential record) and R03 (the fills meet the code standard). Each
// domain's test makes a goCodeProof for its own ID and names each subtest with its test record, numbered by the
// domain's position: ST-ASR 1, ST-BLD 2, ST-DECL 3, ST-DIS 4, ST-DLV 5, ST-EXT 6, ST-GEN 7 and ST-SUP 8. Extending
// has a test of its own, in pattern_rule_ext_test.go.

// latheWorkspace is the part of lathe check --json that the pattern proofs read.
type latheWorkspace struct {
	Kinds []struct {
		Kind       string `json:"kind"`
		Stage      string `json:"stage"`
		Complexity string `json:"complexity"`
	} `json:"kinds"`
	Records []struct {
		ID     string         `json:"id"`
		Kind   string         `json:"kind"`
		Parent string         `json:"parent"`
		Domain string         `json:"domain"`
		Fields map[string]any `json:"fields"`
	} `json:"records"`
	Links []struct {
		Source   string `json:"source"`
		Relation string `json:"relation"`
		Target   string `json:"target"`
	} `json:"links"`
	Problems []struct {
		Check   string `json:"check"`
		Record  string `json:"record"`
		Message string `json:"message"`
	} `json:"problems"`
}

// checked holds the workspace that the first call of checkedWorkspace read: each domain's proof reads the same spec,
// so lathe check runs once for the test binary.
var checked struct {
	sync.Mutex
	workspace *latheWorkspace
}

// checkedWorkspace runs lathe check --json on the spec of the repository and gives its product's workspace.
func checkedWorkspace(t *testing.T) latheWorkspace {
	t.Helper()
	checked.Lock()
	defer checked.Unlock()
	if checked.workspace != nil {
		return *checked.workspace
	}
	lathe := latheCLI(t)
	root, err := RepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(lathe, "check", "--json")
	command.Dir = root
	output, err := command.Output()
	if err != nil && len(output) == 0 {
		t.Fatalf("lathe check --json: %v", err)
	}
	var check struct {
		Products []struct {
			Workspace latheWorkspace `json:"workspace"`
		} `json:"products"`
	}
	if err := json.Unmarshal(output, &check); err != nil || len(check.Products) == 0 {
		t.Fatalf("the output is not lathe check --json: %v", err)
	}
	checked.workspace = &check.Products[0].Workspace
	return *checked.workspace
}

// domainFills gives each hand-written technical record of the domain with the slots it fills: the records of the
// technical stage whose kind is accidental, that nothing provides, and that no tooling record generates. A record of
// an essential kind of the technical stage, such as an output contract or a library contract, is a promise and not
// code, and a standard or its rule is the rules that code follows: none of them fills a slot.
func domainFills(workspace latheWorkspace, domain string) (map[string][]string, map[string]bool) {
	technical := map[string]bool{}
	for _, kind := range workspace.Kinds {
		if kind.Stage == "technical-specification" && kind.Complexity == "accidental" {
			technical[kind.Kind] = true
		}
	}
	provided := map[string]bool{}
	fills := map[string][]string{}
	for _, link := range workspace.Links {
		switch link.Relation {
		case "provided_by", "generates":
			provided[link.Source] = true
		case "fills":
			fills[link.Source] = append(fills[link.Source], link.Target)
		}
	}
	records := map[string][]string{}
	for _, record := range workspace.Records {
		if record.Domain == domain && technical[record.Kind] && !provided[record.ID] {
			records[record.ID] = fills[record.ID]
		}
	}
	return records, technical
}

// goCodeProof holds what the three proofs of one domain read.
type goCodeProof struct {
	domain    string
	workspace latheWorkspace
	records   map[string][]string
}

func newGoCodeProof(t *testing.T, domain string) goCodeProof {
	t.Helper()
	workspace := checkedWorkspace(t)
	records, _ := domainFills(workspace, domain)
	if len(records) == 0 {
		t.Fatalf("the spec names no hand-written technical record of %s", domain)
	}
	return goCodeProof{domain: domain, workspace: workspace, records: records}
}

// oneSlotEach proves ST-PAT-GO-CODE-R01 in the domain.
func (p goCodeProof) oneSlotEach(t *testing.T) {
	records := p.records
	{
		for id, slots := range records {
			if len(slots) != 1 {
				t.Errorf("%s fills %d slots of ST-PAT-GO-CODE, not one: %v", id, len(slots), slots)
			}
			for _, slot := range slots {
				if !strings.HasPrefix(slot, "ST-PAT-GO-CODE-SL") {
					t.Errorf("%s fills %s, which is not a slot of ST-PAT-GO-CODE", id, slot)
				}
			}
		}
	}
}

// accountedFor is the check of the pinned Lathe that names an accidental record which serves no essential record.
const accountedFor = "accidental-not-accounted-for"

// accountedForCanaries are records that the check reports today; see servesAnEssentialRecord.
var accountedForCanaries = []string{"ST-ASR-ENTRY01", "ST-SUP-TOOL01"}

// servesAnEssentialRecord proves ST-PAT-GO-CODE-R02 in the domain. A fill serves an essential record as the pinned
// Lathe counts it (its check accidental-not-accounted-for): it builds an essential obligation, implements or sits
// under an essential record other than its product or domain, carries out an essential rule that it meets, persists
// an essential aggregate, links to an essential record through a relation of the product, or holds an essential
// record. Thus the test reads that check's findings, and fails on each one that names a fill of the domain.
//
// A canary guards the reading: the pinned Lathe must still name the check, and the check must still report the
// fills that are known not to serve an essential record yet (ST-ASR-ENTRY01 of Assuring and ST-SUP-TOOL01 of
// Supplying, whose proofs of R02 are not automated yet). If Lathe renames the check or stops it, the test fails in
// place of passing on an empty list.
func (p goCodeProof) servesAnEssentialRecord(t *testing.T) {
	reported := map[string]bool{}
	for _, problem := range p.workspace.Problems {
		if problem.Check == accountedFor {
			reported[problem.Record] = true
		}
	}
	for _, canary := range accountedForCanaries {
		if !reported[canary] {
			t.Fatalf("the pinned Lathe check reports no %s finding for %s, so the test cannot see a fill that serves no essential record; if %s is accounted for now, take it off accountedForCanaries", accountedFor, canary, canary)
		}
	}
	var faults []string
	for _, problem := range p.workspace.Problems {
		if _, filled := p.records[problem.Record]; filled && problem.Check == accountedFor {
			faults = append(faults, problem.Message)
		}
	}
	slices.Sort(faults)
	for _, fault := range faults {
		t.Error(fault)
	}
}

// meetsTheCodeStandard proves ST-PAT-GO-CODE-R03 in the domain, against the list of known faults and its ratchet.
func (p goCodeProof) meetsTheCodeStandard(t *testing.T) {
	domain, records := p.domain, p.records
	{
		root, err := RepositoryRoot(".")
		if err != nil {
			t.Fatal(err)
		}
		spec, err := ReadCommittedSpec(root, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := ReadCommittedTree(root, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		// The files that the domain's fills name, with their helper files, and their packages.
		files := map[string]bool{}
		for _, record := range spec.Technical {
			if _, filled := records[record.ID]; filled {
				files[record.File] = true
				for _, helper := range record.Helpers {
					files[helper] = true
				}
			}
		}
		// A fault is the domain's when its path is a file of a fill, a file in the folder of a fill's file (such as a
		// source file that no record names, ST-STD-CODE-R05), or the package of a fill's file.
		folders := map[string]bool{}
		for file := range files {
			folders[path.Dir(file)] = true
		}
		inScope := func(fault CodeFault) bool {
			if files[fault.Path] || folders[path.Dir(fault.Path)] && path.Ext(fault.Path) != "" {
				return true
			}
			for file := range files {
				if strings.HasPrefix(file, fault.Path+"/") && !strings.Contains(strings.TrimPrefix(file, fault.Path+"/"), "/") {
					return true
				}
			}
			return false
		}
		all, err := CodeFaults(spec, tree)
		if err != nil {
			t.Fatal(err)
		}
		var list TechnicalRecord
		for _, record := range spec.Technical {
			if record.ID == knownCodeFaultsRecord {
				list = record
			}
		}
		data, held := tree.File(list.File)
		if !held {
			t.Fatalf("the spec at HEAD has no list of known faults %s", knownCodeFaultsRecord)
		}
		known, err := readKnownCodeFaults(data)
		if err != nil {
			t.Fatal(err)
		}
		var got, listed []CodeFault
		for _, fault := range all {
			if inScope(fault) {
				got = append(got, fault)
			}
		}
		for _, fault := range known {
			if inScope(fault) {
				listed = append(listed, fault)
			}
		}
		if added := difference(got, listed); len(added) > 0 {
			t.Errorf("%d faults in the files of %s's fills that the list of known faults does not hold:\n%s", len(added), domain, faultLines(added))
		}
		if gone := difference(listed, got); len(gone) > 0 {
			t.Errorf("%d listed faults of %s's fills are gone:\n%s", len(gone), domain, faultLines(gone))
		}
		base, why, skip := ratchetBase(t, root)
		if skip || base == "" {
			t.Log("the ratchet against the base is not checked: " + why)
			return
		}
		baseKeys, ratchet := baseKnownCodeFaults(t, root, base)
		if !ratchet {
			return
		}
		for _, fault := range listed {
			if !baseKeys[[2]string{fault.Rule, fault.Key}] {
				t.Errorf("the fault %s %s of %s's fills is not in the list at the base commit %s", fault.Rule, fault.Key, domain, base)
			}
		}
	}
}

func TestDistributingFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-DIS")
	t.Run("ST-PAT-GO-CODE-R01-T04 Distributing's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R02-T04 Distributing's fills each serve an essential record", proof.servesAnEssentialRecord)
	t.Run("ST-PAT-GO-CODE-R03-T04 Distributing's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestAssuringFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-ASR")
	t.Run("ST-PAT-GO-CODE-R01-T01 Assuring's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R03-T01 Assuring's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestBuildingFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-BLD")
	t.Run("ST-PAT-GO-CODE-R01-T02 Building's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R03-T02 Building's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestDeclaringFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-DECL")
	t.Run("ST-PAT-GO-CODE-R01-T03 Declaring's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R02-T03 Declaring's fills each serve an essential record", proof.servesAnEssentialRecord)
	t.Run("ST-PAT-GO-CODE-R03-T03 Declaring's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestDeliveringFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-DLV")
	t.Run("ST-PAT-GO-CODE-R01-T05 Delivering's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R02-T05 Delivering's fills each serve an essential record", proof.servesAnEssentialRecord)
	t.Run("ST-PAT-GO-CODE-R03-T05 Delivering's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestGeneratingFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-GEN")
	t.Run("ST-PAT-GO-CODE-R01-T07 Generating's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R02-T07 Generating's fills each serve an essential record", proof.servesAnEssentialRecord)
	t.Run("ST-PAT-GO-CODE-R03-T07 Generating's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}

func TestSupplyingFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-SUP")
	t.Run("ST-PAT-GO-CODE-R03-T08 Supplying's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}
