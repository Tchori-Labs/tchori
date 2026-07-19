package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/term"

	"github.com/tchori-labs/tchori/internal/apply"
	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/mcpserv"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/registry"
	"github.com/tchori-labs/tchori/internal/state"
	"github.com/tchori-labs/tchori/internal/version"
)

// --- validate ----------------------------------------------------------------

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration: schema, references, cycles, provider resource checks",
		Args:  cobra.NoArgs,
		RunE:  exitRun(runValidate),
	}
}

func runValidate(cmd *cobra.Command, _ []string) (int, error) {
	ctx := cmd.Context()

	rt, cleanup, ds := buildRuntime(ctx, flagPluginDir)
	emitDiags(ds)
	if ds.HasErrors() {
		return 1, nil
	}
	defer cleanup()

	order, ods := rt.Config.Order()
	emitDiags(ods)
	if ods.HasErrors() {
		return 1, nil
	}

	// Before planning there are no resolved values, so references compose as
	// unknowns — Compose converts them to the attribute's type, and providers
	// must tolerate unknowns in ValidateResourceConfig.
	unknownRef := func(config.Ref) (cty.Value, diag.Diagnostics) {
		return cty.UnknownVal(cty.DynamicPseudoType), nil
	}

	failed := false
	for _, addr := range order {
		r := rt.Config.Resources[addr]
		schema, unsupported, known := rt.Schemas[r.Provider].LookupResourceType(r.Type)
		if !known {
			emitDiags(diag.Diagnostics{diag.Errorf(addr, fmt.Sprintf("unknown resource type %q", r.Type),
				fmt.Sprintf("provider %q does not define resource type %q", r.Provider, r.Type))})
			failed = true
			continue
		}
		if schema == nil {
			emitDiags(diag.Diagnostics{diag.Errorf(addr,
				fmt.Sprintf("unsupported schema for resource type %q", r.Type), unsupported)})
			failed = true
			continue
		}
		cv, cds := provider.Compose(r.Config, schema.Block.ImpliedType(), false, unknownRef)
		emitDiags(cds)
		if cds.HasErrors() {
			failed = true
			continue
		}
		vds := rt.Providers[r.Provider].ValidateResource(ctx, r.Type, cv)
		emitDiags(vds)
		if vds.HasErrors() {
			failed = true
		}
	}
	if failed {
		return 1, nil
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Configuration is valid.")
	return 0, nil
}

// --- plan --------------------------------------------------------------------

func newPlanCmd() *cobra.Command {
	var out string
	var refresh bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Compute changes; exit 2 when changes are pending, 0 when none",
		Args:  cobra.NoArgs,
		RunE: exitRun(func(cmd *cobra.Command, _ []string) (int, error) {
			return runPlan(cmd, out, refresh)
		}),
	}
	cmd.Flags().StringVar(&out, "out", "", "write the plan document to this file")
	cmd.Flags().BoolVar(&refresh, "refresh", true, "refresh state via ReadResource before diffing")
	return cmd
}

func runPlan(cmd *cobra.Command, out string, refresh bool) (int, error) {
	ctx := cmd.Context()

	rt, cleanup, ds := buildRuntime(ctx, flagPluginDir)
	emitDiags(ds)
	if ds.HasErrors() {
		return 1, nil
	}
	defer cleanup()

	st, err := state.Load(stateFileName)
	if err != nil {
		return 1, err
	}

	planner := &plan.Planner{
		Config:        rt.Config,
		State:         st,
		Providers:     rt.Providers,
		Schemas:       rt.Schemas,
		EngineVersion: version.Version,
		Refresh:       refresh,
	}
	pl, pds := planner.Plan(ctx)
	emitDiags(pds)
	if pds.HasErrors() {
		return 1, nil
	}

	if out != "" {
		if err := plan.Write(pl, out); err != nil {
			return 1, fmt.Errorf("writing plan to %s: %s", out, err)
		}
	}
	if err := writePlanOutput(cmd.OutOrStdout(), pl); err != nil {
		return 1, err
	}
	if pl.HasChanges() {
		return 2, nil
	}
	return 0, nil
}

// writePlanOutput prints the plan document as JSON when -json is set, else a
// human summary: one line per non-no-op change and a totals line.
func writePlanOutput(w io.Writer, pl *plan.Plan) error {
	if flagJSON {
		b, err := json.MarshalIndent(pl, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = w.Write(b)
		return err
	}
	if !pl.HasChanges() {
		_, _ = fmt.Fprintln(w, "No changes. Configuration matches state.")
		return nil
	}
	for _, c := range pl.Changes {
		if c.Action == "no-op" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s %s\n", actionSymbol(c.Action), c.Address)
	}
	s := pl.Summary
	_, _ = fmt.Fprintf(w, "Plan: %d to create, %d to update, %d to delete, %d to replace.\n",
		s.Create, s.Update, s.Delete, s.Replace)
	return nil
}

func actionSymbol(action string) string {
	switch action {
	case "create":
		return "+"
	case "update":
		return "~"
	case "delete":
		return "-"
	case "replace":
		return "-/+"
	default:
		return " "
	}
}

// --- apply -------------------------------------------------------------------

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply PLANFILE",
		Short: "Execute a saved plan file (there is no plan-less apply)",
		Args:  cobra.ExactArgs(1),
		RunE:  exitRun(runApply),
	}
}

func runApply(cmd *cobra.Command, args []string) (int, error) {
	ctx := cmd.Context()

	pl, err := plan.Read(args[0])
	if err != nil {
		return 1, fmt.Errorf("reading plan %s: %s", args[0], err)
	}

	rt, cleanup, ds := buildRuntime(ctx, flagPluginDir)
	emitDiags(ds)
	if ds.HasErrors() {
		return 1, nil
	}
	defer cleanup()

	st, err := state.Load(stateFileName)
	if err != nil {
		return 1, err
	}

	ads := apply.Apply(ctx, pl, rt.Config, rt.Providers, rt.Schemas, st, stateFileName)
	emitDiags(ads)
	if ads.HasErrors() {
		return 1, nil
	}

	s := pl.Summary
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Apply complete: %d created, %d updated, %d deleted, %d replaced.\n",
		s.Create, s.Update, s.Delete, s.Replace)
	return 0, nil
}

// --- destroy -----------------------------------------------------------------

func newDestroyCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Plan deletion of all managed resources; -out writes the plan, no -out applies after TTY confirmation",
		Args:  cobra.NoArgs,
		RunE: exitRun(func(cmd *cobra.Command, _ []string) (int, error) {
			return runDestroy(cmd, out)
		}),
	}
	cmd.Flags().StringVar(&out, "out", "", "write the destroy plan to this file instead of applying")
	return cmd
}

func runDestroy(cmd *cobra.Command, out string) (int, error) {
	ctx := cmd.Context()

	rt, cleanup, ds := buildRuntime(ctx, flagPluginDir)
	emitDiags(ds)
	if ds.HasErrors() {
		return 1, nil
	}
	defer cleanup()

	st, err := state.Load(stateFileName)
	if err != nil {
		return 1, err
	}

	planner := &plan.Planner{
		Config:        rt.Config,
		State:         st,
		Providers:     rt.Providers,
		Schemas:       rt.Schemas,
		EngineVersion: version.Version,
		Refresh:       true,
		Destroy:       true,
	}
	pl, pds := planner.Plan(ctx)
	emitDiags(pds)
	if pds.HasErrors() {
		return 1, nil
	}

	if out != "" {
		if err := plan.Write(pl, out); err != nil {
			return 1, fmt.Errorf("writing plan to %s: %s", out, err)
		}
		if err := writePlanOutput(cmd.OutOrStdout(), pl); err != nil {
			return 1, err
		}
		if pl.HasChanges() {
			return 2, nil
		}
		return 0, nil
	}

	// No -out: interactive destroy. A real terminal must confirm with "yes";
	// automation must go through the reviewable path (destroy -out + apply).
	if !pl.HasChanges() {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No resources to destroy.")
		return 0, nil
	}
	if err := writePlanOutput(cmd.OutOrStdout(), pl); err != nil {
		return 1, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return 1, errors.New(`destroy without -out needs interactive confirmation on a TTY; use "destroy -out FILE" then "apply FILE"`)
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), `Destroy all resources listed above? Only "yes" is accepted: `)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 1, err
	}
	if strings.TrimSpace(line) != "yes" {
		return 1, errors.New(`destroy canceled: confirmation was not "yes"`)
	}

	ads := apply.Apply(ctx, pl, rt.Config, rt.Providers, rt.Schemas, st, stateFileName)
	emitDiags(ads)
	if ads.HasErrors() {
		return 1, nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Destroy complete: %d deleted.\n", pl.Summary.Delete)
	return 0, nil
}

// --- state -------------------------------------------------------------------

func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect the state file",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List managed resource addresses",
			Args:  cobra.NoArgs,
			RunE:  exitRun(runStateList),
		},
		&cobra.Command{
			Use:   "show ADDRESS",
			Short: "Show one resource's state as JSON",
			Args:  cobra.ExactArgs(1),
			RunE:  exitRun(runStateShow),
		},
	)
	return cmd
}

func runStateList(cmd *cobra.Command, _ []string) (int, error) {
	st, err := state.Load(stateFileName)
	if err != nil {
		return 1, err
	}
	for _, addr := range slices.Sorted(maps.Keys(st.Resources)) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), addr)
	}
	return 0, nil
}

func runStateShow(cmd *cobra.Command, args []string) (int, error) {
	st, err := state.Load(stateFileName)
	if err != nil {
		return 1, err
	}
	rs, ok := st.Resources[args[0]]
	if !ok {
		return 1, fmt.Errorf("no resource %q in state", args[0])
	}
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return 1, err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return 0, nil
}

// --- providers ---------------------------------------------------------------

func newProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "Manage provider binaries",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install SOURCE VERSION",
			Short: "Download a provider from the OpenTofu registry into the local cache",
			Args:  cobra.ExactArgs(2),
			RunE:  exitRun(runProvidersInstall),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List cached provider binaries",
			Args:  cobra.NoArgs,
			RunE:  exitRun(runProvidersList),
		},
	)
	return cmd
}

func runProvidersInstall(cmd *cobra.Command, args []string) (int, error) {
	cacheDir, err := providerCacheDir()
	if err != nil {
		return 1, err
	}
	// TCHORI_REGISTRY_URL optionally redirects registry.Install to a mirror
	// or test fixture. Empty/unset preserves the default
	// https://registry.opentofu.org (per the internal/registry contract).
	baseURL := os.Getenv("TCHORI_REGISTRY_URL")
	path, err := registry.Install(cmd.Context(), args[0], args[1], baseURL, cacheDir)
	if err != nil {
		return 1, fmt.Errorf("installing %s %s: %s", args[0], args[1], err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Installed %s %s\n  %s\n", args[0], args[1], path)
	return 0, nil
}

func runProvidersList(cmd *cobra.Command, _ []string) (int, error) {
	cacheDir, err := providerCacheDir()
	if err != nil {
		return 1, err
	}
	installed, err := registry.List(cacheDir)
	if err != nil {
		return 1, err
	}
	if flagJSON {
		b, err := json.MarshalIndent(installed, "", "  ")
		if err != nil {
			return 1, err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return 0, nil
	}
	for _, it := range installed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", it.Source, it.Version, it.Path)
	}
	return 0, nil
}

// --- mcp ---------------------------------------------------------------------

// newMCPCmd serves the four read/plan MCP tools over stdio until the client
// disconnects (stdin EOF) or the process receives SIGINT. Serve returns
// ctx.Err() after a clean session close on cancellation, so context.Canceled
// is a clean shutdown: (0, nil) through the same exitRun pattern every
// sibling command uses; anything else is (1, err).
func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve state_list/state_show/plan/provider_schema over MCP stdio",
		Args:  cobra.NoArgs,
		RunE: exitRun(func(cmd *cobra.Command, _ []string) (int, error) {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			if err := mcpserv.Serve(ctx, "."); err != nil && !errors.Is(err, context.Canceled) {
				return 1, err
			}
			return 0, nil
		}),
	}
}

// --- version -----------------------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tchori engine version",
		Args:  cobra.NoArgs,
		RunE: exitRun(func(cmd *cobra.Command, _ []string) (int, error) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return 0, nil
		}),
	}
}
