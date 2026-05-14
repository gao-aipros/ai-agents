package static

import "embed"

// FS contains all static files (CSS, JS).
//
//go:embed *.css *.js
var FS embed.FS
