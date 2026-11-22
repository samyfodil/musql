// createConsole's options, filled in with the stock console's defaults. No
// React and no DOM here, so Node tests it as it is.

export const BRANDING = {
	name: "musql cloud",
	studioName: "musql studio",
	logo: null,
	favicon: null,
	docsURL: null,
	supportURL: null,
};

const STOCK_PAGES = ["databases", "groups", "members", "usage", "settings", "tokens", "users"];

/** normalize fills in o's defaults; a page placed on "database" joins the database tabs. */
export function normalize(o = {}) {
	const pages = o.pages ?? [];
	for (const s of [...pages, ...(o.databaseTabs ?? [])]) {
		if (!s?.key || !s.label || !s.component) throw new Error(`createConsole: a page or tab needs a key, a label and a component (${s?.key ?? "?"})`);
	}
	for (const p of pages) if (STOCK_PAGES.includes(p.key)) throw new Error(`createConsole: the page key ${p.key} is the console's own`);
	return {
		branding: { ...BRANDING, ...dropUndefined(o.branding) },
		theme: o.theme ?? null,
		pages: pages.filter((p) => (p.placement ?? "org") === "org"),
		databaseTabs: [...pages.filter((p) => p.placement === "database"), ...(o.databaseTabs ?? [])],
		banners: o.banners ?? [],
		api: { baseURL: o.api?.baseURL?.replace(/\/+$/, "") || null },
	};
}

const dropUndefined = (o) => Object.fromEntries(Object.entries(o ?? {}).filter(([, v]) => v !== undefined));

const decls = (vars) =>
	Object.entries(vars)
		.filter(([, v]) => typeof v === "string" || typeof v === "number")
		.map(([k, v]) => `${k.startsWith("--") ? k : `--${k}`}: ${v};`)
		.join(" ");

/**
 * themeCSS is the style sheet for a theme: CSS variables over style.css's
 * tokens, either one set for both schemes or {light, dark}. Its selectors
 * outrank style.css's whatever the order the two load in.
 */
export function themeCSS(theme) {
	if (!theme) return "";
	const split = typeof theme.light === "object" || typeof theme.dark === "object";
	const light = decls(split ? (theme.light ?? {}) : theme);
	const dark = decls(split ? (theme.dark ?? {}) : theme);
	const out = [];
	if (light) out.push(`html:root { ${light} }`);
	if (dark) out.push(`@media (prefers-color-scheme: dark) { html:root:not([data-theme="light"]) { ${dark} } }`, `html:root[data-theme="dark"] { ${dark} }`);
	return out.join("\n");
}
