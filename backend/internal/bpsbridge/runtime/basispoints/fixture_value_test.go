package basispoints

import "fmt"

// fixtureValue checks the shape of a test fixture before using it.
func fixtureValue[T any](value any) T {
	result, ok := value.(T)
	if !ok {
		panic(fmt.Sprintf("fixture value type %T, expected %T", value, result))
	}
	return result
}
