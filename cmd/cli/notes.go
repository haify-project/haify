package main

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// printNote prints what the controller said about an operation that
// succeeded, under the command's own confirmation line.
//
// The controller's reply to a create or a resize can carry more than "done":
// that a running guest could not be shown its grown disk and what to run for
// it, that a snapshot is crash consistent and why, that a filesystem was
// grown with its volume. plain is the reply that says nothing more, which is
// not repeated.
func printNote(message string, plain ...string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	for _, p := range plain {
		if message == p {
			zap.L().Debug("printNote: nothing beyond the confirmation", zap.String("message", message))
			return
		}
	}
	zap.L().Debug("printNote: printing the controller's note", zap.String("message", message))
	fmt.Println(message)
}
