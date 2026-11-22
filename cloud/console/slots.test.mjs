// createConsole's options and theme, without a browser: node slots.test.mjs
import assert from "node:assert/strict";
import { test } from "node:test";
import { BRANDING, normalize, themeCSS } from "./src/options.js";

const C = () => null;

test("no options is the stock console", () => {
	const o = normalize({});
	assert.deepEqual(o.branding, BRANDING);
	assert.equal(o.branding.name, "musql cloud");
	assert.deepEqual([o.pages, o.databaseTabs, o.banners], [[], [], []]);
	assert.equal(o.api.baseURL, null);
	assert.equal(themeCSS(o.theme), "");
});

test("branding keeps the defaults it does not name", () => {
	const o = normalize({ branding: { name: "musql.dev", docsURL: undefined } });
	assert.equal(o.branding.name, "musql.dev");
	assert.equal(o.branding.studioName, "musql studio");
	assert.equal(o.branding.docsURL, null);
});

test("a page placed on the database is a database tab", () => {
	const o = normalize({
		pages: [{ key: "billing", label: "Billing", component: C }, { key: "jwks", label: "JWKS", component: C, placement: "database" }],
		databaseTabs: [{ key: "quota", label: "Quota", component: C }],
	});
	assert.deepEqual(o.pages.map((p) => p.key), ["billing"]);
	assert.deepEqual(o.databaseTabs.map((p) => p.key), ["jwks", "quota"]);
});

test("a page needs a key, a label and a component, and its own key", () => {
	assert.throws(() => normalize({ pages: [{ key: "x", label: "X" }] }), /component/);
	assert.throws(() => normalize({ databaseTabs: [{ label: "X", component: C }] }), /key/);
	assert.throws(() => normalize({ pages: [{ key: "usage", label: "Usage", component: C }] }), /console's own/);
});

test("the API's base URL loses a trailing slash", () => {
	assert.equal(normalize({ api: { baseURL: "https://api.example.com/" } }).api.baseURL, "https://api.example.com");
});

test("one set of variables covers both schemes, and outranks style.css", () => {
	const css = themeCSS({ accent: "#0b7285", "--radius": "4px" });
	assert.match(css, /^html:root \{ --accent: #0b7285; --radius: 4px; \}/);
	assert.match(css, /prefers-color-scheme: dark\) \{ html:root:not\(\[data-theme="light"\]\) \{ --accent: #0b7285;/);
	assert.match(css, /html:root\[data-theme="dark"\] \{ --accent: #0b7285;/);
});

test("light and dark sets go to their own schemes", () => {
	const css = themeCSS({ light: { accent: "teal" }, dark: { accent: "cyan" } });
	assert.match(css, /^html:root \{ --accent: teal; \}/);
	assert.doesNotMatch(css, /html:root\[data-theme="dark"\] \{ --accent: teal/);
	assert.match(css, /html:root\[data-theme="dark"\] \{ --accent: cyan; \}/);
	assert.equal(themeCSS({ dark: { accent: "cyan" } }).startsWith("@media"), true);
});
