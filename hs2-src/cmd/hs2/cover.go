package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Per-install cover page.
//
// The built-in cover (served to a probe that reaches the backend without
// authenticating) used to be ONE page, byte-for-byte identical on every
// install, and its text is in the public source — so one known sha256 found
// every hs2 server in a bulk active scan. buildCover turns a per-install random
// seed (cover_seed in the config, written by the installer, NOT derived from
// the shared key) into a page whose brand, copy, colours, layout tokens,
// section set and size all vary, so no two installs share a hash and a naive
// structural match does not catch the family either.
//
// What this does NOT do (on purpose, so the UI never overclaims): it is not a
// disguise against a determined prober. The TLS stack is still Go's, and a
// classifier trained on a few generated pages could still recognise the genre.
// It is defence against cheap bulk enumeration; a real cover is still
// backend_addr pointed at your own site.
//
// STABILITY CONTRACT: the bytes are a pure function of (seed, year). The same
// seed yields the same page across restarts and across binary rebuilds — a
// golden test pins exact bytes for a known seed, so any accidental change to
// the generator fails CI. Only the copyright year ticks over time (as on any
// real site), and that moves for everyone at once, so it is not a per-install
// tell. To ever change the output intentionally, add a versioned path and key
// it on a stored field — never silently, or every server's page changes on
// upgrade day (a correlated fleet event).

// coverRNG is a deterministic byte stream from the seed: block i is
// sha256(seedKey || be64(i)). SHA-256 is byte-stable forever and independent of
// the Go version (unlike math/rand, whose streams differ between v1 and v2), so
// a seed renders the same page on any build.
type coverRNG struct {
	key []byte
	ctr uint64
	buf [sha256.Size]byte
	off int
}

func newCoverRNG(seed string) *coverRNG {
	k := sha256.Sum256([]byte("hs2-cover-v1\x00" + seed))
	r := &coverRNG{key: k[:]}
	r.off = len(r.buf) // force a refill on first use
	return r
}

func (r *coverRNG) refill() {
	h := sha256.New()
	h.Write(r.key)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], r.ctr)
	h.Write(c[:])
	r.ctr++
	h.Sum(r.buf[:0])
	r.off = 0
}

func (r *coverRNG) u32() uint32 {
	var b [4]byte
	for i := range b {
		if r.off >= len(r.buf) {
			r.refill()
		}
		b[i] = r.buf[r.off]
		r.off++
	}
	return binary.BigEndian.Uint32(b[:])
}

// intn returns a value in [0,n) with rejection sampling (unbiased).
func (r *coverRNG) intn(n int) int {
	if n <= 1 {
		return 0
	}
	limit := uint32(0xffffffff) - (0xffffffff % uint32(n))
	for {
		v := r.u32()
		if v <= limit {
			return int(v % uint32(n))
		}
	}
}

func (r *coverRNG) pick(list []string) string { return list[r.intn(len(list))] }
func (r *coverRNG) between(lo, hi int) int    { return lo + r.intn(hi-lo+1) } // inclusive
func (r *coverRNG) chance(pct int) bool       { return r.intn(100) < pct }

// shuffled returns 0..n-1 in a seed-determined order (Fisher–Yates).
func (r *coverRNG) shuffled(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := r.intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// token returns a lowercase [a-z][a-z0-9]{len-1} string — looks like a CSS
// minifier's class name, so random class names are unremarkable.
func (r *coverRNG) token(n int) string {
	const first = "abcdefghijklmnopqrstuvwxyz"
	const rest = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	b[0] = first[r.intn(len(first))]
	for i := 1; i < n; i++ {
		b[i] = rest[r.intn(len(rest))]
	}
	return string(b)
}

// ---- vocabulary (large and plausible, so the ENSEMBLE is not obviously
// generated; collisions across installs are negligible for a 128-bit seed) ----

var coverRoots = []string{
	"Oak", "Cedar", "Pine", "Birch", "Alder", "Maple", "Elm", "Ash", "Fern", "Moss",
	"Stone", "Flint", "Slate", "Ridge", "Vale", "Cove", "Bay", "Harbor", "Haven", "Beacon",
	"North", "Summit", "Vertex", "Apex", "Keystone", "Anchor", "Compass", "Lumen", "Nova", "Orbit",
	"Atlas", "Delta", "Arc", "Forge", "Anvil", "Kiln", "Loom", "Quill", "Ledger", "Signal",
	"Current", "Ember", "Spark", "Drift", "Tide", "Meadow", "Willow", "Aspen", "Juniper", "Cypress",
	"Vista", "River", "Brook", "Grove", "Cliff", "Dune", "Marsh", "Reef", "Fjord", "Glade",
}

var coverParts = []string{
	"line", "works", "labs", "byte", "stack", "logic", "forge", "craft", "scale", "wave",
	"point", "path", "span", "core", "loop", "hub", "field", "gate", "port", "yard",
	"bridge", "mark", "ware", "base", "deck",
}

var coverOrgTypes = []string{"Studio", "Labs", "Collective", "Group", "Works", "Software", "Systems", "Consulting", "Digital", "Technologies"}

var coverFields = []string{"web", "cloud", "platform", "backend", "data", "infrastructure", "software"}
var coverSizes = []string{"small", "boutique", "senior", "independent"}

var coverHeroH1 = []string{
	"Dependable software for growing teams.",
	"Build it once. Build it to last.",
	"The quiet infrastructure behind good products.",
	"Software that gets out of the way.",
	"Systems that simply keep working.",
	"Engineering for teams that ship.",
	"Calm, reliable systems by design.",
	"We build the boring parts well.",
	"Thoughtful software, carefully maintained.",
	"Infrastructure you can forget about.",
	"Built to last, easy to hand over.",
	"Steady hands for production systems.",
	"Less firefighting. More shipping.",
	"The software behind the software.",
	"Quietly keeping teams online.",
	"Good engineering, kept running.",
}

// hero paragraph templates: %s = brand, field is substituted too. Varied in
// structure (not one fill-in template) so the copy does not read as generated.
var coverHeroP = []string{
	"%[1]s is a %[2]s %[3]s studio that designs, builds, and maintains the systems behind growing products.",
	"We build and run dependable %[3]s software for teams who would rather focus on their product than their plumbing.",
	"%[1]s helps teams ship and operate %[3]s systems that stay quiet, stay up, and stay easy to change.",
	"For over a decade, %[1]s has designed and maintained production %[3]s systems for a handful of clients at a time.",
	"A %[2]s team of senior engineers, %[1]s takes on %[3]s work that needs to be done carefully and kept running.",
	"%[1]s designs, builds, and looks after the %[3]s infrastructure your product depends on.",
	"We are %[1]s — a %[3]s studio for teams that value systems which simply keep working.",
	"%[1]s partners with growing teams to build %[3]s software that is a pleasure to hand over and a breeze to run.",
}

var coverCTAs = []string{"Get in touch", "Start a project", "Talk to us", "Say hello", "Work with us", "Contact us"}

// stroke icons (24x24 inner markup) for the service cards.
var coverIcons = []string{
	`<rect x="3" y="4" width="18" height="14" rx="2"/><path d="M3 9h18M8 21h8"/>`,
	`<path d="M17 18a4 4 0 0 0 0-8 6 6 0 0 0-11.6-1.8A4.5 4.5 0 0 0 6 18z"/>`,
	`<path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z"/><path d="M12 8v4l3 2"/>`,
	`<path d="M4 7h16M4 12h16M4 17h10"/>`,
	`<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>`,
	`<path d="M12 2 4 6v6c0 5 3.4 8 8 10 4.6-2 8-5 8-10V6z"/>`,
	`<path d="M3 3v18h18"/><path d="M7 14l4-4 3 3 5-6"/>`,
	`<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M5 19l2-2M17 7l2-2"/>`,
	`<path d="M4 17V9l8-5 8 5v8l-8 5z"/><path d="M12 12l8-5M12 12v9M12 12 4 7"/>`,
	`<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>`,
}

type coverService struct {
	title string
	blurb []string
}

var coverServices = []coverService{
	{"Web", []string{"Fast, accessible sites and dashboards, built to last and easy to hand over.", "Front ends that load quickly and stay maintainable long after launch."}},
	{"Cloud", []string{"Right-sized deployments, sensible monitoring, and backups that actually restore.", "Infrastructure that scales with you and does not surprise you on the bill."}},
	{"Support", []string{"Steady maintenance and on-call help, so small problems stay small.", "We keep an eye on the systems we build, and answer when something breaks."}},
	{"Platform", []string{"Internal tools and services your team can build on with confidence.", "The shared foundations that let the rest of your product move faster."}},
	{"Data", []string{"Pipelines and stores that keep your numbers correct and queryable.", "Reporting you can trust, without a data team of your own."}},
	{"Security", []string{"Sensible hardening, reviews, and the unglamorous work that keeps you safe.", "Practical security that fits how your team actually works."}},
	{"Mobile", []string{"Native-feeling apps that share a clean core with the web.", "Mobile clients built to the same standard as everything else."}},
	{"Reliability", []string{"Monitoring, alerting, and the quiet tuning that keeps uptime boring.", "We measure what matters and fix the causes, not the symptoms."}},
}

type coverSection struct {
	id, title string
	body      []string
}

var coverSections = []coverSection{
	{"about", "About", []string{
		"We are a small, senior team that has shipped and maintained production systems for over a decade. We take on a handful of engagements at a time, so each one gets real attention from the people actually doing the work.",
		"A compact group of engineers who like problems that stay solved. We work closely with a few teams at once rather than spreading thin across many.",
	}},
	{"approach", "How we work", []string{
		"We start small, ship something real early, and grow it with you. No big-bang rewrites, no surprises — just steady progress you can see.",
		"Plain language, short feedback loops, and code we are happy to hand over. We would rather explain a trade-off than hide it.",
	}},
	{"work", "Our work", []string{
		"From early-stage products to systems carrying real load, we have built and run software across a range of industries. Most of our work comes from people we have worked with before.",
		"We have helped teams launch, scale, and quietly keep running the software their business depends on.",
	}},
	{"team", "The team", []string{
		"A handful of engineers and designers who have worked together for years. Everyone here writes code, talks to clients, and owns what they ship.",
		"Senior people, no layers. The person you talk to is the person doing the work.",
	}},
}

var coverContact = []string{
	"Have a project in mind, or an existing system that needs a steady hand? Tell us a little about what you are building and we will get back to you within a couple of working days.",
	"Thinking about a new build, or looking for someone to look after what you already have? Send us a note about your project and we will reply within a few days.",
	"If you have something you would like built or kept running, we would love to hear about it. Drop us a line and we will be in touch shortly.",
}

// buildCover renders the cover page for a seed. An empty seed returns the fixed
// legacy page byte-for-byte (backward compatible: an old config, or a
// hand-written one, keeps a stable page rather than a per-restart-random one).
// It returns the page and the Last-Modified offset in days (seed-derived, so
// the "last modified N days ago" distance is not itself a constant).
func buildCover(seed string, now time.Time) (page []byte, modOffsetDays int) {
	if seed == "" {
		return coverFixedPage, 37
	}
	r := newCoverRNG(seed)

	// --- brand -------------------------------------------------------------
	var brand string
	switch r.intn(3) {
	case 0:
		brand = r.pick(coverRoots) + r.pick(coverParts) // "Oakline"
	case 1:
		brand = r.pick(coverRoots) + " " + r.pick(coverOrgTypes) // "Summit Labs"
	default:
		brand = r.pick(coverRoots) // "Cedar"
	}
	org := r.pick(coverOrgTypes)
	field := r.pick(coverFields)
	size := r.pick(coverSizes)

	// --- palette (continuous hue; saturation/lightness vary within a readable
	// band so the colour tokens are not a fixed string either; neutrals stay
	// fixed for contrast) --------------------------------------------------
	hue := r.intn(360)
	sat := r.between(56, 72)
	lum := r.between(52, 60)
	accentL := fmt.Sprintf("hsl(%d %d%% %d%%)", hue, sat, lum)
	accentD := fmt.Sprintf("hsl(%d %d%% %d%%)", hue, min(sat+14, 92), lum+18)

	// --- layout tokens (vary CSS bytes + geometry, safely) -----------------
	radius := r.between(8, 16)
	radiusSm := r.between(6, 10)
	maxw := []int{980, 1040, 1100, 1180, 1240}[r.intn(5)]
	heroSize := r.between(40, 52)
	gap := r.between(16, 24)
	cls := func() string { return r.token(r.between(4, 7)) }
	cWrap, cBar, cBrand, cNav := cls(), cls(), cls(), cls()
	cHero, cBtn, cGrid, cCard := cls(), cls(), cls(), cls()
	cIc, cSect, cFoot := cls(), cls(), cls()

	// --- favicon -----------------------------------------------------------
	favPaths := []string{
		`<path d="M9 22V10l14 12V10" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>`,
		`<circle cx="16" cy="16" r="7" fill="none" stroke="#fff" stroke-width="2.6"/>`,
		`<path d="M10 16h12M16 10v12" stroke="#fff" stroke-width="2.8" stroke-linecap="round"/>`,
		`<path d="M9 20l7-10 7 10z" fill="none" stroke="#fff" stroke-width="2.4" stroke-linejoin="round"/>`,
		`<rect x="10" y="10" width="12" height="12" rx="3" fill="none" stroke="#fff" stroke-width="2.6"/>`,
		`<path d="M10 22V10h6a4 4 0 0 1 0 8h-6" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>`,
		`<path d="M11 11l10 5-10 5z" fill="#fff"/>`,
		`<path d="M16 9l2 5 5 .4-3.8 3.3 1.2 5-4.4-2.7-4.4 2.7 1.2-5L9 14.4l5-.4z" fill="#fff"/>`,
	}
	favFill := fmt.Sprintf("hsl(%d %d%% %d%%)", hue, sat, lum-4)
	fav := fmt.Sprintf(`<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><rect width='32' height='32' rx='%d' fill='%s'/>%s</svg>`,
		r.between(6, 9), favFill, strings.ReplaceAll(favPaths[r.intn(len(favPaths))], `"`, `'`))
	favData := "data:image/svg+xml," + urlishEscape(fav)

	// brand mark in the header: the same glyph family (white on the accent).
	headMark := favPaths[r.intn(len(favPaths))]

	// --- content choices ---------------------------------------------------
	h1 := r.pick(coverHeroH1)
	heroP := fmt.Sprintf(r.pick(coverHeroP), brand, size, field)
	cta := r.pick(coverCTAs)
	year := now.Year()

	// 2–4 service cards, distinct, shuffled.
	nCards := r.between(2, 4)
	svcOrder := r.shuffled(len(coverServices))[:nCards]
	iconOrder := r.shuffled(len(coverIcons))

	// 1–2 long sections (shuffled subset), then Contact (common on real sites).
	nSect := r.between(1, 2)
	secOrder := r.shuffled(len(coverSections))[:nSect]

	// nav: services + the chosen long sections + contact, in page order.
	navIDs := []string{"services"}
	for _, i := range secOrder {
		navIDs = append(navIDs, coverSections[i].id)
	}
	navIDs = append(navIDs, "contact")

	// --- render ------------------------------------------------------------
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	p("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	p("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	p("<title>%s</title>\n", htmlEsc(brand))
	p("<meta name=\"description\" content=\"%s is a %s %s %s building dependable software for growing teams.\">\n",
		htmlEsc(brand), size, field, strings.ToLower(org))
	p("<link rel=\"icon\" type=\"image/svg+xml\" href=\"%s\">\n", favData)
	p("<style>\n")
	p("  :root{--bg:#fff;--fg:#1b2130;--muted:#5b647a;--line:#e7e9f0;--card:#f7f8fb;--accent:%s;--accent-fg:#fff;--shadow:0 1px 2px rgba(20,30,60,.06),0 8px 24px rgba(20,30,60,.05)}\n", accentL)
	p("  @media (prefers-color-scheme:dark){:root{--bg:#0f131c;--fg:#e8ebf4;--muted:#9aa3bb;--line:#222838;--card:#151a26;--accent:%s;--accent-fg:#0f131c;--shadow:0 1px 2px rgba(0,0,0,.3),0 10px 30px rgba(0,0,0,.35)}}\n", accentD)
	p("  *{box-sizing:border-box}html{-webkit-text-size-adjust:100%%}\n")
	p("  body{margin:0;background:var(--bg);color:var(--fg);font-family:-apple-system,BlinkMacSystemFont,\"Segoe UI\",Roboto,Helvetica,Arial,sans-serif;line-height:1.6;-webkit-font-smoothing:antialiased}\n")
	p("  a{color:inherit;text-decoration:none}\n")
	p("  .%s{max-width:%dpx;margin:0 auto;padding:0 24px}\n", cWrap, maxw)
	p("  header{border-bottom:1px solid var(--line)}\n")
	p("  .%s{display:flex;align-items:center;justify-content:space-between;height:68px}\n", cBar)
	p("  .%s{display:flex;align-items:center;gap:10px;font-weight:700;font-size:18px;letter-spacing:-.01em}\n", cBrand)
	p("  .%s svg{display:block;border-radius:%dpx}\n", cBrand, radiusSm)
	p("  .%s{display:flex;gap:28px}.%s a{color:var(--muted);font-size:15px}.%s a:hover{color:var(--fg)}\n", cNav, cNav, cNav)
	p("  @media (max-width:640px){.%s{display:none}}\n", cNav)
	p("  .%s{padding:84px 0 64px}.%s h1{font-size:%dpx;line-height:1.12;letter-spacing:-.025em;margin:0 0 18px;max-width:18ch}\n", cHero, cHero, heroSize)
	p("  .%s p{font-size:19px;color:var(--muted);max-width:52ch;margin:0 0 30px}\n", cHero)
	p("  @media (max-width:640px){.%s{padding:56px 0 44px}.%s h1{font-size:%dpx}.%s p{font-size:17px}}\n", cHero, cHero, heroSize-11, cHero)
	p("  .%s{display:inline-block;background:var(--accent);color:var(--accent-fg);font-weight:600;font-size:15px;padding:12px 22px;border-radius:%dpx;transition:opacity .15s}.%s:hover{opacity:.9}\n", cBtn, radiusSm+2, cBtn)
	p("  .%s{display:grid;grid-template-columns:repeat(%d,1fr);gap:%dpx;padding:8px 0 40px}\n", cGrid, nCards, gap)
	p("  @media (max-width:820px){.%s{grid-template-columns:1fr}}\n", cGrid)
	p("  .%s{background:var(--card);border:1px solid var(--line);border-radius:%dpx;padding:26px;box-shadow:var(--shadow)}\n", cCard, radius)
	p("  .%s h3{margin:0 0 8px;font-size:18px;letter-spacing:-.01em}.%s p{margin:0;color:var(--muted);font-size:15px}\n", cCard, cCard)
	p("  .%s{padding:4px 0 60px}.%s h2{font-size:26px;letter-spacing:-.02em;margin:0 0 12px}.%s p{color:var(--muted);max-width:60ch;margin:0;font-size:16px}\n", cSect, cSect, cSect)
	p("  .%s{width:38px;height:38px;border-radius:%dpx;background:color-mix(in srgb,var(--accent) 16%%,transparent);display:flex;align-items:center;justify-content:center;margin-bottom:16px;color:var(--accent)}\n", cIc, radiusSm+1)
	p("  footer{border-top:1px solid var(--line);color:var(--muted);font-size:14px}\n")
	p("  .%s{display:flex;flex-wrap:wrap;gap:14px;align-items:center;justify-content:space-between;padding:28px 0}.%s nav{display:flex;gap:20px}.%s nav a{font-size:14px}\n", cFoot, cFoot, cFoot)
	p("</style>\n</head>\n<body>\n")

	// header
	p("<header>\n  <div class=\"%s %s\">\n", cWrap, cBar)
	p("    <a class=\"%s\" href=\"/\">\n", cBrand)
	p("      <svg width=\"28\" height=\"28\" viewBox=\"0 0 32 32\" aria-hidden=\"true\"><rect width=\"32\" height=\"32\" rx=\"7\" fill=\"%s\"/>%s</svg>\n", favFill, headMark)
	p("      %s\n    </a>\n    <nav class=\"%s\">\n", htmlEsc(brand), cNav)
	for _, id := range navIDs {
		p("      <a href=\"/#%s\">%s</a>\n", id, navLabel(id, secTitle(id)))
	}
	p("    </nav>\n  </div>\n</header>\n\n<main>\n")

	// hero
	p("  <section class=\"%s %s\">\n    <h1>%s</h1>\n    <p>%s</p>\n    <a class=\"%s\" href=\"/#contact\">%s</a>\n  </section>\n\n",
		cWrap, cHero, htmlEsc(h1), htmlEsc(heroP), cBtn, htmlEsc(cta))

	// services
	p("  <section class=\"%s %s\" id=\"services\">\n", cWrap, cGrid)
	for n, si := range svcOrder {
		s := coverServices[si]
		p("    <div class=\"%s\">\n      <div class=\"%s\"><svg width=\"20\" height=\"20\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\">%s</svg></div>\n      <h3>%s</h3>\n      <p>%s</p>\n    </div>\n",
			cCard, cIc, coverIcons[iconOrder[n]], htmlEsc(s.title), htmlEsc(s.blurb[r.intn(len(s.blurb))]))
	}
	p("  </section>\n\n")

	// long sections
	for _, idx := range secOrder {
		s := coverSections[idx]
		p("  <section class=\"%s %s\" id=\"%s\">\n    <h2>%s</h2>\n    <p>%s</p>\n  </section>\n\n",
			cWrap, cSect, s.id, htmlEsc(s.title), htmlEsc(s.body[r.intn(len(s.body))]))
	}

	// contact
	p("  <section class=\"%s %s\" id=\"contact\">\n    <h2>Contact</h2>\n    <p>%s</p>\n  </section>\n", cWrap, cSect, htmlEsc(r.pick(coverContact)))
	p("</main>\n\n")

	// footer
	p("<footer>\n  <div class=\"%s %s\">\n    <div>&copy; %d %s %s. All rights reserved.</div>\n    <nav>\n", cWrap, cFoot, year, htmlEsc(brand), org)
	for _, id := range navIDs {
		p("      <a href=\"/#%s\">%s</a>\n", id, navLabel(id, secTitle(id)))
	}
	p("    </nav>\n  </div>\n</footer>\n</body>\n</html>\n")

	return []byte(b.String()), r.between(18, 400)
}

// secTitle maps a section id to its display title (services/contact are fixed;
// the long-section ids come from coverSections).
func secTitle(id string) string {
	switch id {
	case "services":
		return "Services"
	case "contact":
		return "Contact"
	}
	for _, s := range coverSections {
		if s.id == id {
			return s.title
		}
	}
	return id
}

// navLabel shortens a long section title for the nav bar (e.g. "How we work"
// -> "Approach"), so the nav reads like a real site's.
func navLabel(id, title string) string {
	switch id {
	case "approach":
		return "Approach"
	case "work":
		return "Work"
	case "team":
		return "Team"
	}
	return title
}

func htmlEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// urlishEscape percent-escapes only what a data: URI in an href needs: the
// characters that break the attribute or the URL. Matches the hand-escaping of
// the legacy favicon so generated pages stay valid and compact.
func urlishEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c == '<':
			b.WriteString("%3C")
		case c == '>':
			b.WriteString("%3E")
		case c == '#':
			b.WriteString("%23")
		case c == '%':
			b.WriteString("%25")
		case c == '"':
			b.WriteString("%22")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// coverFixedPage is the legacy cover, served verbatim when no seed is set
// (an older config written before per-install pages, or a hand-written one).
// Keeping it byte-for-byte unchanged means an un-migrated install is not
// disturbed; a migrated one gets a per-seed page instead (see buildCover).
var coverFixedPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Oakline</title>
<meta name="description" content="Oakline is a small studio building dependable web and cloud software for growing teams.">
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%234f6bed'/%3E%3Cpath d='M9 22V10l14 12V10' fill='none' stroke='white' stroke-width='2.6' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E">
<style>
  :root{
    --bg:#ffffff; --fg:#1b2130; --muted:#5b647a; --line:#e7e9f0;
    --card:#f7f8fb; --accent:#4f6bed; --accent-fg:#ffffff; --shadow:0 1px 2px rgba(20,30,60,.06),0 8px 24px rgba(20,30,60,.05);
  }
  @media (prefers-color-scheme:dark){
    :root{
      --bg:#0f131c; --fg:#e8ebf4; --muted:#9aa3bb; --line:#222838;
      --card:#151a26; --accent:#7e93f4; --accent-fg:#0f131c; --shadow:0 1px 2px rgba(0,0,0,.3),0 10px 30px rgba(0,0,0,.35);
    }
  }
  *{box-sizing:border-box}
  html{-webkit-text-size-adjust:100%}
  body{
    margin:0; background:var(--bg); color:var(--fg);
    font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
    line-height:1.6; -webkit-font-smoothing:antialiased;
  }
  a{color:inherit;text-decoration:none}
  .wrap{max-width:1040px;margin:0 auto;padding:0 24px}
  header{border-bottom:1px solid var(--line)}
  .bar{display:flex;align-items:center;justify-content:space-between;height:68px}
  .brand{display:flex;align-items:center;gap:10px;font-weight:700;font-size:18px;letter-spacing:-.01em}
  .brand svg{display:block;border-radius:7px}
  nav{display:flex;gap:28px}
  nav a{color:var(--muted);font-size:15px}
  nav a:hover{color:var(--fg)}
  @media (max-width:640px){nav{display:none}}
  .hero{padding:84px 0 64px}
  .hero h1{font-size:44px;line-height:1.12;letter-spacing:-.025em;margin:0 0 18px;max-width:18ch}
  .hero p{font-size:19px;color:var(--muted);max-width:52ch;margin:0 0 30px}
  @media (max-width:640px){.hero{padding:56px 0 44px}.hero h1{font-size:33px}.hero p{font-size:17px}}
  .btn{display:inline-block;background:var(--accent);color:var(--accent-fg);font-weight:600;font-size:15px;
    padding:12px 22px;border-radius:9px;transition:opacity .15s}
  .btn:hover{opacity:.9}
  .grid{display:grid;grid-template-columns:repeat(3,1fr);gap:20px;padding:8px 0 40px}
  @media (max-width:820px){.grid{grid-template-columns:1fr}}
  .card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:26px;box-shadow:var(--shadow)}
  .card h3{margin:0 0 8px;font-size:18px;letter-spacing:-.01em}
  .card p{margin:0;color:var(--muted);font-size:15px}
  .sect{padding:4px 0 60px}
  .sect h2{font-size:26px;letter-spacing:-.02em;margin:0 0 12px}
  .sect p{color:var(--muted);max-width:60ch;margin:0;font-size:16px}
  .ic{width:38px;height:38px;border-radius:10px;background:color-mix(in srgb,var(--accent) 16%,transparent);
    display:flex;align-items:center;justify-content:center;margin-bottom:16px;color:var(--accent)}
  footer{border-top:1px solid var(--line);color:var(--muted);font-size:14px}
  .foot{display:flex;flex-wrap:wrap;gap:14px;align-items:center;justify-content:space-between;padding:28px 0}
  .foot nav{display:flex;gap:20px}
  .foot nav a{font-size:14px}
</style>
</head>
<body>
<header>
  <div class="wrap bar">
    <a class="brand" href="/">
      <svg width="28" height="28" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="7" fill="#4f6bed"/><path d="M9 22V10l14 12V10" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
      Oakline
    </a>
    <nav>
      <a href="/#services">Services</a>
      <a href="/#about">About</a>
      <a href="/#contact">Contact</a>
    </nav>
  </div>
</header>

<main>
  <section class="wrap hero">
    <h1>Dependable software for growing teams.</h1>
    <p>Oakline is a small studio that designs, builds, and maintains web and cloud applications — the quiet infrastructure your product runs on.</p>
    <a class="btn" href="/#contact">Get in touch</a>
  </section>

  <section class="wrap grid" id="services">
    <div class="card">
      <div class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="14" rx="2"/><path d="M3 9h18M8 21h8"/></svg></div>
      <h3>Web</h3>
      <p>Fast, accessible sites and dashboards, built to last and easy to hand over.</p>
    </div>
    <div class="card">
      <div class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 18a4 4 0 0 0 0-8 6 6 0 0 0-11.6-1.8A4.5 4.5 0 0 0 6 18z"/></svg></div>
      <h3>Cloud</h3>
      <p>Right-sized deployments, sensible monitoring, and backups that actually restore.</p>
    </div>
    <div class="card">
      <div class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z"/><path d="M12 8v4l3 2"/></svg></div>
      <h3>Support</h3>
      <p>Steady maintenance and on-call help, so small problems stay small.</p>
    </div>
  </section>

  <section class="wrap sect" id="about">
    <h2>About</h2>
    <p>We are a small, senior team that has shipped and maintained production systems for over a decade. We take on a handful of engagements at a time, so each one gets real attention from the people actually doing the work.</p>
  </section>

  <section class="wrap sect" id="contact">
    <h2>Contact</h2>
    <p>Have a project in mind, or an existing system that needs a steady hand? Tell us a little about what you are building and we will get back to you within a couple of working days.</p>
  </section>
</main>

<footer>
  <div class="wrap foot">
    <div>&copy; 2026 Oakline Studio. All rights reserved.</div>
    <nav>
      <a href="/#services">Services</a>
      <a href="/#about">About</a>
      <a href="/#contact">Contact</a>
    </nav>
  </div>
</footer>
</body>
</html>
`)
