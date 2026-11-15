(async () => {
	const data = JSON.parse(
		document.getElementById("fmp-report-data").textContent,
	);
	const app = document.getElementById("app");
	data.resources ||= [];
	let reviewFilter = "all", grouping = "none", diffMode = "unified";
	let reviewed = new Set(), storageOK = true, storageKey;
	let preferences = {};
	try {
		const bytes = new TextEncoder().encode(JSON.stringify(data));
		const digest = await crypto.subtle.digest("SHA-256", bytes);
		storageKey = "gpt-review-v1:" + [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, "0")).join("");
		const saved = JSON.parse(localStorage.getItem(storageKey) || "[]");
		if (!Array.isArray(saved) || !saved.every(id => typeof id === "string")) throw new Error("Invalid review state");
		reviewed = new Set(saved);
		preferences = JSON.parse(localStorage.getItem("gpt-report-preferences") || "{}");
		if (!preferences || typeof preferences !== "object" || Array.isArray(preferences)) throw new Error("Invalid preferences");
		localStorage.setItem(storageKey, JSON.stringify([...reviewed]));
	} catch { storageOK = false; }
	function savePreferences() {
		try { localStorage.setItem("gpt-report-preferences", JSON.stringify(preferences)); } catch { storageOK = false; }
	}
	function reviewID(res) { return String(res.index); }
	function progressText() {
		return `${data.resources.filter(r => reviewed.has(reviewID(r))).length}/${data.resources.length} reviewed · local annotation${storageOK ? "" : " · session only (storage unavailable)"}`;
	}
	function announce(text) { document.getElementById("review-announcement").textContent = text; }
	function scopedHash(index, view = diffMode) {
		const params = new URLSearchParams();
		if (view !== "unified") params.set("view", view);
		if (filters.query.trim()) params.set("query", filters.query.trim());
		if (reviewFilter !== "all") params.set("review", reviewFilter);
		if (grouping !== "none") params.set("group", grouping);
		return `#${index === undefined ? "resources" : "resource/" + index}${params.size ? "?" + params : ""}`;
	}
	function markReviewed(res) {
		const rows = filteredResources(), position = rows.findIndex(r => r.index === res.index);
		const id = reviewID(res);
		if (reviewed.has(id)) reviewed.delete(id); else reviewed.add(id);
		try {
			if (!storageKey || !storageOK) throw new Error("Session only");
			localStorage.setItem(storageKey, JSON.stringify([...reviewed]));
		} catch { storageOK = false; }
		if (reviewFilter === "unreviewed" && reviewed.has(id)) {
			const next = rows[position + 1] || rows[position - 1];
			location.hash = scopedHash(next?.index);
		} else render();
		announce(progressText());
	}
	const filters = {
		query: "",
	};
	const queryFilterKeys = new Set([
		"action",
		"kind",
		"namespace",
		"cluster",
		"producer",
		"apiVersion",
		"name",
		"logicalId",
	]);
	let pendingFocus;
	let focusedIndex = -1;
	let activeSuggestion = -1;

	function el(tag, attrs = {}, children = []) {
		const node = document.createElement(tag);
		for (const [key, value] of Object.entries(attrs)) {
			if (key === "class") node.className = value;
			else if (key === "text") node.textContent = value;
			else if (key === "hidden") node.hidden = value;
			else node.setAttribute(key, value);
		}
		for (const child of children) node.append(child);
		return node;
	}

	function fieldLabel(text, control) {
		return el("label", { class: "field" }, [
			el("span", { class: "sr-only", text }),
			control,
		]);
	}

	function actionButton(text, onClick, className = "") {
		const button = el("button", { class: className, text, type: "button" });
		button.addEventListener("click", onClick);
		return button;
	}

	function route() {
		const hash = location.hash.replace(/^#/, "") || "overview";
		if (hash.startsWith("resource/")) {
			const [path, query] = hash.split("?");
			return {
				name: "resource",
				index: Number(path.split("/")[1]),
				view: new URLSearchParams(query || "").get("view") || "unified",
				params: new URLSearchParams(query || ""),
			};
		}
		const [name, query] = hash.split("?");
		return { name, params: new URLSearchParams(query || "") };
	}

	function setActiveNav(name) {
		document
			.querySelectorAll("[data-route]")
			.forEach((a) => { a.classList.toggle("active", a.dataset.route === name); a.setAttribute("aria-current", a.dataset.route === name ? "page" : "false"); });
	}

	function render() {
		const r = route();
		const focusID = document.activeElement?.id;
		const listScroll = app.querySelector(".workspace-list")?.scrollTop || 0;
		if (r.name === "resource" || r.name === "resources") {
			filters.query = resourceQueryFromParams(r.params);
			reviewFilter = r.params.get("review") === "unreviewed" ? "unreviewed" : "all";
			grouping = ["cluster", "producer", "kind"].includes(r.params.get("group")) ? r.params.get("group") : "none";
			diffMode = r.params.get("view") === "split" ? "split" : "unified";
		}
		setActiveNav(r.name === "resource" ? "resources" : r.name);
		focusedIndex = -1;
		app.replaceChildren();
		app.classList.toggle("workspace-app", r.name === "resource" || r.name === "resources");
		if (r.name === "resources" || r.name === "resource") renderWorkspace(r);
		else renderOverview();
		const sidebar = app.querySelector(".workspace-list");
		if (sidebar) {
			sidebar.scrollTop = listScroll;
			const selected = sidebar.querySelector(".selected");
			if (selected && sidebar.clientHeight) {
				const bounds = sidebar.getBoundingClientRect(), row = selected.getBoundingClientRect();
				if (row.top < bounds.top) sidebar.scrollTop -= bounds.top - row.top;
				else if (row.bottom > bounds.bottom) sidebar.scrollTop += row.bottom - bounds.bottom;
			}
		}
		(document.getElementById(pendingFocus || focusID) || (r.name === "resource" && app.querySelector(".workspace-detail")) || app).focus({ preventScroll: true });
		pendingFocus = undefined;
		const nav = document.querySelector('[data-route="resources"]');
		if (nav) nav.setAttribute("href", scopedHash());
	}

	function renderWorkspace(r) {
		const sidebar = el("aside", { class: "workspace-list", "aria-label": "Changed resources" });
		const detail = el("section", { class: "workspace-detail", tabindex: "-1", "aria-label": "Resource detail" });
		const workspace = el("div", { class: `workspace ${r.name === "resource" ? "has-selection" : ""}` }, [sidebar, detail]);
		app.append(el("div", { class: "workspace-heading" }, [el("h1", { text: "Review workspace" }), el("p", { class: "review-progress", text: progressText() })]), filterBar(), workspace);
		renderFilteredList();
		if (r.name === "resource") {
			const start = app.children.length;
			renderResource(r.index, diffMode);
			while (app.children.length > start) detail.append(app.children[start]);
		} else detail.append(el("div", { class: "empty-state", text: "Select a resource to inspect its manifest diff. Review marks are local notes, not policy approval." }));
	}

	function renderOverview() {
		app.append(
			el("div", { class: "eyebrow", text: "Impact Summary" }),
			el("h1", { text: titleForStatus() }),
			el("div", { class: "meta" }, [
				el("span", { text: `Base: ${data.meta.base || "unknown"}` }),
				el("span", { text: `Target: ${data.meta.target || "unknown"}` }),
				el("span", { text: `Generated: ${data.meta.generatedAt}` }),
			]),
			summaryGrid(),
		);
		app.append(actionLinks());
		app.append(el("section", { class: "review-brief" }, [
			el("div", {}, [el("strong", { text: "Report status" }), el("p", { text: `${data.meta.status || "unknown"}${data.meta.warnings?.length ? " · partial render warnings" : ""}${data.meta.diffTruncated ? " · report diff truncated" : ""}` })]),
			el("div", {}, [el("strong", { text: "Deterministic policy" }), el("p", { text: data.policies.policyFailed ? "Failed" : "No failure reported · evaluation status unavailable" })]),
			el("div", {}, [el("strong", { text: "Your review" }), el("p", { text: progressText() })]),
		]));
		renderWarningSection();
		renderClusterSection();
		renderSingleClusterPolicySignals();
		renderAIAssessment();
		renderKindSection();
		renderChangesPreview();
	}

	function titleForStatus() {
		const clusters = new Set(data.resources.map(r => r.cluster || "default")).size;
		return `${data.summary.total} resource changes across ${clusters} cluster${clusters === 1 ? "" : "s"}`;
	}

	function summaryGrid() {
		return el("section", { class: "summary-grid" }, [
			metric("added", `+${data.summary.added}`, "New resources", "added"),
			metric(
				"modified",
				`~${data.summary.modified}`,
				"Modified resources",
				"modified",
			),
			metric(
				"deleted",
				`-${data.summary.deleted}`,
				"Deleted resources",
				"deleted",
			),
			metric("", `${data.summary.total}`, "Total changes", ""),
		]);
	}

	function metric(kind, value, label, action) {
		const href =
			action !== undefined
				? `#resources${action ? "?query=" + encodeURIComponent("action:" + action) : ""}`
				: null;
		if (href)
			return el("a", { href, class: `metric ${kind}` }, [
				el("span", { class: "metric-value", text: value }),
				el("span", { class: "metric-label", text: label }),
			]);
		return el("div", { class: `metric ${kind}` }, [
			el("span", { class: "metric-value", text: value }),
			el("span", { class: "metric-label", text: label }),
		]);
	}

	function actionLinks() {
		const children = [
			el("a", {
				class: "button",
				href: "#resources",
				text: "Open Resource Browser",
			}),
			el("a", {
				class: "button secondary",
				href: "#overview",
				text: `${data.resources.length} resources indexed`,
			}),
		];
		const labels = suggestedLabelPills();
		if (labels) children.push(labels);
		return el("div", { class: "overview-actions" }, children);
	}

	function renderClusterSection() {
		const rows = Object.entries(data.summary.clusterBreakdown || {}).sort(
			(a, b) => b[1].total - a[1].total || a[0].localeCompare(b[0]),
		);
		if (rows.length === 0) return;
		// Only show cluster breakdown in multi-cluster mode.
		if (rows.length <= 1) return;
		app.append(el("h2", { text: "Clusters" }));
		const grid = el("div", { class: "cluster-grid" });
		const policyGroups = groupPolicySignalsByCluster(policySignals());
		for (const [cluster, row] of rows) {
			const clusterLabel = cluster || "(default)";
			const href = `#resources?query=${encodeURIComponent("cluster:" + (cluster || "default"))}`;
			const children = [
				el("div", { class: "card-title" }, [
					el("strong", { text: clusterLabel }),
					el("span", { class: "pill", text: String(row.total) }),
				]),
				el("small", {
					text: `${row.added} added · ${row.modified} modified · ${row.deleted} deleted`,
				}),
			];
			const signals = uniquePolicySignals(
				policyGroups.get(cluster || "default") || [],
			);
			if (signals.length > 0) children.push(policyPills(signals, 3));
			grid.append(el("a", { class: "card cluster-card", href }, children));
		}
		app.append(grid);
	}

	function renderWarningSection() {
		const warnings = data.meta.warnings || [];
		if (warnings.length === 0) return;
		app.append(el("h2", { text: "Warnings" }));
		const list = el("div", { class: "warning-list" });
		for (const warning of warnings) {
			list.append(
				el("div", { class: "card warning-card" }, [
					el("div", { class: "card-title" }, [
						el("strong", { text: "Partial render" }),
						el("span", { class: "pill policy-signal warning", text: "warning" }),
					]),
					el("small", { text: warning }),
				]),
			);
		}
		app.append(list);
	}

	function renderSingleClusterPolicySignals() {
		const rows = Object.entries(data.summary.clusterBreakdown || {});
		if (rows.length > 1) return;
		const signals = uniquePolicySignals(policySignals());
		if (signals.length === 0) return;
		app.append(policyPills(signals));
	}

	function renderAIAssessment() {
		if (!data.ai) return;
		app.append(el("h2", { text: "AI Assessment" }));
		const meta = [data.ai.provider, data.ai.model].filter(Boolean).join(" · ");
		const children = [
			el("div", { class: "card-title" }, [
				el("strong", { text: "Generated review aid" }),
				el("span", { class: "pill policy-signal info", text: "ai" }),
			]),
		];
		if (meta) children.push(el("small", { text: meta }));
		if (data.ai.summary) children.push(el("p", { text: data.ai.summary }));
		if (data.ai.truncated)
			children.push(el("small", { text: "AI input was truncated; assessment may not cover every diff detail." }));
		app.append(el("section", { class: "card ai-card" }, children));
	}

	function renderKindSection() {
		const rows = Object.entries(data.summary.kindBreakdown || {}).sort(
			(a, b) => b[1].total - a[1].total || a[0].localeCompare(b[0]),
		);
		if (rows.length === 0) return;
		app.append(el("h2", { text: "Resource kinds" }));
		const grid = el("div", { class: "kind-grid" });
		for (const [kind, row] of rows) {
			grid.append(
				el("div", { class: "card" }, [
					el("div", { class: "card-title" }, [
						el("strong", { text: kind }),
						el("span", { class: "pill", text: String(row.total) }),
					]),
					el("small", {
						text: `${row.added} added · ${row.modified} modified · ${row.deleted} deleted`,
					}),
				]),
			);
		}
		app.append(grid);
	}

	function suggestedLabelPills() {
		const signals = (data.policies.labels || []).map((label) => ({
			severity: "label",
			text: label,
		}));
		if (signals.length === 0) return null;
		return policyPills(signals);
	}

	function policySignals() {
		const signals = [];
		for (const item of data.policies.classifications || []) {
			signals.push({
				cluster: item.cluster || item.Cluster || "default",
				priority: item.priority || item.Priority || 0,
				severity: item.severity || item.Severity || "info",
				text: `${item.provenance || item.Provenance || "unattributed"}: ${item.id || item.ID || "classification"}`,
			});
		}
		for (const item of data.policies.violations || []) {
			signals.push({
				cluster: item.cluster || item.Cluster || "default",
				priority: item.priority || item.Priority || 0,
				severity: item.severity || item.Severity || "error",
				text: item.id || item.ID || item.message || "violation",
			});
		}
		return signals;
	}

	function groupPolicySignalsByCluster(signals) {
		const groups = new Map();
		for (const signal of signals) {
			const cluster = signal.cluster || "default";
			if (!groups.has(cluster)) groups.set(cluster, []);
			groups.get(cluster).push(signal);
		}
		return groups;
	}

	function uniquePolicySignals(signals) {
		const seen = new Set();
		const out = [];
		for (const signal of sortedPolicySignals(signals)) {
			const key = `${signal.severity}\x00${signal.text}`;
			if (seen.has(key)) continue;
			seen.add(key);
			out.push(signal);
		}
		return out;
	}

	function sortedPolicySignals(signals) {
		return signals.slice().sort((a, b) => {
			const priorityDiff = policyPriority(b) - policyPriority(a);
			if (priorityDiff !== 0) return priorityDiff;
			const severityDiff = severityRank(b.severity) - severityRank(a.severity);
			if (severityDiff !== 0) return severityDiff;
			return a.text.localeCompare(b.text);
		});
	}

	function policyPriority(signal) {
		return typeof signal.priority === "number" ? signal.priority : 0;
	}

	function severityRank(severity) {
		if (severity === "error") return 3;
		if (severity === "warning") return 2;
		if (severity === "info") return 1;
		return 0;
	}

	function policyPills(signals, limit = signals.length) {
		const sortedSignals = sortedPolicySignals(signals);
		const wrap = el("div", { class: "policy-pills" });
		for (const signal of sortedSignals.slice(0, limit)) {
			wrap.append(
				el("span", {
					class: `pill policy-signal ${signal.severity}`,
					text: signal.text,
				}),
			);
		}
		if (sortedSignals.length > limit) {
			const hidden = sortedSignals
				.slice(limit)
				.map((signal) => signal.text)
				.join(", ");
			wrap.append(
				el("span", {
					class: "pill policy-signal more",
					title: hidden,
					text: `+${sortedSignals.length - limit} more`,
				}),
			);
		}
		return wrap;
	}

	function renderChangesPreview() {
		const preview = data.resources.slice(0, 8);
		const truncated = preview.length < data.resources.length;
		app.append(el("h2", { text: "Changes" }));
		if (data.resources.length > 0) {
			app.append(el("p", {
				class: "filter-count",
				text: truncated
					? `Showing ${preview.length} of ${data.resources.length} changes`
					: `Showing all ${data.resources.length} changes`,
			}));
		}
		const list = el("div", { class: "resource-list" });
		for (const res of preview)
			list.append(resourceCard(res));
		if (data.resources.length === 0)
			list.append(
				el("div", { class: "empty-state", text: "No changed resources." }),
			);
		app.append(list);
		if (truncated) {
			app.append(el("div", { class: "overview-actions" }, [
				el("a", {
					class: "button secondary",
					href: "#resources",
					text: `View all ${data.resources.length} changes`,
				}),
			]));
		}
	}

	function renderFilteredList() {
		let list = app.querySelector(".resource-list");
		if (list) {
			list.classList.add("no-animate");
			list.replaceChildren();
		}
		else {
			list = el("div", { class: "resource-list" });
			(app.querySelector(".workspace-list") || app).append(list);
		}
		const rows = filteredResources();
		const groups = new Map();
		for (const res of rows) {
			const key = grouping === "none" ? "" : res[grouping] || (grouping === "cluster" ? "default" : "unknown");
			if (!groups.has(key)) groups.set(key, []);
			groups.get(key).push(res);
		}
		for (const [key, resources] of groups) {
			if (key) list.append(el("h2", { class: "group-heading", text: `${key} · ${resources.length}` }));
			for (const res of resources) list.append(resourceCard(res));
		}
		if (rows.length === 0)
			list.append(
				el("div", {
					class: "empty-state",
					text: "No resources match the current filters.",
				}),
			);
		const count = app.querySelector(".filter-count");
		if (count)
			count.textContent = `${rows.length} of ${data.resources.length} resources`;
		const clear = app.querySelector(".clear-filters");
		if (clear) clear.hidden = !hasActiveFilters();
		focusedIndex = -1;
		const r = route();
		const nav = app.querySelector(".resource-nav");
		if (nav && r.name === "resource") nav.replaceChildren(...resourceNav(r.index).children);
		const notice = app.querySelector(".scope-notice");
		if (notice) notice.hidden = rows.some(row => row.index === r.index);
	}

	function filterBar() {
		const bar = el("div", { class: "filterbar" });
		const search = el("input", {
			id: "filter-query",
			name: "query",
			"aria-label": "Search resources",
			"aria-autocomplete": "list",
			role: "combobox",
			"aria-expanded": "false",
			"aria-controls": "query-suggestions",
			autocomplete: "off",
			placeholder: "Search resources, e.g. podinfo cluster:staging action:modified",
			value: filters.query,
		});
		const suggestions = el("div", {
			id: "query-suggestions",
			class: "query-suggestions",
			"aria-label": "Search suggestions",
			role: "listbox",
			hidden: true,
		});
		search.addEventListener("input", () => {
			filters.query = search.value;
			syncResourceFiltersToHash();
			renderFilteredList();
			renderSuggestions(search, suggestions);
		});
		search.addEventListener("focus", () => renderSuggestions(search, suggestions));
		search.addEventListener("blur", () => {
			setTimeout(() => {
				suggestions.hidden = true;
				search.setAttribute("aria-expanded", "false");
			}, 120);
		});
		search.addEventListener("keydown", (event) => {
			handleSuggestionKeydown(event, search, suggestions);
		});
		const clear = actionButton("Clear filters", () => {
			clearFilters();
			render();
		}, "clear-filters");
		clear.hidden = !hasActiveFilters();
		bar.append(
			el("div", { class: "search-field" }, [
				fieldLabel("Search resources", search),
				suggestions,
			]),
			clear,
			el("span", {
				class: "filter-count",
				text: `${filteredResources().length} of ${data.resources.length} resources`,
			}),
		);
		const review = el("select", { id: "review-filter", "aria-label": "Review filter" });
		for (const [value, text] of [["all", "All changes"], ["unreviewed", "Unreviewed only"]]) review.append(el("option", { value, text }));
		review.value = reviewFilter;
		review.addEventListener("change", () => { reviewFilter = review.value; syncResourceFiltersToHash(); render(); });
		const group = el("select", { id: "group-filter", "aria-label": "Group resources" });
		for (const value of ["none", "cluster", "producer", "kind"]) group.append(el("option", { value, text: value === "none" ? "No grouping" : `Group by ${value}` }));
		group.value = grouping;
		group.addEventListener("change", () => { grouping = group.value; syncResourceFiltersToHash(); render(); });
		bar.append(fieldLabel("Review filter", review), fieldLabel("Group resources", group));
		return bar;
	}

	function hasActiveFilters() {
		return filters.query.trim() !== "" || reviewFilter !== "all";
	}

	function clearFilters() {
		filters.query = "";
		reviewFilter = "all";
		syncResourceFiltersToHash();
	}

	function syncResourceFiltersToHash() {
		const r = route();
		const next = scopedHash(r.name === "resource" ? r.index : undefined);
		if (location.hash !== next) history.replaceState(null, "", next);
		for (const link of app.querySelectorAll(".back-to-list")) link.setAttribute("href", scopedHash());
		const breadcrumb = app.querySelector(".breadcrumb")?.querySelector("a");
		if (breadcrumb) breadcrumb.setAttribute("href", scopedHash());
		const nav = document.querySelector('[data-route="resources"]');
		if (nav) nav.setAttribute("href", scopedHash());
	}

	function resourceQueryFromParams(params) {
		const query = params.get("query");
		if (query) return query;
		const parts = [];
		for (const key of queryFilterKeys) {
			const value = params.get(key);
			if (value) parts.push(`${key}:${value}`);
		}
		return parts.join(" ");
	}

	function renderSuggestions(input, panel) {
		const suggestions = querySuggestions(input.value, input.selectionStart || input.value.length);
		activeSuggestion = -1;
		panel.replaceChildren();
		panel.hidden = suggestions.length === 0;
		input.setAttribute("aria-expanded", String(!panel.hidden));
		for (const [index, suggestion] of suggestions.entries()) {
			const option = el("button", {
				class: "query-suggestion",
				role: "option",
				type: "button",
				"data-index": String(index),
				id: `query-option-${index}`,
			});
			option.append(
				el("span", { class: "suggestion-label", text: suggestion.label }),
				el("span", { class: "suggestion-detail", text: suggestion.detail }),
			);
			option.addEventListener("mousedown", (event) => event.preventDefault());
			option.addEventListener("click", () => applySuggestion(input, panel, suggestion));
			panel.append(option);
		}
	}

	function handleSuggestionKeydown(event, input, panel) {
		const options = [...panel.querySelectorAll(".query-suggestion")];
		if (panel.hidden || options.length === 0) return;
		if (event.key === "ArrowDown") {
			event.preventDefault();
			event.stopPropagation();
			activeSuggestion = Math.min(activeSuggestion + 1, options.length - 1);
			markActiveSuggestion(options);
		} else if (event.key === "ArrowUp") {
			event.preventDefault();
			event.stopPropagation();
			activeSuggestion = Math.max(activeSuggestion - 1, 0);
			markActiveSuggestion(options);
		} else if (event.key === "Enter" && activeSuggestion >= 0) {
			event.preventDefault();
			event.stopPropagation();
			options[activeSuggestion].click();
		} else if (event.key === "Escape") {
			event.stopPropagation();
			panel.hidden = true;
			input.setAttribute("aria-expanded", "false");
		}
	}

	function markActiveSuggestion(options) {
		document.getElementById("filter-query").setAttribute("aria-activedescendant", `query-option-${activeSuggestion}`);
		for (const [index, option] of options.entries()) {
			option.classList.toggle("active", index === activeSuggestion);
			option.setAttribute("aria-selected", String(index === activeSuggestion));
		}
	}

	function querySuggestions(query, cursor) {
		const current = currentQueryToken(query, cursor);
		const raw = current.value.toLowerCase();
		if (!raw) return [];
		const separator = raw.indexOf(":");
		if (separator >= 0) {
			const key = normalizeFilterKey(raw.slice(0, separator));
			const prefix = raw.slice(separator + 1);
			if (!queryFilterKeys.has(key)) return [];
			return filterValues(key)
				.filter((value) => {
					const normalized = value.toLowerCase();
					return normalized.startsWith(prefix) && normalized !== prefix;
				})
				.slice(0, 8)
				.map((value) => ({
					label: `${key}:${value}`,
					detail: "filter value",
					value: `${key}:${value}`,
					start: current.start,
					end: current.end,
				}));
		}
		return [...queryFilterKeys]
			.filter((key) => key.toLowerCase().startsWith(raw))
			.slice(0, 8)
			.map((key) => ({
				label: `${key}:`,
				detail: "filter key",
				value: `${key}:`,
				start: current.start,
				end: current.end,
			}));
	}

	function applySuggestion(input, panel, suggestion) {
		const before = input.value.slice(0, suggestion.start);
		const after = input.value.slice(suggestion.end).replace(/^\s+/, "");
		const suffix = suggestion.value.endsWith(":") ? "" : " ";
		const spacer = after ? " " : "";
		filters.query = `${before}${suggestion.value}${suffix}${spacer}${after}`.replace(/\s+$/, suffix);
		input.value = filters.query;
		const nextCursor = before.length + suggestion.value.length + suffix.length;
		input.setSelectionRange(nextCursor, nextCursor);
		syncResourceFiltersToHash();
		renderFilteredList();
		renderSuggestions(input, panel);
		input.focus();
	}

	function currentQueryToken(query, cursor) {
		let start = cursor;
		while (start > 0 && !/\s/.test(query[start - 1])) start--;
		let end = cursor;
		while (end < query.length && !/\s/.test(query[end])) end++;
		return { start, end, value: query.slice(start, end) };
	}

	function filterValues(key) {
		return unique(
			data.resources.map((resource) => {
				if (key === "namespace") return resource.namespace || "cluster-scoped";
				if (key === "cluster") return resource.cluster || "default";
				if (key === "producer") return resource.producer || "unknown";
				if (key === "name") return resourceName(resource);
				return resource[key] || "";
			}),
		);
	}

	function unique(values) {
		return [...new Set(values)]
			.filter(Boolean)
			.sort((a, b) => a.localeCompare(b));
	}

	function filteredResources() {
		const parsed = parseResourceQuery(filters.query);
		return data.resources.filter((r) => {
			if (reviewFilter === "unreviewed" && reviewed.has(reviewID(r))) return false;
			const haystack =
				`${resourceName(r)} ${r.logicalId || ""} ${r.namespace || "cluster-scoped"} ${r.kind} ${r.cluster || "default"} ${r.producer || ""} ${r.action} ${r.apiVersion || ""}`.toLowerCase();
			return (
				parsed.terms.every((term) => haystack.includes(term.value)) &&
				parsed.filters.every((filter) =>
					resourceFieldValue(r, filter.key).includes(filter.value),
				)
			);
		});
	}

	function parseResourceQuery(query) {
		const tokens = tokenizeQuery(query);
		const parsed = { terms: [], filters: [] };
		for (const [index, token] of tokens.entries()) {
			const trimmed = token.trim();
			const separator = token.indexOf(":");
			if (separator > 0) {
				const key = normalizeFilterKey(token.slice(0, separator));
				const value = token.slice(separator + 1).trim().toLowerCase();
				if (queryFilterKeys.has(key) && !value) continue;
				if (queryFilterKeys.has(key) && value) {
					parsed.filters.push({ index, key, value, raw: token });
					continue;
				}
			}
			if (isPartialFilterKey(trimmed)) continue;
			if (trimmed) parsed.terms.push({ index, value: trimmed.toLowerCase(), raw: token });
		}
		return parsed;
	}

	function isPartialFilterKey(token) {
		const normalized = token.toLowerCase();
		if (!normalized) return false;
		return [...queryFilterKeys].some((key) => key.toLowerCase().startsWith(normalized));
	}

	function tokenizeQuery(query) {
		const tokens = [];
		let token = "";
		let quote = "";
		for (const char of query.trim()) {
			if ((char === '"' || char === "'") && !quote) {
				quote = char;
				continue;
			}
			if (char === quote) {
				quote = "";
				continue;
			}
			if (/\s/.test(char) && !quote) {
				if (token) tokens.push(token);
				token = "";
				continue;
			}
			token += char;
		}
		if (token) tokens.push(token);
		return tokens;
	}

	function normalizeFilterKey(key) {
		const normalized = key.toLowerCase();
		if (normalized === "api" || normalized === "apiversion") {
			return "apiVersion";
		}
		if (normalized === "logicalid") return "logicalId";
		return normalized;
	}

	function resourceFieldValue(resource, key) {
		if (key === "namespace") return (resource.namespace || "cluster-scoped").toLowerCase();
		if (key === "cluster") return (resource.cluster || "default").toLowerCase();
		if (key === "producer") return (resource.producer || "unknown").toLowerCase();
		if (key === "name") return resourceName(resource).toLowerCase();
		return String(resource[key] || "").toLowerCase();
	}

	function sparkline(res) {
		const total = res.addedLines + res.deletedLines;
		if (total === 0) return el("div", { class: "sparkline" });
		const wrap = el("div", { class: "sparkline" });
		const addedPct = ((res.addedLines / total) * 100).toFixed(1);
		const deletedPct = ((res.deletedLines / total) * 100).toFixed(1);
		const addedBar = el("div", {
			class: "sparkline-bar added",
			style: `width:${addedPct}%`,
		});
		const deletedBar = el("div", {
			class: "sparkline-bar deleted",
			style: `width:${deletedPct}%;left:${addedPct}%`,
		});
		wrap.append(addedBar, deletedBar);
		return wrap;
	}

	function resourceName(res) {
		return res.name || res.slot || res.logicalId || "(unnamed)";
	}

	function resourceCard(res) {
		return el(
			"a",
			{ class: `resource-card${route().index === res.index ? " selected" : ""}`, href: scopedHash(res.index), tabindex: "0", "aria-current": route().index === res.index ? "true" : "false" },
			[
				el("div", { class: "resource-title" }, [
					el("strong", {
						text: `${res.kind} / ${res.namespace || "cluster-scoped"} / ${resourceName(res)}`,
						title: res.logicalId || res.name,
					}),
					el("span", { class: "resource-badges" }, [
						el("span", {
							class: "pill cluster",
							text: res.cluster || "default",
						}),
						el("span", { class: `pill ${res.action}`, text: `${res.action}${reviewed.has(reviewID(res)) ? " · ✓ reviewed" : ""}` }),
					]),
				]),
				el("div", {
					class: "resource-meta",
					text: `Cluster: ${res.cluster || "default"} · ${res.apiVersion} · Producer: ${res.producer || "unknown"}`,
				}),
				el("div", { class: "resource-foot" }, [
					el("span", { text: `+${res.addedLines}` }),
					el("span", { text: `-${res.deletedLines}` }),
					sparkline(res),
				]),
			],
		);
	}

	function diffStats(res) {
		let added = 0,
			deleted = 0,
			context = 0;
		for (const row of res.diffRows) {
			if (row.type === "added") added++;
			else if (row.type === "deleted") deleted++;
			else if (row.type === "context") context++;
		}
		return el("div", { class: "diff-stats" }, [
			el("span", { class: "added", text: `+${added} added` }),
			el("span", { class: "deleted", text: `-${deleted} deleted` }),
			el("span", { class: "context", text: `${context} unchanged` }),
		]);
	}

	function copyButton(res) {
		const btn = el("button", { class: "copy-btn", text: "Copy diff", type: "button" });
		btn.addEventListener("click", () => {
			const lines = res.diffRows.map((row) => {
				if (row.type === "hunk") return row.oldText;
				const sign =
					row.type === "added" ? "+" : row.type === "deleted" ? "-" : " ";
				return sign + (row.type === "added" ? row.newText : row.oldText);
			});
			copyText(btn, lines.join("\n"), "Copy diff");
		});
		return btn;
	}

	function copyLinkButton(index, view) {
		const btn = el("button", { class: "copy-btn", text: "Copy link", type: "button" });
		btn.addEventListener("click", () => {
			copyText(btn, resourceURL(index, view), "Copy link");
		});
		return btn;
	}

	function resourceURL(index, view) {
		const url = new URL(location.href);
		url.hash = scopedHash(index, view);
		return url.href;
	}

	function copyText(button, text, resetText) {
		Promise.resolve().then(() => writeClipboard(text)).then(() => {
			button.textContent = "Copied";
			button.classList.add("copied");
			setTimeout(() => {
				button.textContent = resetText;
				button.classList.remove("copied");
			}, 1500);
		}).catch(() => { button.textContent = "Copy failed"; announce("Clipboard unavailable. Select and copy the diff or address manually."); });
	}

	function writeClipboard(text) {
		if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(text);
		const textarea = el("textarea");
		textarea.value = text;
		textarea.setAttribute("readonly", "");
		textarea.style.position = "fixed";
		textarea.style.left = "-9999px";
		document.body.append(textarea);
		textarea.select();
		try {
			if (!document.execCommand("copy")) return Promise.reject(new Error("Copy unavailable"));
			return Promise.resolve();
		} finally {
			textarea.remove();
		}
	}

	function renderResource(index, view) {
		app.append(el("a", { class: "back-to-list", href: scopedHash(), text: "← Back to resources" }));
		const res = data.resources.find(r => r.index === index);
		if (!res) {
			app.append(
				el("div", { class: "empty-state", text: "Resource not found." }),
			);
			return;
		}
		app.append(el("p", { class: "scope-notice", hidden: filteredResources().some(r => r.index === index), text: "This linked resource is outside the current filters. Filters have been preserved; return to the list or adjust them to continue." }));
		app.append(
			el("div", { class: "breadcrumb" }, [
				el("a", { href: scopedHash(), text: "Resources" }),
				document.createTextNode(` / ${res.kind} / ${resourceName(res)}`),
			]),
			el("section", { class: "detail-header" }, [
				el("div", {}, [
					el("div", { class: "eyebrow", text: res.action }),
					el("h1", { text: `${res.kind} ${resourceName(res)}`, title: res.logicalId || res.name }),
					el("p", {
						text: `${res.namespace || "cluster-scoped"} · ${res.cluster || "default"} · ${res.apiVersion} · Producer: ${res.producer || "unknown"}`,
					}),
				]),
				el("div", { class: "detail-actions" }, [
					Object.assign(actionButton(reviewed.has(reviewID(res)) ? "✓ Reviewed — undo" : "Mark reviewed", () => markReviewed(res), "review-button"), { id: "mark-reviewed" }),
					resourceNav(index),
					diffToggle(index, view),
					copyLinkButton(index, view),
					copyButton(res),
				]),
			]),
			diffStats(res),
			el("section", { class: "detail-panel" }, [diffView(res, view)]),
		);
	}

	function resourceNav(index) {
		const rows = filteredResources();
		const position = rows.findIndex(r => r.index === index);
		const previous =
			position > 0
				? el("a", { href: scopedHash(rows[position - 1].index), text: "Previous" })
				: el("span", { text: "Previous", "aria-disabled": "true" });
		const next =
			position >= 0 && position < rows.length - 1
				? el("a", { href: scopedHash(rows[position + 1].index), text: "Next" })
				: el("span", { text: "Next", "aria-disabled": "true" });
		return el("div", { class: "resource-nav", "aria-label": "Resource navigation" }, [
			previous,
			next,
		]);
	}

	function diffToggle(index, view) {
		const toggle = el("div", { class: "toggle" });
		for (const mode of ["unified", "split"]) {
			const button = el("button", {
				class: view === mode ? "active" : "",
				"aria-pressed": String(view === mode),
				text: mode === "split" ? "Split" : "Unified",
				type: "button",
			});
			button.addEventListener("click", () => {
				location.hash = scopedHash(index, mode);
			});
			toggle.append(button);
		}
		return toggle;
	}

	function diffView(res, view) {
		const wrap = el("div", { class: `diff ${view}` });
		if (!res.diffRows.length && !res.truncated) wrap.append(el("p", { class: "empty-state", text: "No diff lines available for this resource." }));
		for (const row of res.diffRows)
			wrap.append(view === "split" ? splitRow(row) : unifiedRow(row));
		if (res.truncated)
			wrap.append(
				el("div", {
					class: "empty-state",
					text: "Diff truncated by html-report-max-resource-diff-bytes.",
				}),
			);
		return wrap;
	}

	function unifiedRow(row) {
		const sign =
			row.type === "added" ? "+" : row.type === "deleted" ? "-" : " ";
		const number =
			row.type === "added" ? row.newLine : row.oldLine || row.newLine;
		const text =
			row.type === "hunk"
				? row.oldText
				: `${sign}${row.type === "added" ? row.newText : row.oldText}`;
		return el("div", { class: `diff-row ${row.type}` }, [
			el("div", {
				class: "cell line-no",
				text: row.type === "hunk" ? "" : String(number || ""),
			}),
			el("div", { class: "cell", text }),
		]);
	}

	function splitRow(row) {
		if (row.type === "hunk")
			return el("div", { class: "diff-row hunk" }, [
				el("div", { class: "cell line-no" }),
				el("div", { class: "cell", text: row.oldText }),
				el("div", { class: "cell line-no" }),
				el("div", { class: "cell", text: row.newText }),
			]);
		return el("div", { class: `diff-row ${row.type}` }, [
			el("div", {
				class: "cell line-no",
				text: row.oldLine ? String(row.oldLine) : "",
			}),
			el("div", {
				class: `cell ${row.oldLine ? "" : "empty"}`,
				text: row.oldLine ? `${row.type === "deleted" ? "-" : " "}${row.oldText || ""}` : "",
			}),
			el("div", {
				class: "cell line-no",
				text: row.newLine ? String(row.newLine) : "",
			}),
			el("div", {
				class: `cell ${row.newLine ? "" : "empty"}`,
				text: row.newLine ? `${row.type === "added" ? "+" : " "}${row.newText || ""}` : "",
			}),
		]);
	}

	function handleKeydown(e) {
		if (e.ctrlKey || e.metaKey || e.altKey || e.target.isContentEditable || ["INPUT", "SELECT", "TEXTAREA", "BUTTON"].includes(e.target.tagName)) return;
		if (e.key === "?") { e.preventDefault(); showHelp(); return; }
		if (document.getElementById("keyboard-help")?.open) return;
		if (e.shiftKey) return;
		if (e.key === "/") { e.preventDefault(); const search = document.getElementById("filter-query"); if (search) search.focus(); else { pendingFocus = "filter-query"; location.hash = scopedHash(); } return; }
		const current = route();
		if (current.name === "resource") {
			const rows = filteredResources(), position = rows.findIndex(r => r.index === current.index);
			if (["j", "k"].includes(e.key) && position >= 0) { e.preventDefault(); const next = rows[position + (e.key === "j" ? 1 : -1)]; if (next) location.hash = scopedHash(next.index); return; }
			if (e.key === "r") { const res = data.resources.find(r => r.index === current.index); if (res) { e.preventDefault(); markReviewed(res); } return; }
		}
		const cards = () => [...app.querySelectorAll(".resource-card")];
		const r = route();

		if (r.name === "resources") {
			const all = cards();
			if (e.key === "ArrowDown") {
				e.preventDefault();
				focusedIndex = Math.min(focusedIndex + 1, all.length - 1);
				all[focusedIndex]?.focus();
			} else if (e.key === "ArrowUp") {
				e.preventDefault();
				focusedIndex = Math.max(focusedIndex - 1, 0);
				all[focusedIndex]?.focus();
			} else if (e.key === "Enter" && focusedIndex >= 0 && all[focusedIndex]) {
				location.hash = all[focusedIndex]
					.getAttribute("href")
					.replace(/^.*#/, "#");
			}
		}

		if (r.name === "resource" && e.key === "Escape") {
			e.preventDefault();
			location.hash = scopedHash();
		}
	}

	function showHelp() {
		const dialog = document.getElementById("keyboard-help");
		if (!dialog.open) dialog.showModal();
	}
	const theme = document.getElementById("theme-select");
	function applyTheme() { document.documentElement.setAttribute("data-theme", ["light", "dark"].includes(preferences.theme) ? preferences.theme : "system"); }
	if (theme) {
		theme.value = preferences.theme || "system";
		theme.addEventListener("change", () => { preferences.theme = theme.value; applyTheme(); savePreferences(); });
		applyTheme();
	}
	document.getElementById("help-button")?.addEventListener("click", showHelp);
	window.addEventListener("hashchange", render);
	window.addEventListener("keydown", handleKeydown);
	render();
})();
