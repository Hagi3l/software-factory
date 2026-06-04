package web

import (
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/harness-demo/vault/internal/store"
	"github.com/harness-demo/vault/internal/web/views"
)

// render writes a templ component as an HTML response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

const dateLayout = "2006-01-02"

func toSecretView(sec store.Secret) views.Secret {
	v := views.Secret{
		ID:      sec.ID,
		Name:    sec.Name,
		Tag:     sec.Tag,
		Updated: sec.UpdatedAt.Format(dateLayout),
	}
	if sec.ExpiresAt != nil {
		v.Expires = sec.ExpiresAt.Format(dateLayout)
		v.Expired = sec.ExpiresAt.Before(time.Now())
	}
	return v
}

func toSecretViews(secs []store.Secret) []views.Secret {
	out := make([]views.Secret, 0, len(secs))
	for _, sec := range secs {
		out = append(out, toSecretView(sec))
	}
	return out
}

func toAuditViews(entries []store.AuditEntry) []views.Audit {
	out := make([]views.Audit, 0, len(entries))
	for _, e := range entries {
		out = append(out, views.Audit{Action: e.Action, Target: e.Target, At: e.At.Format("Jan 2 15:04")})
	}
	return out
}

// computeStats derives the dashboard cards: total secrets, how many expire within 7 days
// (or are already expired), and the most recent activity time.
func computeStats(secs []store.Secret, audit []store.AuditEntry) views.Stats {
	st := views.Stats{Total: len(secs), LastSeen: "—"}
	soon := time.Now().Add(7 * 24 * time.Hour)
	for _, sec := range secs {
		if sec.ExpiresAt != nil && sec.ExpiresAt.Before(soon) {
			st.Expiring++
		}
	}
	if len(audit) > 0 {
		st.LastSeen = audit[0].At.Format("Jan 2 15:04")
	}
	return st
}
