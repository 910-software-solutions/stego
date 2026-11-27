package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/910-software-solutions/stego/internal/delivery"
)

// runEnv parses stego env check (ST-DLV-CLI02) and its flags, reads the platform requirements file and the
// environment file, and prints every platform requirement that the environment does not meet (ST-DLV-UC02). The
// check reads secret names and keys only and changes no file. A file that cannot be read is a failed check.
func runEnv(args []string) error {
	// An error can hold a key name of the environment file: it is escaped too (#344 security review L2).
	if err := runEnvTo(args, os.Stdout); err != nil {
		return errors.New(printable(err.Error()))
	}
	return nil
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
	fs.Usage = printEnvUsage
	requirementsPath := fs.String("requirements", "", "the service's platform requirements file")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
	// Each text that comes from a file is printed escaped, so that a file author cannot forge a line with a control
	// or a bidirectional character (#338 security finding F1).
	for _, row := range unmet {
		fmt.Fprintf(stdout, "%s: %s: %s\n", row.Status, printable(row.Requirement), printable(row.Detail))
	}
	for _, notice := range reader.Fit().Notices {
		fmt.Fprintf(stdout, "notice: %s\n", printable(notice))
	}
	for _, value := range reader.Fit().Unused {
		fmt.Fprintf(stdout, "unused: %s: no platform requirement and no listed environment variable names it\n", printable(value))
	}
	if len(unmet) > 0 {
		return fmt.Errorf("the environment %s does not meet the platform requirements of the service %s: %d unmet", printable(fit.Environment), printable(fit.Service), len(unmet))
	}
	fmt.Fprintf(stdout, "The environment %s meets each platform requirement of the service %s (%d requirements, %d environment variables).\n",
		printable(fit.Environment), printable(fit.Service), len(requirements.Requirements), len(requirements.Environment))
	return nil
}

// printable escapes each control, format and bidirectional character of a text as \u{XXXX}, so that the text
// shows as one plain line.
func printable(text string) string {
	var out strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == utf8.RuneError {
			fmt.Fprintf(&out, "\\u{%04X}", r)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// readCheckedFile reads a regular file of at most limit bytes and refuses a larger one before it reads it all. It
// refuses a file that is not regular, such as a pipe or a device, and opens it without blocking (#338 F7).
func readCheckedFile(path string, limit int) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the file is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return data, nil
}

func printEnvUsage() { fmt.Fprint(os.Stderr, envUsage) }

// envUsage is the help of stego env check: the command, the schema of the environment file and one example
// (docs/environment-file.md has the full text).
const envUsage = `Usage: stego env check --requirements <platform-requirements.yaml> <environment-file>

Checks an environment file against the service's platform requirements, which stego apply writes to
deploy/platform-requirements.yaml. Prints every requirement that the environment does not meet, each value of
the wrong type and each value that no requirement names, and exits non-zero when one requirement is unmet or one
value has the wrong type. The check reads the key names of a secret, never a secret value, and changes no file.

The environment file is one YAML map with these fields (docs/environment-file.md):
  version              the schema version, 1
  service              the service of the platform requirements
  environment          the name of the environment
  image                the image by digest: repository@sha256:<digest>
  destination          {cluster, namespace, overlay_path}
  secrets              name: {key: value, ...}; only the key names are read
  configuration        variable: value; each value has the type, allowed values and range of its variable
  backing_stores       name: {engine}
  topics               name: {topic}
  identity_issuers     name: {issuer}, an HTTPS URL
  ingresses            name: {port, host}
  egresses             name: {address, port}; the address is a host name or an IP address
  cluster_permissions  name: {api_group, resource, verbs}
  pod_security         name: {level}: privileged, baseline or restricted

Example:
  version: 1
  service: tasks
  environment: staging
  image: quay.io/example/tasks@sha256:3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a
  destination: {cluster: https://api.staging.example.com:6443, namespace: tasks-staging, overlay_path: overlays/staging}
  secrets:
    tasks-database: {url: <secret>, migration-url: <secret>, worker-url: <secret>}
  configuration:
    PORT: 8080
    STEGO_SPICEDB_ENDPOINT: spicedb.staging.example.com:50051
  backing_stores:
    tasks-db: {engine: postgres}
  egresses:
    spicedb: {address: spicedb.staging.example.com, port: 50051}
`
