package graphql

import (
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/graphql-go/graphql/gqlerrors"
)

// type Schema interface{}

type Result struct {
	Data   interface{}                `json:"data"`
	Errors []gqlerrors.FormattedError `json:"errors,omitempty"`
}

func (r *Result) HasErrors() bool {
	return (len(r.Errors) > 0)
}
