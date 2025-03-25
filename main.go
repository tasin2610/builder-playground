package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/ferranbt/builder-playground/internal"
	"github.com/spf13/cobra"
)

var outputFlag string
var genesisDelayFlag uint64
var withOverrides []string
var watchdog bool
var dryRun bool
var interactive bool
var timeout time.Duration
var logLevelFlag string

var rootCmd = &cobra.Command{
	Use:   "playground",
	Short: "",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

var cookCmd = &cobra.Command{
	Use:   "cook",
	Short: "Cook a recipe",
	RunE: func(cmd *cobra.Command, args []string) error {
		recipeNames := []string{}
		for _, recipe := range recipes {
			recipeNames = append(recipeNames, recipe.Name())
		}
		return fmt.Errorf("please specify a recipe to cook. Available recipes: %s", recipeNames)
	},
}

var artifactsCmd = &cobra.Command{
	Use:   "artifacts",
	Short: "List available artifacts",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("please specify a service name")
		}
		serviceName := args[0]
		component := internal.FindComponent(serviceName)
		if component == nil {
			return fmt.Errorf("service %s not found", serviceName)
		}
		releaseService, ok := component.(internal.ReleaseService)
		if !ok {
			return fmt.Errorf("service %s is not a release service", serviceName)
		}
		output := outputFlag
		if output == "" {
			homeDir, err := internal.GetHomeDir()
			if err != nil {
				return fmt.Errorf("failed to get home directory: %w", err)
			}
			output = homeDir
		}
		location, err := internal.DownloadRelease(output, releaseService.ReleaseArtifact())
		if err != nil {
			return fmt.Errorf("failed to download release: %w", err)
		}
		fmt.Println(location)
		return nil
	},
}

var recipes = []internal.Recipe{
	&internal.L1Recipe{},
	&internal.OpRecipe{},
}

func main() {
	for _, recipe := range recipes {
		recipeCmd := &cobra.Command{
			Use:   recipe.Name(),
			Short: recipe.Description(),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runIt(recipe)
			},
		}
		// add the flags from the recipe
		recipeCmd.Flags().AddFlagSet(recipe.Flags())
		// add the common flags
		recipeCmd.Flags().StringVar(&outputFlag, "output", "", "Output folder for the artifacts")
		recipeCmd.Flags().BoolVar(&watchdog, "watchdog", false, "enable watchdog")
		recipeCmd.Flags().StringArrayVar(&withOverrides, "override", []string{}, "override a service's config")
		recipeCmd.Flags().BoolVar(&dryRun, "dry-run", false, "dry run the recipe")
		recipeCmd.Flags().BoolVar(&dryRun, "mise-en-place", false, "mise en place mode")
		recipeCmd.Flags().Uint64Var(&genesisDelayFlag, "genesis-delay", internal.MinimumGenesisDelay, "")
		recipeCmd.Flags().BoolVar(&interactive, "interactive", false, "interactive mode")
		recipeCmd.Flags().DurationVar(&timeout, "timeout", 0, "") // Used for CI
		recipeCmd.Flags().StringVar(&logLevelFlag, "log-level", "info", "log level")

		cookCmd.AddCommand(recipeCmd)
	}

	// reuse the same output flag for the artifacts command
	artifactsCmd.Flags().StringVar(&outputFlag, "output", "", "Output folder for the artifacts")

	rootCmd.AddCommand(cookCmd)
	rootCmd.AddCommand(artifactsCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func runIt(recipe internal.Recipe) error {
	var logLevel internal.LogLevel
	if err := logLevel.Unmarshal(logLevelFlag); err != nil {
		return fmt.Errorf("failed to parse log level: %w", err)
	}

	log.Printf("Log level: %s\n", logLevel)

	builder := recipe.Artifacts()
	builder.OutputDir(outputFlag)
	builder.GenesisDelay(genesisDelayFlag)
	artifacts, err := builder.Build()
	if err != nil {
		return err
	}

	svcManager := recipe.Apply(&internal.ExContext{LogLevel: logLevel}, artifacts)
	if err := svcManager.Validate(); err != nil {
		return fmt.Errorf("failed to validate manifest: %w", err)
	}

	// generate the dot graph
	dotGraph := svcManager.GenerateDotGraph()
	if err := artifacts.Out.WriteFile("graph.dot", dotGraph); err != nil {
		return err
	}

	if dryRun {
		return nil
	}

	dockerRunner, err := internal.NewLocalRunner(artifacts.Out, svcManager, nil, interactive)
	if err != nil {
		return fmt.Errorf("failed to create docker runner: %w", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-sig
		cancel()
	}()

	if err := dockerRunner.Run(); err != nil {
		dockerRunner.Stop()
		return fmt.Errorf("failed to run docker: %w", err)
	}

	if !interactive {
		// print services info
		fmt.Printf("\n========= Services started =========\n")
		for _, ss := range svcManager.Services() {
			ports := ss.Ports()
			sort.Slice(ports, func(i, j int) bool {
				return ports[i].Name < ports[j].Name
			})

			portsStr := []string{}
			for _, p := range ports {
				portsStr = append(portsStr, fmt.Sprintf("%s: %d/%d", p.Name, p.Port, p.HostPort))
			}
			fmt.Printf("- %s (%s)\n", ss.Name, strings.Join(portsStr, ", "))
		}
	}

	if err := internal.WaitForReady(ctx, svcManager); err != nil {
		dockerRunner.Stop()
		return fmt.Errorf("failed to wait for service readiness: %w", err)
	}

	// get the output from the recipe
	output := recipe.Output(svcManager)
	if len(output) > 0 {
		fmt.Printf("\n========= Output =========\n")
		for k, v := range output {
			fmt.Printf("- %s: %v\n", k, v)
		}
	}

	watchdogErr := make(chan error, 1)
	if watchdog {
		go func() {
			if err := internal.RunWatchdog(svcManager); err != nil {
				watchdogErr <- fmt.Errorf("watchdog failed: %w", err)
			}
		}()
	}

	var timerCh <-chan time.Time
	if timeout > 0 {
		timerCh = time.After(timeout)
	}

	select {
	case <-ctx.Done():
		fmt.Println("Stopping...")
	case err := <-dockerRunner.ExitErr():
		fmt.Println("Service failed:", err)
	case err := <-watchdogErr:
		fmt.Println("Watchdog failed:", err)
	case <-timerCh:
		fmt.Println("Timeout reached")
	}

	if err := dockerRunner.Stop(); err != nil {
		return fmt.Errorf("failed to stop docker: %w", err)
	}
	return nil
}
