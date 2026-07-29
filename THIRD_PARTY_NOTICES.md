# Third-party notices

Harness itself is licensed under the [MIT License](LICENSE). This file records the
third-party components it bundles or depends on, and reproduces the notices their
licenses require us to carry.

Two categories, with different obligations:

- **Vendored web assets** — committed to this repository and compiled into the binary
  via `embed.FS`. Distributing the repo or the binary redistributes these, so their
  notices are reproduced in full below.
- **Go modules** — referenced by `go.mod`, *not* vendored (there is no `vendor/`
  directory), so the source tree does not redistribute them. Their obligations attach
  to any **binary** you build and ship, since the Go toolchain links them statically.
  They are listed below for that purpose.

---

## Vendored web assets

### Control room (`internal/controlroom/assets/static/`)

| Component | Version | License |
|---|---|---|
| [htmx](https://htmx.org) (`htmx.min.js`) | 2.0.4 | Zero-Clause BSD |
| [htmx SSE extension](https://htmx.org/extensions/sse/) (`htmx-ext-sse.min.js`) | — | see note below |
| [Alpine.js](https://alpinejs.dev) (`alpine.min.js`) | 3.14.9 | MIT |
| [Tailwind CSS](https://tailwindcss.com) (compiled into `app.css`) | 4.3.0 | MIT |
| [Geist Sans / Geist Mono](https://github.com/vercel/geist-font) (`fonts/*.woff2`) | — | SIL Open Font License 1.1 |

`alerts.js`, `board-autoscroll.js`, `dag.js`, `lineage.js`, `ticker.js` and `wizard.js`
in that directory are original to this project and carry the repository's MIT license.

**Note on the SSE extension.** It is published by the htmx project as
[`bigskysoftware/htmx-extensions`](https://github.com/bigskysoftware/htmx-extensions),
whose npm package (`htmx-ext-sse`) declares no `license` field and whose repository
carries no separate `LICENSE` file. It is distributed as part of htmx, which is
Zero-Clause BSD; we record it here on that basis and note the upstream gap rather than
assert a license the project has not stated.

### Vault demo application (`demo/vault/app/internal/web/static/`)

The demo app is a self-contained Go web application used to exercise the harness. It
vendors its own copies:

| Component | Version | License |
|---|---|---|
| [htmx](https://htmx.org) (`htmx.min.js`) | 2.0.4 | Zero-Clause BSD |
| [Alpine.js](https://alpinejs.dev) (`alpine.min.js`) | 3.14.8 | MIT |
| [Tailwind CSS](https://tailwindcss.com) (compiled into `app.css`) | 4.3.0 | MIT |

`localtime.js` is original to this project.

---

## Required notices

### Geist Sans and Geist Mono — SIL Open Font License 1.1

> Copyright 2024 The Geist Project Authors (https://github.com/vercel/geist-font)
>
> Earlier releases carry the notice: Copyright (c) 2023 Vercel, in collaboration with
> basement.studio.

The OFL requires its full text to accompany the font files. It is bundled at
[`internal/controlroom/assets/static/fonts/OFL.txt`](internal/controlroom/assets/static/fonts/OFL.txt),
which sits inside the `//go:embed static` tree — so it travels with the fonts both in
this repository and inside the compiled binary, where it is served at
`/static/fonts/OFL.txt`.

Note the OFL's naming clause: the Reserved Font Name may not be used to distribute
modified versions of the fonts. These files are unmodified.

### Alpine.js — MIT License

> Copyright © 2019-2025 Caleb Porzio and contributors

### Tailwind CSS — MIT License

> Copyright (c) Tailwind Labs, Inc.

The MIT permission notice applying to both of the above:

```
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### htmx — Zero-Clause BSD

```
Permission to use, copy, modify, and/or distribute this software for
any purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

0BSD imposes no attribution requirement; htmx is listed for completeness.

---

## Go modules

Direct dependencies from [`go.mod`](go.mod). These are not vendored here, but they are
statically linked into any binary built from this tree — if you distribute such a
binary, the Apache-2.0 and BSD terms below require you to carry their license texts.

| Module | License |
|---|---|
| `github.com/a-h/templ` | MIT — Copyright (c) 2021 Adrian Hesketh |
| `github.com/anthropics/anthropic-sdk-go` | MIT — Copyright 2023 Anthropic, PBC. |
| `github.com/mdlayher/vsock` | MIT — Copyright (C) 2017-2022 Matt Layher |
| `github.com/minio/minio-go/v7` | Apache-2.0 |
| `github.com/nats-io/nats-server/v2` | Apache-2.0 |
| `github.com/nats-io/nats.go` | Apache-2.0 |
| `github.com/openai/openai-go/v3` | Apache-2.0 |
| `go.opentelemetry.io/otel` and the `otel/*`, `contrib/*` modules | Apache-2.0 |
| `golang.org/x/sync` | BSD-3-Clause — Copyright 2009 The Go Authors |
| `gopkg.in/yaml.v3` | MIT and Apache-2.0 (dual) |

Indirect dependencies are pinned in [`go.sum`](go.sum). The repository already gates on
dependency licenses — `make license-scan` runs `go-licenses check` as one of the qa
stage's postconditions (it needs `go-licenses` on PATH; the sandbox image bakes it in).
To enumerate every module and license actually linked into a build:

```sh
go install github.com/google/go-licenses@latest
go-licenses report ./cmd/harness
```

Each module's authoritative license text ships in its own source, available under
`$(go env GOMODCACHE)/<module>@<version>/` after `go mod download`.

---

## External tools

The harness shells out to or coordinates with these at runtime; it does not bundle or
link them, so no notice obligation arises. Listed because a deployment needs them:

- [beads](https://github.com/steveyegge/beads) (`bd`) — the work-item store.
- Docker / gVisor — sandbox backends.
- Git — repository operations inside the runner's broker.
