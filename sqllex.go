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

import "strings"

// skipNonCode returns the index just past the string literal, quoted identifier or comment that starts at s[i], or i
// when none starts there. It follows each dialect's quoting:
//
//   - '...' everywhere, a quote escaped by doubling it; a backslash escapes too on MySQL and MariaDB, and in a
//     PostgreSQL E'...' string
//   - "..." everywhere (an identifier, or a string on MySQL), doubled to escape, with backslashes on MySQL
//   - `...` on MySQL, MariaDB and SQLite, and [...] on SQL Server and SQLite, with ]] escaping a bracket
//   - -- to the end of the line (on MySQL and MariaDB, -- followed by whitespace, or #), and /* ... */, nested on
//     PostgreSQL, CockroachDB and SQL Server
//   - $tag$...$tag$ on PostgreSQL and CockroachDB, only when dollarQuotes is set
//
// An unterminated region runs to the end of s.
func skipNonCode(driverName, s string, i int, dollarQuotes bool) int {
	mysqlish := driverName == "mysql" || driverName == "mariadb"
	pg := driverName == "pgx" || driverName == "cockroachdb"
	switch ch := s[i]; {
	case ch == '\'':
		escapeE := pg && i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i == 1 || !isIdentByte(s[i-2]))
		return skipQuoted(s, i, '\'', mysqlish || escapeE)
	case ch == '"':
		return skipQuoted(s, i, '"', mysqlish)
	case ch == '`' && (mysqlish || driverName == "sqlite"):
		return skipQuoted(s, i, '`', false)
	case ch == '[' && (driverName == "mssql" || driverName == "sqlite"):
		for j := i + 1; j < len(s); j++ {
			if s[j] == ']' {
				if j+1 < len(s) && s[j+1] == ']' {
					j++
					continue
				}
				return j + 1
			}
		}
		return len(s)
	case ch == '-' && strings.HasPrefix(s[i:], "--") && (!mysqlish || i+2 == len(s) || s[i+2] <= ' '),
		ch == '#' && mysqlish:
		// On MySQL and MariaDB, -- starts a comment only before whitespace (1--1 is arithmetic), and # starts one too
		if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
			return i + j + 1
		}
		return len(s)
	case ch == '/' && strings.HasPrefix(s[i:], "/*"):
		if !pg && driverName != "mssql" {
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				return i + 2 + j + 2
			}
			return len(s)
		}
		// PostgreSQL, CockroachDB and SQL Server nest block comments, and so does the SQL Server driver's own scanner
		depth := 0
		for j := i; j+1 < len(s); j++ {
			switch {
			case s[j] == '/' && s[j+1] == '*':
				depth++
				j++
			case s[j] == '*' && s[j+1] == '/':
				depth--
				j++
				if depth == 0 {
					return j + 1
				}
			}
		}
		return len(s)
	case ch == '$' && dollarQuotes && pg && (i == 0 || !isIdentByte(s[i-1])):
		// $tag$ where the tag is empty or an identifier; $1 is a parameter, not a quote
		j := i + 1
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			return i
		}
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		if j >= len(s) || s[j] != '$' {
			return i
		}
		tag := s[i : j+1]
		if k := strings.Index(s[j+1:], tag); k >= 0 {
			return j + 1 + k + len(tag)
		}
		return len(s)
	}
	return i
}

// skipQuoted returns the index just past the region quoted by q that starts at s[i]. A doubled quote is an escaped
// quote, and so is a backslash before any character when backslash is set.
func skipQuoted(s string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(s); j++ {
		switch {
		case backslash && s[j] == '\\':
			j++
		case s[j] == q:
			if j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

// isWordByte reports whether b is a letter, a digit or an underscore: a byte of a function name or a dollar-quote tag.
func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// isIdentByte reports whether b may appear in an unquoted identifier, which on PostgreSQL may hold a '$'.
func isIdentByte(b byte) bool {
	return b == '_' || b == '$' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
