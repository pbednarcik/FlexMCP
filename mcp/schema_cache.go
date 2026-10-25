package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// SchemaCache stores pre-computed JSON schemas for tool input/output types.
//
// SchemaCache is intended to amortise the cost of JSON-Schema reflection in
// stateless or serverless deployments (e.g., AWS Lambda, Google Cloud
// Functions) where servers are reconstructed on every invocation. Schemas can
// be warmed once at build time, persisted to disk via [SchemaCache.Save], and
// reloaded at start-up via [LoadSchemaCache]. Cached schemas are then consumed
// by [WithCachedInputSchema] and [WithCachedOutputSchema] in lieu of the
// reflection performed by [WithInputSchema] and [WithOutputSchema].
//
// SchemaCache is safe for concurrent use by multiple goroutines.
type SchemaCache struct {
	mu      sync.RWMutex
	schemas map[string]json.RawMessage
}

// NewSchemaCache creates a new empty SchemaCache.
func NewSchemaCache() *SchemaCache {
	return &SchemaCache{schemas: make(map[string]json.RawMessage)}
}

// Warm stores the JSON schema for the given type name. Schemas are typically
// produced by [SchemaFor]. Passing a nil schema removes any existing entry
// for typeName.
func (c *SchemaCache) Warm(typeName string, schema map[string]any) {
	if c == nil {
		return
	}
	if schema == nil {
		c.mu.Lock()
		delete(c.schemas, typeName)
		c.mu.Unlock()
		return
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return
	}
	c.WarmRaw(typeName, raw)
}

// WarmRaw stores a pre-marshaled JSON schema under the given type name.
// This avoids an unmarshal/marshal round-trip when callers already hold the
// schema as raw JSON. Passing a nil or empty schema removes any existing
// entry for typeName.
func (c *SchemaCache) WarmRaw(typeName string, schema json.RawMessage) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schemas == nil {
		c.schemas = make(map[string]json.RawMessage)
	}
	if len(schema) == 0 {
		delete(c.schemas, typeName)
		return
	}
	// Copy to avoid aliasing the caller's slice.
	clone := make(json.RawMessage, len(schema))
	copy(clone, schema)
	c.schemas[typeName] = clone
}

// Get retrieves a cached schema by type name as a generic map. The second
// return value reports whether the entry was present.
func (c *SchemaCache) Get(typeName string) (map[string]any, bool) {
	raw, ok := c.GetRaw(typeName)
	if !ok {
		return nil, false
	}
	out := make(map[string]any)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// GetRaw retrieves a cached schema by type name as raw JSON. The returned
// slice is a copy and may be safely mutated by the caller.
func (c *SchemaCache) GetRaw(typeName string) (json.RawMessage, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	raw, ok := c.schemas[typeName]
	if !ok {
		return nil, false
	}
	clone := make(json.RawMessage, len(raw))
	copy(clone, raw)
	return clone, true
}

// Has reports whether a schema is cached under typeName.
func (c *SchemaCache) Has(typeName string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.schemas[typeName]
	return ok
}

// Len returns the number of cached schemas.
func (c *SchemaCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.schemas)
}

// Keys returns the cached type names in sorted order. The result is a fresh
// slice and may be modified by the caller.
func (c *SchemaCache) Keys() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.schemas))
	for k := range c.schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MarshalJSON serialises the cache as a JSON object whose keys are type
// names and whose values are the cached JSON schemas. Keys are emitted in
// sorted order so that the encoded form is deterministic and diff-friendly.
func (c *SchemaCache) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("{}"), nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]string, 0, len(c.schemas))
	for k := range c.schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := make([]byte, 0, 64+len(c.schemas)*64)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		keyBytes, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("marshal schema cache key: %w", err)
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		buf = append(buf, c.schemas[k]...)
	}
	buf = append(buf, '}')
	return buf, nil
}

// UnmarshalJSON loads a previously serialised cache, replacing any existing
// contents. The input must be a JSON object whose values are themselves JSON
// schemas; values are stored verbatim as raw JSON.
func (c *SchemaCache) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("mcp: SchemaCache.UnmarshalJSON on nil receiver")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode schema cache: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.schemas = make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		clone := make(json.RawMessage, len(v))
		copy(clone, v)
		c.schemas[k] = clone
	}
	return nil
}

// Save writes the cache to a file as JSON. Parent directories are created if
// needed. The file is written atomically by writing to a temporary sibling
// and renaming it into place.
func (c *SchemaCache) Save(path string) error {
	if c == nil {
		return errors.New("mcp: SchemaCache.Save on nil receiver")
	}
	data, err := c.MarshalJSON()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create schema cache directory: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".schema-cache-*.json")
	if err != nil {
		return fmt.Errorf("create temp schema cache file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write schema cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close schema cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename schema cache into place: %w", err)
	}
	return nil
}

// LoadSchemaCache reads a cache from a file produced by [SchemaCache.Save].
func LoadSchemaCache(path string) (*SchemaCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema cache: %w", err)
	}
	c := NewSchemaCache()
	if err := c.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return c, nil
}

// TypeKey returns the canonical cache key for the Go type T as derived from
// reflection: the type's name, then '#' and a fingerprint of its structure,
// e.g. "example.com/app/tools.WeatherInput#1f3a5c7e9b0d2f41".
//
// The name spells named types with their full import path, inside pointers,
// slices, arrays, maps and anonymous structs too, so two packages with the
// same name never share a key. The fingerprint is a hash of everything schema
// generation reads from the type: kinds, the names, tags, embedding and types
// of exported and embedded struct fields, element and key types, array
// lengths, and the same for every type T refers to. So two types declared
// with one name in different functions of a package get different keys, and
// stay apart however often T refers to each of them. Two types with the same
// name and structure may share a key, which is safe because their schemas are
// equal. The key holds no addresses and depends on no map order, so it is the
// same in every process and on every platform: a cache saved after [WarmFor]
// still hits in another process for the same type.
//
// Renaming, moving or changing the type changes this key, so callers that
// need a stable identifier across refactors should pass an explicit key to
// [SchemaCache.Warm] and [WithCachedInputSchemaKey]. A cache saved under older
// keys, which named the package alone or carried no fingerprint, still loads;
// a lookup by the new key misses and regenerates the schema.
//
// TypeKey reflects over the whole type on every call; it is meant for tool
// construction, not for a request path.
func TypeKey[T any]() string {
	t := reflect.TypeFor[T]()
	var key strings.Builder
	// The name is t.String() with import paths for package names: sized from
	// it, the builder rarely grows twice.
	key.Grow(len(t.String()) + 64 + 1 + fingerprintDigits)
	writeTypeKey(&key, t)
	key.WriteByte('#')
	writeFingerprint(&key, t)
	return key.String()
}

// fingerprintDigits is the length of a TypeKey fingerprint: a 64-bit hash in
// hexadecimal.
const fingerprintDigits = 16

// writeFingerprint writes the fingerprint TypeKey gives t to key: the FNV-1a
// 64-bit hash of t's structural spelling, as fingerprintDigits lowercase
// hexadecimal digits.
func writeFingerprint(key *strings.Builder, t reflect.Type) {
	hash := fnv.New64a()
	_, _ = hash.Write(typeSpelling(t))
	sum := hash.Sum64()
	const digits = "0123456789abcdef"
	for shift := 4 * (fingerprintDigits - 1); shift >= 0; shift -= 4 {
		key.WriteByte(digits[(sum>>shift)&0xf])
	}
}

// typeSpelling is the structural spelling of t that TypeKey's fingerprint
// hashes.
func typeSpelling(t reflect.Type) []byte {
	speller := typeSpeller{spelling: make([]byte, 0, 256)}
	speller.spell(t)
	return speller.spelling
}

// typeSpeller spells a type's structure: what schema generation reads from
// it, and nothing that differs between processes or platforms.
//
// A named type is spelled by its import path and name, then '=' and its
// underlying structure. The walk numbers named types from 1 in the order it
// first meets them, an order that is fixed: fields in declaration order, a
// map's key before its element. A named type met again, as in a recursive
// type or in a second field of one type, is spelled '@' and its number:
//
//	example.com/app.Node=struct{Kids []@1 "";}
//
// So the spelling terminates and stays linear in the number of types, two
// distinct types with one name stay apart wherever they recur, and two types
// of one structure, numbered in one order, spell the same. Unexported fields
// that are not embedded are left out, as schema generation leaves them out.
// That keeps a standard type such as time.Time, whose schema is given by its
// identity, spelled the same whatever unexported fields a Go release gives
// it. Channels, functions and interfaces, which schema generation ignores or
// leaves unrestricted, are spelled in full all the same, so their keys name
// the types they hold.
type typeSpeller struct {
	spelling []byte
	// seen holds the named types met so far, in the order met: a type's
	// number is its index plus one.
	seen []reflect.Type
}

// spell appends the spelling of t.
func (s *typeSpeller) spell(t reflect.Type) {
	if name := t.Name(); name != "" {
		pkg := t.PkgPath()
		if pkg == "" {
			// A predeclared type: its name is its structure.
			s.spelling = append(s.spelling, name...)
			return
		}
		if number := slices.Index(s.seen, t); number >= 0 {
			s.spelling = append(s.spelling, '@')
			s.spelling = strconv.AppendInt(s.spelling, int64(number+1), 10)
			return
		}
		s.seen = append(s.seen, t)
		s.spelling = append(s.spelling, pkg...)
		s.spelling = append(s.spelling, '.')
		s.spelling = append(s.spelling, name...)
		s.spelling = append(s.spelling, '=')
	}
	switch t.Kind() {
	case reflect.Pointer:
		s.spelling = append(s.spelling, '*')
		s.spell(t.Elem())
	case reflect.Slice:
		s.spelling = append(s.spelling, "[]"...)
		s.spell(t.Elem())
	case reflect.Array:
		s.spelling = append(s.spelling, '[')
		s.spelling = strconv.AppendInt(s.spelling, int64(t.Len()), 10)
		s.spelling = append(s.spelling, ']')
		s.spell(t.Elem())
	case reflect.Map:
		s.spelling = append(s.spelling, "map["...)
		s.spell(t.Key())
		s.spelling = append(s.spelling, ']')
		s.spell(t.Elem())
	case reflect.Chan:
		s.spelling = append(s.spelling, t.ChanDir().String()...)
		s.spelling = append(s.spelling, ' ')
		s.spell(t.Elem())
	case reflect.Func:
		s.spelling = append(s.spelling, "func("...)
		for in := range t.Ins() {
			s.spell(in)
			s.spelling = append(s.spelling, ',')
		}
		if t.IsVariadic() {
			s.spelling = append(s.spelling, "..."...)
		}
		s.spelling = append(s.spelling, ")("...)
		for out := range t.Outs() {
			s.spell(out)
			s.spelling = append(s.spelling, ',')
		}
		s.spelling = append(s.spelling, ')')
	case reflect.Interface:
		s.spelling = append(s.spelling, "interface{"...)
		for method := range t.Methods() {
			if method.PkgPath != "" {
				s.spelling = append(s.spelling, method.PkgPath...)
				s.spelling = append(s.spelling, '.')
			}
			s.spelling = append(s.spelling, method.Name...)
			s.spelling = append(s.spelling, ' ')
			s.spell(method.Type)
			s.spelling = append(s.spelling, ';')
		}
		s.spelling = append(s.spelling, '}')
	case reflect.Struct:
		s.spelling = append(s.spelling, "struct{"...)
		for field := range t.Fields() {
			if !field.Anonymous && !field.IsExported() {
				continue
			}
			if field.Anonymous {
				s.spelling = append(s.spelling, "embed "...)
			}
			if field.PkgPath != "" {
				s.spelling = append(s.spelling, field.PkgPath...)
				s.spelling = append(s.spelling, '.')
			}
			s.spelling = append(s.spelling, field.Name...)
			s.spelling = append(s.spelling, ' ')
			s.spell(field.Type)
			s.spelling = append(s.spelling, ' ')
			s.spelling = strconv.AppendQuote(s.spelling, string(field.Tag))
			s.spelling = append(s.spelling, ';')
		}
		s.spelling = append(s.spelling, '}')
	default:
		s.spelling = append(s.spelling, t.Kind().String()...)
	}
}

// writeTypeKey writes the name part of t's TypeKey to key.
func writeTypeKey(key *strings.Builder, t reflect.Type) {
	if name := t.Name(); name != "" {
		if pkg := t.PkgPath(); pkg != "" {
			key.WriteString(pkg)
			key.WriteByte('.')
		}
		key.WriteString(name)
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		key.WriteByte('*')
		writeTypeKey(key, t.Elem())
	case reflect.Slice:
		key.WriteString("[]")
		writeTypeKey(key, t.Elem())
	case reflect.Array:
		key.WriteByte('[')
		key.WriteString(strconv.Itoa(t.Len()))
		key.WriteByte(']')
		writeTypeKey(key, t.Elem())
	case reflect.Map:
		key.WriteString("map[")
		writeTypeKey(key, t.Key())
		key.WriteByte(']')
		writeTypeKey(key, t.Elem())
	case reflect.Struct:
		key.WriteString("struct {")
		for i := range t.NumField() {
			field := t.Field(i)
			if i > 0 {
				key.WriteByte(';')
			}
			key.WriteByte(' ')
			if !field.Anonymous {
				if field.PkgPath != "" {
					key.WriteString(field.PkgPath)
					key.WriteByte('.')
				}
				key.WriteString(field.Name)
				key.WriteByte(' ')
			}
			writeTypeKey(key, field.Type)
			if field.Tag != "" {
				key.WriteByte(' ')
				key.WriteString(strconv.Quote(string(field.Tag)))
			}
		}
		key.WriteString(" }")
	default:
		key.WriteString(t.String())
	}
}

// SchemaFor returns the JSON schema generated for the Go type T, formatted as
// a generic map suitable for [SchemaCache.Warm]. It returns nil if reflection
// or marshalling fails. Use [SchemaForRaw] when an error is desired.
func SchemaFor[T any]() map[string]any {
	raw, err := SchemaForRaw[T]()
	if err != nil {
		return nil
	}
	out := make(map[string]any)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// SchemaForRaw returns the JSON schema generated for the Go type T as raw
// JSON. It is the lower-level building block underlying [SchemaFor],
// [WithInputSchema] and [WithOutputSchema].
func SchemaForRaw[T any]() (json.RawMessage, error) {
	return schemaForRawMessage[T]()
}

// WarmFor computes the schema for T via reflection and stores it in cache.
// If key is provided, the first non-empty entry is used as the cache key;
// otherwise [TypeKey] is used. WarmFor is a no-op when cache is nil. It
// returns an error only when schema generation itself fails.
func WarmFor[T any](cache *SchemaCache, key ...string) error {
	if cache == nil {
		return nil
	}
	raw, err := SchemaForRaw[T]()
	if err != nil {
		return err
	}
	k := TypeKey[T]()
	for _, candidate := range key {
		if candidate != "" {
			k = candidate
			break
		}
	}
	cache.WarmRaw(k, raw)
	return nil
}

// WithCachedInputSchema is a cache-aware variant of [WithInputSchema]. It
// looks up the schema for T in cache using [TypeKey]; on a miss it falls
// back to reflection and stores the freshly computed schema back into the
// cache so that subsequent invocations are served from memory. Passing a nil
// cache is equivalent to [WithInputSchema].
func WithCachedInputSchema[T any](cache *SchemaCache) ToolOption {
	return WithCachedInputSchemaKey[T](cache, TypeKey[T]())
}

// WithCachedInputSchemaKey is a variant of [WithCachedInputSchema] that uses
// an explicit cache key instead of one derived from T's type name. Use this
// when stability of the key across renames or package moves matters.
//
// An empty key disables the cache entirely for this call: the schema is
// always reflected fresh and never read from or written to cache. This
// mirrors [WarmFor] and prevents distinct types from colliding on a shared
// empty-string cache slot.
func WithCachedInputSchemaKey[T any](cache *SchemaCache, key string) ToolOption {
	return func(t *Tool) {
		var (
			raw json.RawMessage
			ok  bool
		)
		if key != "" {
			raw, ok = cache.GetRaw(key)
		}
		if !ok {
			fresh, err := SchemaForRaw[T]()
			if err != nil {
				return
			}
			raw = fresh
			if key != "" {
				cache.WarmRaw(key, fresh)
			}
		}
		t.InputSchema.Type = ""
		t.InputSchema.Types = nil
		t.InputSchema.AdditionalFields = nil
		t.RawInputSchema = raw
	}
}

// WithCachedOutputSchema is a cache-aware variant of [WithOutputSchema]. It
// looks up the schema for T in cache using [TypeKey]; on a miss it falls
// back to reflection and stores the freshly computed schema back into the
// cache. Passing a nil cache is equivalent to [WithOutputSchema].
func WithCachedOutputSchema[T any](cache *SchemaCache) ToolOption {
	return WithCachedOutputSchemaKey[T](cache, TypeKey[T]())
}

// WithCachedOutputSchemaKey is a variant of [WithCachedOutputSchema] that
// uses an explicit cache key instead of one derived from T's type name.
//
// An empty key disables the cache entirely for this call: the schema is
// always reflected fresh and never read from or written to cache. This
// mirrors [WarmFor] and prevents distinct types from colliding on a shared
// empty-string cache slot.
func WithCachedOutputSchemaKey[T any](cache *SchemaCache, key string) ToolOption {
	return func(t *Tool) {
		var (
			raw json.RawMessage
			ok  bool
		)
		if key != "" {
			raw, ok = cache.GetRaw(key)
		}
		if !ok {
			fresh, err := SchemaForRaw[T]()
			if err != nil {
				return
			}
			raw = fresh
			if key != "" {
				cache.WarmRaw(key, fresh)
			}
		}
		// Decoded into a fresh schema: decoding into the one set before
		// would merge the two (upstream #1055).
		var outputSchema ToolOutputSchema
		if err := json.Unmarshal(raw, &outputSchema); err != nil {
			return
		}
		t.OutputSchema = outputSchema
		// Always set the type to "object" as of the current MCP spec.
		// https://modelcontextprotocol.io/specification/2025-06-18/server/tools#output-schema
		t.OutputSchema.Type = "object"
		// Decoding the generated schema recorded struct field order. Clear it
		// so these tools keep emitting properties sorted by name.
		t.OutputSchema.PropertyOrder = nil
	}
}
