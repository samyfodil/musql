// Function hints for the SQL editor: completion of function names with their
// signatures (functions.js), and the signature of the call the cursor is in,
// its current argument marked, as a tooltip while typing.
import { StateField } from "@codemirror/state";
import { showTooltip } from "@codemirror/view";
import { SQLite } from "@codemirror/lang-sql";
import { FUNCTIONS, byName } from "./functions.js";

const options = FUNCTIONS.map((f) => ({
	label: f.name,
	type: "function",
	detail: `(${f.args})`,
	info: `${f.about}  [${f.kind}]`,
	apply: f.name,
	boost: -1, // below the schema's own names
}));

function complete(ctx) {
	const w = ctx.matchBefore(/[A-Za-z_][\w]*/);
	if (!w || (w.from === w.to && !ctx.explicit)) return null;
	return { from: w.from, options, validFor: /^\w*$/ };
}

// callAt finds the call whose parentheses hold pos: the function's name and
// which argument pos is in. Strings, quoted names and comments are skipped;
// it looks back at most 2,000 characters.
function callAt(doc, pos) {
	const from = Math.max(0, pos - 2000);
	const text = doc.sliceString(from, pos);
	const stack = []; // per open paren: {at, commas}
	for (let i = 0; i < text.length; i++) {
		const c = text[i];
		if (c === "'" || c === '"' || c === "`") {
			const end = text.indexOf(c, i + 1);
			if (end < 0) return null;
			i = end;
		} else if (c === "-" && text[i + 1] === "-") {
			const end = text.indexOf("\n", i);
			if (end < 0) return null;
			i = end;
		} else if (c === "(") stack.push({ at: i, commas: 0 });
		else if (c === ")") stack.pop();
		else if (c === "," && stack.length) stack.at(-1).commas++;
		else if (c === ";") stack.length = 0;
	}
	const open = stack.at(-1);
	if (!open) return null;
	const name = /([A-Za-z_]\w*)\s*$/.exec(text.slice(0, open.at))?.[1]?.toLowerCase();
	const f = name && byName.get(name);
	return f ? { f, arg: open.commas } : null;
}

function signature({ f, arg }) {
	const dom = document.createElement("div");
	dom.className = "fn-sig";
	const args = f.args ? f.args.split(/,\s*/) : [];
	dom.append(`${f.name}(`);
	args.forEach((a, k) => {
		if (k) dom.append(", ");
		const s = document.createElement("span");
		// An argument list ending in … repeats its last pattern.
		if (k === Math.min(arg, args.length - 1) || (a === "…" && arg >= k)) s.className = "arg";
		s.textContent = a;
		dom.append(s);
	});
	dom.append(")");
	const about = document.createElement("span");
	about.className = "about";
	about.textContent = f.about;
	dom.append(about);
	return dom;
}

const sigField = StateField.define({
	create: () => null,
	update(_, tr) {
		const sel = tr.state.selection.main;
		if (!sel.empty) return null;
		const c = callAt(tr.state.doc, sel.head);
		return c ? { pos: sel.head, above: true, create: () => ({ dom: signature(c) }) } : null;
	},
	provide: (f) => showTooltip.from(f),
});

/** The editor extensions: function completion and the signature tooltip. */
export const functionHints = [SQLite.language.data.of({ autocomplete: complete }), sigField];
