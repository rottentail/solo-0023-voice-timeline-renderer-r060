package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/audit"
	"voice-timeline-renderer/internal/auditdemo"
)

func main() {
	var (
		outDir   = flag.String("out", "./var/audio-audit", "directory that will hold the audit records")
		scenario = flag.String("scenario", "", "run one scenario instead of the full demonstration")
		verbose  = flag.Bool("verbose", false, "print report paths and findings")
	)
	flag.Parse()
	if err := run(context.Background(), *outDir, *scenario, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "audio audit demonstration failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, outDir, scenario string, verbose bool) error {
	root, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	store, err := audit.NewStore(root)
	if err != nil {
		return err
	}
	runner, err := auditdemo.NewRunner(ctx, root)
	if err != nil {
		return err
	}
	defer runner.Close()

	delivery, err := runner.Baseline(ctx)
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("historical delivery: digest=%s frames=%d peak=%.3f dBFS\n",
			delivery.Digest, delivery.Frames, delivery.PeakDBFS)
	}

	names := auditdemo.ScenarioNames()
	if scenario != "" {
		names = []string{scenario}
	}
	failed := false
	for _, name := range names {
		spec, ok := auditdemo.Scenarios()[name]
		if !ok {
			return fmt.Errorf("unknown scenario %q", name)
		}
		if err := auditdemo.RunScenario(ctx, store, runner, delivery, spec, verbose); err != nil {
			failed = true
			fmt.Printf("%-24s ERROR %v\n", name, err)
		}
	}
	if failed {
		return fmt.Errorf("at least one scenario did not reach its required outcome")
	}
	return nil
}

var _ = os.Stdout
