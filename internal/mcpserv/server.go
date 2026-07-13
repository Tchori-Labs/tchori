// Package mcpserv implements tchori's MCP stdio server: exactly four
// read/plan tools (state_list, state_show, plan, provider_schema) and no
// apply tool — applying is CLI/CI-only by design.
package mcpserv

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/runtime"
	"github.com/tchori-labs/tchori/internal/state"
	"github.com/tchori-labs/tchori/internal/version"
)

// Serve runs an MCP stdio server exposing exactly:
//
//	state_list()                -> {"addresses": [...]}
//	state_show(address string)  -> the resource's state JSON
//	plan()                      -> the plan.Plan document as JSON
//	provider_schema(name string)-> resource-type schemas as JSON
//
// Dependencies (config, state, providers) are built lazily per tool call;
// state_list/state_show only read state.json and never launch providers.
func Serve(ctx context.Context, workdir string) error {
	return newServer(workdir).Run(ctx, &mcp.StdioTransport{})
}

// newServer builds the tool-registered MCP server for workdir.
func newServer(workdir string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "tchori",
		Version: version.Version,
	}, nil)
	h := &handlers{workdir: workdir}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "state_list",
		Description: "List the addresses of all resources in tchori state.",
	}, h.stateList)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "state_show",
		Description: "Show the recorded state of one resource by address.",
	}, h.stateShow)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "plan",
		Description: "Run a tchori plan and return the plan document as JSON.",
	}, h.planTool)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "provider_schema",
		Description: "Return a configured provider's resource-type schemas as JSON.",
	}, h.providerSchema)
	return s
}

// handlers carries the per-server working directory into tool handlers.
type handlers struct {
	workdir string
}

// jsonResult wraps v as a single JSON text content block.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}

// errResult reports a tool-level failure: IsError=true with the structured
// diagnostics rendered as a JSON text content block (the agent retry loop).
func errResult(ds diag.Diagnostics) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(ds)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}

func (h *handlers) statePath() string {
	return filepath.Join(h.workdir, "state.json")
}

type stateListResult struct {
	Addresses []string `json:"addresses"`
}

// stateList reads state.json only; no providers are launched.
func (h *handlers) stateList(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
	st, err := state.Load(h.statePath())
	if err != nil {
		return errResult(diag.Diagnostics{diag.Errorf("", "failed to load state", err.Error())})
	}
	addrs := make([]string, 0, len(st.Resources))
	for addr := range st.Resources {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	return jsonResult(stateListResult{Addresses: addrs})
}

// stateShowInput: Address has no omitempty tag, so the SDK's schema
// inference marks it required (per research-mcp-sdk.md).
type stateShowInput struct {
	Address string `json:"address" jsonschema:"the resource address, e.g. null_resource.demo"`
}

type stateShowResult struct {
	Address    string          `json:"address"`
	Type       string          `json:"type"`
	Provider   string          `json:"provider"`
	Attributes json.RawMessage `json:"attributes"`
}

// stateShow reads state.json only; no providers are launched.
func (h *handlers) stateShow(_ context.Context, _ *mcp.CallToolRequest, in stateShowInput) (*mcp.CallToolResult, any, error) {
	st, err := state.Load(h.statePath())
	if err != nil {
		return errResult(diag.Diagnostics{diag.Errorf("", "failed to load state", err.Error())})
	}
	rs, ok := st.Resources[in.Address]
	if !ok {
		return errResult(diag.Diagnostics{diag.Errorf(in.Address, "resource not in state",
			fmt.Sprintf("no resource %q in state", in.Address))})
	}
	return jsonResult(stateShowResult{
		Address:    in.Address,
		Type:       rs.Type,
		Provider:   rs.Provider,
		Attributes: rs.Attributes,
	})
}

// planTool builds the full provider runtime, runs the planner with refresh
// on, and returns the plan document as JSON text.
func (h *handlers) planTool(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
	rt, ds := runtime.Build(ctx, runtime.Options{Workdir: h.workdir})
	if ds.HasErrors() {
		return errResult(ds)
	}
	defer rt.Close()

	p := &plan.Planner{
		Config:        rt.Config,
		State:         rt.State,
		Providers:     rt.Providers,
		Schemas:       rt.Schemas,
		EngineVersion: version.Version,
		Refresh:       true,
	}
	pl, pds := p.Plan(ctx)
	if pds.HasErrors() {
		return errResult(pds)
	}
	return jsonResult(pl)
}

// providerSchemaInput: Name has no omitempty tag, so it is required.
type providerSchemaInput struct {
	Name string `json:"name" jsonschema:"the provider's local name from the tchori config"`
}

// attrJSON/nestedBlockJSON/blockJSON/schemaJSON render provider schemas as
// agent-friendly JSON. cty.Type implements json.Marshaler (Type.MarshalJSON,
// pinned in research-cty.md), e.g. "string" or ["map","string"].
type attrJSON struct {
	Type      cty.Type `json:"type"`
	Required  bool     `json:"required,omitempty"`
	Optional  bool     `json:"optional,omitempty"`
	Computed  bool     `json:"computed,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
}

type nestedBlockJSON struct {
	Nesting string     `json:"nesting"`
	Block   *blockJSON `json:"block"`
}

type blockJSON struct {
	Attributes map[string]attrJSON        `json:"attributes,omitempty"`
	Blocks     map[string]nestedBlockJSON `json:"blocks,omitempty"`
}

type schemaJSON struct {
	Version int64      `json:"version"`
	Block   *blockJSON `json:"block"`
}

func renderBlock(b *provider.SchemaBlock) *blockJSON {
	out := &blockJSON{}
	if b == nil {
		return out
	}
	if len(b.Attributes) > 0 {
		out.Attributes = map[string]attrJSON{}
		for name, a := range b.Attributes {
			out.Attributes[name] = attrJSON{
				Type:      a.Type,
				Required:  a.Required,
				Optional:  a.Optional,
				Computed:  a.Computed,
				Sensitive: a.Sensitive,
			}
		}
	}
	if len(b.Blocks) > 0 {
		out.Blocks = map[string]nestedBlockJSON{}
		for name, nb := range b.Blocks {
			out.Blocks[name] = nestedBlockJSON{Nesting: nb.Nesting, Block: renderBlock(nb.Block)}
		}
	}
	return out
}

type providerSchemaResult struct {
	Provider      string                `json:"provider"`
	ResourceTypes map[string]schemaJSON `json:"resource_types"`
	// UnsupportedResourceTypes lists resource types the provider defines
	// whose schema tchori could not convert (e.g. nested_type attributes),
	// keyed by type name with the conversion error's detail as the value.
	// Omitted when empty. See issue #5: these types are tolerated at
	// Schemas() time and simply absent from ResourceTypes; surfacing them
	// here tells the agent why a type it expected is missing, rather than
	// leaving it to guess.
	UnsupportedResourceTypes map[string]string `json:"unsupported_resource_types,omitempty"`
}

// providerSchema builds the provider runtime and dumps the named provider's
// resource-type schemas as JSON text.
func (h *handlers) providerSchema(ctx context.Context, _ *mcp.CallToolRequest, in providerSchemaInput) (*mcp.CallToolResult, any, error) {
	rt, ds := runtime.Build(ctx, runtime.Options{Workdir: h.workdir})
	if ds.HasErrors() {
		return errResult(ds)
	}
	defer rt.Close()

	ps, ok := rt.Schemas[in.Name]
	if !ok {
		return errResult(diag.Diagnostics{diag.Errorf("", "unknown provider",
			fmt.Sprintf("no provider %q in configuration", in.Name))})
	}
	out := providerSchemaResult{Provider: in.Name, ResourceTypes: map[string]schemaJSON{}}
	for typeName, sch := range ps.ResourceTypes {
		out.ResourceTypes[typeName] = schemaJSON{Version: sch.Version, Block: renderBlock(sch.Block)}
	}
	if len(ps.UnsupportedResources) > 0 {
		out.UnsupportedResourceTypes = ps.UnsupportedResources
	}
	return jsonResult(out)
}
