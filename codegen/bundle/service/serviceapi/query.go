package serviceapi

import (
	"context"
	"regexp"
	"strings"

	"github.com/microbus-io/errors"
)

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

/*
Query is used to select a subset of records using filtering options.

Select is an optional comma-separated subset of fields to include in the result. For example, "column_one,column_two".
If not specified, all fields are included by default.

OrderBy is a comma-separated subset of fields to order the results by.
A hyphen before the field name denotes a descending order. For example, "-column_one,column_two".
If not specified, the default sort order is by "id".
*/
type Query struct {
	Key  ObjKey   `json:"key,omitzero"`
	Keys []ObjKey `json:"keys,omitzero"`
	Q    string   `json:"q,omitzero"`

	Select  string `json:"select,omitzero"`
	OrderBy string `json:"orderBy,omitzero"`
	Limit   int    `json:"limit,omitzero"`
	Offset  int    `json:"offset,omitzero"`

	// HINT: Add additional filtering options here
	Example string `json:"example,omitzero"` // Do not remove the example
}

// Validate validates the filtering options of the query.
func (q *Query) Validate(ctx context.Context) error {
	if q == nil {
		return errors.New("nil object")
	}
	for col := range strings.SplitSeq(q.Select, ",") {
		col := strings.TrimSpace(strings.ToLower(col))
		if col != "" && !safeIdentifier.MatchString(col) {
			return errors.New("invalid column name to select: %s", col)
		}
	}
	for orderBy := range strings.SplitSeq(q.OrderBy, ",") {
		orderBy := strings.TrimSpace(strings.ToLower(orderBy))
		if orderBy != "" && !safeIdentifier.MatchString(strings.TrimPrefix(orderBy, "-")) {
			return errors.New("invalid column name to order by: %s", orderBy)
		}
	}
	if q.Limit < 0 {
		return errors.New("limit can't be negative")
	}
	if q.Offset < 0 {
		return errors.New("offset can't be negative")
	}

	// HINT: Validate filtering options here as required
	q.Example = strings.TrimSpace(q.Example) // Do not remove the example
	if len([]rune(q.Example)) > 256 {
		return errors.New("length of Example must not exceed 256 characters")
	}

	return nil
}
