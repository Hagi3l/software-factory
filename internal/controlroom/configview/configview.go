// Package configview is the read-only projection behind the control room's Config view
// (specs/control-room.md, "The config view", T4.26): the declared factory at rest — the
// role-flow pipeline, the gate checks, the resolved soul roster, policy, and redacted infra
// — made readable. It is a pure config.* → ConfigView projection: it imports internal/config
// (the validated factory config), internal/core (souls), and the control room's own dag
// renderer (to draw the role-flow graph), and returns presentation structs the views render.
//
// Two design points the spec fixes:
//   - It is NOT a query.Reader method. Config does not come from the beads/git/artifact
//     stores; it is the in-process validated object the running factory holds, so the
//     composition root threads it into controlroom.Options and the handler builds this view
//     (config is restart-static — there is nothing to read live).
//   - Rendered and raw are one model, two projections. Each section's "raw" fold is the
//     *effective config re-serialized with the same redactions applied* (labeled "effective
//     config (redacted)"), never the file bytes — so the raw fold can never leak what the
//     rendered view masks. Redaction is by allowlist (mask topology: nats url, model/otel
//     endpoints, artifact path; keep the egress allowlist and image/rootfs digests), so a
//     field added to infra later cannot silently leak.
package configview

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/controlroom/dag"
	"github.com/Loxstomper/software-factory/internal/core"
)

// redactedMark is what a masked (allowlisted-sensitive) value renders as in both the
// structured view and the raw fold. A non-empty marker so a masked field reads as
// "deliberately hidden", not "unset"; an unset field stays empty (see mask).
const redactedMark = "«redacted»"

// ReqPlannerKey is the persona-route key (GET /config/souls/{name}/persona) for the
// requirements planner, which is not a soul and so has no soul Name. The server looks up
// soul names first, so a soul of this name would shadow it — but no soul fulfills the
// trusted requirements stage, so the collision is theoretical.
const ReqPlannerKey = "requirements-planner"

// ConfigView is the whole rendered Config page model. It is built once per request from the
// validated config (cheap; config is static) with redaction applied a single time, so the
// structured sections and the per-section raw folds agree by construction.
type ConfigView struct {
	Identity Identity
	// PipelineSVG is the role-flow graph rendered server-side to SVG by the shared dag
	// renderer (T4.6) fed the declared stages instead of issues: stages are nodes, `produces`
	// edges solid, `on_failure` edges dashed amber. Empty when there is no DAG.
	PipelineSVG string
	Stages      []StageRow
	Checks      []CheckRow
	Roles       []RoleRow
	// ReqPlanner is the trusted, non-sandboxed requirements planner, shown beside the soul
	// roster because it carries a persona (its system prompt) too — but set apart, since it
	// is not a sandboxed soul. Its Configured flag is false when no requirements_planner is
	// declared (the wizard is then disabled), and the view omits the card.
	ReqPlanner ReqPlannerRow
	Policy     PolicyView
	Infra      InfraView

	// Warnings are the non-fatal config advisories config.Warnings() returns (T2.13) — the same
	// list `software-factory validate` prints to stderr at startup: producer/verifier model-family overlap
	// weakening N-version independence, or a package-proxy / git-remote named but not allowlisted.
	// Surfacing them here makes the safety signal visible where an operator inspects the running
	// factory, not only in the launch logs. nil/empty when the config is clean — the view then
	// renders no advisories section (a quiet page means a quiet config).
	Warnings []string

	// Per-section raw folds: the effective config re-serialized to YAML with redaction
	// applied (InfraYAML), or verbatim where the section holds no secrets (the rest).
	StagesYAML string
	ChecksYAML string
	SoulsYAML  string
	PolicyYAML string
	InfraYAML  string
}

// Identity is the "which factory is this" strip: the config root, the active infra overlay
// (infra.<env>.yaml in force), the autonomy profile, and that the config passed startup
// validation (always true here — the server only runs after Validate succeeds).
type Identity struct {
	Root      string
	Env       string
	Profile   string
	Validated bool
}

// StageRow is one workflow DAG stage, flattened for the stages table.
type StageRow struct {
	Name          string
	Kind          string // human-readable: "human"/"agent"/"plan"/"resolve"/"trusted-merge"
	Role          string
	Precondition  string
	Postcondition []string
	OnFailure     string
	Produces      []string
}

// CheckRow is one entry of the check registry: a postcondition name and the shell command
// that realizes it in the verification sandbox. Independent marks a scanner the gate keeps
// running past a failure so one qa pass aggregates every finding (T2.12); the proofs and the
// metric stay fail-fast and read false.
type CheckRow struct {
	Name        string
	Command     string
	Independent bool
}

// RoleRow groups the souls that fulfill one role, ordered by selection specificity so the
// orchestrator's own resolution (selectSoul) is legible — most-specific first, the
// empty-selector catch-all last.
type RoleRow struct {
	Role  string
	Souls []SoulRow
}

// SoulRow is one soul shown resolved: its model joined to provider+cost, its sandbox profile
// joined to the concrete digest-pinned artifact, its selector, and whether it is the
// catch-all default (empty selector). PersonaPath is the path label; the persona body
// (the soul's verbatim system prompt) is fetched lazily by the view from the persona route.
type SoulRow struct {
	Name        string
	Model       string
	Provider    string
	Cost        string // formatted per-Mtok price, "" when unpriced or the model is unknown
	Sandbox     string // logical profile name the soul declares
	Image       string // concrete artifact the active backend resolves it to (digest in prod)
	PersonaPath string // linked, not inlined
	Selector    []KV   // sorted selector pairs
	CatchAll    bool   // empty selector — the default soul for the role
}

// KV is a sorted selector key/value pair, rendered as a chip.
type KV struct{ Key, Val string }

// ReqPlannerRow is the requirements planner shown resolved like a soul (model joined to
// provider+cost) but flagged as the trusted, non-sandboxed interactive planner — it has no
// sandbox and never runs untrusted. Key is its persona-route key (ReqPlannerKey); Configured
// is false when no requirements_planner block is declared.
type ReqPlannerRow struct {
	Key         string
	Model       string
	Provider    string
	Cost        string // formatted per-Mtok price, "" when unpriced or the model is unknown
	PersonaPath string
	Configured  bool
}

// PolicyView is the termination/autonomy policy, formatted for display (uncapped dimensions
// show ∞ rather than a misleading 0).
type PolicyView struct {
	Profile    string
	MaxRetries string
	Budget     BudgetView
	EpicBudget BudgetView
	DeadLetter string
	TCBPaths   []string
}

// BudgetView is a budget's three dimensions formatted, with 0 (uncapped) shown as ∞.
type BudgetView struct {
	Tokens string
	USD    string
	Wall   string
}

// InfraView is the environment overlay, redacted: topology fields (nats url, endpoints,
// artifact path) are masked, operational policy (the egress allowlist) and build provenance
// (image/rootfs digests, model providers + cost) are kept visible.
type InfraView struct {
	SandboxBackend   string
	Egress           string
	Limits           string
	Profiles         []ProfileRow
	NATSURL          string // redacted
	Allowlist        []string
	ArtifactsBackend string
	ArtifactsPath    string // redacted
	OTelEndpoint     string // redacted
	Models           []ModelRow
}

// ProfileRow resolves a logical sandbox profile name to its concrete bootable artifact.
type ProfileRow struct {
	Name   string
	Image  string
	Rootfs string
}

// ModelRow is one registry model: its provider, formatted cost, and (redacted) endpoint.
type ModelRow struct {
	Name     string
	Provider string
	Cost     string
	Endpoint string // redacted when set
}

// Build projects the validated config into the view model. env is the active infra overlay
// name (infra.<env>.yaml). A nil config yields a zero view (the handler renders the
// not-attached notice instead of calling Build, but Build stays total for safety).
func Build(cfg *config.Config, env string) ConfigView {
	v := ConfigView{}
	if cfg == nil {
		return v
	}
	v.Identity = Identity{Root: cfg.Root, Env: env, Validated: true}
	v.Warnings = cfg.Warnings() // non-fatal advisories (T2.13); nil when the config is clean

	if h := cfg.Harness; h != nil {
		v.Identity.Profile = profileLabel(h.Policy.Profile)
		v.Stages = stageRows(h.DAG)
		v.PipelineSVG = dag.RenderSVGWith(roleFlowGraph(h.DAG), dag.RenderOptions{
			NodeFill: stageFill,
			NodeHref: func(string) string { return "" }, // stage nodes are not click-through
			Label:    "declared role-flow pipeline",
		})
		v.Checks = checkRows(h.Checks, h.IndependentChecks)
		v.Policy = policyView(h.Policy)
		v.StagesYAML = marshalYAML(h.DAG)
		v.ChecksYAML = marshalYAML(h.Checks)
		v.PolicyYAML = marshalYAML(h.Policy)
	}

	v.Roles = roleRows(cfg)
	v.ReqPlanner = reqPlannerRow(cfg)
	v.SoulsYAML = marshalYAML(cfg.Souls)

	if cfg.Infra != nil {
		v.Infra = infraView(cfg.Infra)
		v.InfraYAML = marshalYAML(redactInfra(cfg.Infra))
	}
	return v
}

// profileLabel renders the autonomy profile, defaulting an empty profile to the autonomous
// default (config.Policy.ApprovalRequired treats unset as autonomous).
func profileLabel(p string) string {
	if p == "" {
		return config.ProfileAutonomous + " (default)"
	}
	return p
}

// stageRows flattens the DAG map to a stable, name-sorted slice for the stages table.
func stageRows(d map[string]config.Stage) []StageRow {
	names := make([]string, 0, len(d))
	for n := range d {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([]StageRow, 0, len(names))
	for _, n := range names {
		st := d[n]
		rows = append(rows, StageRow{
			Name:          n,
			Kind:          stageKindLabel(st),
			Role:          st.Role,
			Precondition:  st.Precondition,
			Postcondition: st.Postcondition,
			OnFailure:     st.OnFailure,
			Produces:      st.Produces,
		})
	}
	return rows
}

// stageKindLabel names a stage's kind for display. An agent stage carries no Kind in config
// (it names a Role instead), so it is labeled "agent"; the hybrid/non-agent kinds keep their
// declared name.
func stageKindLabel(st config.Stage) string {
	if st.Kind != "" {
		return st.Kind
	}
	if st.Role != "" {
		return "agent"
	}
	return ""
}

// roleFlowGraph builds the declared role-flow as a dag.Graph: each stage is a node (id =
// stage name, title = role), each `produces` target a produces-kind edge, each `on_failure`
// target an on_failure-kind edge. Self-loops (a stage whose on_failure is itself — the common
// "retry me" case, e.g. plan→plan) are dropped: they render as a zero-length line and only
// add hover noise, and the stage's on_failure is already in the stages table. Cross-stage
// back-edges (qa→implement) are kept — they are the informative branch.
func roleFlowGraph(d map[string]config.Stage) dag.Graph {
	names := make([]string, 0, len(d))
	for n := range d {
		names = append(names, n)
	}
	sort.Strings(names)

	g := dag.Graph{}
	for _, n := range names {
		st := d[n]
		g.Nodes = append(g.Nodes, dag.Node{ID: n, Title: st.Role, Status: stageStatus(st)})
	}
	for _, n := range names {
		st := d[n]
		for _, p := range st.Produces {
			g.Edges = append(g.Edges, dag.Edge{From: n, To: p, Kind: dag.EdgeProduces})
		}
		if st.OnFailure != "" && st.OnFailure != n {
			g.Edges = append(g.Edges, dag.Edge{From: n, To: st.OnFailure, Kind: dag.EdgeOnFailure})
		}
	}
	return g
}

// stageStatus maps a stage to the token stageFill tints by (a stage-kind palette distinct
// from the issue-status palette). A plain agent stage (no Kind, a Role) reads as "agent".
func stageStatus(st config.Stage) string {
	if st.Kind != "" {
		return st.Kind
	}
	if st.Role != "" {
		return "agent"
	}
	return ""
}

// stageFill is the role-flow node palette: a stage-kind tint, deliberately separate from the
// board's issue-status palette (dag.statusFill) so the two graphs do not look the same.
func stageFill(status string) string {
	switch status {
	case "human":
		return "#cbd5e1" // slate-300 — the human-authored requirements head
	case "agent":
		return "#7dd3fc" // sky-300 — a sandbox-gated agent stage
	case config.StageKindPlan:
		return "#c4b5fd" // violet-300 — decomposition (ungated agent stage)
	case config.StageKindResolve:
		return "#fcd34d" // amber-300 — conflict resolution (gated, conflict-spawned)
	case config.StageKindTrustedMerge:
		return "#6ee7b7" // emerald-300 — the trusted merge to main
	default:
		return "#e2e8f0" // slate-200
	}
}

// checkRows flattens the check registry to a name-sorted slice, marking each entry named in
// independent as a scanner the gate keeps running past a failure (T2.12).
func checkRows(checks map[string]string, independent []string) []CheckRow {
	indep := make(map[string]bool, len(independent))
	for _, n := range independent {
		indep[n] = true
	}
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([]CheckRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, CheckRow{Name: n, Command: checks[n], Independent: indep[n]})
	}
	return rows
}

// roleRows groups souls by role and orders each group by selection specificity, mirroring the
// orchestrator's selectSoul: most-specific selector first, the empty-selector catch-all last,
// ties broken by name (cfg.Souls is loaded name-sorted). Each soul is resolved against the
// infra registry (model→provider+cost, sandbox→concrete artifact).
func roleRows(cfg *config.Config) []RoleRow {
	byRole := map[string][]core.Soul{}
	for _, s := range cfg.Souls {
		byRole[s.Role] = append(byRole[s.Role], s)
	}
	roles := make([]string, 0, len(byRole))
	for r := range byRole {
		roles = append(roles, r)
	}
	sort.Strings(roles)

	rows := make([]RoleRow, 0, len(roles))
	for _, r := range roles {
		souls := byRole[r]
		// Most-specific selector first; equal specificity keeps the name order cfg.Souls
		// arrives in (sort is stable), matching selectSoul's lowest-name tie-break.
		sort.SliceStable(souls, func(i, j int) bool {
			return len(souls[i].Selector) > len(souls[j].Selector)
		})
		sr := make([]SoulRow, 0, len(souls))
		for _, s := range souls {
			sr = append(sr, soulRow(cfg, s))
		}
		rows = append(rows, RoleRow{Role: r, Souls: sr})
	}
	return rows
}

// reqPlannerRow resolves the requirements planner into its display form, mirroring soulRow:
// its model is joined to the infra registry for provider+cost, its persona path shown
// relative to the config root. Configured is false (a zero row) when no requirements_planner
// is declared, which the view reads to omit the card.
func reqPlannerRow(cfg *config.Config) ReqPlannerRow {
	if cfg.Harness == nil || cfg.Harness.RequirementsPlanner == nil {
		return ReqPlannerRow{}
	}
	rp := cfg.Harness.RequirementsPlanner
	row := ReqPlannerRow{
		Key:         ReqPlannerKey,
		Model:       rp.Model,
		PersonaPath: relPersona(cfg.Root, cfg.RequirementsPlannerPersonaPath()),
		Configured:  true,
	}
	if cfg.Infra != nil {
		if mp, ok := cfg.Infra.Models[rp.Model]; ok {
			row.Provider = mp.Provider
			row.Cost = formatCost(mp.Cost)
		}
	}
	return row
}

// soulRow resolves one soul against the infra registry into its display form.
func soulRow(cfg *config.Config, s core.Soul) SoulRow {
	row := SoulRow{
		Name:        s.Name,
		Model:       s.Model,
		Sandbox:     s.Sandbox,
		PersonaPath: relPersona(cfg.Root, s.Persona),
		Selector:    sortedSelector(s.Selector),
		CatchAll:    len(s.Selector) == 0,
	}
	if cfg.Infra != nil {
		if mp, ok := cfg.Infra.Models[s.Model]; ok {
			row.Provider = mp.Provider
			row.Cost = formatCost(mp.Cost)
		}
		row.Image = cfg.Infra.Sandbox.ResolveImage(s.Sandbox)
	}
	return row
}

// relPersona shows a persona path relative to the config root when it sits under it (the
// common case after resolvePersonas makes it absolute), so the roster reads cleanly without
// the host's absolute path prefix; an out-of-tree path is shown verbatim.
func relPersona(root, persona string) string {
	if root != "" && strings.HasPrefix(persona, root+"/") {
		return strings.TrimPrefix(persona, root+"/")
	}
	return persona
}

// sortedSelector returns a soul's selector as key-sorted KV pairs for a deterministic render.
func sortedSelector(sel map[string]string) []KV {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kvs := make([]KV, 0, len(keys))
	for _, k := range keys {
		kvs = append(kvs, KV{Key: k, Val: sel[k]})
	}
	return kvs
}

// formatCost renders a model's per-million-token price compactly, or "" when fully unpriced
// (a model with no cost block contributes $0 — its spend is bounded by token/retry caps).
func formatCost(c config.ModelCost) string {
	if c == (config.ModelCost{}) {
		return ""
	}
	return fmt.Sprintf("$%s in · $%s out /Mtok", trimFloat(c.InputPerMTok), trimFloat(c.OutputPerMTok))
}

// policyView formats the termination/autonomy policy.
func policyView(p config.Policy) PolicyView {
	return PolicyView{
		Profile:    profileLabel(p.Profile),
		MaxRetries: fmt.Sprintf("%d", p.MaxRetries),
		Budget:     budgetView(p.Budget),
		EpicBudget: budgetView(p.EpicBudget),
		DeadLetter: p.DeadLetter,
		TCBPaths:   p.TCBPaths,
	}
}

// budgetView formats a budget, showing an uncapped (zero) dimension as ∞ rather than 0.
func budgetView(b config.Budget) BudgetView {
	return BudgetView{
		Tokens: capInt(b.Tokens),
		USD:    capUSD(b.USD),
		Wall:   capDur(b.Wall),
	}
}

// infraView projects the redacted infra overlay into its structured form.
func infraView(in *config.Infra) InfraView {
	iv := InfraView{
		SandboxBackend:   in.Sandbox.Backend,
		Egress:           in.Sandbox.Egress,
		Limits:           formatLimits(in.Sandbox.Limits),
		NATSURL:          mask(in.NATS.URL),
		Allowlist:        in.Broker.Allowlist,
		ArtifactsBackend: in.Artifacts.Backend,
		ArtifactsPath:    mask(in.Artifacts.Path),
		OTelEndpoint:     mask(in.OTel.Endpoint),
	}
	profiles := make([]string, 0, len(in.Sandbox.Profiles))
	for n := range in.Sandbox.Profiles {
		profiles = append(profiles, n)
	}
	sort.Strings(profiles)
	for _, n := range profiles {
		p := in.Sandbox.Profiles[n]
		iv.Profiles = append(iv.Profiles, ProfileRow{Name: n, Image: p.Image, Rootfs: p.Rootfs})
	}
	models := make([]string, 0, len(in.Models))
	for n := range in.Models {
		models = append(models, n)
	}
	sort.Strings(models)
	for _, n := range models {
		mp := in.Models[n]
		iv.Models = append(iv.Models, ModelRow{
			Name:     n,
			Provider: mp.Provider,
			Cost:     formatCost(mp.Cost),
			Endpoint: mask(mp.Endpoint),
		})
	}
	return iv
}

// redactInfra returns a copy of the infra overlay with the allowlisted topology fields masked,
// for the raw fold. It copies the Models map so the original registry the factory runs on is
// never mutated; the egress allowlist and sandbox profiles (image/rootfs digests) are kept
// visible — operational policy and build provenance, not secrets.
func redactInfra(in *config.Infra) config.Infra {
	out := *in
	out.NATS.URL = mask(out.NATS.URL)
	out.OTel.Endpoint = mask(out.OTel.Endpoint)
	out.Artifacts.Path = mask(out.Artifacts.Path)
	if in.Models != nil {
		m := make(map[string]config.ModelProvider, len(in.Models))
		for k, v := range in.Models {
			v.Endpoint = mask(v.Endpoint)
			m[k] = v
		}
		out.Models = m
	}
	return out
}

// mask hides a sensitive value, leaving an unset (empty) one empty so the view distinguishes
// "deliberately hidden" from "not configured".
func mask(s string) string {
	if s == "" {
		return ""
	}
	return redactedMark
}

// marshalYAML serializes a config section for its raw fold. A marshal error is shown inline
// (it cannot leak a secret — the input is already redacted where it matters) rather than
// failing the whole page.
func marshalYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "# could not render: " + err.Error()
	}
	return string(b)
}

// formatLimits renders the sandbox resource ceiling on one line.
func formatLimits(l config.SandboxLimits) string {
	parts := []string{fmt.Sprintf("cpu %d", l.CPU)}
	if l.Mem != "" {
		parts = append(parts, "mem "+l.Mem)
	}
	if l.Disk != "" {
		parts = append(parts, "disk "+l.Disk)
	}
	if w := l.Wall.Duration(); w > 0 {
		parts = append(parts, "wall "+w.String())
	}
	return strings.Join(parts, " · ")
}

// capInt / capUSD / capDur format a budget dimension, rendering an uncapped (zero) value as ∞.
func capInt(n int) string {
	if n <= 0 {
		return "∞"
	}
	return groupThousands(n)
}

func capUSD(v float64) string {
	if v <= 0 {
		return "∞"
	}
	return "$" + trimFloat(v)
}

func capDur(d config.Duration) string {
	if d.Duration() <= 0 {
		return "∞"
	}
	return d.Duration().String()
}

// trimFloat formats a float without trailing zeros (15 not 15.0000, 0.3 not 0.3000).
func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// groupThousands renders an int with thousands separators (2000000 → "2,000,000").
func groupThousands(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}
