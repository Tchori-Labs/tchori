package sensitive

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"
)

// RecoveryVersion is the current authenticated sensitive projection generation.
const RecoveryVersion = 4

// RecoveryVersionSupported reports whether a persisted projection generation
// can still be opened and migrated by this engine.
func RecoveryVersionSupported(version int) bool {
	return version >= 1 && version <= RecoveryVersion
}

const projectionContractVersion = 1

type recoveryPayload struct {
	Version            int           `json:"version"`
	ContractVersion    int           `json:"contract_version,omitempty"`
	ProjectionSHA256   []byte        `json:"projection_sha256"`
	ProjectionPaths    []string      `json:"projection_paths,omitempty"`
	ProjectionRedacted []string      `json:"projection_redacted,omitempty"`
	DirectPaths        []string      `json:"direct_paths,omitempty"`
	Sets               []recoverySet `json:"sets,omitempty"`
	Values             []recoverySet `json:"values,omitempty"`
}

type recoverySet struct {
	Path  []recoveryPathStep `json:"path"`
	Value []byte             `json:"value"`
}

type recoveryPathStep struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Index int    `json:"index,omitempty"`
}

// HasSensitiveSets reports whether redacting this specification can change set
// element identity. Such sets require authenticated recovery data in state.
func (s *Spec) HasSensitiveSets() bool { return len(s.setPrefixes) != 0 }

// Marshal produces a deterministic public JSON projection without rebuilding
// cty sets after redaction. Duplicate projected elements therefore remain
// distinct array entries. Unknown values are represented as null and reported.
func (s *Spec) Marshal(v cty.Value) (json.RawMessage, []string, []string, error) {
	return s.marshal(v, false)
}

// Project produces a public projection and its authenticated generation
// contract. The contract is mandatory even when no values require recovery;
// otherwise it carries directly sensitive values, identity-sensitive sets, and
// confidential map boundaries. Callers must encrypt it before current state
// persistence.
func (s *Spec) Project(v cty.Value) (json.RawMessage, []string, []byte, error) {
	public, redacted, unknown, values, err := s.project(v, true)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(unknown) != 0 {
		return nil, nil, nil, fmt.Errorf("state contains unknown values at %v", unknown)
	}
	values = uniqueRecoveryValues(values)
	contract, err := encodeProjectionContract(public, s.Paths(), redacted, directRecoveryPaths(values, s), values)
	if err != nil {
		return nil, nil, nil, err
	}
	return public, redacted, contract, nil
}

// NewProjectionContract binds an already-produced public projection to its
// authoritative sensitivity contract when no typed value recovery is needed.
func NewProjectionContract(public json.RawMessage, paths, redacted []string) ([]byte, error) {
	return encodeProjectionContract(public, paths, redacted, nil, nil)
}

// ValidateProjectionContract verifies a current generation contract after its
// encrypted state envelope has authenticated resource identity and purpose.
func ValidateProjectionContract(public json.RawMessage, contract []byte, generationVersion int) ([]string, []string, error) {
	payload, err := decodeRecoveryPayload(contract)
	if err != nil || generationVersion != RecoveryVersion ||
		payload.Version != RecoveryVersion || payload.ContractVersion != projectionContractVersion ||
		len(payload.Sets) != 0 ||
		!equalStrings(payload.ProjectionPaths, sortedUnique(payload.ProjectionPaths)) ||
		!equalStrings(payload.ProjectionRedacted, sortedUnique(payload.ProjectionRedacted)) ||
		!equalStrings(payload.DirectPaths, sortedUnique(payload.DirectPaths)) {
		return nil, nil, errors.New("invalid authenticated sensitive projection contract")
	}
	if err := validateProjectionDigest(public, payload.ProjectionSHA256); err != nil {
		return nil, nil, err
	}
	return append([]string(nil), payload.ProjectionPaths...), append([]string(nil), payload.ProjectionRedacted...), nil
}

// RebindProjectionContract updates authenticated plaintext metadata after a
// state migration has deliberately retained historical redaction provenance.
// The typed recovered values remain unchanged.
func RebindProjectionContract(public json.RawMessage, contract []byte, paths, redacted []string) ([]byte, error) {
	payload, err := decodeRecoveryPayload(contract)
	if err != nil || payload.Version != RecoveryVersion ||
		payload.ContractVersion != projectionContractVersion || len(payload.Sets) != 0 {
		return nil, errors.New("invalid authenticated sensitive projection contract")
	}
	if err := validateProjectionDigest(public, payload.ProjectionSHA256); err != nil {
		return nil, err
	}
	return encodeProjectionContract(public, paths, redacted, payload.DirectPaths, payload.Values)
}

func encodeProjectionContract(public json.RawMessage, paths, redacted, directPaths []string, values []recoverySet) ([]byte, error) {
	root, err := decodeJSON(public)
	if err != nil {
		return nil, fmt.Errorf("decode public state projection: %w", err)
	}
	canonicalPublic, err := json.Marshal(root)
	if err != nil {
		return nil, errors.New("encode public state projection")
	}
	sum := sha256.Sum256(canonicalPublic)
	contract, err := json.Marshal(recoveryPayload{
		Version: RecoveryVersion, ContractVersion: projectionContractVersion,
		ProjectionSHA256: sum[:], ProjectionPaths: sortedUnique(paths),
		ProjectionRedacted: sortedUnique(redacted), DirectPaths: sortedUnique(directPaths), Values: values,
	})
	if err != nil {
		return nil, fmt.Errorf("encode sensitive projection contract: %w", err)
	}
	return contract, nil
}

func decodeRecoveryPayload(recovery []byte) (recoveryPayload, error) {
	var payload recoveryPayload
	dec := json.NewDecoder(bytes.NewReader(recovery))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return recoveryPayload{}, errors.New("invalid sensitive recovery payload")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return recoveryPayload{}, errors.New("invalid sensitive recovery payload")
	}
	return payload, nil
}

func validateProjectionDigest(public json.RawMessage, want []byte) error {
	if len(want) != sha256.Size {
		return errors.New("invalid sensitive recovery payload")
	}
	root, err := decodeJSON(public)
	if err != nil {
		return fmt.Errorf("decode public state: %w", err)
	}
	canonicalPublic, err := json.Marshal(root)
	if err != nil {
		return errors.New("encode public state projection")
	}
	sum := sha256.Sum256(canonicalPublic)
	if !bytes.Equal(sum[:], want) {
		return errors.New("sensitive recovery does not match public state")
	}
	return nil
}

func (s *Spec) marshal(v cty.Value, capture bool) (json.RawMessage, []string, []string, error) {
	public, redacted, unknown, _, err := s.project(v, capture)
	return public, redacted, unknown, err
}

func (s *Spec) project(v cty.Value, capture bool) (json.RawMessage, []string, []string, []recoverySet, error) {
	var redacted, unknown []string
	var values []recoverySet
	out, err := s.projectValue(v, nil, "", capture, false, false, false, &redacted, &unknown, &values)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sort.Strings(redacted)
	sort.Strings(unknown)
	return json.RawMessage(out), sortedUnique(redacted), sortedUnique(unknown), values, nil
}

func (s *Spec) projectValue(v cty.Value, path cty.Path, logical string, capture, insideCaptured, inheritedSensitive, legacyMapProjection bool, redacted, unknown *[]string, values *[]recoverySet) ([]byte, error) {
	instance := PathString(path)
	literal, exempt := s.exempt[instance]
	literalExempt := exempt && literalMatches(v, literal)
	sensitiveHere := inheritedSensitive && !literalExempt
	if contains(s.paths, logical) && !literalExempt {
		sensitiveHere = true
		if v.IsKnown() && !v.IsNull() {
			*redacted = append(*redacted, instance)
		}
	}
	if !v.IsKnown() {
		*unknown = append(*unknown, instance)
		return []byte("null"), nil
	}
	ty := v.Type()
	if !legacyMapProjection && s.recoversMap(ty, logical) && !insideCaptured {
		if capture {
			if !v.IsWhollyKnown() {
				return nil, fmt.Errorf("sensitive map %q contains unknown values", logical)
			}
			if err := appendRecoveryValue(v, path, logical, "sensitive map", values); err != nil {
				return nil, err
			}
		}
		return []byte("null"), nil
	}
	if v.IsNull() {
		if capture && s.capturesDirectValue(logical, instance, v, insideCaptured, sensitiveHere) {
			if err := appendRecoveryValue(v, path, logical, "sensitive value", values); err != nil {
				return nil, err
			}
		}
		return []byte("null"), nil
	}
	if ty.IsSetType() && contains(s.setPrefixes, logical) && !insideCaptured {
		if capture {
			if !v.IsWhollyKnown() {
				return nil, fmt.Errorf("sensitive set %q contains unknown values", logical)
			}
			if err := appendRecoveryValue(v, path, logical, "sensitive set", values); err != nil {
				return nil, err
			}
		}
		insideCaptured = true
	}
	if capture && s.capturesDirectValue(logical, instance, v, insideCaptured, sensitiveHere) {
		if err := appendRecoveryValue(v, path, logical, "sensitive value", values); err != nil {
			return nil, err
		}
	}
	composite := ty.IsObjectType() || ty.IsMapType() || ty.IsListType() || ty.IsTupleType() || ty.IsSetType()
	if sensitiveHere && ((!legacyMapProjection && ty.IsMapType()) || !composite || (!insideCaptured && !s.hasSetAtOrBelow(logical))) {
		return []byte("null"), nil
	}

	switch {
	case ty.IsObjectType():
		names := make([]string, 0, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			names = append(names, name)
		}
		sort.Strings(names)
		fields := make([][]byte, 0, len(names))
		for _, name := range names {
			child, err := s.projectValue(v.GetAttr(name), appendPath(path, cty.GetAttrStep{Name: name}), join(logical, name), capture, insideCaptured, sensitiveHere, legacyMapProjection, redacted, unknown, values)
			if err != nil {
				return nil, err
			}
			key, _ := json.Marshal(name)
			fields = append(fields, append(append(key, ':'), child...))
		}
		return joinJSON('{', '}', fields), nil
	case ty.IsMapType():
		fields := make([][]byte, 0, v.LengthInt())
		it := v.ElementIterator()
		for it.Next() {
			key, value := it.Element()
			name := key.AsString()
			child, err := s.projectValue(value, appendPath(path, cty.IndexStep{Key: key}), logical, capture, insideCaptured, sensitiveHere, legacyMapProjection, redacted, unknown, values)
			if err != nil {
				return nil, err
			}
			encodedKey, _ := json.Marshal(name)
			fields = append(fields, append(append(encodedKey, ':'), child...))
		}
		return joinJSON('{', '}', fields), nil
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		items := make([][]byte, 0, v.LengthInt())
		it := v.ElementIterator()
		for i := 0; it.Next(); i++ {
			_, value := it.Element()
			child, err := s.projectValue(value, appendPath(path, cty.IndexStep{Key: cty.NumberIntVal(int64(i))}), logical, capture, insideCaptured, sensitiveHere, legacyMapProjection, redacted, unknown, values)
			if err != nil {
				return nil, err
			}
			items = append(items, child)
		}
		if ty.IsSetType() {
			sort.Slice(items, func(i, j int) bool { return bytes.Compare(items[i], items[j]) < 0 })
		}
		return joinJSON('[', ']', items), nil
	default:
		out, err := ctyjson.Marshal(v, ty)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", instance, err)
		}
		return out, nil
	}
}

func (s *Spec) hasSetAtOrBelow(logical string) bool {
	for _, prefix := range s.setPrefixes {
		if logical == "" || prefix == logical || strings.HasPrefix(prefix, logical+".") {
			return true
		}
	}
	return false
}

func (s *Spec) recoversMap(ty cty.Type, logical string) bool {
	return ty.IsMapType() &&
		coveredBySensitivePath(logical, s.paths) &&
		s.typeHasAffectedSet(ty, logical)
}

func (s *Spec) typeHasAffectedSet(ty cty.Type, logical string) bool {
	switch {
	case ty.IsSetType():
		return contains(s.setPrefixes, logical) || s.typeHasAffectedSet(ty.ElementType(), logical)
	case ty.IsListType(), ty.IsMapType():
		return s.typeHasAffectedSet(ty.ElementType(), logical)
	case ty.IsTupleType():
		for _, elementType := range ty.TupleElementTypes() {
			if s.typeHasAffectedSet(elementType, logical) {
				return true
			}
		}
	case ty.IsObjectType():
		for name, attributeType := range ty.AttributeTypes() {
			if s.typeHasAffectedSet(attributeType, join(logical, name)) {
				return true
			}
		}
	}
	return false
}

func appendPath(path cty.Path, step cty.PathStep) cty.Path {
	out := make(cty.Path, len(path), len(path)+1)
	copy(out, path)
	return append(out, step)
}

func joinJSON(open, close byte, values [][]byte) []byte {
	var out bytes.Buffer
	out.WriteByte(open)
	for i, value := range values {
		if i != 0 {
			out.WriteByte(',')
		}
		out.Write(value)
	}
	out.WriteByte(close)
	return out.Bytes()
}

func recoveryPath(path cty.Path) ([]recoveryPathStep, error) {
	out := make([]recoveryPathStep, 0, len(path))
	for _, step := range path {
		switch s := step.(type) {
		case cty.GetAttrStep:
			out = append(out, recoveryPathStep{Kind: "attr", Name: s.Name})
		case cty.IndexStep:
			switch s.Key.Type() {
			case cty.String:
				out = append(out, recoveryPathStep{Kind: "key", Name: s.Key.AsString()})
			case cty.Number:
				index, accuracy := s.Key.AsBigFloat().Int64()
				if accuracy != 0 || index < 0 {
					return nil, errors.New("sensitive set recovery path has an invalid index")
				}
				out = append(out, recoveryPathStep{Kind: "index", Index: int(index)})
			default:
				return nil, errors.New("sensitive set recovery path has an unsupported key")
			}
		default:
			return nil, errors.New("sensitive set recovery path has an unsupported step")
		}
	}
	return out, nil
}

// Restore authenticates and reconstructs state under the current projection
// contract. State-policy rotation uses restoreGeneration with the paths that
// produced the persisted projection, then projects once under the new policy.
func (s *Spec) Restore(public json.RawMessage, recovery []byte, ty cty.Type) (cty.Value, error) {
	return s.restoreGeneration(public, recovery, ty, s.paths, RecoveryVersion)
}

// RestoreProjected authenticates and reconstructs state using the sensitivity
// paths and recovery generation recorded with that state. Recovery versions 2
// through 4 authenticate their own generation paths; the path argument
// preserves safe migration of version 1 state.
func (s *Spec) RestoreProjected(public json.RawMessage, recovery []byte, ty cty.Type, generationPaths []string, generationVersion int) (cty.Value, error) {
	return s.restoreGeneration(public, recovery, ty, generationPaths, generationVersion)
}

func (s *Spec) restoreGeneration(public json.RawMessage, recovery []byte, ty cty.Type, generationPaths []string, generationVersion int) (cty.Value, error) {
	root, err := decodeJSON(public)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode public state: %w", err)
	}

	generation := s.withPaths(generationPaths)
	if len(recovery) == 0 {
		if generationVersion >= RecoveryVersion {
			return cty.NilVal, fmt.Errorf("authenticated sensitive projection contract is required for generation %d", RecoveryVersion)
		}
		expected := map[string]expectedSet{}
		if err := generation.collectExpectedRecovery(root, ty, "", nil, expected, generationVersion >= 3, generationVersion >= 4, generation.paths); err != nil {
			return cty.NilVal, err
		}
		if len(expected) != 0 {
			return cty.NilVal, errors.New("sensitive set recovery is required; legacy redacted state cannot safely reconstruct set identity")
		}
		return ctyjson.Unmarshal(public, ty)
	}

	payload, err := decodeRecoveryPayload(recovery)
	if err != nil {
		return cty.NilVal, err
	}
	if !RecoveryVersionSupported(payload.Version) ||
		(generationVersion != 0 && payload.Version != generationVersion) {
		return cty.NilVal, errors.New("invalid sensitive recovery payload")
	}
	if len(payload.ProjectionSHA256) != sha256.Size {
		return cty.NilVal, errors.New("invalid sensitive recovery payload")
	}
	if generationVersion >= RecoveryVersion && payload.ContractVersion != projectionContractVersion {
		return cty.NilVal, errors.New("authenticated sensitive projection contract is required")
	}
	if payload.ContractVersion != 0 {
		if payload.Version != RecoveryVersion || payload.ContractVersion != projectionContractVersion ||
			!equalStrings(payload.ProjectionRedacted, sortedUnique(payload.ProjectionRedacted)) {
			return cty.NilVal, errors.New("invalid authenticated sensitive projection contract")
		}
	}
	if payload.Version >= 2 {
		if !equalStrings(payload.ProjectionPaths, sortedUnique(payload.ProjectionPaths)) {
			return cty.NilVal, errors.New("invalid sensitive recovery projection contract")
		}
		generation = s.withPaths(payload.ProjectionPaths)
	}
	if payload.Version >= 4 {
		if !equalStrings(payload.DirectPaths, sortedUnique(payload.DirectPaths)) {
			return cty.NilVal, errors.New("invalid sensitive recovery direct-path contract")
		}
	} else if len(payload.DirectPaths) != 0 {
		return cty.NilVal, errors.New("invalid sensitive recovery payload")
	}

	canonicalPublic, err := json.Marshal(root)
	if err != nil {
		return cty.NilVal, errors.New("encode public state projection")
	}
	publicSum := sha256.Sum256(canonicalPublic)
	if !bytes.Equal(publicSum[:], payload.ProjectionSHA256) {
		return cty.NilVal, errors.New("sensitive recovery does not match public state")
	}

	includeMaps := payload.Version >= 3
	includeSensitiveValues := payload.Version >= 4
	recoveredValues := payload.Sets
	if includeMaps {
		if len(payload.Sets) != 0 {
			return cty.NilVal, errors.New("invalid sensitive recovery payload")
		}
		recoveredValues = payload.Values
	} else if len(payload.Values) != 0 {
		return cty.NilVal, errors.New("invalid sensitive recovery payload")
	}
	expected := map[string]expectedSet{}
	if err := generation.collectExpectedRecovery(root, ty, "", nil, expected, includeMaps, includeSensitiveValues, payload.DirectPaths); err != nil {
		return cty.NilVal, err
	}
	if len(recoveredValues) != len(expected) || (payload.ContractVersion == 0 && len(expected) == 0) {
		return cty.NilVal, errors.New("sensitive recovery path set does not match its generation contract")
	}
	seen := map[string]bool{}
	for _, recovered := range recoveredValues {
		key := recoveryPathKey(recovered.Path)
		want, ok := expected[key]
		if !ok || seen[key] {
			return cty.NilVal, errors.New("sensitive recovery path does not match public state")
		}
		seen[key] = true
		value, err := msgpack.Unmarshal(recovered.Value, want.Type)
		if err != nil || !value.IsKnown() ||
			(payload.Version >= 4 && !value.IsWhollyKnown()) ||
			(payload.Version == 3 && want.Type.IsMapType() && !value.IsWhollyKnown()) ||
			(!want.AllowNull && value.IsNull()) || !value.Type().Equals(want.Type) {
			return cty.NilVal, errors.New("invalid sensitive recovery value")
		}

		var projectedRedacted, projectedUnknown []string
		var projectedValues []recoverySet
		projected, err := generation.projectValue(
			value, nil, want.Logical, false, false,
			coveredBySensitivePath(want.Logical, generation.paths),
			payload.Version < 3,
			&projectedRedacted, &projectedUnknown, &projectedValues,
		)
		if err != nil || len(projectedUnknown) != 0 {
			return cty.NilVal, errors.New("invalid sensitive recovery value")
		}
		canonicalExpected, err := json.Marshal(want.Public)
		if err != nil || !bytes.Equal(projected, canonicalExpected) {
			return cty.NilVal, errors.New("sensitive recovery value does not match its generation-time projection")
		}

		encoded, err := ctyjson.Marshal(value, want.Type)
		if err != nil {
			return cty.NilVal, errors.New("invalid sensitive recovery value")
		}
		decoded, err := decodeJSON(encoded)
		if err != nil || setJSONPath(root, recovered.Path, decoded) != nil {
			return cty.NilVal, errors.New("sensitive recovery path does not match public state")
		}
	}
	assembled, err := json.Marshal(root)
	if err != nil {
		return cty.NilVal, errors.New("encode restored state")
	}
	value, err := ctyjson.Unmarshal(assembled, ty)
	if err != nil {
		return cty.NilVal, fmt.Errorf("decode restored state: %w", err)
	}
	return value, nil
}

func (s *Spec) withPaths(paths []string) *Spec {
	paths = sortedUnique(paths)
	setPrefixes := affectedSets(s.allSetPrefixes, paths)
	return &Spec{
		paths:          paths,
		exempt:         map[string]any{},
		setPrefixes:    setPrefixes,
		allSetPrefixes: s.allSetPrefixes,
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type expectedSet struct {
	Type      cty.Type
	Logical   string
	Public    any
	AllowNull bool
}

func (s *Spec) collectExpectedRecovery(v any, ty cty.Type, logical string, path []recoveryPathStep, out map[string]expectedSet, includeMaps, includeSensitiveValues bool, directPaths []string) error {
	if includeMaps && s.recoversMap(ty, logical) {
		if v != nil {
			return fmt.Errorf("public state at %q exposes a sensitive map", logical)
		}
		cloned := append([]recoveryPathStep(nil), path...)
		out[recoveryPathKey(cloned)] = expectedSet{Type: ty, Logical: logical, Public: nil, AllowNull: true}
		return nil
	}
	if ty.IsSetType() && contains(s.setPrefixes, logical) {
		if v == nil {
			return nil
		}
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("public state at %q is not a set array", logical)
		}
		cloned := append([]recoveryPathStep(nil), path...)
		out[recoveryPathKey(cloned)] = expectedSet{Type: ty, Logical: logical, Public: v}
		return nil
	}
	if includeSensitiveValues && contains(directPaths, logical) && v == nil {
		cloned := append([]recoveryPathStep(nil), path...)
		out[recoveryPathKey(cloned)] = expectedSet{Type: ty, Logical: logical, Public: nil, AllowNull: true}
		return nil
	}
	if v == nil {
		return nil
	}
	switch {
	case ty.IsObjectType():
		object, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("public state at %q is not an object", logical)
		}
		for name, childType := range ty.AttributeTypes() {
			child, exists := object[name]
			if !exists {
				return fmt.Errorf("public state at %q lacks attribute %q", logical, name)
			}
			if err := s.collectExpectedRecovery(child, childType, join(logical, name), appendRecoveryPath(path, recoveryPathStep{Kind: "attr", Name: name}), out, includeMaps, includeSensitiveValues, directPaths); err != nil {
				return err
			}
		}
	case ty.IsMapType():
		object, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("public state at %q is not a map", logical)
		}
		for key, child := range object {
			if err := s.collectExpectedRecovery(child, ty.ElementType(), logical, appendRecoveryPath(path, recoveryPathStep{Kind: "key", Name: key}), out, includeMaps, includeSensitiveValues, directPaths); err != nil {
				return err
			}
		}
	case ty.IsTupleType():
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("public state at %q is not a tuple array", logical)
		}
		elementTypes := ty.TupleElementTypes()
		if len(items) != len(elementTypes) {
			return fmt.Errorf("public state at %q has tuple arity %d; want %d", logical, len(items), len(elementTypes))
		}
		for i, child := range items {
			if err := s.collectExpectedRecovery(child, elementTypes[i], logical, appendRecoveryPath(path, recoveryPathStep{Kind: "index", Index: i}), out, includeMaps, includeSensitiveValues, directPaths); err != nil {
				return err
			}
		}
	case ty.IsListType() || ty.IsSetType():
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("public state at %q is not an array", logical)
		}
		for i, child := range items {
			if err := s.collectExpectedRecovery(child, ty.ElementType(), logical, appendRecoveryPath(path, recoveryPathStep{Kind: "index", Index: i}), out, includeMaps, includeSensitiveValues, directPaths); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendRecoveryValue(v cty.Value, path cty.Path, logical, label string, values *[]recoverySet) error {
	if !v.IsNull() && !v.IsWhollyKnown() {
		return fmt.Errorf("%s %q contains unknown values", label, logical)
	}
	raw, err := msgpack.Marshal(v, v.Type())
	if err != nil {
		return fmt.Errorf("encode %s %q: %w", label, logical, err)
	}
	rp, err := recoveryPath(path)
	if err != nil {
		return err
	}
	*values = append(*values, recoverySet{Path: rp, Value: raw})
	return nil
}

func uniqueRecoveryValues(values []recoverySet) []recoverySet {
	seen := make(map[string]bool, len(values))
	out := make([]recoverySet, 0, len(values))
	for _, value := range values {
		key := recoveryPathKey(value.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return recoveryPathKey(out[i].Path) < recoveryPathKey(out[j].Path)
	})
	return out
}
func directRecoveryPaths(values []recoverySet, s *Spec) []string {
	paths := make([]string, 0, len(values))
	for _, value := range values {
		logical := recoveryLogicalPath(value.Path)
		if coveredBySensitivePath(logical, s.paths) && !s.hasSetAtOrBelow(logical) {
			paths = append(paths, logical)
		}
	}
	return sortedUnique(paths)
}

func recoveryLogicalPath(path []recoveryPathStep) string {
	logical := ""
	for _, step := range path {
		if step.Kind == "attr" {
			logical = join(logical, step.Name)
		}
	}
	return logical
}

func appendRecoveryPath(path []recoveryPathStep, step recoveryPathStep) []recoveryPathStep {
	out := make([]recoveryPathStep, len(path), len(path)+1)
	copy(out, path)
	return append(out, step)
}

func recoveryPathKey(path []recoveryPathStep) string {
	encoded, _ := json.Marshal(path)
	return string(encoded)
}

func setJSONPath(root any, path []recoveryPathStep, value any) error {
	if len(path) == 0 {
		return errors.New("root sensitive recovery is unsupported")
	}
	current := root
	for i, step := range path {
		last := i == len(path)-1
		switch step.Kind {
		case "attr", "key":
			object, ok := current.(map[string]any)
			if !ok {
				return errors.New("recovery object path mismatch")
			}
			if _, ok := object[step.Name]; !ok {
				return errors.New("recovery object path missing")
			}
			if last {
				object[step.Name] = value
				return nil
			}
			current = object[step.Name]
		case "index":
			array, ok := current.([]any)
			if !ok || step.Index < 0 || step.Index >= len(array) {
				return errors.New("recovery array path mismatch")
			}
			if last {
				array[step.Index] = value
				return nil
			}
			current = array[step.Index]
		default:
			return errors.New("recovery path kind is invalid")
		}
	}
	return errors.New("recovery path is empty")
}

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("multiple JSON values")
	}
	return value, nil
}
func (s *Spec) capturesDirectValue(logical, instance string, v cty.Value, insideCaptured, sensitiveHere bool) bool {
	if insideCaptured || !sensitiveHere || s.hasSetAtOrBelow(logical) {
		return false
	}
	literal, exempt := s.exempt[instance]
	return !exempt || !literalMatches(v, literal)
}
