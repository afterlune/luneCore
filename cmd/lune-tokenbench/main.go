package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := runCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lune-tokenbench run|compare [options]")
	}
	switch args[0] {
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		configPath := flags.String("config", "config.json", "path to the project config.json")
		casesPath := flags.String("cases", "testdata/tokenbench/cases.json", "path to the synthetic case file")
		profile := flags.String("profile", "", "model profile to evaluate; defaults to config defaultModel")
		outputPath := flags.String("out", "tokenbench-report.json", "report output path")
		repeat := flags.Int("repeat", 1, "runs per case")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *repeat < 1 || *repeat > 20 {
			return errors.New("repeat must be between 1 and 20")
		}
		return runBenchmark(*configPath, *casesPath, *profile, *outputPath, *repeat)
	case "compare":
		flags := flag.NewFlagSet("compare", flag.ContinueOnError)
		baselinePath := flags.String("baseline", "", "baseline report JSON")
		candidatePath := flags.String("candidate", "", "candidate report JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *baselinePath == "" || *candidatePath == "" {
			return errors.New("compare requires --baseline and --candidate")
		}
		return compareReports(*baselinePath, *candidatePath, os.Stdout)
	default:
		return fmt.Errorf("unknown command %q; use run or compare", args[0])
	}
}
