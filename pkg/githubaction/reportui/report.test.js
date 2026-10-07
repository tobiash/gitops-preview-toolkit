import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";

// Minimal DOM for exercising the embedded report runtime without a browser dependency.
class Element {
	constructor(tagName) {
		this.tagName = tagName.toUpperCase();
		this.children = [];
		this.textContent = "";
		this.classList = { add() {}, toggle() {} };
	}
	setAttribute(key, value) { this[key] = value; }
	append(...children) { this.children.push(...children); }
	replaceChildren(...children) { this.children = children; }
	addEventListener() {}
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

function render(hash, inputResources = resources) {
	const app = new Element("main");
	const data = {
		meta: {}, summary: { total: inputResources.length }, policies: {}, resources: inputResources,
	};
	const document = {
		getElementById: (id) => id === "app" ? app : { textContent: JSON.stringify(data) },
		createElement: (tag) => new Element(tag),
		createTextNode: (text) => Object.assign(new Element("text"), { textContent: text }),
		querySelectorAll: () => [],
	};
	runInNewContext(readFileSync(new URL("./report.js", import.meta.url), "utf8"), {
		document, location: { hash }, URLSearchParams, window: { addEventListener() {} },
	});
	return app;
}

test("cards and detail headings display slots, logical fallback, and real names", () => {
	const app = render("#overview");
	assert.deepEqual(app.querySelectorAll(".resource-card").map((card) => card.querySelector("strong").textContent), [
		"Secret / apps / credentials", "Secret / apps / connection",
		"Secret / apps / xr/parent/c", "Secret / apps / named-secret",
	]);
	assert.equal(app.querySelectorAll(".resource-card")[0].querySelector("strong").title, "xr/parent/a");
	for (const [index, name] of ["credentials", "connection", "xr/parent/c", "named-secret"].entries()) {
		assert.equal(render(`#resource/${index}`).querySelector("h1").textContent, `Secret ${name}`);
	}
});

test("resource browser searches by slot and logical identity", () => {
	for (const [query, expected] of [
		["credentials", 0], ["xr/parent/b", 1], ["name:connection", 1],
		["logicalId:xr/parent/c", 2], ["name:named-secret", 3],
	]) {
		const cards = render(`#resources?query=${encodeURIComponent(query)}`).querySelectorAll(".resource-card");
		assert.equal(cards.length, 1, query);
		assert.equal(cards[0].href, `#resource/${expected}`, query);
	}
});

test("overview identifies its limited preview and links to the complete resource list", () => {
	const many = Array.from({ length: 21 }, (_, index) => ({
		...resources[0], index, name: `resource-${index}`, logicalId: undefined,
	}));
	const overview = render("#overview", many);
	assert.equal(overview.querySelectorAll(".resource-card").length, 8);
	assert.equal(overview.querySelector(".filter-count").textContent, "Showing 8 of 21 changes");
	const all = overview.querySelectorAll("a").find((link) => link.textContent === "View all 21 changes");
	assert.equal(all.href, "#resources");
	assert.equal(render(all.href, many).querySelectorAll(".resource-card").length, 21);
	const small = render("#overview");
	assert.equal(small.querySelector(".filter-count").textContent, "Showing all 4 changes");
});
