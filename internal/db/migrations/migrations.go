// Package migrations embeds the goose migrations so the binary can apply
// them without the SQL files on disk.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
