// Package server provides MCP (Model Context Protocol) server implementations.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	neturl "net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/pbednarcik/flexmcp/internal/argsdecode"
	"github.com/pbednarcik/flexmcp/mcp"
)

// compiledSchemaCache compiles and caches JSON Schema validators keyed by
// tool name, one per name: the compilation of the tool's current schema.
// Registration caches a batch's schemas only once the batch is registered,
// so a refused batch leaves the cache as it was. A tool registered again with
// a different schema replaces its entry, and one registered without a schema
// drops it, so the cache holds no schema a tool no longer has; deleting the
// tool drops it too.
//
// The same cache type backs both input and output schema validation; each
// validator owns its own cache instance so a tool's input and output schemas
// do not collide.
type compiledSchemaCache struct {
	mu     sync.RWMutex
	cached map[string]*cachedToolSchema
}

// cachedToolSchema records the result of compiling the schema whose digest
// it carries. compileErr carries a failed compilation to its caller; only a
// registered tool's schema, which compiled, is cached.
type cachedToolSchema struct {
	digest     string
	compiled   *jsonschema.Schema
	compileErr error
}

// newCompiledSchemaCache returns a fresh empty schema cache.
func newCompiledSchemaCache() *compiledSchemaCache {
	return &compiledSchemaCache{
		cached: make(map[string]*cachedToolSchema),
	}
}

// invalidate drops cached compilations for the given tool names. Used when
// tools are re-registered or removed so that stale schemas are not reused.
func (c *compiledSchemaCache) invalidate(names ...string) {
	if c == nil || len(names) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range names {
		delete(c.cached, name)
	}
}

// compile returns the compilation of schemaJSON for the tool called toolName
// without caching it: the cached entry when it holds the same schema, else a
// fresh one. Registration compiles with it and caches what it compiled with
// keep once the tool is registered; a call that finds no schema kept compiles
// with it too, and so caches nothing.
func (c *compiledSchemaCache) compile(toolName string, schemaJSON []byte) *cachedToolSchema {
	digest := schemaDigest(schemaJSON)
	c.mu.RLock()
	entry, ok := c.cached[toolName]
	c.mu.RUnlock()
	if ok && entry.digest == digest {
		return entry
	}
	compiled, compileErr := compileSchema(toolName, schemaJSON)
	return &cachedToolSchema{digest: digest, compiled: compiled, compileErr: compileErr}
}

// keep caches entry as the tool called toolName's; a nil entry, for a tool
// without the schema, drops the name's.
func (c *compiledSchemaCache) keep(toolName string, entry *cachedToolSchema) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry == nil {
		delete(c.cached, toolName)
		return
	}
	c.cached[toolName] = entry
}

// schema returns the compiled schema e holds, nil for a nil e.
func (e *cachedToolSchema) schema() *jsonschema.Schema {
	if e == nil {
		return nil
	}
	return e.compiled
}

// inputSchemaValidator validates tool call arguments against the tool's
// declared input schema, reusing a compiledSchemaCache to amortize the
// cost of JSON Schema compilation across calls.
type inputSchemaValidator struct {
	*compiledSchemaCache
}

// newInputSchemaValidator returns a fresh validator with an empty cache.
func newInputSchemaValidator() *inputSchemaValidator {
	return &inputSchemaValidator{compiledSchemaCache: newCompiledSchemaCache()}
}

// invalidate drops cached compilations for the given tool names. The
// receiver-level method preserves the historical nil-safety guarantee: a
// nil *inputSchemaValidator is a valid no-op.
func (v *inputSchemaValidator) invalidate(names ...string) {
	if v == nil {
		return
	}
	v.compiledSchemaCache.invalidate(names...)
}

// keep caches the input schema registration compiled for the tool called
// name, once the tool is registered. Safe to call on a nil receiver.
func (v *inputSchemaValidator) keep(name string, entry *cachedToolSchema) {
	if v == nil {
		return
	}
	v.compiledSchemaCache.keep(name, entry)
}

// compileAtRegistration compiles a tool's input schema when the validator is
// on, so a schema that cannot be compiled (an unsupported dialect, a $ref the
// compiler will not fetch) is refused at registration rather than served
// unvalidated (BaseProtocol.md rows 32 and 34). It returns the compilation,
// nil for a tool with none, uncached: once the tool is registered the server
// keeps the schema with the tool, so no call marshals, hashes or compiles it
// again, and caches it with keep. A nil validator compiles nothing.
func (v *inputSchemaValidator) compileAtRegistration(tool mcp.Tool) (*cachedToolSchema, error) {
	if v == nil {
		return nil, nil
	}
	schemaJSON, ok := schemaJSONFor(tool)
	if !ok {
		return nil, nil
	}
	entry := v.compile(tool.Name, schemaJSON)
	if entry.compileErr != nil {
		return nil, fmt.Errorf("tool %q input schema cannot be compiled: %w", tool.Name, entry.compileErr)
	}
	return entry, nil
}

// validate validates raw arguments against the tool's input schema, read
// from the tool and compiled uncached. A schema that cannot be compiled was
// refused at registration; should it fail anyway, validation is skipped
// rather than failing the call. The boolean
// return reports whether validation actually ran; the error is set only when
// validation ran and the arguments did not satisfy the schema.
func (v *inputSchemaValidator) validate(tool mcp.Tool, args callArguments) (bool, error) {
	schemaJSON, ok := schemaJSONFor(tool)
	if !ok {
		return false, nil
	}

	entry := v.compile(tool.Name, schemaJSON)
	if entry.compileErr != nil {
		return false, nil
	}
	return validateArguments(entry.compiled, args)
}

// validateRegistered validates raw arguments against the schema registration
// compiled for the tool; kept reports whether registration kept one. A tool
// without a kept schema falls back to validate, which reads the schema from
// the tool, so a registration path that forgets the cache degrades to the
// per-call read rather than switching validation off.
func (v *inputSchemaValidator) validateRegistered(tool mcp.Tool, compiled *jsonschema.Schema, kept bool, args callArguments) (bool, error) {
	if !kept {
		return v.validate(tool, args)
	}
	return validateArguments(compiled, args)
}

// validateArguments validates raw arguments against a compiled input schema;
// a nil schema validates nothing. The results are validate's.
func validateArguments(compiled *jsonschema.Schema, args callArguments) (bool, error) {
	if compiled == nil {
		return false, nil
	}

	value, bounded, err := args.normalize()
	if err != nil {
		return true, fmt.Errorf("invalid arguments: %w", err)
	}

	validate := validateTree
	if bounded {
		validate = validateRead
	}
	if err := validate(compiled, value); err != nil {
		return true, formatValidationError(err)
	}
	return true, nil
}

// validateTree validates value, a tree jsonschema.UnmarshalJSON built or the
// decoded arguments of a call, against compiled, as validateRead does once
// argumentsPastBounds finds nothing to refuse.
func validateTree(compiled *jsonschema.Schema, value any) error {
	if argumentsPastBounds(value, 1) {
		return findArgumentPastBounds(value, nil, 1)
	}
	return validateRead(compiled, value)
}

// validateRead validates value, a validator tree, against compiled.
// jsonschema v6.0.3 reads every number a schema checks through big.Rat and
// panics on one big.Rat cannot read; that panic is recovered here, around
// Validate alone, and the number is the error. Any other panic is raised
// again (blameUnreadableNumber).
func validateRead(compiled *jsonschema.Schema, value any) error {
	recovered, err := validateRecovering(compiled, value)
	if recovered == nil {
		return err
	}
	return blameUnreadableNumber(value, recovered, func(zeroed any) *recoveredPanic {
		again, _ := validateRecovering(compiled, zeroed)
		return again
	})
}

// validateRecovering runs compiled.Validate on value and returns what a panic
// in it raised, nil when it returned.
func validateRecovering(compiled *jsonschema.Schema, value any) (recovered *recoveredPanic, err error) {
	defer func() {
		if r := recover(); r != nil {
			recovered = &recoveredPanic{value: r, stack: debug.Stack()}
		}
	}()
	return nil, compiled.Validate(value)
}

// recoveredPanic is a panic recovered from the validator or the compiler,
// with the stack it was raised at, raised again when no unreadable number
// raised it.
type recoveredPanic struct {
	value any
	stack []byte
}

// Error renders the panic's value and the stack it was raised at.
func (p *recoveredPanic) Error() string {
	return fmt.Sprintf("%v\n\npanicked at:\n%s", p.value, p.stack)
}

// Unwrap returns the panic's value when it is an error.
func (p *recoveredPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// blameUnreadableNumber returns the first number in value that big.Rat cannot
// read when such a number raised recovered, a panic of a run on value: when
// run, on a copy of value with every such number read as 0, raises none. Any
// other panic is raised again, its value and stack with it.
func blameUnreadableNumber(value any, recovered *recoveredPanic, run func(zeroed any) *recoveredPanic) *numberOutOfRange {
	outOfRange := findNumberOutOfRange(value, nil)
	if outOfRange == nil || run(unreadableNumbersAsZero(value)) != nil {
		panic(recovered)
	}
	return outOfRange
}

// unreadableNumbersAsZero returns a copy of value, a validator tree, in which
// every number big.Rat cannot read is 0; value itself is left as it is.
func unreadableNumbersAsZero(value any) any {
	return unreadableNumbersReplaced(value, func() json.Number { return "0" })
}

// unreadableNumbersReplaced returns a copy of value, a validator tree, in
// which every number big.Rat cannot read is the next number next makes;
// value itself is left as it is.
func unreadableNumbersReplaced(value any, next func() json.Number) any {
	switch v := value.(type) {
	case json.Number:
		if _, ok := new(big.Rat).SetString(string(v)); !ok {
			return next()
		}
	case []any:
		replaced := make([]any, len(v))
		for i, item := range v {
			replaced[i] = unreadableNumbersReplaced(item, next)
		}
		return replaced
	case map[string]any:
		replaced := make(map[string]any, len(v))
		for key, member := range v {
			replaced[key] = unreadableNumbersReplaced(member, next)
		}
		return replaced
	}
	return value
}

// numberOutOfRange is a number in a value to validate, or in a schema, that
// big.Rat cannot read, at the place it stands: one whose exponent does not
// fit an int64 or, unless it is zero, whose scale is past a million either
// way, such as 0e10000000000000000000 or 1e-1000001, though float64 reads
// both as 0, or, in a call's arguments, one past numberPastBounds' bounds. It
// fails validation as a value that does not fit the schema does.
type numberOutOfRange struct {
	at      []string
	literal string
}

// maxShownNumber bounds how much of a number's literal a failure repeats.
const maxShownNumber = 64

// Error renders the failure as formatLeafMessage renders the validator's.
func (e *numberOutOfRange) Error() string {
	literal := e.literal
	if len(literal) > maxShownNumber {
		literal = literal[:maxShownNumber] + "..."
	}
	return validationLine(e.at, "number "+literal+" is out of range")
}

// nestedTooDeep is an object or array in a call's arguments nested past
// maxArgumentDepth, at the place it stands.
type nestedTooDeep struct {
	at []string
}

// Error renders the failure as formatLeafMessage renders the validator's.
func (e *nestedTooDeep) Error() string {
	return validationLine(e.at, "value is nested deeper than "+strconv.Itoa(maxArgumentDepth)+" levels")
}

// hasUnreadableNumber reports whether value, a validator tree, holds a number
// big.Rat cannot read: findNumberOutOfRange's answer, without the order and
// the place it finds one in.
func hasUnreadableNumber(value any) bool {
	switch v := value.(type) {
	case json.Number:
		_, ok := new(big.Rat).SetString(string(v))
		return !ok
	case []any:
		return slices.ContainsFunc(v, hasUnreadableNumber)
	case map[string]any:
		for _, member := range v {
			if hasUnreadableNumber(member) {
				return true
			}
		}
	}
	return false
}

// findNumberOutOfRange returns the first number in value, a validator tree,
// that big.Rat cannot read, objects' members taken in key order, or nil.
// at is the place value stands.
func findNumberOutOfRange(value any, at []string) *numberOutOfRange {
	switch v := value.(type) {
	case json.Number:
		if _, ok := new(big.Rat).SetString(string(v)); !ok {
			return &numberOutOfRange{at: slices.Clone(at), literal: string(v)}
		}
	case []any:
		for i, item := range v {
			if found := findNumberOutOfRange(item, append(at, strconv.Itoa(i))); found != nil {
				return found
			}
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if found := findNumberOutOfRange(v[key], append(at, key)); found != nil {
				return found
			}
		}
	}
	return nil
}

// A call's arguments are refused before the validator reads them when they
// hold a number past either bound below or nest past maxArgumentDepth.
// jsonschema v6.0.3 reads a number through big.Rat, whose work grows with its
// digits and, unless it is zero, its decimal scale; each node of its error
// tree copies the instance path, so its memory grows with the square of the
// depth. A float64, written shortest or exactly, has at most 1075 digits and a
// scale within 1074 either way; the request's decode refuses one past range.
const (
	maxNumberDigits  = 1100
	maxNumberScale   = 1100
	maxArgumentDepth = 128
)

// numberPastBounds reports whether literal has more than maxNumberDigits
// digits before its exponent or, unless they are all zero, a scale (its
// exponent less its fraction's digits) past maxNumberScale either way. A
// literal that is no JSON number's is left to big.Rat.
func numberPastBounds(literal string) bool {
	i := 0
	if i < len(literal) && (literal[i] == '-' || literal[i] == '+') {
		i++
	}
	digits, fraction, nonzero, inFraction := 0, 0, false, false
	for ; i < len(literal) && literal[i] != 'e' && literal[i] != 'E'; i++ {
		switch c := literal[i]; {
		case '0' <= c && c <= '9':
			digits++
			if inFraction {
				fraction++
			}
			nonzero = nonzero || c != '0'
		case c == '.' && !inFraction:
			inFraction = true
		default:
			return false
		}
	}
	if digits > maxNumberDigits {
		return true
	}
	// Past saturated no fraction within maxNumberDigits brings the scale back.
	const saturated = maxNumberScale + maxNumberDigits + 1
	exponent, negative := 0, false
	if i < len(literal) {
		i++
		if i < len(literal) && (literal[i] == '-' || literal[i] == '+') {
			negative = literal[i] == '-'
			i++
		}
		for ; i < len(literal); i++ {
			c := literal[i]
			if c < '0' || c > '9' {
				return false
			}
			exponent = min(exponent*10+int(c-'0'), saturated)
		}
	}
	if !nonzero {
		return false
	}
	if negative {
		exponent = -exponent
	}
	scale := exponent - fraction
	return scale > maxNumberScale || scale < -maxNumberScale
}

// argumentsPastBounds reports whether value, a call's arguments as the
// validator reads them at level, holds a number past the bounds or an object
// or array past maxArgumentDepth. It neither sorts nor allocates.
func argumentsPastBounds(value any, level int) bool {
	switch v := value.(type) {
	case json.Number:
		return numberPastBounds(string(v))
	case []any:
		if level > maxArgumentDepth {
			return true
		}
		for _, item := range v {
			if argumentsPastBounds(item, level+1) {
				return true
			}
		}
	case map[string]any:
		if level > maxArgumentDepth {
			return true
		}
		for _, member := range v {
			if argumentsPastBounds(member, level+1) {
				return true
			}
		}
	}
	return false
}

// findArgumentPastBounds returns the first number or nesting in value that
// argumentsPastBounds finds, members in key order, or nil. at and level are
// the place value stands.
func findArgumentPastBounds(value any, at []string, level int) error {
	switch v := value.(type) {
	case json.Number:
		if numberPastBounds(string(v)) {
			return &numberOutOfRange{at: slices.Clone(at), literal: string(v)}
		}
	case []any:
		if level > maxArgumentDepth {
			return &nestedTooDeep{at: slices.Clone(at)}
		}
		for i, item := range v {
			if err := findArgumentPastBounds(item, append(at, strconv.Itoa(i)), level+1); err != nil {
				return err
			}
		}
	case map[string]any:
		if level > maxArgumentDepth {
			return &nestedTooDeep{at: slices.Clone(at)}
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if err := findArgumentPastBounds(v[key], append(at, key), level+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// schemaDigest returns a stable hex-encoded SHA-256 of the canonical schema
// bytes. The cache compares a digest rather than the raw bytes so it stays
// cheap to look up even for large schemas; collisions are not a
// security concern because both sides of the comparison are produced by the
// same mcp-go server process.
func schemaDigest(schemaJSON []byte) string {
	sum := sha256.Sum256(schemaJSON)
	return hex.EncodeToString(sum[:])
}

// schemaJSONFor returns the JSON representation of the tool's input schema.
// It prefers RawInputSchema when set, otherwise marshals the structured
// InputSchema. The boolean return is false when the tool has no usable input
// schema (e.g. an empty structured schema with no properties).
func schemaJSONFor(tool mcp.Tool) ([]byte, bool) {
	if len(tool.RawInputSchema) > 0 {
		return tool.RawInputSchema, true
	}
	if tool.InputSchema.Type == "" && tool.InputSchema.Types == nil && len(tool.InputSchema.Properties) == 0 && len(tool.InputSchema.Required) == 0 && tool.InputSchema.AdditionalProperties == nil && len(tool.InputSchema.AdditionalFields) == 0 {
		return nil, false
	}
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, false
	}
	return data, true
}

// compileSchema parses a JSON Schema document and compiles it for the given
// tool. The schema is registered against an opaque virtual URL so that
// "$ref" resolution works without touching the filesystem or network.
func compileSchema(toolName string, schemaJSON []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("decode tool %q input schema: %w", toolName, err)
	}
	resourceURL := fmt.Sprintf("mem:///flexmcp/tools/%s/input-schema.json", toolName)
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, fmt.Errorf("register tool %q input schema: %w", toolName, err)
	}
	recovered, compiled, err := compileRecovering(c, resourceURL)
	if recovered != nil {
		// Compiling validates the schema against its metaschema, which reads
		// its numeric keywords through big.Rat and panics on a bound big.Rat
		// cannot read; that bound is the error. Any other panic is raised again.
		outOfRange := blameUnreadableNumber(doc, recovered, func(zeroed any) *recoveredPanic {
			c := jsonschema.NewCompiler()
			if c.AddResource(resourceURL, zeroed) != nil {
				return nil
			}
			again, _, _ := compileRecovering(c, resourceURL)
			return again
		})
		return nil, fmt.Errorf("compile tool %q input schema: %w", toolName, outOfRange)
	}
	if err != nil {
		return nil, fmt.Errorf("compile tool %q input schema: %w", toolName, err)
	}
	// A bound the metaschema does not read as a number compiles, and the
	// compiler drops one big.Rat cannot read: it is refused as well.
	if outOfRange := findBoundOutOfRange(doc, resourceURL); outOfRange != nil {
		return nil, fmt.Errorf("compile tool %q input schema: %w", toolName, outOfRange)
	}
	return compiled, nil
}

// findBoundOutOfRange returns a bound in doc, the schema compiled at
// resourceURL, whose number big.Rat cannot read, nil when there is none: a
// bound the compiler dropped. Which keywords are bounds, and where, is what
// jsonschema compiles for the schema's dialect, so it is read from a
// compilation of doc in which every such number is a distinct positive
// integer, and each bound that compilation holds is looked up in doc. A
// number where the dialect reads no bound, as in an annotation, an unknown
// keyword or a keyword of a later dialect, is no bound. Schemas are taken
// depth first, members in key order.
func findBoundOutOfRange(doc any, resourceURL string) *numberOutOfRange {
	if !hasUnreadableNumber(doc) {
		return nil
	}
	n := 0
	readable := unreadableNumbersReplaced(doc, func() json.Number {
		n++
		return json.Number(strconv.Itoa(n))
	})
	c := jsonschema.NewCompiler()
	if c.AddResource(resourceURL, readable) != nil {
		return nil
	}
	recovered, compiled, err := compileRecovering(c, resourceURL)
	if recovered != nil || err != nil {
		return nil
	}
	seen := map[*jsonschema.Schema]bool{}
	var find func(s *jsonschema.Schema) *numberOutOfRange
	find = func(s *jsonschema.Schema) *numberOutOfRange {
		if s == nil || seen[s] {
			return nil
		}
		seen[s] = true
		at, ok := schemaPointer(s.Location, resourceURL)
		if !ok {
			return nil
		}
		if object, ok := valueAt(doc, at).(map[string]any); ok {
			for _, bound := range compiledBounds(s) {
				if number, ok := object[bound].(json.Number); ok {
					if found := findNumberOutOfRange(number, append(at, bound)); found != nil {
						return found
					}
				}
			}
		}
		for _, sub := range subschemas(s) {
			if found := find(sub); found != nil {
				return found
			}
		}
		return nil
	}
	return find(compiled)
}

// compiledBounds returns the keywords s, a compiled schema, holds bounds of.
// Draft 4 reads an exclusive bound from maximum or minimum.
func compiledBounds(s *jsonschema.Schema) []string {
	var bounds []string
	add := func(held bool, keywords ...string) {
		if held {
			bounds = append(bounds, keywords...)
		}
	}
	add(s.MultipleOf != nil, "multipleOf")
	add(s.Maximum != nil, "maximum")
	add(s.ExclusiveMaximum != nil, "exclusiveMaximum", "maximum")
	add(s.Minimum != nil, "minimum")
	add(s.ExclusiveMinimum != nil, "exclusiveMinimum", "minimum")
	add(s.MinLength != nil, "minLength")
	add(s.MaxLength != nil, "maxLength")
	add(s.MinItems != nil, "minItems")
	add(s.MaxItems != nil, "maxItems")
	add(s.MinProperties != nil, "minProperties")
	add(s.MaxProperties != nil, "maxProperties")
	add(s.MinContains != nil, "minContains")
	add(s.MaxContains != nil, "maxContains")
	return bounds
}

// subschemas returns the schemas s, a compiled schema, applies or refers to,
// members in key order.
func subschemas(s *jsonschema.Schema) []*jsonschema.Schema {
	subs := []*jsonschema.Schema{s.Ref, s.RecursiveRef, s.Not, s.If, s.Then, s.Else, s.PropertyNames,
		s.UnevaluatedProperties, s.Contains, s.Items2020, s.UnevaluatedItems, s.ContentSchema}
	if s.DynamicRef != nil {
		subs = append(subs, s.DynamicRef.Ref)
	}
	subs = append(subs, s.AllOf...)
	subs = append(subs, s.AnyOf...)
	subs = append(subs, s.OneOf...)
	subs = append(subs, s.PrefixItems...)
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		subs = append(subs, s.Properties[name])
	}
	for _, name := range slices.Sorted(maps.Keys(s.DependentSchemas)) {
		subs = append(subs, s.DependentSchemas[name])
	}
	for _, name := range slices.Sorted(maps.Keys(s.Dependencies)) {
		if sub, ok := s.Dependencies[name].(*jsonschema.Schema); ok {
			subs = append(subs, sub)
		}
	}
	patterns := slices.SortedFunc(maps.Keys(s.PatternProperties), func(a, b jsonschema.Regexp) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, pattern := range patterns {
		subs = append(subs, s.PatternProperties[pattern])
	}
	for _, sub := range []any{s.AdditionalProperties, s.AdditionalItems, s.Items} {
		switch sub := sub.(type) {
		case *jsonschema.Schema:
			subs = append(subs, sub)
		case []*jsonschema.Schema:
			subs = append(subs, sub...)
		}
	}
	return subs
}

// schemaPointer returns the place in the document at resourceURL that
// location, a compiled schema's, names, and whether it names one there.
func schemaPointer(location, resourceURL string) ([]string, bool) {
	fragment, ok := strings.CutPrefix(location, resourceURL+"#")
	if !ok {
		return nil, false
	}
	pointer, err := neturl.PathUnescape(fragment)
	if err != nil {
		return nil, false
	}
	if pointer == "" {
		return nil, true
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, token := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens, true
}

// valueAt returns the member of doc at the place at names, nil for none.
func valueAt(doc any, at []string) any {
	for _, token := range at {
		switch v := doc.(type) {
		case map[string]any:
			doc = v[token]
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			doc = v[i]
		default:
			return nil
		}
	}
	return doc
}

// compileRecovering compiles the schema at resourceURL with c and returns what
// a panic in Compile raised, nil when it returned.
func compileRecovering(c *jsonschema.Compiler, resourceURL string) (recovered *recoveredPanic, compiled *jsonschema.Schema, err error) {
	defer func() {
		if r := recover(); r != nil {
			recovered = &recoveredPanic{value: r, stack: debug.Stack()}
		}
	}()
	compiled, err = c.Compile(resourceURL)
	return nil, compiled, err
}

// callArguments is what input validation reads of a call: raw, the arguments
// as the client sent them, or, when raw is nil, decoded, the arguments as the
// request holds them. With raw set, decoded is the request's Arguments when
// they are still the decode of raw, else nil. The raw bytes travel typed, so
// no call boxes them into an interface to reach the validator.
type callArguments struct {
	raw     json.RawMessage
	decoded any
}

// normalize returns the arguments in the form the JSON Schema validator
// reads, as normalizeArgumentsForValidation does for either form. Arguments
// sent as raw are read as sent, numbers exact: from their decode when every
// number in them reads the same there (scanArguments), else from a
// second decode that keeps each number's literal. bounded reports that the
// scan of raw found nothing argumentsPastBounds would.
func (a callArguments) normalize() (value any, bounded bool, err error) {
	if a.raw == nil {
		value, err = normalizeArgumentsForValidation(a.decoded)
		return value, false, err
	}
	if a.decoded != nil {
		if exact, shallow := scanArguments(a.raw); exact {
			return a.decoded, shallow, nil
		}
	}
	value, err = normalizeRawArguments(a.raw)
	return value, false, err
}

// argumentsToValidate is what input validation reads of a call: the arguments
// as the client sent them, which need no encoding again to be read and keep
// their numbers exact, unless a hook that runs before the call holds the
// request and could have changed them, or they are null, which validates as
// an empty object. Sent as raw, they carry their decode too: with no such
// hook nothing writes Arguments between the request's decode and validation,
// since middlewares run after it.
//
// The call of a tool that reads its arguments on demand (onDemand) carries
// no Arguments unless a hook set them: validation reads the getters' map,
// built here and shared with the handler, or, for arguments that are not an
// object, their decode, as a call decoded at once carries it, so that it
// reads what it reads without the opt-in.
func (s *MCPServer) argumentsToValidate(request mcp.CallToolRequest, onDemand bool) callArguments {
	raw := request.Params.RawArguments
	arguments := request.Params.Arguments
	if arguments == nil && len(raw) > 0 {
		if m := request.GetArguments(); m != nil {
			arguments = m
		}
	}
	if len(raw) == 0 || string(raw) == "null" || hooksSeeCallArguments(s.hooks) {
		if arguments == nil && onDemand && len(raw) > 0 && string(raw) != "null" {
			arguments = decodedArguments(request.Params)
		}
		return callArguments{decoded: arguments}
	}
	return callArguments{raw: raw, decoded: arguments}
}

// markRoundTrip marks params, through argsdecode, when a before-hook is
// registered and the call carries raw arguments other than null: validation
// then read a round trip of them. Call it once validation ran, whether the
// call passed or not.
func (s *MCPServer) markRoundTrip(params *mcp.CallToolParams) {
	if hooksSeeCallArguments(s.hooks) && len(params.RawArguments) > 0 && string(params.RawArguments) != "null" {
		// MarkRoundTrip is an indirect call, so what it is given escapes. The
		// copy is declared here, so that only a call that is marked pays for
		// it, and the caller's request stays on its stack.
		marked := *params
		argsdecode.MarkRoundTrip(&marked)
		*params = marked
	}
}

// hooksSeeCallArguments reports whether a hook runs before a tools/call with
// the request in hand.
func hooksSeeCallArguments(hooks *Hooks) bool {
	return hooks != nil && (len(hooks.OnBeforeAny) > 0 || len(hooks.OnBeforeCallTool) > 0)
}

// normalizeArgumentsForValidation converts the raw arguments value (which may
// be a json.RawMessage, a map, or already-decoded slice/scalars) into the
// loosely-typed representation that the JSON Schema validator expects. Missing
// arguments are normalised to an empty object so that schemas can still
// validate `required` constraints sensibly.
func normalizeArgumentsForValidation(rawArgs any) (any, error) {
	if rawArgs == nil {
		return map[string]any{}, nil
	}
	if raw, ok := rawArgs.(json.RawMessage); ok {
		return normalizeRawArguments(raw)
	}
	encoded, err := json.Marshal(rawArgs)
	if err != nil {
		return nil, err
	}
	return normalizeRawArguments(encoded)
}

// normalizeRawArguments decodes raw arguments for the validator, numbers kept
// exact, as jsonschema.UnmarshalJSON does: in one pass when mcp's decoder
// settles them (argsdecode.Numbers), else with jsonschema.UnmarshalJSON
// itself. Blank arguments are an empty object.
func normalizeRawArguments(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	if value, ok := argsdecode.Numbers(raw); ok {
		return value, nil
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// scanArguments reports whether the validator reads every number in raw, a
// JSON text, the same from the float64 decoding raw into an any makes of it as
// from its literal (exact), and whether no object or array in it nests past
// maxArgumentDepth (shallow). The validator reads a float64 through big.Rat as
// the shortest decimal that rounds to it: 2^53+1 would be read as 2^53.
//
// The rule is on the literal of each number outside a string: it has no
// exponent ('e' or 'E') and at most 15 digits. Every digit counts: the
// integer part's, its single leading zero included, and the fraction's, its
// leading and trailing zeros included. The minus sign and the decimal point
// are not digits. So 0.00000000000001 (15 digits) passes, 0.000000000000001
// and 9007199254740993 (16 digits each) do not, and neither does 1e3.
//
// Such a literal is zero or lies in [1e-14, 1e15), inside float64's normal
// range, where a decimal of at most 15 significant digits is the shortest
// decimal of the float64 nearest it, up to trailing zeros: 10^15 < 2^52, so no
// two such decimals round to the same float64. A minus zero reads as zero
// either way. The scan reads raw once and allocates nothing.
func scanArguments(raw []byte) (exact, shallow bool) {
	depth, deepest := 0, 0
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		case c == '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' {
					i++
				}
			}
		case c == '-' || '0' <= c && c <= '9':
			digits := 0
			for ; i < len(raw); i++ {
				d := raw[i]
				if '0' <= d && d <= '9' {
					digits++
					continue
				}
				if d == 'e' || d == 'E' {
					return false, false
				}
				if d != '-' && d != '.' {
					break
				}
			}
			if digits > 15 {
				return false, false
			}
			i--
		}
	}
	return true, deepest <= maxArgumentDepth
}

// A failing call's feedback is bounded: the first maxValidationMessages
// failures, each cut at maxValidationMessageBytes, then the count of the
// rest. The validator reports every failing array item, and each report
// repeats the schema's own enum or pattern, so unbounded feedback would let
// a request of a few kilobytes draw a response of hundreds of megabytes.
// A failure's path is cut before its reason, never below minShownLocation.
const (
	maxValidationMessages     = 10
	maxValidationMessageBytes = 512
	minShownLocation          = 64
)

// formatValidationError turns the validator's structured error into a single
// flat message that's friendly for an LLM to read. Each individual constraint
// violation is rendered as `<path>: <message>` and joined with semicolons.
func formatValidationError(err error) error {
	if outOfRange, ok := errors.AsType[*numberOutOfRange](err); ok {
		return fmt.Errorf("input schema validation failed: %s", outOfRange.Error())
	}
	if tooDeep, ok := errors.AsType[*nestedTooDeep](err); ok {
		return fmt.Errorf("input schema validation failed: %s", tooDeep.Error())
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return fmt.Errorf("input schema validation failed: %w", err)
	}
	var parts []string
	rest := collectValidationMessages(verr, &parts)
	if len(parts) == 0 {
		return fmt.Errorf("input schema validation failed: %s", verr.Error())
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", rest))
	}
	return fmt.Errorf("input schema validation failed: %s", strings.Join(parts, "; "))
}

// collectValidationMessages walks the leaf causes of a validation error tree
// and appends one message per leaf to *out, the first maxValidationMessages
// of them, returning how many leaves were left unrendered. Non-leaf nodes
// don't carry useful information for the model (they're aggregator nodes
// like "anyOf failed").
func collectValidationMessages(verr *jsonschema.ValidationError, out *[]string) int {
	if verr == nil {
		return 0
	}
	if len(verr.Causes) == 0 {
		if len(*out) >= maxValidationMessages {
			return 1
		}
		*out = append(*out, formatLeafMessage(verr))
		return 0
	}
	rest := 0
	for _, cause := range verr.Causes {
		rest += collectValidationMessages(cause, out)
	}
	return rest
}

// validationMessages renders the validator's error kinds as English
// sentences, the form a model can act on.
var validationMessages = message.NewPrinter(language.English)

func formatLeafMessage(verr *jsonschema.ValidationError) string {
	return validationLine(verr.InstanceLocation, verr.ErrorKind.LocalizedString(validationMessages))
}

// validationLine renders one failure as `<path>: <text>` in at most
// maxValidationMessageBytes and a "...": the path is cut to what the text
// leaves it, then the line.
func validationLine(at []string, text string) string {
	location := jsonPointer(at)
	if location == "" {
		location = "<root>"
	}
	msg := location + ": " + text
	if room := max(maxValidationMessageBytes-len("...: ")-len(text), minShownLocation); len(msg) > maxValidationMessageBytes && len(location) > room {
		msg = cutAtRune(location, room) + "...: " + text
	}
	if len(msg) > maxValidationMessageBytes {
		msg = cutAtRune(msg, maxValidationMessageBytes) + "..."
	}
	return msg
}

// cutAtRune returns s, longer than n bytes, cut to at most n at a rune's start.
func cutAtRune(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// jsonPointer renders a list of path segments as a RFC 6901 JSON Pointer.
// Empty paths render as the empty string (the validator's convention for the
// root document) which the caller substitutes for "<root>".
func jsonPointer(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, seg := range segments {
		b.WriteByte('/')
		b.WriteString(escapeJSONPointer(seg))
	}
	return b.String()
}

func escapeJSONPointer(seg string) string {
	seg = strings.ReplaceAll(seg, "~", "~0")
	seg = strings.ReplaceAll(seg, "/", "~1")
	return seg
}

// validationToolResult builds the SEP-1303 compliant tool execution error
// surfaced when input schema validation fails. It uses the standard
// NewToolResultError helper so validation failures are shaped identically
// to errors produced by tool handlers themselves.
func validationToolResult(err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(err.Error())
}
