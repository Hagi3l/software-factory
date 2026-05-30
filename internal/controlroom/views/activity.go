package views

import "time"

// activityTime formats an activity entry timestamp for the feed. A zero time renders
// empty (e.g. a hand-built entry in a test). It is a plain string helper, not a class
// helper, so keeping it in Go is fine — the Tailwind scanner only needs to see class
// literals, which live in the templ switch (activityBadge).
func activityTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04:05")
}
