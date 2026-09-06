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
	// NestedType retains per-leaf metadata that cty.Type cannot represent.
	NestedType map[string]*Attr
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
	// UnsupportedResources records resource types the provider defines whose
	// schema tchori could not convert (e.g. nested_type attributes — see
	// blockFromProto), keyed by type name with the conversion error's detail
	// text as the value. Schemas() tolerates these instead of failing the
	// whole provider: a type here is only reported when a config or state
	// entry actually references it (see LookupResourceType). Providers like
	// cloudflare/cloudflare expose hundreds of resource types — many unused,
	// some with nested attributes — and one unused type must not poison the
	// provider's fully-supported flat resources (issue #5).
	UnsupportedResources map[string]string
}

// LookupResourceType resolves typeName against ps, distinguishing the two
// failure cases callers must report differently:
//
//   - schema, "", true: the type is known and its schema converted cleanly.
//   - nil, detail, true: the type is known (the provider defines it) but its
//     schema failed to convert — detail is the stored conversion error, e.g.
//     a nested_type attribute message from blockFromProto.
//   - nil, "", false: the provider does not define this resource type at all.
func (ps *ProviderSchemas) LookupResourceType(typeName string) (schema *Schema, unsupportedDetail string, known bool) {
	if s, ok := ps.ResourceTypes[typeName]; ok {
		return s, "", true
	}
	if detail, ok := ps.UnsupportedResources[typeName]; ok {
		return nil, detail, true
	}
	return nil, "", false
}

// ImpliedType returns the marker-free cty object type of engine values for
// this block. Attr.Type may contain optional-attribute markers because it is a
// conversion target, but markers are part of cty type identity and must never
// enter constructed, decoded, or stored values (issue #50). Terraform's
// configschema.Block.ImpliedType applies the same deep normalization.
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
	return cty.Object(atys).WithoutOptionalAttributesDeep()
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
		Provider:             providerSchema,
		ResourceTypes:        make(map[string]*Schema, len(resp.ResourceSchemas)),
		UnsupportedResources: map[string]string{},
	}
	for typeName, ps := range resp.ResourceSchemas {
		s, err := schemaFromProto(ps)
		if err != nil {
			// Tolerate until used (issue #5): a resource type this engine
			// cannot convert must not poison the whole provider. Record the
			// detail and keep going; LookupResourceType surfaces it only if
			// something actually references this type.
			out.UnsupportedResources[typeName] = err.Error()
			continue
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

// blockFromProto converts a wire Schema.Block. Flat attribute types arrive as
// JSON-encoded cty type bytes (e.g. `"string"`, `["map","string"]`) and are
// parsed with ctyjson.UnmarshalType; nested_type attributes (pa.NestedType)
// are converted recursively by attrFromProto/nestedObjectType.
func blockFromProto(pb *tfplugin6.Schema_Block) (*SchemaBlock, error) {
	b := &SchemaBlock{
		Attributes: map[string]*Attr{},
		Blocks:     map[string]*NestedBlock{},
	}
	if pb == nil {
		return b, nil
	}
	for _, pa := range pb.Attributes {
		attr, err := attrFromProto(pa)
		if err != nil {
			return nil, err
		}
		b.Attributes[pa.Name] = attr
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

// attrFromProto preserves both the wire type and nested sensitivity metadata.
func attrFromProto(pa *tfplugin6.Schema_Attribute) (*Attr, error) {
	var ty cty.Type
	var nested map[string]*Attr
	var err error
	if pa.NestedType != nil {
		ty, nested, err = nestedObjectType(pa.NestedType)
	} else if len(pa.Type) != 0 {
		ty, err = ctyjson.UnmarshalType(pa.Type)
	} else {
		return nil, fmt.Errorf("attribute %q: neither type nor nested_type is set", pa.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("attribute %q: %w", pa.Name, err)
	}
	return &Attr{
		Type: ty, Required: pa.Required, Optional: pa.Optional,
		Computed: pa.Computed, Sensitive: pa.Sensitive, NestedType: nested,
	}, nil
}

// nestedObjectType converts a tfprotov6 nested_type (Schema_Object) into its
// cty type: SINGLE -> object, LIST -> list(object), SET -> set(object),
// MAP -> map(object). Each nested attribute recurses through
// attrFromProto, so a nested_type attribute nested inside another
// nested_type attribute converts the same way arbitrarily deep (issue #7).
//
// Non-required nested attributes (Optional or Computed-only) are marked
// optional via cty.ObjectWithOptionalAttrs, mirroring how Terraform itself
// treats non-required nested attributes: it lets a config that omits an
// optional nested attribute/object entirely (this engine's Compose already
// fills the missing key with cty.NullVal of the attribute's type either
// way) and a source value with fewer attributes convert cleanly wherever a
// nested value flows through cty/convert (e.g. resolving a ${ref} into a
// nested attribute). ImpliedType deliberately strips these markers before a
// type is used for a value: marker-bearing null and concrete object values
// cannot coexist in cty collections and caused issue #50.
//
// A nesting mode this function does not recognize (i.e. not
// SINGLE/LIST/SET/MAP) is a genuinely unconvertible schema: the caller
// records it in ProviderSchemas.UnsupportedResources and tolerates it until
// the resource type is actually used (issue #5's tolerate-until-used
// machinery), exactly like any other conversion failure from this file.
func nestedObjectType(so *tfplugin6.Schema_Object) (cty.Type, map[string]*Attr, error) {
	atys := make(map[string]cty.Type, len(so.Attributes))
	nested := make(map[string]*Attr, len(so.Attributes))
	var optional []string
	for _, pa := range so.Attributes {
		attr, err := attrFromProto(pa)
		if err != nil {
			return cty.NilType, nil, err
		}
		atys[pa.Name] = attr.Type
		nested[pa.Name] = attr
		if !pa.Required {
			optional = append(optional, pa.Name)
		}
	}
	obj := cty.ObjectWithOptionalAttrs(atys, optional)
	switch so.Nesting {
	case tfplugin6.Schema_Object_SINGLE:
		return obj, nested, nil
	case tfplugin6.Schema_Object_LIST:
		return cty.List(obj), nested, nil
	case tfplugin6.Schema_Object_SET:
		return cty.Set(obj), nested, nil
	case tfplugin6.Schema_Object_MAP:
		return cty.Map(obj), nested, nil
	default:
		return cty.NilType, nil, fmt.Errorf("nested attribute type (nested_type): unsupported nesting mode %s", so.Nesting)
	}
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
