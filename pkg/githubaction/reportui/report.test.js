import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";
import { webcrypto } from "node:crypto";

// Minimal DOM for exercising the embedded report runtime without a browser dependency.
class Element {
	constructor(tagName) {
		this.tagName = tagName.toUpperCase();
		this.children = [];
		this.textContent = "";
		this.listeners = {};
		this.classList = { add() {}, remove() {}, toggle() {} };
	}
	setAttribute(key, value) { this[key] = value; }
	append(...children) { for (const child of children) { if (child.parent) child.parent.children.splice(child.parent.children.indexOf(child), 1); child.parent = this; this.children.push(child); } }
	replaceChildren(...children) { this.children.forEach(c => c.parent = null); this.children = []; this.append(...children); }
	addEventListener(event, fn) { this.listeners[event] = fn; }
	getAttribute(key) { return this[key]; }
	click() { this.listeners.click?.({}); }
	focus() {}
	querySelector(selector) {
		return this.querySelectorAll(selector)[0] || null;
	}
	querySelectorAll(selector) {
		return this.children.flatMap((child) => {
			const matches = selector.startsWith(".")
				? (child.className || "").split(" ").includes(selector.slice(1))
				: child.tagName === selector.toUpperCase();
			return [...(matches ? [child] : []), ...child.querySelectorAll(selector)];
		});
	}
}

const resources = [
	{ name: "", slot: "credentials", logicalId: "xr/parent/a" },
	{ name: "", slot: "connection", logicalId: "xr/parent/b" },
	{ name: "", logicalId: "xr/parent/c" },
	{ name: "named-secret" },
].map((identity, index) => ({
	...identity, index, kind: "Secret", namespace: "apps", action: "added",
	apiVersion: "v1", producer: "XR apps/parent", addedLines: 0, deletedLines: 0, diffRows: [],
}));

async function runtime(hash, inputResources = resources, storage = new Map(), blocked = false) {
	const app = new Element("main");
	const announcement = new Element("div");
	const data = {
		meta: {}, summary: { total: inputResources.length }, policies: {}, resources: inputResources,
	};
	const document = {
		getElementById: (id) => id === "app" ? app : id === "fmp-report-data" ? { textContent: JSON.stringify(data) } : id === "review-announcement" ? announcement : app.querySelectorAll("input").concat(app.querySelectorAll("button"), app.querySelectorAll("select")).find(e => e.id === id),
		createElement: (tag) => new Element(tag),
		createTextNode: (text) => Object.assign(new Element("text"), { textContent: text }),
		querySelectorAll: () => [],
		querySelector: () => null,
	};
	const listeners = {}, location = { hash, href: "file:///report.html" + hash };
	const clipboard = { writes: [], fail: false };
	await runInNewContext(readFileSync(new URL("./report.js", import.meta.url), "utf8"), {
		document, location, URLSearchParams, URL, TextEncoder, crypto: webcrypto,
		navigator: { clipboard: { writeText(text) { if (clipboard.fail) return Promise.reject(Error("denied")); clipboard.writes.push(text); return Promise.resolve(); } } },
		setTimeout() {},
		localStorage: { getItem(key) { if (blocked) throw Error("blocked"); return storage.get(key); }, setItem(key, value) { if (blocked) throw Error("blocked"); storage.set(key, value); } },
		history: { replaceState(_state, _title, hash) { location.hash = hash; } },
		window: { addEventListener(key, fn) { listeners[key] = fn; } },
	});
	return { app, location, listeners, storage, announcement, clipboard, navigate(hash) { location.hash = hash; listeners.hashchange(); }, key(key, extra = {}) { const event = { key, target: app, preventDefault() {}, ...extra }; listeners.keydown(event); } };
}
async function render(...args) { return (await runtime(...args)).app; }

test("cards and detail headings display slots, logical fallback, and real names", async () => {
	const app = await render("#overview");
	assert.deepEqual(app.querySelectorAll(".resource-card").map((card) => card.querySelector("strong").textContent), [
		"Secret / apps / credentials", "Secret / apps / connection",
		"Secret / apps / xr/parent/c", "Secret / apps / named-secret",
	]);
	assert.equal(app.querySelectorAll(".resource-card")[0].querySelector("strong").title, "xr/parent/a");
	for (const [index, name] of ["credentials", "connection", "xr/parent/c", "named-secret"].entries()) {
		assert.equal((await render(`#resource/${index}`)).querySelector(".workspace-detail").querySelector("h1").textContent, `Secret ${name}`);
	}
});

test("resource browser searches by slot and logical identity", async () => {
	for (const [query, expected] of [
		["credentials", 0], ["xr/parent/b", 1], ["name:connection", 1],
		["logicalId:xr/parent/c", 2], ["name:named-secret", 3],
	]) {
		const cards = (await render(`#resources?query=${encodeURIComponent(query)}`)).querySelectorAll(".resource-card");
		assert.equal(cards.length, 1, query);
		assert.equal(cards[0].href, `#resource/${expected}?${new URLSearchParams({ query })}`, query);
	}
});

test("overview identifies its limited preview and links to the complete resource list", async () => {
	const many = Array.from({ length: 21 }, (_, index) => ({
		...resources[0], index, name: `resource-${index}`, logicalId: undefined,
	}));
	const overview = await render("#overview", many);
	assert.equal(overview.querySelectorAll(".resource-card").length, 8);
	assert.equal(overview.querySelector(".filter-count").textContent, "Showing 8 of 21 changes");
	const all = overview.querySelectorAll("a").find((link) => link.textContent === "View all 21 changes");
	assert.equal(all.href, "#resources");
	assert.equal((await render(all.href, many)).querySelectorAll(".resource-card").length, 21);
	const small = await render("#overview");
	assert.equal(small.querySelector(".filter-count").textContent, "Showing all 4 changes");
});

test("filtered navigation, diff modes and legacy filters preserve scope", async () => {
	const env = await runtime("#resource/0?query=kind%3ASecret&view=split&group=producer&review=unreviewed");
	const next = env.app.querySelector(".resource-nav").querySelector("a");
	assert.equal(next.href, "#resource/1?view=split&query=kind%3ASecret&review=unreviewed&group=producer");
	env.navigate(next.href);
	assert.equal(env.app.querySelector(".workspace-detail").querySelector("h1").textContent, "Secret connection");
	env.navigate("#resources?logicalId=xr%2Fparent%2Fb");
	assert.equal(env.app.querySelectorAll(".resource-card").length, 1);
	env.navigate("#resource/0?query=name%3Anamed-secret");
	assert.match(env.app.querySelector(".scope-notice").textContent, /outside/);
	assert.equal(env.app.querySelector(".resource-nav").querySelectorAll("a").length, 0);
	env.navigate("#resource/999");
	assert.match(env.app.querySelector(".workspace-detail").querySelector(".empty-state").textContent, /not found/);
});

test("review persists, is isolated by actual payload, and advances without skipping", async () => {
	const env = await runtime("#resource/0?review=unreviewed");
	env.app.querySelector(".review-button").click();
	assert.equal(env.location.hash, "#resource/1?review=unreviewed");
	env.listeners.hashchange();
	env.app.querySelector(".review-button").click();
	assert.equal(env.location.hash, "#resource/2?review=unreviewed");
	const reload = await runtime("#resources?review=unreviewed", resources, env.storage);
	assert.equal(reload.app.querySelectorAll(".resource-card").length, 2);
	const changed = await runtime("#resources?review=unreviewed", resources.map(r => ({ ...r, producer: "changed" })), env.storage);
	assert.equal(changed.app.querySelectorAll(".resource-card").length, 4);
	for (const value of env.storage.values()) assert.doesNotMatch(value, /credentials|diffRows|Secret/);
});

test("blocked and corrupt storage degrade to clearly labelled session progress", async () => {
	for (const blocked of [true, false]) {
		const storage = new Map();
		if (!blocked) { const first = await runtime("#resources", resources, storage); storage.set([...first.storage.keys()][0], "broken JSON"); }
		const env = await runtime("#resource/0", resources, storage, blocked);
		env.app.querySelector(".review-button").click();
		assert.match(env.app.querySelector(".review-progress").textContent, /1\/4 reviewed.*session only/);
	}
});

test("keyboard navigation respects editable targets and modifier combinations", async () => {
	const env = await runtime("#resource/0?query=kind%3ASecret&view=split");
	for (const target of [{ tagName: "INPUT" }, { tagName: "DIV", isContentEditable: true }]) env.key("r", { target });
	env.key("r", { ctrlKey: true });
	assert.equal(env.storage.values().next().value, "[]");
	env.key("j");
	assert.equal(env.location.hash, "#resource/1?view=split&query=kind%3ASecret");
	env.listeners.hashchange();
	env.key("r");
	assert.match(env.announcement.textContent, /1\/4 reviewed/);
	env.key("Escape");
	assert.equal(env.location.hash, "#resources?view=split&query=kind%3ASecret");
});

test("editing search updates filtered navigation and return links without rebuilding the diff", async () => {
	const env = await runtime("#resource/0?view=split");
	const diff = env.app.querySelector(".diff");
	const search = env.app.querySelector("input");
	search.value = "name:named-secret";
	// Input normally also opens suggestions; the minimal DOM has no selection API,
	// so test the empty suggestion path with a complete matching value.
	search.selectionStart = search.value.length;
	search.listeners.input();
	assert.equal(env.app.querySelector(".diff"), diff);
	assert.equal(env.app.querySelector(".resource-nav").querySelectorAll("a").length, 0);
	assert.equal(env.app.querySelector(".scope-notice").hidden, false);
	assert.equal(env.app.querySelector(".back-to-list").href, "#resources?view=split&query=name%3Anamed-secret");
});

test("copy links include scope and clipboard rejection is announced without throwing", async () => {
	const env = await runtime("#resource/1?query=kind%3ASecret&review=unreviewed&group=kind&view=split");
	const copy = env.app.querySelectorAll(".copy-btn");
	copy[0].click();
	await new Promise(setImmediate);
	assert.equal(env.clipboard.writes[0], "file:///report.html#resource/1?view=split&query=kind%3ASecret&review=unreviewed&group=kind");
	env.clipboard.fail = true;
	copy[1].click();
	await new Promise(setImmediate);
	assert.equal(copy[1].textContent, "Copy failed");
	assert.match(env.announcement.textContent, /Clipboard unavailable/);
});

test("empty and truncated diffs are explicit, hostile resource text stays text", async () => {
	const input = [{ ...resources[0], name: '<script>alert("x")</script>', truncated: true }];
	const app = await render("#resource/0", input);
	assert.equal(app.querySelector(".workspace-detail").querySelector("h1").textContent, `Secret ${input[0].name}`);
	assert.match(app.querySelector(".diff").querySelector(".empty-state").textContent, /truncated/);
	assert.match((await render("#resource/0")).querySelector(".diff").querySelector(".empty-state").textContent, /No diff lines/);
});
