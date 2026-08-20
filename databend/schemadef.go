package databend

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// IndexKind distinguishes the two index types that change how a search
// compiles on this engine.
type IndexKind int

const (
	// InvertedIndex is a tokenised full-text index, searched with
	// match()/query().
	InvertedIndex IndexKind = iota

	// Ngram makes LIKE prefix and substring searches index-backed. It changes
	// no SQL; it decides whether a full-scan warning is emitted.
	NgramIndex
)

func (k IndexKind) String() string {
	if k == NgramIndex {
		return "ngram"
	}
	return "inverted"
}

// Index is one index as the table declares it — the shape you can read
// straight off SHOW CREATE TABLE.
//
// Declaring indexes rather than restating their consequences on every field is
// what makes a hand-written schema safe. The two per-field flags that matter,
// Ngram and StopWords, are *derived* from these declarations, so the failure
// the old shape invited — claiming `filters = 'english_stop'` on a column whose
// index has no such filter, which routes 33 ordinary words onto needless full
// scans — is no longer expressible.
type Index struct {
	Name    string
	Kind    IndexKind
	Columns []string

	// Tokenizer and Filters are recorded as declared. Filters is the half that
	// changes an answer rather than a cost: `english_stop` deletes 33 words
	// from the query text before the index is consulted, so each of them is a
	// search that returns zero rows and raises nothing.
	Tokenizer string
	Filters   []string
}

func (ix Index) covers(col string) bool {
	for _, c := range ix.Columns {
		if c == col {
			return true
		}
	}
	return false
}

// Def is a schema as data: the on-disk form that a deployment writes instead of
// editing Go.
//
// # Why JSON and not YAML
//
// This module has no dependencies and that is a design constraint rather than
// an accident — it is vendored into a Grafana datasource plugin, and a search
// compiler that drags a YAML parser into someone else's build is a worse
// trade than one that asks for braces. JSON is in the standard library. A
// deployment that prefers YAML can convert it in one line at deploy time.
type Def struct {
	// Table is the qualified table the predicate runs against. It is needed
	// for the anti-join that a bare exclusion compiles to; see Schema.Table.
	Table string `json:"table"`

	// Default is the field name a bare term searches. It must name a declared
	// field of kind "text": a bare word has to reach an inverted index, and a
	// default pointing at a plain column would turn every free-text search
	// into a LIKE scan without saying so.
	Default string `json:"default"`

	// Time and Severity name the timestamp and level fields. Time is
	// effectively required by any log view; Severity is genuinely optional and
	// its absence is reported as a note rather than an error.
	Time     string `json:"time,omitempty"`
	Severity string `json:"severity,omitempty"`

	// Variant names the single VARIANT column unknown field names are routed
	// into. It is the one-bag spelling of Bags and exists because most tables
	// have exactly one.
	//
	// Empty, with no Bags either, is legal and means unknown field names are a
	// compile error instead — which is a real deployment shape (a fixed-column
	// access log has no bag) and is reported as a note at load time, because it
	// changes what a user's query does.
	Variant string `json:"variant,omitempty"`

	// Bags are the VARIANT columns, when there is more than one or when one of
	// them needs a prefix or a typed key. A table split by role — resource
	// attributes in one column, event attributes in another — is the ordinary
	// shape rather than the exception.
	//
	// Per-key types are *overrides* and are expected to be rare. They are rare
	// because this engine does not need them the way a fixed-type map does: a
	// VARIANT is self-describing per value, an inverted index covers it by JSON
	// path with no per-key DDL, and a numeric comparison resolves the type at
	// emission with a cast. Declare a key only where the emission-time rule is
	// wrong for it.
	Bags []BagDef `json:"bags,omitempty"`

	// TimeZone pins bare timestamp literals to a fixed `+HH:MM` offset.
	TimeZone string `json:"time_zone,omitempty"`

	// CaseInsensitive compares plain string columns with lower() on both
	// sides. It is a pointer so that an omitted key can mean "on" — the
	// useful default — while an explicit `false` still turns it off.
	CaseInsensitive *bool `json:"case_insensitive,omitempty"`

	// Display lists field names a row-level view selects, in order.
	Display []string `json:"display,omitempty"`

	Indexes []IndexDef `json:"indexes,omitempty"`
	Fields  []FieldDef `json:"fields"`
}

// IndexDef is one index in the on-disk form.
type IndexDef struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Columns   []string `json:"columns"`
	Tokenizer string   `json:"tokenizer,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

// BagDef is one VARIANT column in the on-disk form.
type BagDef struct {
	Column string `json:"column"`

	// Prefix addresses this bag explicitly: with prefix "resource",
	// `resource.pod` reads the resource column. A bag with no prefix is a
	// catch-all and is tried in declaration order.
	Prefix string `json:"prefix,omitempty"`

	// Keys types individual keys, by leaf name. Values are the same kind
	// strings a field uses.
	Keys map[string]string `json:"keys,omitempty"`
}

// FieldDef is one searchable field in the on-disk form.
type FieldDef struct {
	// Name is what the user types. It is also the column name unless Column
	// says otherwise.
	Name string `json:"name"`

	// Column is the SQL column expression, when it differs from Name.
	Column string `json:"column,omitempty"`

	// Kind is "text", "string", "number" or "timestamp".
	Kind string `json:"kind"`

	// Aliases are extra names that resolve to this same field — `message` for
	// `msg`, `timestamp` for `ts`, the habits users bring from other tools.
	Aliases []string `json:"aliases,omitempty"`

	// Derived records the expression a STORED computed column is generated
	// from. It is never used to build a query — the column is materialised, so
	// queries read the column — but it is what lets a schema describe the
	// table completely enough to write the migration that creates it, and it
	// documents at a glance that the field is not a column the writer sets.
	Derived string `json:"derived,omitempty"`

	// Example is one real value, used only by the dashboard help text.
	Example string `json:"example,omitempty"`
}

var kindNames = map[string]Kind{
	"text":      Text,
	"string":    String,
	"number":    Number,
	"timestamp": Timestamp,
}

// KindName renders a Kind the way a schema file spells it.
func KindName(k Kind) string {
	for name, kk := range kindNames {
		if kk == k {
			return name
		}
	}
	return "?"
}

// ParseDef reads a schema definition from JSON.
func ParseDef(raw []byte) (Def, error) {
	var d Def
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Def{}, fmt.Errorf("schema: %w", err)
	}
	return d, nil
}

// LoadSchema reads a schema file and builds the Schema, returning the notes
// the definition earned along the way.
//
// The notes are the point of the second return value. A schema that omits a
// VARIANT bag or a severity column is *valid* — those are real table shapes —
// but a user who types `store_id:7` against a bagless schema gets an error,
// and a panel built over a severity-less schema colours nothing. Both facts
// are knowable when the file is read and neither is visible at query time, so
// they are surfaced here and the caller is expected to print them. Silence
// until the query is the failure mode this exists to prevent.
func LoadSchema(path string) (Schema, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Schema{}, nil, fmt.Errorf("schema: %w", err)
	}
	d, err := ParseDef(raw)
	if err != nil {
		return Schema{}, nil, err
	}
	return d.Schema()
}

// Schema resolves a definition into the compiler's contract, refusing anything
// that would compile to SQL the engine rejects or, worse, silently answers
// wrongly.
func (d Def) Schema() (Schema, []string, error) {
	s := Schema{
		Table:           d.Table,
		Default:         strings.ToLower(d.Default),
		Time:            strings.ToLower(d.Time),
		Severity:        strings.ToLower(d.Severity),
		Variant:         d.Variant,
		TimeZone:        d.TimeZone,
		CaseInsensitive: d.CaseInsensitive == nil || *d.CaseInsensitive,
		Fields:          make(map[string]Field, len(d.Fields)*2),
	}

	// Indexes first: every per-field flag below is derived from them.
	var notes []string
	byCol := map[string]string{} // column -> inverted index name
	for _, ixd := range d.Indexes {
		if ixd.Name == "" {
			return Schema{}, nil, fmt.Errorf("schema: an index has no name")
		}
		if len(ixd.Columns) == 0 {
			return Schema{}, nil, fmt.Errorf("schema: index %q covers no columns", ixd.Name)
		}
		var kind IndexKind
		switch strings.ToLower(ixd.Kind) {
		case "inverted", "":
			kind = InvertedIndex
		case "ngram":
			kind = NgramIndex
		default:
			return Schema{}, nil, fmt.Errorf(
				"schema: index %q has kind %q; valid kinds are \"inverted\" and \"ngram\"",
				ixd.Name, ixd.Kind)
		}
		if kind == InvertedIndex {
			for _, col := range ixd.Columns {
				if prev, dup := byCol[col]; dup {
					// The engine refuses this outright — `CREATE INVERTED
					// INDEX idx_line_only ON …(line)` against a table whose
					// idx_msg already covers (msg, line) returns
					// `[1601] INVERTED index for columns (line) already
					// exist` — so a schema claiming both is describing a table
					// that cannot be built.
					return Schema{}, nil, fmt.Errorf(
						"schema: column %q is covered by two inverted indexes (%s and %s); "+
							"the engine rejects overlapping inverted index column sets [1601]",
						col, prev, ixd.Name)
				}
				byCol[col] = ixd.Name
			}
		}
		for _, filter := range ixd.Filters {
			switch strings.ToLower(filter) {
			case "english_stop", "english_stemmer":
			default:
				notes = append(notes, fmt.Sprintf(
					"index %s declares filter %q, which this compiler does not model: if it "+
						"deletes words at query time, searches for those words will return zero "+
						"rows and raise nothing",
					ixd.Name, filter))
			}
		}
		s.Indexes = append(s.Indexes, Index{
			Name: ixd.Name, Kind: kind, Columns: ixd.Columns,
			Tokenizer: ixd.Tokenizer, Filters: ixd.Filters,
		})
	}

	// Bags, resolved against the same index declarations. A bag whose column
	// is in an inverted index can have its keys searched by JSON path, which is
	// the difference between an index-backed lookup and a full scan.
	bagCols := map[string]bool{}
	bagPrefixes := map[string]string{}
	for _, bd := range d.Bags {
		if bd.Column == "" {
			return Schema{}, nil, fmt.Errorf("schema: a bag has no column")
		}
		if bagCols[bd.Column] {
			return Schema{}, nil, fmt.Errorf("schema: bag column %q is declared twice", bd.Column)
		}
		bagCols[bd.Column] = true
		if bd.Prefix != "" {
			// A repeated prefix makes the second bag unreachable by any query:
			// resolution stops at the first prefix that matches, so the later
			// one is declared, listed by `lake-search schema` as though it were
			// usable, and addressable by nothing. Silence about a
			// declared-but-unreachable bag is exactly what loading loudly is
			// for.
			key := strings.ToLower(bd.Prefix)
			if prev, dup := bagPrefixes[key]; dup {
				return Schema{}, nil, fmt.Errorf(
					"schema: bags %q and %q both use prefix %q; a name is routed to the first "+
						"match, so the second bag would be unreachable by any query",
					prev, bd.Column, bd.Prefix)
			}
			bagPrefixes[key] = bd.Column
		}
		b := Bag{Column: bd.Column, Prefix: bd.Prefix, Index: byCol[bd.Column]}
		if b.Index != "" {
			b.StopWords = stopWordsOf(s.Indexes, b.Index)
		}
		if len(bd.Keys) > 0 {
			b.Keys = make(map[string]Kind, len(bd.Keys))
			for key, kindName := range bd.Keys {
				kind, ok := kindNames[strings.ToLower(kindName)]
				if !ok {
					return Schema{}, nil, fmt.Errorf(
						"schema: bag %q key %q has kind %q; valid kinds are %s",
						bd.Column, key, kindName, quotedKinds())
				}
				if kind == Text {
					return Schema{}, nil, fmt.Errorf(
						"schema: bag %q key %q is kind \"text\", which a bag key cannot be: a "+
							"text field is a column of an inverted index, and a bag key is a "+
							"path inside one", bd.Column, key)
				}
				b.Keys[strings.ToLower(key)] = kind
			}
		}
		s.Bags = append(s.Bags, b)
	}
	if s.Variant != "" && !bagCols[s.Variant] {
		// The single-column spelling, resolved the same way so that a table
		// whose VARIANT column is in the index group gets indexed bag search
		// without having to restate the column under "bags".
		s.Bags = append(s.Bags, Bag{
			Column:    s.Variant,
			Index:     byCol[s.Variant],
			StopWords: stopWordsOf(s.Indexes, byCol[s.Variant]),
		})
	}
	for prefix, owner := range bagPrefixes {
		for _, b := range s.Bags {
			if strings.EqualFold(b.Column, prefix) && !strings.EqualFold(b.Column, owner) {
				// `kvbag.x` then reads owner['x'], not kvbag['x'] — a user
				// addressing a bag by its own column name silently gets a
				// different column. Not an error, because the prefix is doing
				// what it was told to; loud, because nothing else would say so.
				notes = append(notes, fmt.Sprintf(
					"bag %q is prefixed %q, which is also the column name of bag %q: a name like "+
						"%s.key is routed to %s and never to %s",
					owner, prefix, b.Column, prefix, owner, b.Column))
			}
		}
	}
	if len(s.Bags) == 0 {
		notes = append(notes, "no attribute bag: a field name that is not declared here is a "+
			"compile error rather than a bag lookup, so `store_id:7` will be refused instead "+
			"of read from a VARIANT")
	}
	catchAll := 0
	for _, b := range s.Bags {
		if b.Prefix == "" {
			catchAll++
		}
	}
	if catchAll > 1 {
		notes = append(notes, fmt.Sprintf(
			"%d bags have no prefix: an undeclared field name reaches only the first of them, "+
				"so the others are addressable only by prefix", catchAll))
	}

	if len(d.Fields) == 0 {
		return Schema{}, nil, fmt.Errorf("schema: no fields declared")
	}

	// Fields, resolved against the indexes.
	declared := map[string]bool{} // canonical field names, for role checks
	for _, fd := range d.Fields {
		name := strings.ToLower(fd.Name)
		if name == "" {
			return Schema{}, nil, fmt.Errorf("schema: a field has no name")
		}
		kind, ok := kindNames[strings.ToLower(fd.Kind)]
		if !ok {
			return Schema{}, nil, fmt.Errorf(
				"schema: field %q has kind %q; valid kinds are %s",
				fd.Name, fd.Kind, quotedKinds())
		}
		col := fd.Column
		if col == "" {
			col = fd.Name
		}

		f := Field{Column: col, Kind: kind, Example: fd.Example}
		if kind == Text {
			ixName, covered := byCol[col]
			if !covered {
				// Loud, at load time, because the query-time symptom is
				// [1065] "columns <col> don't have inverted index" — which at
				// least errors — or, if the column is merged with an indexed
				// one, SQL that runs and answers wrongly.
				return Schema{}, nil, fmt.Errorf(
					"schema: field %q is kind \"text\" but no inverted index covers column %q; "+
						"declare the index, or give the field kind \"string\" so it compiles to "+
						"LIKE instead of to a search function",
					fd.Name, col)
			}
			f.Index = ixName
			for _, ix := range s.Indexes {
				if ix.Name != ixName {
					continue
				}
				for _, filter := range ix.Filters {
					if strings.EqualFold(filter, "english_stop") {
						f.StopWords = EnglishStopWords()
					}
				}
			}
		}
		for _, ix := range s.Indexes {
			if ix.Kind == NgramIndex && ix.covers(col) {
				f.Ngram = true
			}
		}

		for _, n := range append([]string{name}, lowerAll(fd.Aliases)...) {
			if _, dup := s.Fields[n]; dup {
				return Schema{}, nil, fmt.Errorf("schema: %q is declared twice", n)
			}
			s.Fields[n] = f
		}
		declared[name] = true
	}

	// Roles.
	if s.Default == "" {
		return Schema{}, nil, fmt.Errorf("schema: no default field")
	}
	def, ok := s.Fields[s.Default]
	if !ok {
		return Schema{}, nil, fmt.Errorf(
			"schema: default field %q is not declared", d.Default)
	}
	if def.Kind != Text {
		return Schema{}, nil, fmt.Errorf(
			"schema: default field %q is kind %q, but a bare term has to reach an inverted "+
				"index; a plain column here turns every free-text search into a full scan "+
				"without saying so", d.Default, KindName(def.Kind))
	}
	if s.Time != "" {
		t, ok := s.Fields[s.Time]
		if !ok {
			return Schema{}, nil, fmt.Errorf("schema: time field %q is not declared", d.Time)
		}
		if t.Kind != Timestamp {
			return Schema{}, nil, fmt.Errorf(
				"schema: time field %q is kind %q, not \"timestamp\"", d.Time, KindName(t.Kind))
		}
	} else {
		notes = append(notes, "no time field: a caller that has to order a log view by time "+
			"cannot learn the column from this schema")
	}
	if s.Severity != "" {
		if _, ok := s.Fields[s.Severity]; !ok {
			return Schema{}, nil, fmt.Errorf(
				"schema: severity field %q is not declared", d.Severity)
		}
	} else {
		notes = append(notes, "no severity field: a log view over this schema cannot colour or "+
			"count by level, and a panel that guesses will pick whatever string column comes "+
			"first")
	}
	if s.Table == "" {
		notes = append(notes, "no table: excluding a full-text term with no positive term "+
			"beside it needs an anti-join, which has to name the table, so that one shape will "+
			"be refused")
	}
	if s.TimeZone != "" && !validOffset(s.TimeZone) {
		return Schema{}, nil, fmt.Errorf(
			"schema: time_zone %q is not a `+HH:MM` or `-HH:MM` offset", s.TimeZone)
	}

	// Every searchable surface must sit in one index group.
	//
	// The engine enforces one search function per statement, and a single
	// query() call reaches only the columns of one index — measured, a table
	// carrying separate idx_line(line) and idx_line2(line2) answers each column
	// alone and fails [1065] "columns line2, line don't have inverted index"
	// for `query('line:x AND line2:x')`. A schema whose text fields and indexed
	// bags are spread across groups therefore describes a table where ordinary
	// queries cannot run, and the right time to say so is now.
	groups := map[string][]string{}
	for name, f := range s.Fields {
		if f.Kind == Text {
			groups[f.Index] = appendUniqueStr(groups[f.Index], name)
		}
	}
	for _, b := range s.Bags {
		if b.Index != "" {
			groups[b.Index] = appendUniqueStr(groups[b.Index], b.Column)
		}
	}
	if len(groups) > 1 {
		var names []string
		for ixName, members := range groups {
			sort.Strings(members)
			names = append(names, ixName+" ("+strings.Join(members, ", ")+")")
		}
		sort.Strings(names)
		return Schema{}, nil, fmt.Errorf(
			"schema: searchable surfaces are spread across %d inverted indexes — %s — but one "+
				"query() call reaches only the columns of one index, so a query touching two of "+
				"them cannot run [1065]; index them together",
			len(groups), strings.Join(names, "; "))
	}

	// Display, defaulted from the roles so a schema need not repeat itself.
	for _, name := range d.Display {
		if _, ok := s.Fields[strings.ToLower(name)]; !ok {
			return Schema{}, nil, fmt.Errorf("schema: display field %q is not declared", name)
		}
		s.Display = append(s.Display, strings.ToLower(name))
	}
	if len(s.Display) == 0 {
		for _, name := range []string{s.Time, s.Severity, s.Default} {
			if name != "" {
				s.Display = append(s.Display, name)
			}
		}
	}

	return s, notes, nil
}

// stopWordsOf returns the stop set the named index installs.
func stopWordsOf(indexes []Index, name string) map[string]bool {
	if name == "" {
		return nil
	}
	for _, ix := range indexes {
		if ix.Name != name {
			continue
		}
		for _, filter := range ix.Filters {
			if strings.EqualFold(filter, "english_stop") {
				return EnglishStopWords()
			}
		}
	}
	return nil
}

func appendUniqueStr(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func quotedKinds() string {
	names := make([]string, 0, len(kindNames))
	for n := range kindNames {
		names = append(names, `"`+n+`"`)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// validOffset checks the `+HH:MM` form the emitter appends to a bare literal.
func validOffset(tz string) bool {
	if len(tz) != 6 || (tz[0] != '+' && tz[0] != '-') || tz[3] != ':' {
		return false
	}
	for _, i := range []int{1, 2, 4, 5} {
		if tz[i] < '0' || tz[i] > '9' {
			return false
		}
	}
	return true
}

// DisplayColumns renders the Display roles as SQL column expressions, aliased
// to the field name where the two differ.
//
// This is what replaces the hardcoded `SELECT ts, level, component, pod, msg`
// that used to sit in the CLI and in the dashboard generator.
func (s Schema) DisplayColumns() []string {
	out := make([]string, 0, len(s.Display))
	for _, name := range s.Display {
		f, ok := s.Fields[name]
		if !ok {
			continue
		}
		if f.Column == name {
			out = append(out, name)
		} else {
			out = append(out, f.Column+" AS "+name)
		}
	}
	return out
}

// TimeColumn returns the SQL column expression of the time role, or "" when the
// schema declares none.
func (s Schema) TimeColumn() string {
	if f, ok := s.Fields[s.Time]; ok {
		return f.Column
	}
	return ""
}

// Preset returns a built-in schema by name.
func Preset(name string) (Schema, []string, error) {
	raw, ok := presets[strings.ToLower(name)]
	if !ok {
		return Schema{}, nil, fmt.Errorf(
			"unknown schema preset %q; built-in presets are %s",
			name, strings.Join(PresetNames(), ", "))
	}
	d, err := ParseDef([]byte(raw))
	if err != nil {
		return Schema{}, nil, err
	}
	return d.Schema()
}

// PresetNames lists the built-in presets, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var presets = map[string]string{
	"k8s-logs":      k8sLogsDef,
	"k8s-logs-line": k8sLogsLineDef,
}
