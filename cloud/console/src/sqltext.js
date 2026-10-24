// Text-level helpers for the SQL a query tab runs.

/**
 * expandEmbeds replaces every embed('text') or embed('text', 'model') call in
 * sql with a ? parameter: {sql, calls: [{text, model}]}, calls in parameter
 * order. Strings, quoted names and comments are skipped, so an "embed(" inside
 * them is left alone. embed is the studio's, not the engine's: the studio
 * embeds the text (embed.js) and binds the vector.
 */
export function expandEmbeds(sql) {
	const calls = [];
	let out = "";
	let i = 0;
	const n = sql.length;
	const literal = (at) => {
		// a '...' literal at at (after spaces): [value, end] or null
		let j = at;
		while (j < n && /\s/.test(sql[j])) j++;
		if (sql[j] !== "'") return null;
		let v = "";
		for (j++; j < n; j++) {
			if (sql[j] === "'") {
				if (sql[j + 1] === "'") {
					v += "'";
					j++;
					continue;
				}
				return [v, j + 1];
			}
			v += sql[j];
		}
		return null;
	};
	while (i < n) {
		const c = sql[i];
		if (c === "'" || c === '"' || c === "`" || c === "[") {
			const close = c === "[" ? "]" : c;
			let j = i + 1;
			for (; j < n; j++) {
				if (sql[j] === close) {
					if (close !== "]" && sql[j + 1] === close) {
						j++;
						continue;
					}
					break;
				}
			}
			out += sql.slice(i, j + 1);
			i = j + 1;
			continue;
		}
		if (c === "-" && sql[i + 1] === "-") {
			const j = sql.indexOf("\n", i);
			const end = j < 0 ? n : j;
			out += sql.slice(i, end);
			i = end;
			continue;
		}
		if (c === "/" && sql[i + 1] === "*") {
			const j = sql.indexOf("*/", i + 2);
			const end = j < 0 ? n : j + 2;
			out += sql.slice(i, end);
			i = end;
			continue;
		}
		if (/[A-Za-z_]/.test(c)) {
			let j = i;
			while (j < n && /[A-Za-z0-9_$]/.test(sql[j])) j++;
			const word = sql.slice(i, j);
			const before = out.trimEnd().at(-1);
			if (word.toLowerCase() === "embed" && before !== ".") {
				let k = j;
				while (k < n && /\s/.test(sql[k])) k++;
				if (sql[k] === "(") {
					const a = literal(k + 1);
					if (a) {
						let [text, e] = a;
						let model;
						while (e < n && /\s/.test(sql[e])) e++;
						if (sql[e] === ",") {
							const b = literal(e + 1);
							if (b) {
								[model, e] = b;
								while (e < n && /\s/.test(sql[e])) e++;
							}
						}
						if (sql[e] === ")") {
							calls.push({ text, model });
							out += "?";
							i = e + 1;
							continue;
						}
					}
				}
			}
			out += word;
			i = j;
			continue;
		}
		out += c;
		i++;
	}
	return { sql: out, calls };
}

const isWord = (c) => (c >= "a" && c <= "z") || (c >= "A" && c <= "Z") || (c >= "0" && c <= "9") || c === "_" || c === "$" || c > "\x7f";

// quoteEnd is the index just past the quote that closes the one at i, or -1 if
// the text ends first. A doubled quote belongs to the string or name; a [name]
// has no such escape.
function quoteEnd(sql, i) {
	const close = sql[i] === "[" ? "]" : sql[i];
	for (let j = i + 1; j < sql.length; j++) {
		if (sql[j] !== close) continue;
		if (close !== "]" && sql[j + 1] === close) j++;
		else return j + 1;
	}
	return -1;
}

/**
 * openCall finds the call the end of sql is inside of: {name, arg}, the name
 * (lower-cased) before the innermost parenthesis still open, and which
 * argument is being written there, counted by the commas at that level. null
 * when that parenthesis is not a call. Strings, quoted names and comments are
 * skipped, so what they hold counts for nothing. The end of sql inside a
 * string or a quoted name is still inside its call; inside a comment, in none.
 */
export function openCall(sql) {
	const open = []; // per open parenthesis: {name, arg}
	let word = null; // the name just read, while only blanks and comments follow it
	const n = sql.length;
	let i = 0;
	while (i < n) {
		const c = sql[i];
		if (c === "'" || c === '"' || c === "`" || c === "[") {
			const end = quoteEnd(sql, i);
			if (end < 0) break;
			word = null;
			i = end;
		} else if (c === "-" && sql[i + 1] === "-") {
			const end = sql.indexOf("\n", i);
			if (end < 0) return null;
			i = end + 1;
		} else if (c === "/" && sql[i + 1] === "*") {
			const end = sql.indexOf("*/", i + 2);
			if (end < 0) return null;
			i = end + 2;
		} else if (isWord(c)) {
			let j = i + 1;
			while (j < n && isWord(sql[j])) j++;
			word = sql.slice(i, j);
			i = j;
		} else {
			if (c === "(") open.push({ name: word, arg: 0 });
			else if (c === ")") open.pop();
			else if (c === "," && open.length) open.at(-1).arg++;
			else if (c === ";") open.length = 0;
			if (!" \t\n\r\f".includes(c)) word = null;
			i++;
		}
	}
	const call = open.at(-1);
	return call?.name ? { name: call.name.toLowerCase(), arg: call.arg } : null;
}

/** A statement's first keyword, upper-cased, past comments and a leading WITH's CTEs not inspected. */
export function verb(sql) {
	const s = sql.replace(/^(\s|--[^\n]*\n|\/\*[\s\S]*?\*\/)*/, "");
	return (/^[A-Za-z]+/.exec(s)?.[0] ?? "").toUpperCase();
}
