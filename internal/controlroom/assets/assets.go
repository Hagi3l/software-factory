// Package assets embeds the control room's static front-end files into the binary.
//
// Everything served under /static lives here: the vendored runtime JS (htmx, the htmx
// SSE extension, Alpine) and the Tailwind-compiled stylesheet (app.css, produced from
// app.tw.css by `go generate`). Embedding them is what makes a deployed harness a single
// self-contained binary with no runtime asset toolchain (specs/control-room.md, "Stack").
//
// app.tw.css is the Tailwind *input* and is intentionally not embedded — it is a build
// source, not a served file. Only the compiled static/ tree ships.
package assets

import "embed"

// FS holds the served static files. It is rooted at the package dir; callers strip the
// "static/" prefix when mounting it at /static (see controlroom.Server).
//
//go:embed static
var FS embed.FS
