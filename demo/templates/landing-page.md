# Acme landing page

A single, self-contained static landing page for a fictional product called **Acme**.
No build step, no framework, no external assets — one `index.html` file at the
repository root that a browser can open directly.

This spec is the contract the acceptance test encodes and the implementation must
satisfy. Keep it small and literal.

## Acceptance criteria

The file `index.html` at the repository root must exist and must contain, as plain
HTML:

1. A document **title** containing the word `Acme` — i.e. a `<title>` element whose
   text includes `Acme`.
2. A top-level **heading**: at least one `<h1>` element.
3. A **tagline**: at least one `<p>` paragraph element (a short sentence describing the
   product).
4. A **call to action**: at least one `<a>` anchor element whose visible text is
   `Get started` (it may link anywhere, e.g. `href="#"`).

That is the whole contract. Anything beyond it (styling, extra sections, layout) is
allowed but not required, and must not remove or break the four elements above.

## Notes

- The page is judged by a text-level acceptance check that looks for these elements in
  `index.html`; it does not render the page or run a browser. Write ordinary,
  well-formed HTML and the check will find them.
- There is no server and no JavaScript requirement. A `Get started` link with
  `href="#"` satisfies the call-to-action criterion.
