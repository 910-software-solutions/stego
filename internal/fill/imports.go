package fill

import (
	"errors"
	"strings"

	"github.com/910-software-solutions/stego/internal/compiler"
)

// querySlot and commandSlot are the slots whose contracts are not the package out/slots.
const (
	querySlot   = "query.query"
	commandSlot = "command.command"
	workerSlot  = "worker.worker"
)

// slotContracts gives the packages that a fill of the slot implements may import besides the standard library and
// the listed fill modules (ST-EXT-INV05): its slot's contract package, out/slots for a protobuf slot, out/queries for
// the query slot and the command generator's package for the command slot, and the packages of the runtime library
// that the generator of the contract package can write into it.
func slotContracts(check compiler.FillCheck, fill, implements string) []string {
	contract := strings.TrimSuffix(check.Module, "/") + "/out/slots"
	switch implements {
	case querySlot:
		if check.QueryContract != "" {
			contract = check.QueryContract
		}
	case commandSlot:
		for _, f := range check.CommandFills {
			if f.Fill == fill {
				contract = f.Contract
			}
		}
	case workerSlot:
		if check.WorkerContract != "" {
			contract = check.WorkerContract
		}
	}
	// The runtime packages come from the generator of the contract (FillCheck.ContractRuntime), not from the
	// generated folder, so the check is the same before the first generation and after out/ is removed.
	return append([]string{contract}, check.ContractRuntime[contract]...)
}

// checkImports refuses a fill that breaks the fill rules (ST-EXT-INV05, ST-EXT-UC01-R03, ST-EXT-UC01-R05,
// ST-EXT-UC01-R07): a Go file that is built into the service, or a package that the fill reaches through a listed fill
// module, that imports anything but the contract package of its slot and the runtime packages that the contract
// reaches (slotContracts), a listed fill module or a package of the fill allow list of the standard library, that
// holds a file that is not Go source, a function without a body or a refused directive, or that uses a refused
// function. A test file, whose name ends in _test.go, is not built into the service, so it may import any module of
// go.mod. The fault names the file and the import, the file or the call.
func checkImports(dir string, check compiler.FillCheck, fill, implements string) error {
	packages, err := listFills(check, []string{fill})
	if err != nil {
		return err
	}
	faults, err := fillFaults(check, dir, fill, slotContracts(check, fill, implements), packages)
	if err != nil {
		return err
	}
	if len(faults) == 0 {
		return nil
	}
	texts := make([]string, len(faults))
	for i, fault := range faults {
		texts[i] = fault.text
	}
	return errors.New(strings.Join(texts, "; "))
}

// standardLibrary tells if an import path is a package of the Go standard
// library: its first element has no dot.
func standardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
