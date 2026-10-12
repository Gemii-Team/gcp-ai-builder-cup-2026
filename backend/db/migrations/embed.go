// Package migrations embeds the goose SQL migrations, so the kiwi binary
// carries the schema it needs.
package migrations

import "embed"

// FS holds the migrations at its root, in goose's file naming.
//
//go:embed *.sql
var FS embed.FS
