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

/** A statement's first keyword, upper-cased, past comments and a leading WITH's CTEs not inspected. */
export function verb(sql) {
	const s = sql.replace(/^(\s|--[^\n]*\n|\/\*[\s\S]*?\*\/)*/, "");
	return (/^[A-Za-z]+/.exec(s)?.[0] ?? "").toUpperCase();
}
