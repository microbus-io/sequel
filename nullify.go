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
	"database/sql"

	"github.com/microbus-io/errors"
)

/*
Nullify returns nil if the value equals to the zero value of its Go data type, else it returns the value.
Use this construct to convert zero values to nil when writing to a nullable database column.

Example:

	db.Exec(
		"INSERT INTO my_table (id, desc, modified_time) VALUES (?,?,?)",
		obj.ID,
		sequel.Nullify(obj.Description),
		sequel.Nullify(obj.ModifiedTime),
	)
*/
func Nullify[T comparable](value T) any {
	if value == *new(T) {
		return nil
	} else {
		return value
	}
}

// Null is a thin wrapper over sql.Null that allows for reading NULL values.
type Null[T any] struct {
	*Binder[T]
}

/*
Nullable is a simple binder that interprets NULL values to be the zero value of their Go data type.

Example:

	var obj Object
	args := []any{
		&obj.ID,
		sequel.Nullable(&obj.Description),
		sequel.Nullable(&obj.ModifiedTime),
	}
	db.QueryRow("SELECT id, desc, modified_time FROM my_table WHERE id=?", id).Scan(args...)
	sequel.ApplyBindings(args...)
*/
func Nullable[T any](ptr *T) *Null[T] {
	return &Null[T]{
		Binder: Bind(func(value T) (err error) {
			*ptr = value
			return nil
		}),
	}
}

// Binder is a thin wrapper over sql.Null that allows for late-binding of its value.
type Binder[T any] struct {
	sql.Null[T]
	apply func(value T) (err error)
}

/*
Bind applies a binding function to the scanned value.

Example:

	var obj Object
	args := []any{
		&obj.ID,
		sequel.Bind(func(tags string) {
			return json.Unmarshal([]byte(tags), &obj.Tags)
		}),
		sequel.Bind(func(modifiedTime time.Time) {
			obj.Year, obj.Month, obj.Day = modifiedTime.Date()
			return nil
		}),
	}
	db.QueryRow("SELECT id, tags, modified_time FROM my_table WHERE id=?", id).Scan(args...)
	sequel.ApplyBindings(args...)
*/
func Bind[T any](binder func(value T) (err error)) *Binder[T] {
	return &Binder[T]{apply: binder}
}

// Apply should be called after scanning the columns from the result set.
func (n *Binder[T]) Apply() (err error) {
	if n.apply != nil {
		err = n.apply(n.V)
	}
	return errors.Trace(err)
}

// ApplyBindings should be called after scanning values from the result set to perform all late binding.
func ApplyBindings(args ...any) (err error) {
	for _, arg := range args {
		if applier, ok := arg.(interface{ Apply() (err error) }); ok {
			err = applier.Apply()
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}

// UnsafeSQL wraps a string to indicate not to use an argument placeholder when inserting it into a SQL statement.
// It should be used to insert values such as NOW() or calculation of other fields.
// Use with caution to avoid SQL injection.
type UnsafeSQL string
