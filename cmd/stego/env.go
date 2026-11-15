package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jsell-rh/stego/internal/delivery"
)

// runEnv parses stego env check (ST-DLV-CLI02) and its flags, reads the platform requirements file and the
// environment file, and prints every platform requirement that the environment does not meet (ST-DLV-UC02). The
// check reads secret names and keys only and changes no file. A file that cannot be read is a failed check.
func runEnv(args []string) error {
	return runEnvTo(args, os.Stdout)
}

func runEnvTo(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printEnvUsage()
		if len(args) == 0 {
			return errors.New("name a subcommand: check")
		}
		return nil
	}
	if args[0] != "check" {
		printEnvUsage()
		return fmt.Errorf("unknown env subcommand: %s", args[0])
	}
	fs := flag.NewFlagSet("env check", flag.ContinueOnError)
	requirementsPath := fs.String("requirements", "", "the service's platform requirements file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *requirementsPath == "" {
		return errors.New("--requirements is missing: name the service's platform requirements file")
	}
	if fs.NArg() != 1 {
		return errors.New("name one environment file to check")
	}
	environmentPath := fs.Arg(0)
	requirementsData, err := readCheckedFile(*requirementsPath, delivery.MaxRequirementsBytes)
	if err != nil {
		return fmt.Errorf("the check failed: platform requirements file %s: %w", *requirementsPath, err)
	}
	requirements, err := delivery.LoadRequirements(requirementsData)
	if err != nil {
		return fmt.Errorf("the check failed: platform requirements file %s: %w", *requirementsPath, err)
	}
	environmentData, err := readCheckedFile(environmentPath, delivery.MaxEnvironmentBytes)
	if err != nil {
		return fmt.Errorf("the check failed: environment file %s: %w", environmentPath, err)
	}
	fit, err := delivery.CheckEnvironment(requirements, environmentData)
	if err != nil {
		return fmt.Errorf("the check failed: environment file %s: %w", environmentPath, err)
	}
	reader := delivery.NewEnvironmentReader(fit)
	unmet := delivery.UnmetRequirements(reader)
	for _, row := range unmet {
		fmt.Fprintf(stdout, "%s: %s: %s\n", row.Status, row.Requirement, row.Detail)
	}
	for _, value := range reader.Fit().Unused {
		fmt.Fprintf(stdout, "unused: %s: no platform requirement names it\n", value)
	}
	if len(unmet) > 0 {
		return fmt.Errorf("the environment %s does not meet the platform requirements of the service %s: %d unmet", fit.Environment, fit.Service, len(unmet))
	}
	fmt.Fprintf(stdout, "The environment %s meets each platform requirement of the service %s (%d requirements).\n",
		fit.Environment, fit.Service, len(requirements.Requirements))
	return nil
}

// readCheckedFile reads a file of at most limit bytes and refuses a larger one before it reads it all.
func readCheckedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return data, nil
}

func printEnvUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego env check --requirements <platform-requirements.yaml> <environment-file>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Checks an environment file against the service's platform requirements. Prints every")
	fmt.Fprintln(os.Stderr, "requirement that the environment does not meet and exits non-zero when one is unmet.")
}
