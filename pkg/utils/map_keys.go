package utils

import (
	"maps"
	"slices"
)

// SortedMapKeys returns the keys of a string-keyed map in sorted order.
func SortedMapKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}
