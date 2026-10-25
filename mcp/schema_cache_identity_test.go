package mcp

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	htmltemplate "html/template"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	texttemplate "text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	first "github.com/pbednarcik/flexmcp/mcp/testdata/schema_cache/first"
	second "github.com/pbednarcik/flexmcp/mcp/testdata/schema_cache/second"
)

type schemaCacheEnvelope[T any] struct {
	Value T `json:"value"`
}

// TestTypeKey_DistinguishesPackagesWithTheSameName pins that TypeKey names a
// type by its import path, so two packages with one name get two keys.
func TestTypeKey_DistinguishesPackagesWithTheSameName(t *testing.T) {
	assert.NotEqual(t, TypeKey[texttemplate.Template](), TypeKey[htmltemplate.Template]())
	assert.NotEqual(t, TypeKey[*texttemplate.Template](), TypeKey[*htmltemplate.Template]())
	assert.Equal(t, "text/template.Template", typeKeyName(t, TypeKey[texttemplate.Template]()))
	assert.Equal(t, "*html/template.Template", typeKeyName(t, TypeKey[*htmltemplate.Template]()))
	assert.Equal(t, "string", typeKeyName(t, TypeKey[string]()))
	assert.Equal(t, "map[string][]*[2]int", typeKeyName(t, TypeKey[map[string][]*[2]int]()))
}

// typeKeyName is the name part of key, a TypeKey, after checking that a
// fingerprint of 16 hexadecimal digits follows it. The name may hold a '#'
// itself, in a struct tag, so the fingerprint is found from the end.
func typeKeyName(t *testing.T, key string) string {
	t.Helper()
	name, fingerprint := typeKeyParts(t, key)
	require.Regexp(t, "^[0-9a-f]{16}$", fingerprint, "key %q", key)
	return name
}

// typeKeyParts splits key, a TypeKey, at its last '#'.
func typeKeyParts(t *testing.T, key string) (name, fingerprint string) {
	t.Helper()
	cut := strings.LastIndexByte(key, '#')
	require.GreaterOrEqual(t, cut, 0, "key %q has no fingerprint", key)
	return key[:cut], key[cut+1:]
}

func checkCachedSchemaIdentity[A, B any](t *testing.T) {
	t.Helper()
	assert.NotEqual(t, TypeKey[A](), TypeKey[B]())

	for _, warm := range []bool{false, true} {
		cache := NewSchemaCache()
		if warm {
			require.NoError(t, WarmFor[A](cache))
			require.NoError(t, WarmFor[B](cache))
			path := filepath.Join(t.TempDir(), "schemas.json")
			require.NoError(t, cache.Save(path))
			var err error
			cache, err = LoadSchemaCache(path)
			require.NoError(t, err)
		}

		wantA, err := json.Marshal(NewTool("a", WithInputSchema[A](), WithOutputSchema[A]()))
		require.NoError(t, err)
		wantB, err := json.Marshal(NewTool("b", WithInputSchema[B](), WithOutputSchema[B]()))
		require.NoError(t, err)
		gotA, err := json.Marshal(NewTool("a", WithCachedInputSchema[A](cache), WithCachedOutputSchema[A](cache)))
		require.NoError(t, err)
		gotB, err := json.Marshal(NewTool("b", WithCachedInputSchema[B](cache), WithCachedOutputSchema[B](cache)))
		require.NoError(t, err)
		assert.JSONEq(t, string(wantA), string(gotA), "warm=%v", warm)
		assert.JSONEq(t, string(wantB), string(gotB), "warm=%v", warm)
		assert.Equal(t, 2, cache.Len(), "warm=%v", warm)
	}
}

// TestCachedSchemas_DistinguishPackagesWithTheSameName pins that a cached
// schema is never served for a type from another package with the same name,
// however the type is wrapped.
func TestCachedSchemas_DistinguishPackagesWithTheSameName(t *testing.T) {
	t.Run("named", checkCachedSchemaIdentity[first.Request, second.Request])
	t.Run("pointer", checkCachedSchemaIdentity[*first.Request, *second.Request])
	t.Run("generic wrapper", checkCachedSchemaIdentity[schemaCacheEnvelope[first.Request], schemaCacheEnvelope[second.Request]])
	t.Run("anonymous struct", checkCachedSchemaIdentity[struct {
		Request first.Request `json:"request"`
	}, struct {
		Request second.Request `json:"request"`
	}])
	t.Run("slice field", checkCachedSchemaIdentity[struct {
		Requests []first.Request `json:"requests"`
	}, struct {
		Requests []second.Request `json:"requests"`
	}])
	t.Run("array field", checkCachedSchemaIdentity[struct {
		Requests [2]first.Request `json:"requests"`
	}, struct {
		Requests [2]second.Request `json:"requests"`
	}])
	t.Run("map field", checkCachedSchemaIdentity[struct {
		Requests map[string]first.Request `json:"requests"`
	}, struct {
		Requests map[string]second.Request `json:"requests"`
	}])
}

// TestCachedSchemas_OldKeysAreRegenerated pins that a cache saved under the
// old package-name keys still loads, and a lookup by the new key misses and
// regenerates rather than serving the old entry.
func TestCachedSchemas_OldKeysAreRegenerated(t *testing.T) {
	cache := NewSchemaCache()
	oldKey := reflect.TypeFor[second.Request]().String()
	cache.Warm(oldKey, SchemaFor[first.Request]())
	path := filepath.Join(t.TempDir(), "schemas.json")
	require.NoError(t, cache.Save(path))
	loaded, err := LoadSchemaCache(path)
	require.NoError(t, err)

	want, err := json.Marshal(NewTool("t", WithInputSchema[second.Request](), WithOutputSchema[second.Request]()))
	require.NoError(t, err)
	got, err := json.Marshal(NewTool("t", WithCachedInputSchema[second.Request](loaded), WithCachedOutputSchema[second.Request](loaded)))
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
	assert.True(t, loaded.Has(oldKey))
	assert.True(t, loaded.Has(TypeKey[second.Request]()))
}

// firstLocalRequest and secondLocalRequest declare a Request type each, in
// different functions of one package: one name, one import path, two
// structures.
func firstLocalRequest(cache *SchemaCache) (key string, cached, fresh *Tool) {
	type Request struct {
		Query string `json:"query"`
	}
	cachedTool := NewTool("first", WithCachedInputSchema[Request](cache), WithCachedOutputSchema[Request](cache))
	freshTool := NewTool("first", WithInputSchema[Request](), WithOutputSchema[Request]())
	return TypeKey[Request](), &cachedTool, &freshTool
}

func secondLocalRequest(cache *SchemaCache) (key string, cached, fresh *Tool) {
	type Request struct {
		Count int `json:"count"`
	}
	cachedTool := NewTool("second", WithCachedInputSchema[Request](cache), WithCachedOutputSchema[Request](cache))
	freshTool := NewTool("second", WithInputSchema[Request](), WithOutputSchema[Request]())
	return TypeKey[Request](), &cachedTool, &freshTool
}

// TestCachedSchemas_DistinguishLocalTypesWithTheSameName pins that two types
// declared with one name in different functions of one package get two keys,
// so neither is served the other's cached schema.
func TestCachedSchemas_DistinguishLocalTypesWithTheSameName(t *testing.T) {
	cache := NewSchemaCache()
	firstKey, firstCached, firstFresh := firstLocalRequest(cache)
	secondKey, secondCached, secondFresh := secondLocalRequest(cache)
	assert.NotEqual(t, firstKey, secondKey)
	assert.Equal(t, 2, cache.Len())

	for _, pair := range [][2]*Tool{{firstFresh, firstCached}, {secondFresh, secondCached}} {
		want, err := json.Marshal(pair[0])
		require.NoError(t, err)
		got, err := json.Marshal(pair[1])
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(got))
	}
}

// thirdLocalRequest declares a Request with the structure of
// firstLocalRequest's, in a third function.
func thirdLocalRequest(cache *SchemaCache) (key string, cached, fresh *Tool) {
	type Request struct {
		Query string `json:"query"`
	}
	cachedTool := NewTool("first", WithCachedInputSchema[Request](cache), WithCachedOutputSchema[Request](cache))
	freshTool := NewTool("first", WithInputSchema[Request](), WithOutputSchema[Request]())
	return TypeKey[Request](), &cachedTool, &freshTool
}

// TestCachedSchemas_ShareKeysForTheSameStructure pins that two types with one
// name and one structure share a key, and that the schema one cached is the
// other's own.
func TestCachedSchemas_ShareKeysForTheSameStructure(t *testing.T) {
	cache := NewSchemaCache()
	firstKey, _, _ := firstLocalRequest(cache)
	thirdKey, thirdCached, thirdFresh := thirdLocalRequest(cache)
	assert.Equal(t, firstKey, thirdKey)
	assert.Equal(t, 1, cache.Len())
	want, err := json.Marshal(thirdFresh)
	require.NoError(t, err)
	got, err := json.Marshal(thirdCached)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
}

// TestCachedSchemas_DistinguishRevisitedTypesWithTheSameName pins that a named
// type met again in a type is told apart from another of the same name: two
// Requests hold one string Node and one int Node each, both declared Node in
// this function, and differ only in which of the two their Chosen field
// repeats. They get two keys, and each its own schema.
func TestCachedSchemas_DistinguishRevisitedTypesWithTheSameName(t *testing.T) {
	type Node struct {
		Value string `json:"value"`
	}
	stringNode := Node{}
	{
		type Node struct {
			Value int `json:"value"`
		}
		checkRevisitedTypes(t, stringNode, Node{})
	}
}

func checkRevisitedTypes[A, B any](t *testing.T, _ A, _ B) {
	t.Helper()
	cache := NewSchemaCache()
	firstKey, firstCached, firstFresh := chosenFirstRequest[A, B](cache)
	secondKey, secondCached, secondFresh := chosenSecondRequest[A, B](cache)
	assert.NotEqual(t, firstKey, secondKey)
	assert.Equal(t, 2, cache.Len())

	firstWant, err := json.Marshal(firstFresh)
	require.NoError(t, err)
	secondWant, err := json.Marshal(secondFresh)
	require.NoError(t, err)
	require.NotEqual(t, string(firstWant), string(secondWant), "the two Requests must have two schemas")
	firstGot, err := json.Marshal(firstCached)
	require.NoError(t, err)
	secondGot, err := json.Marshal(secondCached)
	require.NoError(t, err)
	assert.JSONEq(t, string(firstWant), string(firstGot))
	assert.JSONEq(t, string(secondWant), string(secondGot))
}

// chosenFirstRequest and chosenSecondRequest declare a Request each, holding
// an A and a B, whose Chosen field repeats the A in one and the B in the
// other.
func chosenFirstRequest[A, B any](cache *SchemaCache) (key string, cached, fresh *Tool) {
	type Request struct {
		First  A `json:"first"`
		Second B `json:"second"`
		Chosen A `json:"chosen"`
	}
	cachedTool := NewTool("chosen", WithCachedInputSchema[Request](cache))
	freshTool := NewTool("chosen", WithInputSchema[Request]())
	return TypeKey[Request](), &cachedTool, &freshTool
}

func chosenSecondRequest[A, B any](cache *SchemaCache) (key string, cached, fresh *Tool) {
	type Request struct {
		First  A `json:"first"`
		Second B `json:"second"`
		Chosen B `json:"chosen"`
	}
	cachedTool := NewTool("chosen", WithCachedInputSchema[Request](cache))
	freshTool := NewTool("chosen", WithInputSchema[Request]())
	return TypeKey[Request](), &cachedTool, &freshTool
}

// firstLocalWrapper and secondLocalWrapper declare one Request each, with the
// same field, of a type named Inner declared in each with a different
// structure: the fingerprint follows named types into their structure.
func firstLocalWrapper() string {
	type Inner struct {
		Query string `json:"query"`
	}
	type Request struct {
		Inner Inner `json:"inner"`
	}
	return TypeKey[Request]()
}

func secondLocalWrapper() string {
	type Inner struct {
		Count int `json:"count"`
	}
	type Request struct {
		Inner Inner `json:"inner"`
	}
	return TypeKey[Request]()
}

func TestTypeKey_FollowsNamedFieldTypes(t *testing.T) {
	firstKey, secondKey := firstLocalWrapper(), secondLocalWrapper()
	assert.Equal(t, typeKeyName(t, firstKey), typeKeyName(t, secondKey))
	assert.NotEqual(t, firstKey, secondKey)
}

// TestTypeKey_FingerprintCoversWhatSchemasRead pins that every property of a
// type that schema generation reads changes the fingerprint, and that an
// unexported field, which it leaves out, does not.
func TestTypeKey_FingerprintCoversWhatSchemasRead(t *testing.T) {
	fingerprint := func(key string) string {
		_, fingerprint := typeKeyParts(t, key)
		return fingerprint
	}
	differ := []struct {
		name string
		a, b string
	}{
		{"field name", TypeKey[struct{ A int }](), TypeKey[struct{ B int }]()},
		{"field order", TypeKey[struct{ A, B int }](), TypeKey[struct{ B, A int }]()},
		{"json tag", TypeKey[struct {
			A int `json:"a"`
		}](), TypeKey[struct {
			A int `json:"b,omitempty"`
		}]()},
		{"jsonschema tag", TypeKey[struct {
			A int `jsonschema:"one"`
		}](), TypeKey[struct {
			A int `jsonschema:"two"`
		}]()},
		{"embedded field", TypeKey[struct{ first.Request }](), TypeKey[struct{ Request first.Request }]()},
		{"field type", TypeKey[struct{ A int }](), TypeKey[struct{ A string }]()},
		{"pointer", TypeKey[struct{ A *int }](), TypeKey[struct{ A int }]()},
		{"array length", TypeKey[struct{ A [2]int }](), TypeKey[struct{ A [3]int }]()},
		{"slice element", TypeKey[[]int](), TypeKey[[]string]()},
		{"map key", TypeKey[map[string]int](), TypeKey[map[int]int]()},
		{"map value", TypeKey[map[string]int](), TypeKey[map[string]bool]()},
		{"package", TypeKey[first.Request](), TypeKey[second.Request]()},
		{"channel element", TypeKey[chan texttemplate.Template](), TypeKey[chan htmltemplate.Template]()},
		{"channel direction", TypeKey[chan int](), TypeKey[<-chan int]()},
		{"function parameter", TypeKey[func(texttemplate.Template)](), TypeKey[func(htmltemplate.Template)]()},
		{"function result", TypeKey[func() int](), TypeKey[func() string]()},
		{"variadic function", TypeKey[func(...int)](), TypeKey[func([]int)]()},
		{"interface method", TypeKey[interface{ M(texttemplate.Template) }](), TypeKey[interface{ M(htmltemplate.Template) }]()},
	}
	for _, tt := range differ {
		assert.NotEqual(t, fingerprint(tt.a), fingerprint(tt.b), tt.name)
	}

	assert.Equal(t, firstHiddenField(), secondHiddenField())
	assert.Equal(t, fingerprint(TypeKey[struct {
		A int `json:"a"`
		b int
	}]()), fingerprint(TypeKey[struct {
		A int `json:"a"`
		b string
	}]()))
}

// firstHiddenField and secondHiddenField declare one Hidden each, differing
// only in an unexported field, which schema generation leaves out.
func firstHiddenField() string {
	type Hidden struct {
		A int `json:"a"`
		b int
	}
	_ = Hidden{}.b
	return TypeKey[Hidden]()
}

func secondHiddenField() string {
	type Hidden struct {
		A int `json:"a"`
		b string
	}
	_ = Hidden{}.b
	return TypeKey[Hidden]()
}

// schemaCacheNode refers to itself, through a slice and a pointer.
type schemaCacheNode struct {
	Name   string            `json:"name"`
	Kids   []schemaCacheNode `json:"kids,omitempty"`
	Parent *schemaCacheNode  `json:"parent,omitempty"`
}

// schemaCacheEven and schemaCacheOdd refer to each other.
type schemaCacheEven struct {
	Next *schemaCacheOdd `json:"next,omitempty"`
}

type schemaCacheOdd struct {
	Next *schemaCacheEven `json:"next,omitempty"`
}

func firstLocalNode() string {
	type Node struct {
		Kids  []Node `json:"kids"`
		Label string `json:"label"`
	}
	return TypeKey[Node]()
}

func secondLocalNode() string {
	type Node struct {
		Kids   []Node `json:"kids"`
		Weight int    `json:"weight"`
	}
	return TypeKey[Node]()
}

// TestTypeKey_RecursiveTypesTerminate pins that a type met again while it is
// spelled is spelled by its number alone, so recursive types get a key, and
// two recursive types with one name and different structures get two.
func TestTypeKey_RecursiveTypesTerminate(t *testing.T) {
	const node = "github.com/pbednarcik/flexmcp/mcp.schemaCacheNode"
	assert.Equal(t,
		node+`=struct{Name string "json:\"name\"";Kids []@1 "json:\"kids,omitempty\"";Parent *@1 "json:\"parent,omitempty\"";}`,
		string(typeSpelling(reflect.TypeFor[schemaCacheNode]())))
	assert.Equal(t, node, typeKeyName(t, TypeKey[schemaCacheNode]()))
	assert.Equal(t, "[]"+node, typeKeyName(t, TypeKey[[]schemaCacheNode]()))

	const even, odd = "github.com/pbednarcik/flexmcp/mcp.schemaCacheEven", "github.com/pbednarcik/flexmcp/mcp.schemaCacheOdd"
	assert.Equal(t,
		even+`=struct{Next *`+odd+`=struct{Next *@1 "json:\"next,omitempty\"";} "json:\"next,omitempty\"";}`,
		string(typeSpelling(reflect.TypeFor[schemaCacheEven]())))
	assert.NotEqual(t, TypeKey[schemaCacheEven](), TypeKey[schemaCacheOdd]())

	assert.NotEqual(t, firstLocalNode(), secondLocalNode())
}

// TestTypeKey_IsAFixedString pins keys and their spellings literally, one of
// them with a type met again, so a change to the spelling or the hash, which
// would make every saved cache miss, shows here first. The hash is checked
// against hash/fnv apart.
func TestTypeKey_IsAFixedString(t *testing.T) {
	const name = "github.com/pbednarcik/flexmcp/mcp/testdata/schema_cache/first.Request"
	const spelling = name + `=struct{Query string "json:\"query\"";}`
	assert.Equal(t, spelling, string(typeSpelling(reflect.TypeFor[first.Request]())))

	hash := fnv.New64a()
	_, _ = hash.Write([]byte(spelling))
	assert.Equal(t, fmt.Sprintf("%s#%016x", name, hash.Sum64()), TypeKey[first.Request]())
	assert.Equal(t, name+"#7f0f18b152329f02", TypeKey[first.Request]())

	assert.Equal(t, "map[string][]*[2]int", string(typeSpelling(reflect.TypeFor[map[string][]*[2]int]())))
	assert.Equal(t, "map[string][]*[2]int#a5d9175ed36fc179", TypeKey[map[string][]*[2]int]())

	const node = "github.com/pbednarcik/flexmcp/mcp.schemaCacheNode"
	const nodeSpelling = node + `=struct{Name string "json:\"name\"";Kids []@1 "json:\"kids,omitempty\"";Parent *@1 "json:\"parent,omitempty\"";}`
	assert.Equal(t, nodeSpelling, string(typeSpelling(reflect.TypeFor[schemaCacheNode]())))
	hash.Reset()
	_, _ = hash.Write([]byte(nodeSpelling))
	assert.Equal(t, fmt.Sprintf("%s#%016x", node, hash.Sum64()), TypeKey[schemaCacheNode]())
	assert.Equal(t, node+"#e65d2aab73f08db3", TypeKey[schemaCacheNode]())
}
