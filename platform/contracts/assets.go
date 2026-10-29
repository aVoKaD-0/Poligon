package contracts

import "embed"

//go:embed schemas/*.json catalog/*.json
var Files embed.FS
