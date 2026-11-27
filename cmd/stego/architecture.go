package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/910-software-solutions/stego/internal/compiler"
)

// architectureUsage is the exit status of stego architecture for a usage error: a subcommand, flag, format, view or
// topic that is not known.
const architectureUsage = 2

// runArchitecture implements stego architecture export (ST-GEN-CLI08) and stego architecture facts (ST-GEN-CLI09).
// Each reads the architecture model of the service in the working folder through compiler.ReadArchitecture, prints
// what it asks for on standard output, and writes no file. A model that is missing, does not match the record or is
// not valid is refused with the file and the cause, and nothing goes to standard output.
func runArchitecture(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return exitStatus{architectureUsage, "architecture takes a subcommand: export or facts"}
	}
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	var output []byte
	switch args[0] {
	case "export":
		output, err = runArchitectureExport(projectDir, args[1:])
	case "facts":
		output, err = runArchitectureFacts(projectDir, args[1:])
	case "--help", "-h":
		fmt.Fprintln(os.Stderr, "Usage: stego architecture <subcommand>")
		fmt.Fprintln(os.Stderr, "  export --format "+strings.Join(compiler.ExportFormats, "|")+" [--view <view>]")
		fmt.Fprintln(os.Stderr, "  facts --topic "+strings.Join(compiler.ComplianceTopics, "|")+" [--json]")
		return nil
	default:
		return exitStatus{architectureUsage, fmt.Sprintf("architecture has no subcommand %q; the subcommands are export and facts", args[0])}
	}
	var usage *compiler.ArchitectureUsageError
	if errors.As(err, &usage) {
		return exitStatus{architectureUsage, usage.Error()}
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(output)
	return err
}

// runArchitectureExport parses stego architecture export and gives the export of ST-GEN-QRY05.
func runArchitectureExport(projectDir string, args []string) ([]byte, error) {
	fs := flag.NewFlagSet("architecture export", flag.ContinueOnError)
	format := fs.String("format", "", "the format: "+strings.Join(compiler.ExportFormats, " or "))
	view := fs.String("view", "", "one view, such as one request flow")
	if err := parseArchitectureFlags(fs, args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 || *format == "" {
		return nil, exitStatus{architectureUsage, "architecture export takes --format and no argument: stego architecture export --format " + strings.Join(compiler.ExportFormats, "|") + " [--view <view>]"}
	}
	return compiler.ExportArchitecture(projectDir, *format, *view)
}

// runArchitectureFacts parses stego architecture facts and gives the list of ST-GEN-QRY06.
func runArchitectureFacts(projectDir string, args []string) ([]byte, error) {
	fs := flag.NewFlagSet("architecture facts", flag.ContinueOnError)
	topic := fs.String("topic", "", "the topic: "+strings.Join(compiler.ComplianceTopics, ", "))
	structured := fs.Bool("json", false, "print the facts as one JSON object")
	if err := parseArchitectureFlags(fs, args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 || *topic == "" {
		return nil, exitStatus{architectureUsage, "architecture facts takes --topic and no argument: stego architecture facts --topic " + strings.Join(compiler.ComplianceTopics, "|") + " [--json]"}
	}
	return compiler.ListComplianceFacts(projectDir, *topic, *structured)
}

// parseArchitectureFlags parses the flags of a subcommand. The flag package does not print the error with the usage, so
// main prints a usage error once; --help prints the flags.
func parseArchitectureFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(os.Stderr)
		fs.PrintDefaults()
		return err
	}
	if err != nil {
		return exitStatus{architectureUsage, err.Error()}
	}
	return nil
}
