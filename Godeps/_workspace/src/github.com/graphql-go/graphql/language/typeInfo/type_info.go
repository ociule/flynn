package typeInfo

import (
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/graphql-go/graphql/language/ast"
)

// TypeInfoI defines the interface for TypeInfo Implementation
type TypeInfoI interface {
	Enter(node ast.Node)
	Leave(node ast.Node)
}
