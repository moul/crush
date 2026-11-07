package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A custom command's arguments are filled in the same way whatever order
// the map of values is ranged in: a placeholder that starts with another
// one's name gets its own value, and a value that reads like a placeholder
// is kept as typed.
func TestSubstituteArgsOrderIndependent(t *testing.T) {
	t.Parallel()

	content := "Rename $FILE to $FILE_NAME, then say $NOTE."
	args := map[string]string{
		"FILE":      "a.go",
		"FILE_NAME": "b.go",
		"NOTE":      "$FILE",
	}
	// Map iteration order is random; enough runs see every order.
	for range 100 {
		require.Equal(t, "Rename a.go to b.go, then say $FILE.", substituteArgs(content, args))
	}
}
