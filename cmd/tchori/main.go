// Package main implements the tchori CLI: an agent-native everything-as-code
// engine. Exit codes follow the Terraform convention agents already know:
// 0 = success / no changes, 2 = plan has changes, 1 = error.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tchori-labs/tchori/internal/diag"
)

// Global flag values, bound to the root command's persistent flag set.
var (
	flagChdir     string
	flagJSON      bool
	flagNoColor   bool // accepted for compatibility; tchori's MVP output is colorless
	flagPluginDir string
)

// prettyStderr reports whether diagnostics render human-readable: stderr is
// a TTY and -json is absent. Otherwise diagnostics are one JSON object per
// line — the agent retry loop.
func prettyStderr() bool {
	return term.IsTerminal(int(os.Stderr.Fd())) && !flagJSON
}

// emitDiags writes ds to stderr in the mode selected by prettyStderr.
func emitDiags(ds diag.Diagnostics) {
	if len(ds) == 0 {
		return
	}
	diag.Emit(os.Stderr, ds, prettyStderr())
}

type commandExit struct {
	code int
	err  error
}

func (e *commandExit) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

// exitRun adapts a run() (int, error) command body to cobra's RunE. It returns
// control to main so command and signal cleanup defers run before os.Exit.
func exitRun(run func(cmd *cobra.Command, args []string) (int, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		code, err := run(cmd, args)
		if code == 0 && err != nil {
			code = 1
		}
		if code == 0 {
			return nil
		}
		return &commandExit{code: code, err: err}
	}
}

// normalizeArgs rewrites Terraform-style single-dash long flags (-chdir,
// -json, -out, -refresh=false, ...) to the double-dash form pflag
// understands. Single-character flags and everything after a bare "--" are
// left untouched. tchori has no negative-number arguments, so any
// multi-character token starting with exactly one dash is a flag.
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			out = append(out, "-"+a)
			continue
		}
		out = append(out, a)
	}
	return out
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tchori",
		Short:         "tchori is an agent-native everything-as-code engine",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if flagChdir != "" {
				if err := os.Chdir(flagChdir); err != nil {
					return fmt.Errorf("-chdir: %w", err)
				}
			}
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flagChdir, "chdir", "", "switch to this directory before running the command")
	pf.BoolVar(&flagJSON, "json", false, "force machine-readable output even on a TTY")
	pf.BoolVar(&flagNoColor, "no-color", false, "disable color output (tchori MVP output is already colorless)")
	pf.StringVar(&flagPluginDir, "plugin-dir", "", "directory searched first for provider binaries (local development); resolved after -chdir when relative")

	root.AddCommand(
		newValidateCmd(),
		newPlanCmd(),
		newApplyCmd(),
		newDestroyCmd(),
		newImportCmd(),
		newStateCmd(),
		newProvidersCmd(),
		newMCPCmd(),
		newVersionCmd(),
	)
	return root
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stop()
	go func() {
		<-ctx.Done()
		// The first termination signal requests graceful cancellation. Restore
		// default handling immediately so a second signal hard-terminates a
		// provider or filesystem cleanup that does not return.
		stop()
	}()

	root := newRootCmd()
	root.SetArgs(normalizeArgs(os.Args[1:]))
	if err := root.ExecuteContext(ctx); err != nil {
		var exit *commandExit
		if errors.As(err, &exit) {
			if exit.err != nil {
				emitDiags(diag.Diagnostics{diag.Errorf("", exit.err.Error(), "")})
			}
			return exit.code
		}
		emitDiags(diag.Diagnostics{diag.Errorf("", err.Error(), `run "tchori --help" for usage`)})
		return 1
	}
	return 0
}

func main() {
	os.Exit(runMain())
}
