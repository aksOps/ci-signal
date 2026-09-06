package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ci-signal/internal/config"
	"ci-signal/internal/coordinator"
	"ci-signal/internal/review"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr, coordinator.NewProduction))
}

type coordinatorFactory func(config.Loaded, *slog.Logger) (*coordinator.Coordinator, error)

func run(ctx context.Context, args []string, lookup config.LookupEnv, stdout, stderr io.Writer, factory coordinatorFactory) int {
	loaded, err := load(args, lookup, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cliLogLevel(loaded.Config.Diagnostics.LogLevel)}))
	application, err := factory(loaded, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	result, err := application.Run(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Verdict    review.Verdict              `json:"verdict"`
		Completion review.SubmissionCompletion `json:"completion"`
		UsedAI     bool                        `json:"used_ai"`
		Published  bool                        `json:"published"`
	}{result.Verdict, result.Completion, result.UsedAI, result.Published}); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	switch loaded.Config.Review.ExitPolicy {
	case config.ExitOnNeedsReview:
		if result.Verdict == review.VerdictNeedsReview {
			return 1
		}
	case config.ExitOnIncomplete:
		if result.Completion != review.SubmissionComplete {
			return 1
		}
	}
	return 0
}

func load(args []string, lookup config.LookupEnv, stderr io.Writer) (config.Loaded, error) {
	flags := flag.NewFlagSet("ci-signal", flag.ContinueOnError)
	flags.SetOutput(stderr)
	logLevel := flags.String("log-level", "", "diagnostic level: debug, info, warn, or error")
	logFile := flags.String("log-file", "", "absolute diagnostics log path")
	logToolCalls := flags.Bool("log-tool-calls", false, "log redacted tool arguments")
	logToolResults := flags.Bool("log-tool-results", false, "log redacted tool results")
	logReasoning := flags.Bool("log-reasoning", false, "log reasoning exposed by the runtime")
	dryRun := flags.Bool("dry-run", false, "run review without GitLab mutations")
	if err := flags.Parse(args); err != nil {
		return config.Loaded{}, err
	}
	if flags.NArg() != 0 {
		return config.Loaded{}, errors.New("ci-signal accepts flags only")
	}
	loaded, err := config.LoadFromEnvironment(lookup)
	if err != nil {
		return config.Loaded{}, err
	}
	set := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { set[value.Name] = true })
	if set["log-level"] {
		loaded.Config.Diagnostics.LogLevel = config.LogLevel(*logLevel)
	}
	if set["log-file"] {
		loaded.Config.Diagnostics.LogFile = *logFile
	}
	if set["log-tool-calls"] {
		loaded.Config.Diagnostics.LogToolCalls = *logToolCalls
	}
	if set["log-tool-results"] {
		loaded.Config.Diagnostics.LogToolResults = *logToolResults
	}
	if set["log-reasoning"] {
		loaded.Config.Diagnostics.LogReasoning = *logReasoning
	}
	if set["dry-run"] {
		loaded.Config.Diagnostics.DryRun = *dryRun
	}
	if err := loaded.Config.Validate(); err != nil {
		return config.Loaded{}, err
	}
	return loaded, nil
}

func cliLogLevel(level config.LogLevel) slog.Level {
	switch level {
	case config.LogDebug:
		return slog.LevelDebug
	case config.LogWarn:
		return slog.LevelWarn
	case config.LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
