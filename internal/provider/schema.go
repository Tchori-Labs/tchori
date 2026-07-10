package provider

import (
	"context"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// Attr describes one attribute in a provider schema block.
type Attr struct {
	Type      cty.Type
	Required  bool
	Optional  bool
	Computed  bool
	Sensitive bool
}

// NestedBlock is a nested configuration block inside a SchemaBlock.
type NestedBlock struct {
	Nesting string // "single" | "list" | "set" | "map"
	Block   *SchemaBlock
}

// SchemaBlock is one configuration block: attributes plus nested blocks.
type SchemaBlock struct {
	Attributes map[string]*Attr
	Blocks     map[string]*NestedBlock
}

// Schema is a versioned schema for a provider or a resource type.
type Schema struct {
	Version int64
	Block   *SchemaBlock
}

// ProviderSchemas holds everything tchori uses from GetProviderSchema.
type ProviderSchemas struct {
	Provider      *Schema
	ResourceTypes map[string]*Schema
}

// ImpliedType returns the cty object type a value of this block must
// conform to: one attribute per schema attribute plus one per nested block
// (single = object, list = list(object), set = set(object),
// map = map(object)).
func (b *SchemaBlock) ImpliedType() cty.Type {
	atys := make(map[string]cty.Type, len(b.Attributes)+len(b.Blocks))
	for name, a := range b.Attributes {
		atys[name] = a.Type
	}
	for name, nb := range b.Blocks {
		inner := nb.Block.ImpliedType()
		switch nb.Nesting {
		case "single":
			atys[name] = inner
		case "list":
			atys[name] = cty.List(inner)
		case "set":
			atys[name] = cty.Set(inner)
		case "map":
			atys[name] = cty.Map(inner)
		}
	}
	return cty.Object(atys)
}

// Schemas returns the provider's schemas, calling GetProviderSchema on the
// first call and returning the cached result on every later call.
func (c *Client) Schemas(ctx context.Context) (*ProviderSchemas, diag.Diagnostics) {
	if c.schemas != nil {
		return c.schemas, nil
	}

	resp, err := c.grpc.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf("", "GetProviderSchema RPC failed", err.Error())}
	}
	ds := diagsFromProto(resp.Diagnostics)
	if ds.HasErrors() {
		return nil, ds
	}

	providerSchema, err := schemaFromProto(resp.Provider)
	if err != nil {
		return nil, append(ds, diag.Errorf("", "invalid provider config schema", err.Error()))
	}
	out := &ProviderSchemas{
		Provider:      providerSchema,
		ResourceTypes: make(map[string]*Schema, len(resp.ResourceSchemas)),
	}
	for typeName, ps := range resp.ResourceSchemas {
		s, err := schemaFromProto(ps)
		if err != nil {
			return nil, append(ds, diag.Errorf("", "invalid schema for resource type "+typeName, err.Error()))
		}
		out.ResourceTypes[typeName] = s
	}

	c.schemas = out
	return out, ds
}

// schemaFromProto converts a wire Schema into tchori's Schema. A nil wire
// schema converts to an empty (but non-nil) schema.
func schemaFromProto(ps *tfplugin6.Schema) (*Schema, error) {
	if ps == nil {
		block, _ := blockFromProto(nil)
		return &Schema{Block: block}, nil
	}
	block, err := blockFromProto(ps.Block)
	if err != nil {
		return nil, err
	}
	return &Schema{Version: ps.Version, Block: block}, nil
}

// blockFromProto converts a wire Schema.Block. Attribute types arrive as
// JSON-encoded cty type bytes (e.g. `"string"`, `["map","string"]`) and are
// parsed with ctyjson.UnmarshalType.
func blockFromProto(pb *tfplugin6.Schema_Block) (*SchemaBlock, error) {
	b := &SchemaBlock{
		Attributes: map[string]*Attr{},
		Blocks:     map[string]*NestedBlock{},
	}
	if pb == nil {
		return b, nil
	}
	for _, pa := range pb.Attributes {
		if len(pa.Type) == 0 {
			return nil, fmt.Errorf("attribute %q: nested attribute types (nested_type) are not supported", pa.Name)
		}
		ty, err := ctyjson.UnmarshalType(pa.Type)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: parsing type %s: %w", pa.Name, pa.Type, err)
		}
		b.Attributes[pa.Name] = &Attr{
			Type:      ty,
			Required:  pa.Required,
			Optional:  pa.Optional,
			Computed:  pa.Computed,
			Sensitive: pa.Sensitive,
		}
	}
	for _, pnb := range pb.BlockTypes {
		var nesting string
		switch pnb.Nesting {
		case tfplugin6.Schema_NestedBlock_SINGLE:
			nesting = "single"
		case tfplugin6.Schema_NestedBlock_LIST:
			nesting = "list"
		case tfplugin6.Schema_NestedBlock_SET:
			nesting = "set"
		case tfplugin6.Schema_NestedBlock_MAP:
			nesting = "map"
		default:
			return nil, fmt.Errorf("nested block %q: unsupported nesting mode %s", pnb.TypeName, pnb.Nesting)
		}
		inner, err := blockFromProto(pnb.Block)
		if err != nil {
			return nil, fmt.Errorf("nested block %q: %w", pnb.TypeName, err)
		}
		b.Blocks[pnb.TypeName] = &NestedBlock{Nesting: nesting, Block: inner}
	}
	return b, nil
}

// diagsFromProto converts protocol diagnostics into tchori diagnostics.
// Reused by the other RPC wrappers (Task 8).
func diagsFromProto(pds []*tfplugin6.Diagnostic) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, pd := range pds {
		if pd == nil {
			continue
		}
		if pd.Severity == tfplugin6.Diagnostic_WARNING {
			ds = append(ds, diag.Warnf("", pd.Summary, pd.Detail))
			continue
		}
		ds = append(ds, diag.Errorf("", pd.Summary, pd.Detail))
	}
	return ds
}
