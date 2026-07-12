/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequel

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/microbus-io/errors"
)

type virtualFunc struct {
	name    string
	handler func(driverName string, args string) (string, error)
}

// virtualFuncCacheSize bounds the number of expanded queries retained in the LRU cache.
const virtualFuncCacheSize = 4096

// vfIdentPattern matches an identifier directly followed by '('.
// It is intentionally a single, VF-list-independent pass over the query.
var vfIdentPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\(`)

// virtualFuncs holds the registered virtual functions as a copy-on-write map. Reads are lock-free (a single
// atomic load), which matters because every query that may contain a virtual function reads it; writes are
// rare (process startup, occasional RegisterVirtualFunc) and clone-then-swap under virtualFuncsMutex. A
// reader that loads the pointer is reading an immutable snapshot, so it can never race a concurrent
// registration — the pattern sequel also uses for per-DB telemetry.
var (
	virtualFuncsMutex sync.Mutex // serializes writers so concurrent registrations don't lose updates
	virtualFuncs      atomic.Pointer[map[string]virtualFunc]

	expandCache = newVFCache(virtualFuncCacheSize)
)

func init() {
	m := map[string]virtualFunc{
		"NOW_UTC":            {name: "NOW_UTC", handler: vfNowUTC},
		"REGEXP_TEXT_SEARCH": {name: "REGEXP_TEXT_SEARCH", handler: vfRegexpTextSearch},
		"DATE_ADD_MILLIS":    {name: "DATE_ADD_MILLIS", handler: vfDateAddMillis},
		"DATE_DIFF_MILLIS":   {name: "DATE_DIFF_MILLIS", handler: vfDateDiffMillis},
		"LIMIT_OFFSET":       {name: "LIMIT_OFFSET", handler: vfLimitOffset},
		"JSON_FIELD":         {name: "JSON_FIELD", handler: vfJSONField},
	}
	virtualFuncs.Store(&m)
}

// RegisterVirtualFunc registers a virtual SQL function that will be replaced in queries
// before execution. The name is matched case-insensitively, e.g. registering "NOW_UTC"
// matches NOW_UTC(), now_utc(), Now_Utc(), etc.
// The handler receives the driver name and the string found between the parentheses,
// and returns the replacement SQL expression, or an error.
func RegisterVirtualFunc(name string, handler func(driverName string, args string) (string, error)) {
	upper := strings.ToUpper(name)
	virtualFuncsMutex.Lock()
	// Clone-then-swap: readers hold a pointer to the old map and must never see it mutated.
	old := *virtualFuncs.Load()
	clone := make(map[string]virtualFunc, len(old)+1)
	for k, v := range old {
		clone[k] = v
	}
	clone[upper] = virtualFunc{name: upper, handler: handler}
	virtualFuncs.Store(&clone)
	virtualFuncsMutex.Unlock()
	// Registration invalidates previously-cached expansions.
	expandCache.clear()
}

// unregisterVirtualFunc removes a registered virtual function. It follows the same clone-then-swap
// discipline as RegisterVirtualFunc so readers never see a mutated map. Currently used only by tests to
// undo a throwaway registration.
func unregisterVirtualFunc(name string) {
	upper := strings.ToUpper(name)
	virtualFuncsMutex.Lock()
	old := *virtualFuncs.Load()
	clone := make(map[string]virtualFunc, len(old))
	for k, v := range old {
		if k != upper {
			clone[k] = v
		}
	}
	virtualFuncs.Store(&clone)
	virtualFuncsMutex.Unlock()
	expandCache.clear()
}

// expandVirtualFuncs replaces virtual function calls in the query with driver-specific expressions.
// Parentheses in arguments are balanced so that nested function calls (e.g. DATE_ADD_MILLIS(NOW_UTC(), ?)) work.
// Multiple passes are performed until no more expansions occur, allowing inner virtual functions
// to be expanded before outer ones that depend on them.
//
// The result is cached in a bounded LRU keyed on (driverName, query), so repeated calls with the
// same literal query short-circuit to a map lookup.
func expandVirtualFuncs(driverName string, query string) (string, error) {
	return expandVirtualFuncsWithCache(driverName, query, expandCache)
}

// expandVirtualFuncsWithCache is the testable form of expandVirtualFuncs: it expands using the
// given cache (or no cache if nil). The cache stores both successful expansions and errors, so a
// query whose handler returns an error does not have to be re-scanned every time.
func expandVirtualFuncsWithCache(driverName string, query string, cache *vfCache) (string, error) {
	key := vfCacheKey{driver: driverName, query: query}
	if cache != nil {
		if v, ok := cache.get(key); ok {
			return v.expanded, v.err
		}
	}
	expanded, err := expandVirtualFuncsUncached(driverName, query)
	if cache != nil {
		cache.put(key, vfCacheValue{expanded: expanded, err: err})
	}
	return expanded, err
}

// expandVirtualFuncsUncached performs the actual macro expansion, with no caching.
func expandVirtualFuncsUncached(driverName string, query string) (string, error) {
	// Lock-free load of an immutable snapshot; a concurrent RegisterVirtualFunc swaps in a new map and
	// leaves this one untouched, so the scan below can read vfs without holding any lock.
	vfs := *virtualFuncs.Load()

	// Iterate until no further expansion happens. Each outer iteration runs a single
	// left-to-right scan over the query, expanding every virtual function call it finds.
	// Nested virtual functions inside an expansion result are picked up on the next pass.
	for {
		next, changed, err := expandOnce(driverName, query, vfs)
		if err != nil {
			return "", errors.Trace(err)
		}
		if !changed {
			return next, nil
		}
		query = next
	}
}

// expandOnce performs a single left-to-right scan, expanding every virtual function call.
// It returns the (possibly new) query, whether any expansion occurred, and any error.
func expandOnce(driverName, query string, vfs map[string]virtualFunc) (string, bool, error) {
	var b strings.Builder
	changed := false
	i := 0
	for i < len(query) {
		loc := vfIdentPattern.FindStringIndex(query[i:])
		if loc == nil {
			b.WriteString(query[i:])
			break
		}
		absStart := i + loc[0]
		absEnd := i + loc[1] // position just after the opening '('
		// Identifier excludes the trailing '('.
		name := query[absStart : absEnd-1]
		vf, ok := vfs[strings.ToUpper(name)]
		if !ok {
			// Not a virtual function — skip past the '(' and keep scanning.
			b.WriteString(query[i:absEnd])
			i = absEnd
			continue
		}
		closePos := findBalancedClose(query, absEnd)
		if closePos < 0 {
			// Unbalanced; emit verbatim and continue scanning.
			b.WriteString(query[i:absEnd])
			i = absEnd
			continue
		}
		args := query[absEnd:closePos]
		result, err := vf.handler(driverName, args)
		if err != nil {
			return "", false, err
		}
		b.WriteString(query[i:absStart])
		b.WriteString(result)
		i = closePos + 1
		changed = true
	}
	if !changed {
		// Avoid an unnecessary allocation on the common no-VF path.
		return query, false, nil
	}
	return b.String(), true, nil
}

// findBalancedClose returns the index of the ')' that closes the call whose opening '('
// was at position start-1 (i.e. start is the first char after the opening paren).
// It skips characters inside single- and double-quoted strings. Returns -1 if unbalanced.
func findBalancedClose(s string, start int) int {
	depth := 1
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\'', '"':
			i++
			for i < len(s) && s[i] != ch {
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// vfNowUTC is the handler for the NOW_UTC() virtual function.
// It returns the current UTC timestamp with millisecond precision.
func vfNowUTC(driverName string, args string) (string, error) {
	switch driverName {
	case "mysql":
		return "(UTC_TIMESTAMP(3))", nil
	case "pgx", "cockroachdb":
		return "(NOW() AT TIME ZONE 'UTC')", nil
	case "mssql":
		return "(CONVERT(DATETIME2(3), SYSUTCDATETIME()))", nil
	case "sqlite":
		return "(STRFTIME('%Y-%m-%d %H:%M:%f', 'now'))", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}

// vfRegexpTextSearch is the handler for the REGEXP_TEXT_SEARCH() virtual function.
// The syntax is REGEXP_TEXT_SEARCH(searchExpr IN col1, col2, ...) where searchExpr
// is the expression to match against (e.g. a ? placeholder) and the columns after IN
// are concatenated for the search. For example:
//
//	REGEXP_TEXT_SEARCH(? IN first_name, last_name, email)
func vfRegexpTextSearch(driverName string, args string) (string, error) {
	upper := strings.ToUpper(args)
	i := strings.Index(upper, " IN ")
	if i < 0 {
		return "", errors.New("REGEXP_TEXT_SEARCH requires syntax: REGEXP_TEXT_SEARCH(expr IN col1, col2, ...)")
	}
	searchExpr := strings.TrimSpace(args[:i])
	columnsStr := args[i+4:]
	var columns []string
	for _, col := range strings.Split(columnsStr, ",") {
		col = strings.TrimSpace(col)
		if col != "" {
			columns = append(columns, col)
		}
	}
	concatenated := "''"
	if len(columns) == 1 {
		concatenated = columns[0]
	} else if len(columns) > 1 {
		concatenated = "CONCAT_WS(' '," + strings.Join(columns, ",") + ")"
	}
	switch driverName {
	case "mysql":
		return "(" + concatenated + " REGEXP " + searchExpr + ")", nil
	case "pgx", "cockroachdb":
		return "REGEXP_LIKE(" + concatenated + ", " + searchExpr + ", 'i')", nil
	case "mssql":
		return "REGEXP_LIKE(" + concatenated + ", " + searchExpr + ", 'i')", nil
	case "sqlite":
		return "(" + concatenated + " LIKE ('%' || " + searchExpr + " || '%'))", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}

// lastTopLevelComma finds the last comma in s that is not inside parentheses or quotes.
// Returns -1 if no top-level comma is found.
func lastTopLevelComma(s string) int {
	depth := 0
	lastComma := -1
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' || ch == '"' {
			// Skip to closing quote
			i++
			for i < len(s) && s[i] != ch {
				i++
			}
		} else if ch == '(' {
			depth++
		} else if ch == ')' {
			depth--
		} else if ch == ',' && depth == 0 {
			lastComma = i
		}
	}
	return lastComma
}

// vfDateAddMillis is the handler for the DATE_ADD_MILLIS() virtual function.
// The syntax is DATE_ADD_MILLIS(baseExpr, milliseconds) where baseExpr is a timestamp
// expression and milliseconds is a numeric value. The baseExpr is recursively unpacked,
// so it may contain other virtual functions. For example:
//
//	DATE_ADD_MILLIS(created_at, 5000)
//	DATE_ADD_MILLIS(NOW_UTC(), ?)
func vfDateAddMillis(driverName string, args string) (string, error) {
	// Split by the last comma to separate baseExpr from milliseconds
	lastComma := lastTopLevelComma(args)
	if lastComma < 0 {
		return "", errors.New("DATE_ADD_MILLIS requires syntax: DATE_ADD_MILLIS(baseExpr, milliseconds)")
	}
	baseExpr := strings.TrimSpace(args[:lastComma])
	millis := strings.TrimSpace(args[lastComma+1:])
	if baseExpr == "" || millis == "" {
		return "", errors.New("DATE_ADD_MILLIS requires syntax: DATE_ADD_MILLIS(baseExpr, milliseconds)")
	}
	switch driverName {
	case "mysql":
		return "DATE_ADD(" + baseExpr + ", INTERVAL (" + millis + ") * 1000 MICROSECOND)", nil
	case "pgx", "cockroachdb":
		return "(" + baseExpr + " + MAKE_INTERVAL(secs => (" + millis + ") / 1000.0))", nil
	case "mssql":
		return "DATEADD(MILLISECOND, " + millis + ", " + baseExpr + ")", nil
	case "sqlite":
		return "(STRFTIME('%Y-%m-%d %H:%M:%f', " + baseExpr + ", ((" + millis + ") / 1000.0) || ' seconds'))", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}

// vfDateDiffMillis is the handler for the DATE_DIFF_MILLIS() virtual function.
// The syntax is DATE_DIFF_MILLIS(a, b) which returns the difference (a - b) in milliseconds.
// Both arguments are recursively unpacked, so they may contain other virtual functions.
// For example:
//
//	DATE_DIFF_MILLIS(updated_at, created_at)
//	DATE_DIFF_MILLIS(NOW_UTC(), created_at)
func vfDateDiffMillis(driverName string, args string) (string, error) {
	lastComma := lastTopLevelComma(args)
	if lastComma < 0 {
		return "", errors.New("DATE_DIFF_MILLIS requires syntax: DATE_DIFF_MILLIS(a, b)")
	}
	a := strings.TrimSpace(args[:lastComma])
	b := strings.TrimSpace(args[lastComma+1:])
	if a == "" || b == "" {
		return "", errors.New("DATE_DIFF_MILLIS requires syntax: DATE_DIFF_MILLIS(a, b)")
	}
	switch driverName {
	case "mysql":
		return "(TIMESTAMPDIFF(MICROSECOND, " + b + ", " + a + ") / 1000.0)", nil
	case "pgx", "cockroachdb":
		return "(EXTRACT(EPOCH FROM (" + a + " - " + b + ")) * 1000.0)", nil
	case "mssql":
		return "DATEDIFF_BIG(MILLISECOND, " + b + ", " + a + ")", nil
	case "sqlite":
		return "((JULIANDAY(" + a + ") - JULIANDAY(" + b + ")) * 86400000.0)", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}

// jsonPathElemPattern matches a member name accepted in a JSON_FIELD path. The charset is deliberately
// narrow: every element is spliced into SQL string literals on four dialects, and refusing quotes, brackets
// and dots up front is what makes that splicing safe without per-dialect escaping rules.
var jsonPathElemPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// jsonPathElem is one step of a parsed JSON_FIELD path: a member name, or an array index when index >= 0.
type jsonPathElem struct {
	name  string
	index int
}

// parseJSONPath parses the supported subset of JSONPath: any number of member accesses (.name) and array
// indexes ([0]). Wildcards, recursive descent, filters and quoted member names are rejected — see
// jsonPathElemPattern.
//
// The conventional JSONPath '$' root is accepted but optional, and 'name' is equivalent to '$.name'. The
// path is never passed through to the database — PostgreSQL's #>> wants an array of keys, not a path string,
// so parsing and re-rendering per dialect is mandatory anyway — which leaves the '$' carrying no information
// that this function does not already supply itself. It is accepted because three of the four engines spell
// paths that way natively, so a path copied from their documentation must not be rejected; it is optional
// because demanding a token we then discard and re-emit is ceremony.
func parseJSONPath(path string) ([]jsonPathElem, error) {
	rest := strings.TrimPrefix(path, "$")
	// A leading member name needs no '.' separator, so that 'name' parses as '$.name'.
	if rest != "" && rest[0] != '.' && rest[0] != '[' {
		rest = "." + rest
	}
	var elems []jsonPathElem
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			if !jsonPathElemPattern.MatchString(name) {
				return nil, errors.New("JSON_FIELD path member must match [A-Za-z_][A-Za-z0-9_]*: %s", path)
			}
			elems = append(elems, jsonPathElem{name: name, index: -1})
			rest = rest[end:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, errors.New("JSON_FIELD path has an unclosed '[': %s", path)
			}
			n, err := strconv.Atoi(rest[1:end])
			if err != nil || n < 0 {
				return nil, errors.New("JSON_FIELD path index must be a non-negative integer: %s", path)
			}
			elems = append(elems, jsonPathElem{index: n})
			rest = rest[end+1:]
		default:
			return nil, errors.New("JSON_FIELD path is malformed: %s", path)
		}
	}
	if len(elems) == 0 {
		return nil, errors.New("JSON_FIELD path must name at least one field, e.g. '$.name' or 'name': %s", path)
	}
	return elems, nil
}

// vfJSONField is the handler for the JSON_FIELD() virtual function.
// The syntax is JSON_FIELD(column, '$.path') where column holds JSON text and the path is a literal.
// It extracts one field and returns it as text, with a contract that holds on every driver:
//
//   - a JSON string comes back unquoted and unescaped,
//   - an object or array comes back as its JSON text,
//   - a number or boolean comes back as its text form,
//   - a JSON null, or a path that does not exist, comes back as SQL NULL.
//
// This is the `->>` contract, which is the strongest one all four dialects can agree on. For example:
//
//	SELECT JSON_FIELD(state, '$.userName') FROM flows
//	SELECT id FROM flows WHERE JSON_FIELD(state, '$.tags[0]') = ?
//
// Two constraints, both enforced or documented rather than silently worked around:
//
// The path must be a single-quoted literal, not a ? placeholder: PostgreSQL needs it as an array of keys and
// SQL Server needs it split across two functions, so it has to be known at expansion time. It supports member
// access and array indexes only. The conventional JSONPath '$' root is optional, so '$.name' and 'name' are
// the same path.
//
// The column expression is referenced more than once on MySQL and SQL Server, so it must not itself contain a
// ? placeholder — a bound argument there would be consumed twice and misalign every later placeholder.
//
// Note the SQL Server ceiling: JSON_VALUE returns NVARCHAR(4000) and yields NULL (in lax mode) for a longer
// scalar, so a JSON *string* over 4000 characters reads back as NULL there. Objects and arrays are unaffected
// (they go through JSON_QUERY, which is NVARCHAR(MAX)), as are all other drivers. Extracting large scalars on
// SQL Server needs an OPENJSON ... WITH rowset, which is a different statement shape than a virtual function
// can expand to.
func vfJSONField(driverName string, args string) (string, error) {
	comma := lastTopLevelComma(args)
	if comma < 0 {
		return "", errors.New("JSON_FIELD requires syntax: JSON_FIELD(column, '$.path')")
	}
	col := strings.TrimSpace(args[:comma])
	pathLit := strings.TrimSpace(args[comma+1:])
	if col == "" || len(pathLit) < 2 || pathLit[0] != '\'' || pathLit[len(pathLit)-1] != '\'' {
		return "", errors.New("JSON_FIELD requires a literal path: JSON_FIELD(column, '$.path')")
	}
	elems, err := parseJSONPath(pathLit[1 : len(pathLit)-1])
	if err != nil {
		return "", errors.Trace(err)
	}

	// Canonical '$.a[0].b' form, re-rendered from the parsed elements rather than passed through, so what
	// reaches the database is only ever the validated subset.
	var path strings.Builder
	path.WriteString("$")
	for _, e := range elems {
		if e.index >= 0 {
			path.WriteString("[" + strconv.Itoa(e.index) + "]")
		} else {
			path.WriteString("." + e.name)
		}
	}
	p := "'" + path.String() + "'"

	switch driverName {
	case "mysql":
		// JSON_UNQUOTE maps a JSON null to the *string* 'null', not SQL NULL — the one place MySQL breaks the
		// contract the other three keep. The JSON_TYPE guard restores it.
		extract := "JSON_EXTRACT(" + col + ", " + p + ")"
		return "(CASE WHEN JSON_TYPE(" + extract + ") = 'NULL' THEN NULL ELSE JSON_UNQUOTE(" + extract + ") END)", nil
	case "pgx", "cockroachdb":
		// #>> takes the path as a text[] of keys, and already returns text with scalars unquoted.
		keys := make([]string, 0, len(elems))
		for _, e := range elems {
			if e.index >= 0 {
				keys = append(keys, `"`+strconv.Itoa(e.index)+`"`)
			} else {
				keys = append(keys, `"`+e.name+`"`)
			}
		}
		return "((" + col + ")::jsonb #>> '{" + strings.Join(keys, ",") + "}')", nil
	case "mssql":
		// JSON_VALUE returns scalars (NULL for an object/array); JSON_QUERY returns objects/arrays (NULL for a
		// scalar). Neither alone covers the contract, so they are combined. JSON_QUERY comes first because a
		// scalar over 4000 chars makes JSON_VALUE NULL, and preferring the JSON_QUERY branch keeps that failure
		// confined to scalars rather than letting it swallow objects too.
		return "(COALESCE(JSON_QUERY(" + col + ", " + p + "), JSON_VALUE(" + col + ", " + p + ")))", nil
	case "sqlite":
		// json_extract already returns unquoted scalars, JSON text for objects/arrays, and SQL NULL for a JSON
		// null or a missing path — the contract, exactly.
		return "(JSON_EXTRACT(" + col + ", " + p + "))", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}

// vfLimitOffset is the handler for the LIMIT_OFFSET() virtual function.
// The syntax is LIMIT_OFFSET(limit, offset) where both arguments are numeric expressions
// or ? placeholders. For example:
//
//	SELECT * FROM users ORDER BY id LIMIT_OFFSET(10, 0)
//	SELECT * FROM users ORDER BY id LIMIT_OFFSET(?, ?)
//
// Note: SQL Server requires an ORDER BY clause for OFFSET/FETCH to work.
func vfLimitOffset(driverName string, args string) (string, error) {
	comma := lastTopLevelComma(args)
	if comma < 0 {
		return "", errors.New("LIMIT_OFFSET requires syntax: LIMIT_OFFSET(limit, offset)")
	}
	limit := strings.TrimSpace(args[:comma])
	offset := strings.TrimSpace(args[comma+1:])
	if limit == "" || offset == "" {
		return "", errors.New("LIMIT_OFFSET requires syntax: LIMIT_OFFSET(limit, offset)")
	}
	switch driverName {
	case "mysql", "pgx", "cockroachdb", "sqlite":
		return "LIMIT " + limit + " OFFSET " + offset, nil
	case "mssql":
		return "OFFSET " + offset + " ROWS FETCH NEXT " + limit + " ROWS ONLY", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}
