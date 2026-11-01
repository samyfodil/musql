// Function hints for the SQL editor: completion of function names with their
// signatures (functions.js), and the signature of the call the cursor is in,
// its current argument marked, as a tooltip while typing.
import { StateField } from "@codemirror/state";
import { showTooltip } from "@codemirror/view";
import { SQLite } from "@codemirror/lang-sql";
import { FUNCTIONS, byName, signatureParts } from "./functions.js";
import { openCall } from "./sqltext.js";

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

// How far back from the cursor the text is read to find the call it is in.
const LOOKBACK = 100000;

// callAt finds the known function whose parentheses hold pos, and which of its
// arguments pos is in.
function callAt(doc, pos) {
	const call = openCall(doc.sliceString(Math.max(0, pos - LOOKBACK), pos));
	const f = call && byName.get(call.name);
	return f ? { f, arg: call.arg } : null;
}

function signature({ f, arg }) {
	const dom = document.createElement("div");
	dom.className = "fn-sig";
	dom.append(`${f.name}(`);
	for (const { text, active } of signatureParts(f.args, arg)) {
		if (!active) {
			dom.append(text);
			continue;
		}
		const s = document.createElement("span");
		s.className = "arg";
		s.textContent = text;
		dom.append(s);
	}
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
