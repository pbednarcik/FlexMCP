package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// InputValidationFailure is what input validation found wrong with a call's
// arguments, as an input validation handler sees it: a bounded list of
// violations and what the bounds left out. It is built within fixed budgets
// whatever the request and the schema hold: at most 10 violations, 16 KiB and
// 512 values written in previews. The budgets count the bytes of the field
// values, each string's bytes and each number's decimal digits; their JSON
// encoding may take more where characters are escaped.
type InputValidationFailure struct {
	// Violations holds at most 10 violations, the least first: by their
	// pointer's segments in byte order, then their schema location, then
	// their keyword. A violation whose pointer is unknown comes after every
	// other.
	Violations []InputViolation
	// Omitted counts the violations left out. It is exact unless Incomplete.
	Omitted int
	// Incomplete reports that validation stopped early, so Omitted is a lower
	// bound: the arguments were refused before the validator read them.
	Incomplete bool
	// Cut reports that a budget shortened a field or left an entry out.
	Cut bool
}

// InputViolation is one way a call's arguments fail the tool's input schema.
// Each string is at most 256 bytes, cut on a UTF-8 boundary, but Message;
// each list holds at most 20 entries, and the field values take at most 2 KiB,
// though their JSON encoding may take more where characters are escaped. A
// preview is JSON text; a cut preview is a prefix of it, not JSON.
type InputViolation struct {
	// Pointer is an RFC 6901 JSON Pointer into the arguments validation
	// checked, "" for the root.
	Pointer string
	// PointerUnknown reports that the library gives no pointer that holds and
	// none can be derived exactly; Pointer is then "".
	PointerUnknown bool
	// Keyword names what failed: a schema keyword such as "type", "required"
	// or "maximum", "false" for a false schema, "unevaluatedProperties" for a
	// member its false refused, "refCycle" or "invalidJsonValue" for a failure
	// of the validator itself, or, for arguments refused before the validator
	// read them, "numberOutOfRange", "nestedTooDeep" or "unencodable".
	Keyword string
	// SchemaLocation is an RFC 6901 JSON Pointer into the tool's input schema
	// to the failing keyword, such as "/properties/x/anyOf/1/type"; under a
	// $ref it points into the referenced schema.
	SchemaLocation string
	// Property is the property name the failure is about: the name a
	// propertyNames subschema failed, the member unevaluatedProperties
	// refused, or the property a keyword such as dependentRequired names.
	Property string
	// Expected is the schema's side.
	Expected ViolationExpected
	// Actual is the arguments' side, read from the arguments at Pointer.
	Actual ViolationActual
	// Message is FlexMCP's English line for the violation, as the default
	// refusal writes it: at most 512 bytes and a "...".
	Message string
	// Cut reports that a budget shortened a field or left an entry out.
	Cut bool
}

// ViolationExpected is what the schema asks for. A keyword sets the fields it
// has and leaves the rest zero.
type ViolationExpected struct {
	// Types are the JSON types the schema allows, integer included.
	Types []string
	// Names are the property names the schema requires.
	Names []string
	// Values are previews of the values the schema allows, the first of an
	// enum's or a const's.
	Values []string
	// Bound is the numeric bound a number failed.
	Bound *ViolationBound
	// Limit is the count the schema sets, such as minLength's or minItems'.
	Limit *int
	// Pattern is the regular expression a string failed.
	Pattern string
	// Format is the name of the format a value failed.
	Format string
	// Indexes are the subschemas of a oneOf that both matched.
	Indexes []int
	// Omitted counts the entries the budgets left out of the side's list.
	Omitted int
}

// ViolationBound is a numeric bound of a schema, as text.
type ViolationBound struct {
	// Text is the bound: its exact decimal when it has one of at most 256
	// bytes, else the shortest form of its float64 as encoding/json writes one
	// (1e+21), +Inf or -Inf past float64's range.
	Text string
	// Exact reports that Text is the bound's exact value.
	Exact bool
}

// ViolationActual is what the arguments hold at the violation's pointer. A
// keyword sets the fields it has and leaves the rest zero.
type ViolationActual struct {
	// Kind is the value's JSON type: null, boolean, number, string, array or
	// object.
	Kind string
	// Value is a preview of the value, set for a scalar only. A number reads
	// as its literal or as the shortest form of its float64.
	Value string
	// Count is the count the schema bounds: a string's length in characters,
	// an array's items or the items that matched contains, an object's
	// properties, or the extra items additionalItems refused.
	Count *int
	// Indexes are items of the array: those that matched contains, or the two
	// that are equal.
	Indexes []int
	// Names are properties the object holds that the schema does not allow,
	// the least in byte order.
	Names []string
	// Values are previews of items of the array: the first of two equal ones.
	Values []string
	// Omitted counts the entries the budgets left out of the side's lists.
	Omitted int
}

// The budgets of a handler's view of a failure, beyond the 10 violations of
// maxValidationMessages and the 512 bytes of each message.
const (
	maxSideEntries    = 20
	maxViolationText  = 256
	maxViolationBytes = 2 << 10
	maxFailureBytes   = 16 << 10
	maxFailureNodes   = 512
)

// inputInvalid is a call's arguments failing input validation. It answers the
// text the default refusal writes, and keeps what a handler's view of the
// failure is projected from (projectFailure): the failure validation returned,
// the instance it read and the schema it read it against, compiled at
// resourceURL. unencodable marks arguments that could not be read for
// validation, cause then being the reason.
type inputInvalid struct {
	cause       error
	instance    any
	resourceURL string
	schema      *cachedToolSchema
	text        string
	unencodable bool
}

// Error returns the text of the default refusal, built on its first read: a
// handler that answers the failure itself never needs it. The text is never
// empty, and e stays with the goroutine that handles the call, since a handler
// is given what projectFailure makes of e, never e.
func (e *inputInvalid) Error() string {
	if e.text == "" {
		e.text = validationText(e.cause)
	}
	return e.text
}

// Unwrap returns the failure validation returned.
func (e *inputInvalid) Unwrap() error { return e.cause }

// invalidInput is instance failing schema: cause is the validator's error or
// a pre-check's refusal.
func invalidInput(schema *cachedToolSchema, instance any, cause error) *inputInvalid {
	return &inputInvalid{
		cause:       cause,
		instance:    instance,
		resourceURL: strings.TrimSuffix(schema.compiled.Location, "#"),
		schema:      schema,
	}
}

// validationText is the text of cause as formatValidationError renders it.
func validationText(cause error) string {
	text, _ := formatValidationText(cause)
	return text
}

// unencodableInput is arguments that could not be read for validation
// against schema, such as in-process ones json.Marshal refuses.
func unencodableInput(schema *cachedToolSchema, err error) *inputInvalid {
	return &inputInvalid{
		cause:       err,
		resourceURL: strings.TrimSuffix(schema.compiled.Location, "#"),
		schema:      schema,
		text:        "invalid arguments: " + err.Error(),
		unencodable: true,
	}
}

// projectFailure is inv as an input validation handler sees it: the
// violations, ordered, within the budgets.
func projectFailure(inv *inputInvalid) InputValidationFailure {
	p := &projection{inv: inv}
	if inv.unencodable {
		message, cut := inv.text, false
		if len(message) > maxValidationMessageBytes {
			message, cut = cutAtRune(message, maxValidationMessageBytes)+"...", true
		}
		return p.preCheck(fixedPart{keyword: "unencodable", message: message, cut: cut}, nil)
	}
	if outOfRange, ok := errors.AsType[*numberOutOfRange](inv.cause); ok {
		return p.numberOutOfRange(outOfRange)
	}
	if tooDeep, ok := errors.AsType[*nestedTooDeep](inv.cause); ok {
		f := fixedAt(tooDeep.at, "nestedTooDeep", func(w *cappedWriter) {
			w.WriteString("value is nested deeper than " + strconv.Itoa(maxArgumentDepth) + " levels")
		})
		return p.preCheck(f, func(w *violationWriter) {
			w.str(&w.v.Actual.Kind, instanceKind(valueAt(inv.instance, tooDeep.at)))
		})
	}
	if verr, ok := errors.AsType[*jsonschema.ValidationError](inv.cause); ok {
		return p.tree(verr)
	}
	// No validation answers another failure: nothing is listed.
	return InputValidationFailure{Incomplete: true}
}

// projection selects the violations of a failure and builds their list.
type projection struct {
	inv *inputInvalid
	// kept holds the least violations found so far, at most
	// maxValidationMessages, the least first; total counts them all.
	kept  []violationSite
	total int
	// locations indexes a schema holding draft 4 by location; draft4 builds it,
	// at most once.
	locations map[string]*jsonschema.Schema
}

// violationSite is a node of the validator's error tree that is one
// violation, with what its ancestors tell of it, and its sort key once read.
type violationSite struct {
	node *jsonschema.ValidationError
	// names is the propertyNames node the violation is under, nil for none:
	// the node validated a property's name, not a value of the arguments.
	names *namesPlace
	// referenced marks a node whose schema a reference named: that schema
	// validated the referring schema's value, whatever place it sits in.
	referenced bool

	keyed    bool
	keyword  string
	property string
	// tokens is the place of the keyword within its schema (keywordTokens).
	tokens []string
}

// namesPlace is a propertyNames node a violation is under: the property name
// it validated, and the place of the object holding it when that is known.
type namesPlace struct {
	property string
	pointer  []string
	known    bool
}

// unknown reports whether the site's pointer is unknown.
func (s *violationSite) unknown() bool { return s.names != nil && !s.names.known }

// at is the site's place in the arguments: under propertyNames the object
// whose property name failed, else the node's own.
func (s *violationSite) at() []string {
	if s.names != nil {
		return s.names.pointer
	}
	return s.node.InstanceLocation
}

// numberOutOfRange projects a number refused before the validator read it.
func (p *projection) numberOutOfRange(e *numberOutOfRange) InputValidationFailure {
	literal, cut := e.literal, false
	if len(literal) > maxShownNumber {
		literal, cut = literal[:maxShownNumber], true
	}
	f := fixedAt(e.at, "numberOutOfRange", func(w *cappedWriter) {
		w.WriteString("number ")
		w.WriteString(literal)
		if cut {
			w.WriteString("...")
		}
		w.WriteString(" is out of range")
	})
	return p.preCheck(f, func(w *violationWriter) {
		w.str(&w.v.Actual.Kind, instanceKind(valueAt(p.inv.instance, e.at)))
		w.str(&w.v.Actual.Value, strings.Clone(literal))
		w.cut = w.cut || cut
	})
}

// preCheck is the failure of arguments refused before the validator ran: one
// violation, and Incomplete.
func (p *projection) preCheck(f fixedPart, sides func(w *violationWriter)) InputValidationFailure {
	b := newFailureBuilder()
	b.add(f, sides)
	b.failure.Incomplete = true
	return b.failure
}

// tree projects the validator's error tree: the least violations, each
// within the budgets, and the count of the rest.
func (p *projection) tree(verr *jsonschema.ValidationError) InputValidationFailure {
	p.walk(verr, nil, nil)
	b := newFailureBuilder()
	for i := range p.kept {
		site := &p.kept[i]
		b.add(p.fixed(site), func(w *violationWriter) { p.sides(w, site) })
	}
	b.failure.Omitted += p.total - len(p.kept)
	b.failure.Cut = b.failure.Cut || b.failure.Omitted > 0
	return b.failure
}

// walk visits node and the nodes under it once, adding each violation it
// finds. ancestors are the nodes from the root to node's parent, the context
// a violation under propertyNames is placed from (propertyNamesPointer);
// names is the propertyNames node node is under, nil for none.
//
// A node without causes is a violation, whatever its kind; so is a oneOf that
// matched twice. A node with causes adds its causes, but a contains,
// minContains or maxContains node is one violation: its causes say why items
// did not count, not what the call got wrong.
func (p *projection) walk(node *jsonschema.ValidationError, ancestors []*jsonschema.ValidationError, names *namesPlace) {
	switch k := node.ErrorKind.(type) {
	case *kind.Contains, *kind.MinContains, *kind.MaxContains:
		p.add(violationSite{node: node, names: names})
		return
	case *kind.PropertyNames:
		pointer, known := propertyNamesPointer(ancestors, node, p.inv.instance, p.inv.resourceURL)
		names = &namesPlace{property: k.Property, pointer: pointer, known: known}
	}
	if len(node.Causes) == 0 {
		p.add(violationSite{node: node, names: names, referenced: referencedBy(ancestors, node)})
		return
	}
	ancestors = append(ancestors, node)
	for _, cause := range node.Causes {
		p.walk(cause, ancestors, names)
	}
}

// referencedBy reports whether the last of chain, node's parent, is a
// reference to node's own schema.
func referencedBy(chain []*jsonschema.ValidationError, node *jsonschema.ValidationError) bool {
	if len(chain) == 0 {
		return false
	}
	reference, ok := chain[len(chain)-1].ErrorKind.(*kind.Reference)
	return ok && reference.URL == node.SchemaURL
}

// add counts s and keeps it if it is among the least so far: a bounded
// selection, so the walk keeps at most maxValidationMessages sites.
func (p *projection) add(s violationSite) {
	p.total++
	n := len(p.kept)
	if n == maxValidationMessages && p.compare(&s, &p.kept[n-1]) >= 0 {
		return
	}
	lo, hi := 0, n
	for lo < hi {
		if mid := int(uint(lo+hi) >> 1); p.compare(&s, &p.kept[mid]) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if p.kept == nil {
		p.kept = make([]violationSite, 0, maxValidationMessages)
	}
	if n == maxValidationMessages {
		p.kept = p.kept[:n-1]
	}
	p.kept = slices.Insert(p.kept, lo, s)
}

// compare orders sites: a known pointer before an unknown one, then by the
// pointer's segments in byte order, then by complete schema location, keyword
// and property. The location and the keyword are read only for sites at the
// same pointer.
func (p *projection) compare(a, b *violationSite) int {
	if a.unknown() != b.unknown() {
		if a.unknown() {
			return 1
		}
		return -1
	}
	if !a.unknown() {
		if c := slices.Compare(a.at(), b.at()); c != 0 {
			return c
		}
	}
	if c := p.compareLocations(a, b); c != 0 {
		return c
	}
	return cmp.Or(strings.Compare(a.keyword, b.keyword), strings.Compare(a.property, b.property))
}

// compareLocations orders a and b by their complete schema locations in byte
// order, reading them only as far as they differ. Sites of one schema differ
// at most in their keyword tokens, however long its URL.
func (p *projection) compareLocations(a, b *violationSite) int {
	x, y := p.location(a), p.location(b)
	if a.node.SchemaURL == b.node.SchemaURL {
		x.text, y.text = "", ""
	}
	for {
		c, more := x.next()
		d, moreOther := y.next()
		switch {
		case !more && !moreOther:
			return 0
		case !more:
			return -1
		case !moreOther:
			return 1
		case c != d:
			return cmp.Compare(c, d)
		}
	}
}

// key reads s's keyword, property and keyword tokens, once.
func (p *projection) key(s *violationSite) {
	if s.keyed {
		return
	}
	s.keyed = true
	s.keyword, s.property = keywordOf(s.node.ErrorKind)
	if s.names != nil {
		s.property = s.names.property
	} else if !s.referenced && refusedUnevaluated(s.node) {
		s.keyword = "unevaluatedProperties"
		if at := s.node.InstanceLocation; len(at) > 0 {
			s.property = at[len(at)-1]
		}
	}
	s.tokens = p.keywordTokens(s.node)
}

// refusedUnevaluated reports whether node, by its place, is a member refused by
// a false that is unevaluatedProperties' own subschema: the library validates
// each unevaluated member at its own place, so the member is the node's. A leaf
// of a subschema under the keyword keeps its own row, and the place says
// nothing of a false a reference named (violationSite.referenced).
func refusedUnevaluated(node *jsonschema.ValidationError) bool {
	if _, ok := node.ErrorKind.(*kind.FalseSchema); !ok {
		return false
	}
	_, fragment, ok := strings.Cut(node.SchemaURL, "#")
	if !ok {
		return false
	}
	location, ok := fragmentTokens(fragment)
	return ok && underUnevaluated(location)
}

// underUnevaluated reports whether location, a schema location's tokens, is
// unevaluatedProperties' own subschema: its last token is the keyword, in a
// keyword's place. A token is a keyword's place but after a keyword that maps
// names to schemas, where it is a name, or one that lists them, where it is
// an index; a draft-7 items lists them when the token after it is an index.
func underUnevaluated(location []string) bool {
	for i := 0; i < len(location); i++ {
		switch location[i] {
		case "unevaluatedProperties":
			if i == len(location)-1 {
				return true
			}
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies",
			"allOf", "anyOf", "oneOf", "prefixItems":
			i++
		case "items":
			if i+1 < len(location) && isIndex(location[i+1]) {
				i++
			}
		}
	}
	return false
}

// isIndex reports whether token is an array index: decimal digits only.
func isIndex(token string) bool {
	for i := range len(token) {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return token != ""
}

// propertyNamesPointer is the place of the object whose property name node, a
// propertyNames node, failed, and whether it is known exactly. chain is
// node's ancestors from the root, instance the arguments validated and
// resourceURL the tool's schema's.
//
// The library's own location for node is a slice that validating a later
// sibling overwrites, so it is never read. The place is taken from the
// nearest anchor on chain instead, a node holding its own copy of its place
// (propertyNamesAnchor). The pointer is the anchor's place followed by the
// names of the properties keywords between the anchor's schema and the
// propertyNames keyword's, and holds only if the arguments have an object
// there with the name. Every other case is unknown: the walk never goes past
// the anchor, since a node further up may sit at another place, the library
// making no node where it steps into a member.
func propertyNamesPointer(chain []*jsonschema.ValidationError, node *jsonschema.ValidationError, instance any, resourceURL string) (pointer []string, known bool) {
	names, ok := node.ErrorKind.(*kind.PropertyNames)
	if !ok {
		return nil, false
	}
	parent, ok := strings.CutSuffix(node.SchemaURL, "/propertyNames")
	if !ok {
		return nil, false
	}
	schema, at, ok := propertyNamesAnchor(chain, parent, resourceURL)
	if !ok {
		return nil, false
	}
	rest, ok := strings.CutPrefix(parent, schema)
	if !ok || rest != "" && rest[0] != '/' {
		return nil, false
	}
	tokens, ok := fragmentTokens(rest)
	if !ok || len(tokens)%2 != 0 {
		return nil, false
	}
	pointer = make([]string, 0, len(at)+len(tokens)/2)
	pointer = append(pointer, at...)
	for i := 0; i < len(tokens); i += 2 {
		if tokens[i] != "properties" {
			return nil, false
		}
		pointer = append(pointer, tokens[i+1])
	}
	object, ok := valueAt(instance, pointer).(map[string]any)
	if !ok {
		return nil, false
	}
	if _, ok := object[names.Property]; !ok {
		return nil, false
	}
	return pointer, true
}

// propertyNamesAnchor is the nearest anchor on chain for a propertyNames
// keyword of the schema at parent: its schema and its place in the arguments.
// An anchor is a $ref, whose schema is its target, validated against the
// same value; a group, whose schema is its own; or an anyOf, allOf or oneOf,
// whose schema is the branch parent runs through, and ok is false when parent
// runs through none. With none on chain, the anchor is the root schema at
// the root.
func propertyNamesAnchor(chain []*jsonschema.ValidationError, parent, resourceURL string) (schema string, at []string, ok bool) {
	for _, ancestor := range slices.Backward(chain) {
		var keyword string
		switch k := ancestor.ErrorKind.(type) {
		case *kind.Reference:
			return k.URL, ancestor.InstanceLocation, true
		case *kind.Group:
			return ancestor.SchemaURL, ancestor.InstanceLocation, true
		case *kind.AnyOf:
			keyword = "/anyOf/"
		case *kind.AllOf:
			keyword = "/allOf/"
		case *kind.OneOf:
			keyword = "/oneOf/"
		default:
			continue
		}
		branch, ok := compositeBranch(ancestor.SchemaURL, keyword, parent)
		return branch, ancestor.InstanceLocation, ok
	}
	return resourceURL + "#", nil, true
}

// compositeBranch is the location of the branch of the composite at
// location, its keyword written as "/anyOf/", that parent runs through, and
// whether parent runs through one.
func compositeBranch(location, keyword, parent string) (string, bool) {
	rest, ok := strings.CutPrefix(parent, location)
	if !ok {
		return "", false
	}
	if rest, ok = strings.CutPrefix(rest, keyword); !ok {
		return "", false
	}
	digits := 0
	for digits < len(rest) && '0' <= rest[digits] && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits < len(rest) && rest[digits] != '/' {
		return "", false
	}
	return parent[:len(parent)-len(rest)+digits], true
}

// keywordOf names the keyword k failed, and the property its keyword path
// names after it, as dependentRequired's does. A draft-7 dependency is
// the schema's dependencies keyword, which the library names dependency.
func keywordOf(k jsonschema.ErrorKind) (keyword, property string) {
	switch k := k.(type) {
	case *kind.Dependency:
		return "dependencies", k.Prop
	case *kind.Not:
		return "not", ""
	case *kind.FalseSchema:
		return "false", ""
	case *kind.RefCycle:
		return "refCycle", ""
	case *kind.InvalidJsonValue:
		return "invalidJsonValue", ""
	}
	switch path := k.KeywordPath(); len(path) {
	case 0:
		return "", ""
	case 1:
		return path[0], ""
	default:
		return path[0], path[1]
	}
}

// location reads s's complete schema location: the fragment of its schema's
// URL after the tool's resource URL, its percent-encoding undone, then its
// keyword tokens.
func (p *projection) location(s *violationSite) locationReader {
	p.key(s)
	url, resource := s.node.SchemaURL, p.inv.resourceURL
	if resource != "" && len(url) > len(resource) && url[len(resource)] == '#' && strings.HasPrefix(url, resource) {
		return locationReader{text: url[len(resource)+1:], encoded: true, tokens: s.tokens}
	}
	return locationReader{text: url, tokens: s.tokens}
}

// shownLocation is s's schema location cut to maxViolationText on a UTF-8
// boundary, and whether it was cut. It reads no more of it than that.
func (p *projection) shownLocation(s *violationSite) (string, bool) {
	r := p.location(s)
	shown := make([]byte, 0, maxViolationText/2)
	for len(shown) <= maxViolationText {
		c, ok := r.next()
		if !ok {
			return string(shown), false
		}
		shown = append(shown, c)
	}
	return string(shown[:cutBoundary(shown, maxViolationText)]), true
}

// locationReader reads a schema location a byte at a time, so that a location
// of any length is compared, or cut, without being built.
type locationReader struct {
	// text is the rest of the location's URL part, percent-encoded if encoded.
	text    string
	encoded bool
	// tokens are the keyword tokens still to read; token is the rest of the
	// one being read, which a "/" opened.
	tokens []string
	token  string
	// escaped is the second byte of a token's escape, 0 for none.
	escaped byte
}

// next returns the location's next byte, and false at its end.
func (r *locationReader) next() (byte, bool) {
	if c := r.escaped; c != 0 {
		r.escaped = 0
		return c, true
	}
	if text := r.text; text != "" {
		if r.encoded && text[0] == '%' && len(text) > 2 && isHex(text[1]) && isHex(text[2]) {
			r.text = text[3:]
			return unhex(text[1])<<4 | unhex(text[2]), true
		}
		r.text = text[1:]
		return text[0], true
	}
	if r.token == "" {
		if len(r.tokens) == 0 {
			return 0, false
		}
		r.token, r.tokens = r.tokens[0], r.tokens[1:]
		return '/', true
	}
	c := r.token[0]
	r.token = r.token[1:]
	switch c {
	case '~':
		r.escaped = '0'
		return '~', true
	case '/':
		r.escaped = '1'
		return '~', true
	}
	return c, true
}

// keywordTokens is the place of node's keyword within its schema: a draft-4
// exclusive bound is held by maximum or minimum, and a false schema is its
// own place.
func (p *projection) keywordTokens(node *jsonschema.ValidationError) []string {
	switch k := node.ErrorKind.(type) {
	case *kind.Dependency:
		return []string{"dependencies", k.Prop}
	case *kind.Not:
		return []string{"not"}
	case *kind.FalseSchema, *kind.RefCycle, *kind.InvalidJsonValue:
		return nil
	case *kind.ExclusiveMaximum:
		if p.draft4(node.SchemaURL) {
			return []string{"maximum"}
		}
	case *kind.ExclusiveMinimum:
		if p.draft4(node.SchemaURL) {
			return []string{"minimum"}
		}
	}
	return node.ErrorKind.KeywordPath()
}

// draft4 reports whether the schema at location is of draft 4, where an
// exclusive bound is maximum's or minimum's and exclusiveMaximum a boolean.
func (p *projection) draft4(location string) bool {
	if !p.inv.schema.holdsDraft4() {
		return false
	}
	if p.locations == nil {
		p.locations = schemasByLocation(p.inv.schema.compiled)
	}
	s := p.locations[location]
	return s != nil && s.DraftVersion == 4
}

// schemasByLocation indexes root and the schemas it applies or refers to by
// location, in one walk of them.
func schemasByLocation(root *jsonschema.Schema) map[string]*jsonschema.Schema {
	index := map[string]*jsonschema.Schema{}
	var add func(s *jsonschema.Schema)
	add = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		if _, seen := index[s.Location]; seen {
			return
		}
		index[s.Location] = s
		for _, sub := range subschemas(s) {
			add(sub)
		}
	}
	add(root)
	return index
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// fixedPart is what a violation always carries, whatever room its sides get.
type fixedPart struct {
	pointer, keyword, location, property, message string
	unknown, cut                                  bool
}

// fixed is the fixed part of s's violation.
func (p *projection) fixed(s *violationSite) fixedPart {
	f := fixedPart{unknown: s.unknown()}
	var cut bool
	f.location, f.cut = p.shownLocation(s)
	f.keyword, cut = cappedText(s.keyword)
	f.cut = f.cut || cut
	if !f.unknown {
		f.pointer, cut = cappedPointer(s.at())
		f.cut = f.cut || cut
	}
	f.property, cut = cappedText(s.property)
	f.cut = f.cut || cut
	f.message, cut = violationMessage(s.node.InstanceLocation, func(w *cappedWriter) { kindText(s.node.ErrorKind, w) })
	f.cut = f.cut || cut
	return f
}

// fixedAt is the fixed part of a violation of keyword at at, its reason
// written by text.
func fixedAt(at []string, keyword string, text func(w *cappedWriter)) fixedPart {
	pointer, pointerCut := cappedPointer(at)
	message, messageCut := violationMessage(at, text)
	return fixedPart{pointer: pointer, keyword: keyword, message: message, cut: pointerCut || messageCut}
}

// sides writes the two sides of s's violation, the fields its keyword has.
// The value's kind and preview are read from the instance at the pointer,
// never from the kind; under propertyNames the instance is the property's
// name.
func (p *projection) sides(w *violationWriter, s *violationSite) {
	var instance any
	if s.names != nil {
		instance = s.names.property
	} else {
		instance = valueAt(p.inv.instance, s.node.InstanceLocation)
	}
	w.str(&w.v.Actual.Kind, instanceKind(instance))
	switch instance.(type) {
	case nil, bool, string, json.Number, float64:
		if text, ok := w.preview(instance); ok {
			w.v.Actual.Value = text
		}
	}
	expected, actual := &w.v.Expected, &w.v.Actual
	switch k := s.node.ErrorKind.(type) {
	case *kind.Type:
		w.texts(&expected.Types, &expected.Omitted, k.Want, len(k.Want))
	case *kind.Required:
		w.texts(&expected.Names, &expected.Omitted, k.Missing, len(k.Missing))
	case *kind.Dependency:
		w.texts(&expected.Names, &expected.Omitted, k.Missing, len(k.Missing))
	case *kind.DependentRequired:
		w.texts(&expected.Names, &expected.Omitted, k.Missing, len(k.Missing))
	case *kind.AdditionalProperties:
		w.texts(&actual.Names, &actual.Omitted, leastNames(k.Properties, maxSideEntries), len(k.Properties))
	case *kind.Enum:
		w.previews(&expected.Values, &expected.Omitted, k.Want)
	case *kind.Const:
		w.previews(&expected.Values, &expected.Omitted, []any{k.Want})
	case *kind.Minimum:
		w.bound(k.Want)
	case *kind.Maximum:
		w.bound(k.Want)
	case *kind.ExclusiveMinimum:
		w.bound(k.Want)
	case *kind.ExclusiveMaximum:
		w.bound(k.Want)
	case *kind.MultipleOf:
		w.bound(k.Want)
	case *kind.MinLength:
		w.limitAndCount(k.Want, k.Got)
	case *kind.MaxLength:
		w.limitAndCount(k.Want, k.Got)
	case *kind.MinItems:
		w.limitAndCount(k.Want, k.Got)
	case *kind.MaxItems:
		w.limitAndCount(k.Want, k.Got)
	case *kind.MinProperties:
		w.limitAndCount(k.Want, k.Got)
	case *kind.MaxProperties:
		w.limitAndCount(k.Want, k.Got)
	case *kind.Pattern:
		w.text(&expected.Pattern, k.Want)
	case *kind.Format:
		w.text(&expected.Format, k.Want)
	case *kind.Contains:
		w.number(&actual.Count, 0)
	case *kind.MinContains:
		w.limitAndCount(k.Want, len(k.Got))
		w.indexes(&actual.Indexes, &actual.Omitted, k.Got)
	case *kind.MaxContains:
		w.limitAndCount(k.Want, len(k.Got))
		w.indexes(&actual.Indexes, &actual.Omitted, k.Got)
	case *kind.UniqueItems:
		w.indexes(&actual.Indexes, &actual.Omitted, k.Duplicates[:])
		if items, ok := instance.([]any); ok && 0 <= k.Duplicates[0] && k.Duplicates[0] < len(items) {
			w.previews(&actual.Values, &actual.Omitted, items[k.Duplicates[0]:k.Duplicates[0]+1])
		}
	case *kind.OneOf:
		w.indexes(&expected.Indexes, &expected.Omitted, k.Subschemas)
	case *kind.AdditionalItems:
		w.number(&actual.Count, k.Count)
	}
}

// failureBuilder builds a failure's list within the list's budgets.
type failureBuilder struct {
	failure InputValidationFailure
	// bytes is what the list's field values may still take; nodes what its
	// previews may still write.
	bytes int
	nodes previewBudget
}

// newFailureBuilder starts the list of a failure.
func newFailureBuilder() *failureBuilder {
	return &failureBuilder{
		bytes: maxFailureBytes,
		nodes: previewBudget{nodes: maxFailureNodes, bytes: math.MaxInt},
	}
}

// add appends the violation of f and the sides sides writes, or, when f does
// not fit the smaller of maxViolationBytes and what the list has left, counts
// it as omitted: a violation goes in whole or not at all. The sides take what
// is left of that, entry by entry.
func (b *failureBuilder) add(f fixedPart, sides func(w *violationWriter)) {
	size := 0
	for _, s := range [...]string{f.pointer, f.keyword, f.location, f.property, f.message} {
		size += len(s)
	}
	budget := min(maxViolationBytes, b.bytes)
	if size > budget {
		b.failure.Omitted++
		b.failure.Cut = true
		return
	}
	room := budget - size
	w := &violationWriter{
		v: InputViolation{
			Pointer: f.pointer, PointerUnknown: f.unknown, Keyword: f.keyword, SchemaLocation: f.location,
			Property: f.property, Message: f.message,
		},
		room:  room,
		cut:   f.cut,
		nodes: &b.nodes,
	}
	if sides != nil {
		sides(w)
	}
	w.v.Cut = w.cut
	b.failure.Cut = b.failure.Cut || w.cut
	b.bytes -= size + room - w.room
	b.failure.Violations = append(b.failure.Violations, w.v)
}

// violationWriter writes a violation's sides within the room its budget
// leaves them, counted in the bytes of their values.
type violationWriter struct {
	v     InputViolation
	room  int
	cut   bool
	nodes *previewBudget
}

// take takes n bytes of the room, or reports false and cuts the violation.
func (w *violationWriter) take(n int) bool {
	if n > w.room {
		w.cut = true
		return false
	}
	w.room -= n
	return true
}

// str sets *dst to s, already within maxViolationText, if it fits.
func (w *violationWriter) str(dst *string, s string) {
	if s != "" && w.take(len(s)) {
		*dst = s
	}
}

// text sets *dst to s, cut to maxViolationText, if it fits.
func (w *violationWriter) text(dst *string, s string) {
	text, cut := cappedText(s)
	w.cut = w.cut || cut
	w.str(dst, text)
}

// number sets *dst to n if it fits.
func (w *violationWriter) number(dst **int, n int) {
	if w.take(decimalLen(n)) {
		*dst = &n
	}
}

// limitAndCount sets the schema's limit and the value's count.
func (w *violationWriter) limitAndCount(limit, count int) {
	w.number(&w.v.Expected.Limit, limit)
	w.number(&w.v.Actual.Count, count)
}

// bound sets the expected bound, as boundText renders r, if it fits.
func (w *violationWriter) bound(r *big.Rat) {
	text, exact := boundText(r)
	if w.take(len(text)) {
		w.v.Expected.Bound = &ViolationBound{Text: text, Exact: exact}
	}
}

// reserve takes room for an omitted count of up to total.
func (w *violationWriter) reserve(total int) {
	if total > 0 {
		w.room -= decimalLen(total)
	}
}

// omit counts n entries left out of a side's list.
func (w *violationWriter) omit(omitted *int, n int) {
	if n > 0 {
		*omitted += n
		w.cut = true
	}
}

// texts lists items, each cut to maxViolationText, up to maxSideEntries and
// while they fit; the rest of total counts as omitted.
func (w *violationWriter) texts(list *[]string, omitted *int, items []string, total int) {
	w.reserve(total)
	for _, item := range items {
		if len(*list) == maxSideEntries {
			break
		}
		text, cut := cappedText(item)
		if !w.take(len(text)) {
			break
		}
		w.cut = w.cut || cut
		*list = append(*list, text)
	}
	w.omit(omitted, total-len(*list))
}

// previews lists previews of values, up to maxSideEntries and while they fit
// the room and the list's nodes; the rest count as omitted.
func (w *violationWriter) previews(list *[]string, omitted *int, values []any) {
	w.reserve(len(values))
	for _, v := range values {
		if len(*list) == maxSideEntries {
			break
		}
		text, ok := w.preview(v)
		if !ok {
			break
		}
		*list = append(*list, text)
	}
	w.omit(omitted, len(values)-len(*list))
}

// preview writes a preview of v within the room and the nodes left. A preview
// that writes nothing gives its nodes back.
func (w *violationWriter) preview(v any) (string, bool) {
	limit := min(maxViolationText, w.room)
	if limit <= 0 || w.nodes.nodes <= 0 {
		w.cut = true
		return "", false
	}
	before := *w.nodes
	text, cut := preview(v, limit, w.nodes)
	if text == "" || !w.take(len(text)) {
		*w.nodes = before
		w.cut = true
		return "", false
	}
	w.cut = w.cut || cut
	return text, true
}

// indexes lists items up to maxSideEntries and while they fit; the rest count
// as omitted.
func (w *violationWriter) indexes(list *[]int, omitted *int, items []int) {
	if len(items) == 0 {
		return
	}
	w.reserve(len(items))
	for _, index := range items {
		if len(*list) == maxSideEntries || !w.take(decimalLen(index)) {
			break
		}
		*list = append(*list, index)
	}
	w.omit(omitted, len(items)-len(*list))
}

// cappedPointer is at as a JSON Pointer of at most maxViolationText bytes,
// escaped as it is written, and whether it was cut.
func cappedPointer(at []string) (string, bool) {
	w := newCappedWriter(maxViolationText)
	writePointer(w, at)
	return w.String(), w.Cut()
}

// cappedText is s cut to maxViolationText on a UTF-8 boundary, copied when
// cut so that it holds no more of s, and whether it was cut.
func cappedText(s string) (string, bool) {
	n := cutBoundary(s, maxViolationText)
	if n == len(s) {
		return s, false
	}
	return strings.Clone(s[:n]), true
}

// violationMessage is the line the default refusal writes for a failure at at
// whose reason text writes, built within its bounds, and whether it is cut:
// shorter than the line written whole.
func violationMessage(at []string, text func(w *cappedWriter)) (string, bool) {
	textCut, textLen := false, 0
	message := cappedValidationLine(at, func(w *cappedWriter) {
		text(w)
		textCut, textLen = w.Cut(), len(w.buf)
	})
	return message, textCut || pointerLonger(at, maxValidationMessageBytes-len(": ")-textLen)
}

// pointerLonger reports whether at, as validationLine shows it, takes more
// than n bytes. It stops counting past n.
func pointerLonger(at []string, n int) bool {
	if len(at) == 0 {
		return len("<root>") > n
	}
	size := 0
	for _, segment := range at {
		size += len("/") + len(segment) + strings.Count(segment, "~") + strings.Count(segment, "/")
		if size > n {
			return true
		}
	}
	return false
}

// leastNames returns the n least of names in byte order, from one scan that
// keeps at most n.
func leastNames(names []string, n int) []string {
	if len(names) <= n {
		least := slices.Clone(names)
		slices.Sort(least)
		return least
	}
	least := make([]string, 0, n)
	for _, name := range names {
		if len(least) == n {
			if name >= least[n-1] {
				continue
			}
			least = least[:n-1]
		}
		at, _ := slices.BinarySearch(least, name)
		least = slices.Insert(least, at, name)
	}
	return least
}

// instanceKind names v's JSON type, "" for a value no JSON decodes to.
func instanceKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	}
	return ""
}

// decimalLen is how many bytes n takes in decimal.
func decimalLen(n int) int {
	var scratch [24]byte
	return len(strconv.AppendInt(scratch[:0], int64(n), 10))
}
