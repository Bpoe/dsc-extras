// Package exportinput handles optional stdin filters for resource exports.
package exportinput

import (
	"io"
	"strings"

	"golang.org/x/term"
)

// Reader treats an interactive terminal as empty input. Redirected files and
// pipes are returned unchanged so callers continue reading filters until EOF.
// Call this before wrapping input in a reader that hides its file descriptor.
func Reader(input io.Reader) io.Reader {
	if file, ok := input.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(file.Fd())) {
		return strings.NewReader("")
	}
	return input
}
