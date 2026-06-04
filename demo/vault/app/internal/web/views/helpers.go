package views

import (
	"strconv"

	"github.com/a-h/templ"
)

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// formAction is the POST target for the secret form: /secrets for a new secret (id 0),
// /secrets/{id} for an edit.
func formAction(id int64) templ.SafeURL {
	if id == 0 {
		return templ.SafeURL("/secrets")
	}
	return templ.SafeURL("/secrets/" + itoa64(id))
}
