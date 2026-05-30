package controlroom

// The control room's build pipeline runs entirely through `go generate` (invoked by
// `make generate`): templ compiles the typed view components, then the Tailwind standalone
// CLI compiles the stylesheet. Both outputs are committed so a plain `go build` needs
// neither tool — the binary is self-contained and the toolchain is a build-time-only
// concern (specs/control-room.md, "Stack").
//
// Order matters: templ must run first so the generated *_templ.go carry the utility class
// names Tailwind scans (see assets/app.tw.css @source globs). The Tailwind binary is
// fetched on demand into bin/ by the Makefile (TAILWIND target); $(TAILWIND) is exported
// into the environment when `make generate` calls `go generate`.

//go:generate templ generate
//go:generate sh -c "$TAILWIND -i assets/app.tw.css -o assets/static/app.css --minify"
