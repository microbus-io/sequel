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
	"strings"
	"sync"

	"github.com/microbus-io/errors"
)

type virtualFunc struct {
	name        string
	namePattern *regexp.Regexp
	handler     func(driverName string, args string) (string, error)
}

var (
	virtualFuncsMutex sync.RWMutex
	virtualFuncsList  = []virtualFunc{
		newVirtualFunc("NOW_UTC", vfNowUTC),
		newVirtualFunc("REGEXP_TEXT_SEARCH", vfRegexpTextSearch),
		newVirtualFunc("DATE_ADD_MILLIS", vfDateAddMillis),
		newVirtualFunc("DATE_DIFF_MILLIS", vfDateDiffMillis),
		newVirtualFunc("LIMIT_OFFSET", vfLimitOffset),
	}
)

// RegisterVirtualFunc registers a virtual SQL function that will be replaced in queries
// before execution. The name is matched case-insensitively, e.g. registering "NOW_UTC"
// matches NOW_UTC(), now_utc(), Now_Utc(), etc.
// The handler receives the driver name and the string found between the parentheses,
// and returns the replacement SQL expression, or an error.
func RegisterVirtualFunc(name string, handler func(driverName string, args string) (string, error)) {
	virtualFuncsMutex.Lock()
	defer virtualFuncsMutex.Unlock()
	vf := newVirtualFunc(name, handler)
	// Replace existing entry with the same name, if any
	for i, existing := range virtualFuncsList {
		if existing.name == vf.name {
			virtualFuncsList[i] = vf
			return
		}
	}
	virtualFuncsList = append(virtualFuncsList, vf)
}

// newVirtualFunc creates a virtualFunc with a compiled pattern for the given name.
func newVirtualFunc(name string, handler func(driverName string, args string) (string, error)) virtualFunc {
	return virtualFunc{
		name:        strings.ToUpper(name),
		namePattern: regexp.MustCompile(`(?i)` + regexp.QuoteMeta(name) + `\(`),
		handler:     handler,
	}
}

// expandVirtualFuncs replaces virtual function calls in the query with driver-specific expressions.
// Parentheses in arguments are balanced so that nested function calls (e.g. DATE_ADD_MILLIS(NOW_UTC(), ?)) work.
// Multiple passes are performed until no more expansions occur, allowing inner virtual functions
// to be expanded before outer ones that depend on them.
func expandVirtualFuncs(driverName string, query string) (string, error) {
	virtualFuncsMutex.RLock()
	vfs := virtualFuncsList
	virtualFuncsMutex.RUnlock()
	for {
		prev := query
		for _, vf := range vfs {
			loc := vf.namePattern.FindStringIndex(query)
			if loc == nil {
				continue
			}
			// loc[1] points right after the opening '('
			// Find the balanced closing ')', skipping quoted regions
			depth := 1
			closePos := -1
			for i := loc[1]; i < len(query); i++ {
				ch := query[i]
				if ch == '\'' || ch == '"' {
					// Skip to closing quote
					i++
					for i < len(query) && query[i] != ch {
						i++
					}
				} else if ch == '(' {
					depth++
				} else if ch == ')' {
					depth--
					if depth == 0 {
						closePos = i
						break
					}
				}
			}
			if closePos < 0 {
				continue // Unbalanced parens, skip
			}
			args := query[loc[1]:closePos]
			result, err := vf.handler(driverName, args)
			if err != nil {
				return "", errors.Trace(err)
			}
			query = query[:loc[0]] + result + query[closePos+1:]
		}
		if query == prev {
			break
		}
	}
	return query, nil
}

// vfNowUTC is the handler for the NOW_UTC() virtual function.
// It returns the current UTC timestamp with millisecond precision.
func vfNowUTC(driverName string, args string) (string, error) {
	switch driverName {
	case "mysql":
		return "UTC_TIMESTAMP(3)", nil
	case "pgx":
		return "(NOW() AT TIME ZONE 'UTC')", nil
	case "mssql":
		return "SYSUTCDATETIME()", nil
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
		return concatenated + " REGEXP " + searchExpr, nil
	case "pgx":
		return "REGEXP_LIKE(" + concatenated + ", " + searchExpr + ", 'i')", nil
	case "mssql":
		return "REGEXP_LIKE(" + concatenated + ", " + searchExpr + ", 'i')", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
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
	lastComma := strings.LastIndex(args, ",")
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
	case "pgx":
		return baseExpr + " + MAKE_INTERVAL(secs => (" + millis + ") / 1000.0)", nil
	case "mssql":
		return "DATEADD(MILLISECOND, " + millis + ", " + baseExpr + ")", nil
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
	lastComma := strings.LastIndex(args, ",")
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
		return "TIMESTAMPDIFF(MICROSECOND, " + b + ", " + a + ") / 1000.0", nil
	case "pgx":
		return "EXTRACT(EPOCH FROM (" + a + " - " + b + ")) * 1000.0", nil
	case "mssql":
		return "DATEDIFF_BIG(MILLISECOND, " + b + ", " + a + ")", nil
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
	comma := strings.LastIndex(args, ",")
	if comma < 0 {
		return "", errors.New("LIMIT_OFFSET requires syntax: LIMIT_OFFSET(limit, offset)")
	}
	limit := strings.TrimSpace(args[:comma])
	offset := strings.TrimSpace(args[comma+1:])
	if limit == "" || offset == "" {
		return "", errors.New("LIMIT_OFFSET requires syntax: LIMIT_OFFSET(limit, offset)")
	}
	switch driverName {
	case "mysql", "pgx":
		return "LIMIT " + limit + " OFFSET " + offset, nil
	case "mssql":
		return "OFFSET " + offset + " ROWS FETCH NEXT " + limit + " ROWS ONLY", nil
	default:
		return "", errors.New("unsupported driver name: %s", driverName)
	}
}
