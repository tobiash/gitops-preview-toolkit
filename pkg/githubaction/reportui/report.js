(() => {
	const data = JSON.parse(
		document.getElementById("fmp-report-data").textContent,
	);
	const app = document.getElementById("app");
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
			};
		}
		const [name, query] = hash.split("?");
		return { name, params: new URLSearchParams(query || "") };
	}

	function setActiveNav(name) {
		document
			.querySelectorAll("[data-route]")
			.forEach((a) => a.classList.toggle("active", a.dataset.route === name));
	}

	function render() {
		const r = route();
		setActiveNav(r.name === "resource" ? "resources" : r.name);
		focusedIndex = -1;
		app.replaceChildren();
		if (r.name === "resources") renderResources(r.params);
		else if (r.name === "resource") renderResource(r.index, r.view);
		else renderOverview();
		app.focus({ preventScroll: true });
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
		renderWarningSection();
		renderClusterSection();
		renderSingleClusterPolicySignals();
		renderAIAssessment();
		renderKindSection();
		renderChangesPreview();
	}

	function titleForStatus() {
		if (data.summary.total === 0) return "No Manifest Changes";
		if (data.policies.policyFailed || data.meta.status === "error")
			return "Review Required";
		return "Manifest Changes Detected";
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
		app.append(el("h2", { text: "Classifications" }));
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
				text: item.id || item.ID || "classification",
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

	function renderResources(params) {
		if (params) {
			filters.query = resourceQueryFromParams(params);
			if (!params.get("query") && filters.query) syncResourceFiltersToHash();
		}
		app.replaceChildren();
		app.append(
			el("div", { class: "eyebrow", text: "Resource Browser" }),
			el("h1", { text: "Changed Resources" }),
			filterBar(),
		);
		renderFilteredList();
	}

	function renderFilteredList() {
		let list = app.querySelector(".resource-list");
		if (list) {
			list.classList.add("no-animate");
			list.replaceChildren();
		}
		else {
			list = el("div", { class: "resource-list" });
			app.append(list);
		}
		const rows = filteredResources();
		for (const res of rows) list.append(resourceCard(res));
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
	}

	function filterBar() {
		const bar = el("div", { class: "filterbar" });
		const search = el("input", {
			id: "filter-query",
			name: "query",
			"aria-label": "Search resources",
			"aria-autocomplete": "list",
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
			}, 120);
		});
		search.addEventListener("keydown", (event) => {
			handleSuggestionKeydown(event, search, suggestions);
		});
		const clear = actionButton("Clear filters", () => {
			clearFilters();
			renderResources(new URLSearchParams());
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
		return bar;
	}

	function hasActiveFilters() {
		return filters.query.trim() !== "";
	}

	function clearFilters() {
		filters.query = "";
		syncResourceFiltersToHash();
	}

	function syncResourceFiltersToHash() {
		const params = new URLSearchParams();
		if (filters.query.trim()) params.set("query", filters.query.trim());
		const query = params.toString();
		const next = `#resources${query ? "?" + query : ""}`;
		if (location.hash !== next) history.replaceState(null, "", next);
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
		for (const [index, suggestion] of suggestions.entries()) {
			const option = el("button", {
				class: "query-suggestion",
				role: "option",
				type: "button",
				"data-index": String(index),
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
		}
	}

	function markActiveSuggestion(options) {
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
			{ class: "resource-card", href: `#resource/${res.index}`, tabindex: "0" },
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
						el("span", { class: `pill ${res.action}`, text: res.action }),
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
		url.hash = `resource/${index}?view=${view}`;
		return url.href;
	}

	function copyText(button, text, resetText) {
		writeClipboard(text).then(() => {
			button.textContent = "Copied";
			button.classList.add("copied");
			setTimeout(() => {
				button.textContent = resetText;
				button.classList.remove("copied");
			}, 1500);
		});
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
			document.execCommand("copy");
			return Promise.resolve();
		} finally {
			textarea.remove();
		}
	}

	function renderResource(index, view) {
		const res = data.resources[index];
		if (!res) {
			app.append(
				el("div", { class: "empty-state", text: "Resource not found." }),
			);
			return;
		}
		app.append(
			el("div", { class: "breadcrumb" }, [
				el("a", { href: "#resources", text: "Resources" }),
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
		const previous =
			index > 0
				? el("a", { href: `#resource/${index - 1}`, text: "Previous" })
				: el("span", { text: "Previous", "aria-disabled": "true" });
		const next =
			index < data.resources.length - 1
				? el("a", { href: `#resource/${index + 1}`, text: "Next" })
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
				text: mode === "split" ? "Split" : "Unified",
				type: "button",
			});
			button.addEventListener("click", () => {
				location.hash = `resource/${index}?view=${mode}`;
			});
			toggle.append(button);
		}
		return toggle;
	}

	function diffView(res, view) {
		const wrap = el("div", { class: `diff ${view}` });
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
				text: row.oldText || "",
			}),
			el("div", {
				class: "cell line-no",
				text: row.newLine ? String(row.newLine) : "",
			}),
			el("div", {
				class: `cell ${row.newLine ? "" : "empty"}`,
				text: row.newText || "",
			}),
		]);
	}

	function handleKeydown(e) {
		if (["INPUT", "SELECT", "TEXTAREA", "BUTTON"].includes(e.target.tagName)) return;
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
			location.hash = "#resources";
		}
	}

	window.addEventListener("hashchange", render);
	window.addEventListener("keydown", handleKeydown);
	render();
})();
