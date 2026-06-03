package views

import (
	"strings"

	"github.com/Loxstomper/harness/internal/controlroom/configview"
)

// joinList renders a string slice as a comma-separated line for a table cell. It is a plain
// text helper (no CSS class literals), so keeping it in Go is fine — the Tailwind scanner only
// needs to see class strings, which live in the .templ markup.
func joinList(xs []string) string { return strings.Join(xs, ", ") }

// profileArtifact picks the concrete bootable artifact a sandbox profile resolves to: the
// container image (docker/gvisor) or the rootfs (firecracker), whichever the active overlay
// populated. Text only.
func profileArtifact(p configview.ProfileRow) string {
	if p.Image != "" {
		return p.Image
	}
	return p.Rootfs
}
