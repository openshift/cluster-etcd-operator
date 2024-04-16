package installer

import (
	"k8s.io/apimachinery/pkg/util/sets"
)

// sets.Int32 is a set of int32s, implemented via map[int32]struct{} for minimal memory consumption.
// Deprecated: Use k8s sets instead.
type Int32 = sets.Int32

// NewInt32 creates a Int32 from a list of values.
// Deprecated: Use k8s sets instead.
var NewInt32 = sets.NewInt32

// Int32KeySet creates a Int32 from a keys of a map[int32](? extends interface{}).
// If the value passed in is not actually a map, this will panic.
// Deprecated: Use k8s sets instead.
var Int32KeySet = sets.Int32KeySet[any]
