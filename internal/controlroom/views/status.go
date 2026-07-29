package views

import "github.com/Loxstomper/software-factory/internal/controlroom/query"

// healthTitle is the budget-health dot's tooltip — kept in plain Go (not the .templ) because it
// is text, never a class literal: only the tint classes need to live in the .templ for the
// Tailwind @source scanner to compile them.
func healthTitle(h string) string {
	switch h {
	case query.StatusHealthBreach:
		return "Budget breached — burn is over a configured cap"
	case query.StatusHealthWarn:
		return "Budget warning — burn is at or above 80% of a cap"
	default:
		return "Budget healthy"
	}
}
