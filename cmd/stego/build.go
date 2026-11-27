package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/910-software-solutions/stego/internal/appbuild"
	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/fill"
	"github.com/910-software-solutions/stego/internal/parser"
)

func runBuild(args []string) error {
	if len(args) > 0 && args[0] == "verify-source" {
		flags := flag.NewFlagSet("build verify-source", flag.ContinueOnError)
		record := flags.String("record", "", "build record file")
		expected := flags.String("record-sha256", "", "build record digest from verified provenance")
		source := flags.String("source", "", "complete source snapshot without Git metadata or build caches")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *record == "" || *expected == "" || *source == "" {
			return fmt.Errorf("build verify-source requires --record, --record-sha256, and --source")
		}
		_, err := appbuild.VerifySource(*record, *expected, *source)
		return err
	}
	if len(args) > 0 && args[0] == "download" {
		flags := flag.NewFlagSet("build download", flag.ContinueOnError)
		var options appbuild.DownloadOptions
		flags.StringVar(&options.Source, "source", "", "Git source repository")
		flags.StringVar(&options.Revision, "revision", "", "full source commit ID")
		flags.StringVar(&options.Module, "module", ".", "module path within the source repository")
		flags.StringVar(&options.Target, "target", "", "entry point path within the module")
		flags.StringVar(&options.Go, "go", "", "bin/go in the selected official SDK")
		flags.StringVar(&options.Output, "output", "", "new module cache directory")
		flags.StringVar(&options.RuntimeFolder, "runtime-folder", "", "registry/runtime of a STEGO checkout, for a source with no runtime folder; it must hash to the runtime pin")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || options.Source == "" || options.Revision == "" || options.Target == "" || options.Go == "" || options.Output == "" {
			return fmt.Errorf("build download requires --source, --revision, --target, --go, and --output")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		record, err := appbuild.Download(ctx, options)
		if err != nil {
			return err
		}
		// A download that ends with no output looks the same as one that did nothing (walkthrough re-run, 2026-11-14).
		fmt.Printf("Downloaded the module cache of revision %s into %s: %d module(s), recorded in %s.\n", record.SourceRevision,
			options.Output, len(record.Modules), filepath.Join(options.Output, "module-cache.json"))
		return nil
	}
	if len(args) > 0 && args[0] == "verify" {
		flags := flag.NewFlagSet("build verify", flag.ContinueOnError)
		record := flags.String("record", "", "build record file")
		artifact := flags.String("artifact", "", "application executable")
		expected := flags.String("record-sha256", "", "build record digest from verified provenance")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *record == "" || *artifact == "" || *expected == "" {
			return fmt.Errorf("build verify requires --record, --artifact, and --record-sha256")
		}
		_, err := appbuild.Verify(*record, *artifact, *expected)
		return err
	}
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	var options appbuild.Options
	flags.StringVar(&options.Source, "source", "", "Git source repository")
	flags.StringVar(&options.Revision, "revision", "", "full source commit ID")
	flags.StringVar(&options.Module, "module", ".", "module path within the source repository")
	flags.StringVar(&options.Target, "target", "", "entry point path within the module")
	flags.StringVar(&options.Go, "go", "", "bin/go in the selected official SDK")
	flags.StringVar(&options.Work, "work", "", "new private build directory")
	flags.StringVar(&options.Output, "output", "", "new result directory")
	flags.StringVar(&options.ModuleCache, "module-cache", "", "module cache that stego build download recorded; required, since a build runs offline")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("build accepts no positional arguments")
	}
	if options.ModuleCache == "" {
		return fmt.Errorf("build requires --module-cache: run stego build download first; a build never downloads modules")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options.CheckSource = checkBuildFills
	if _, err := appbuild.Build(ctx, options); err != nil {
		return err
	}
	// The build raises Application built (ST-BLD-EV03); stego image build
	// hears it with --after (ST-BLD-PO01).
	event, err := appbuild.ApplicationBuiltIn(options.Output)
	if err != nil {
		return err
	}
	return printEvent(event)
}

// checkBuildFills refuses a build of the service in module, a copy of the source revision, when a fill that its
// declaration binds is not qualified as the revision holds it: it has no evidence, its files changed after its
// evidence, or a key that SIGNERS lists did not sign that evidence. Plan and apply stop at such a fill
// (ST-GEN-INV10), and so does the build, so that no unqualified fill reaches a binary, also one written after an
// apply that wrote its contract (decision on ST-EXT-Q30). The check reads only the revision: no registry and no
// network, as the build runs offline; plan and apply check each contract digest, which only apply changes. Before
// the fills, it checks the generated folder once, also for a service that binds only slot fills or no fill
// (compiler.CheckGeneratedTree).
func checkBuildFills(module string) error {
	// A module that is no STEGO service builds no generated code.
	if _, err := os.Lstat(filepath.Join(module, "service.yaml")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	state, err := compiler.LoadState(filepath.Join(module, ".stego", "state.yaml"))
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if state.LastApplied == nil || state.LastApplied.Inputs == nil {
		return errors.New("build: the source revision holds no complete generation state, so stego build cannot check its generated folder")
	}
	if err := compiler.CheckGeneratedTree(module, state.LastApplied.Inputs.Options.OutputDir); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	// A module that holds no fill builds no fill code.
	if _, err := os.Lstat(filepath.Join(module, "fills")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	service, err := parser.ParseServiceDeclaration(filepath.Join(module, "service.yaml"))
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	var fills []string
	for _, binding := range service.Slots {
		fills = append(fills, binding.Gate...)
		fills = append(fills, binding.Chain...)
		fills = append(fills, binding.FanOut...)
	}
	for _, c := range service.Commands {
		fills = append(fills, c.Fill...)
	}
	for _, q := range service.Queries {
		fills = append(fills, q.Fill...)
	}
	for _, w := range service.Workers {
		fills = append(fills, w.Fill...)
	}
	moduleName, _, err := compiler.ProjectModuleSettings(module, os.Getenv("STEGO_MODULE"), os.Getenv("STEGO_GO_VERSION"))
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	faults, err := fill.RevisionFaults(module, fills, compiler.RevisionFillCheck(module, moduleName, service))
	if err != nil {
		return err
	}
	if len(faults) == 0 {
		return nil
	}
	return fmt.Errorf("build: the source revision binds fills that are not qualified, so nothing is built:\n  %s", strings.Join(faults, "\n  "))
}
