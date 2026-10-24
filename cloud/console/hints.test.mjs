// The editor's function hints, under Node: node hints.test.mjs
import assert from "node:assert/strict";
import { test } from "node:test";
import { FUNCTIONS, byName, signatureParts } from "./src/functions.js";
import { openCall } from "./src/sqltext.js";

// at is the call at the cursor ¦: only the text before it is read.
const at = (marked) => openCall(marked.slice(0, marked.indexOf("¦")));
const call = (name, arg) => ({ name, arg });

test("the call and argument at the cursor", () => {
	assert.deepEqual(at("round(x, ¦"), call("round", 1));
	assert.deepEqual(at("SELECT round(x)+abs(¦"), call("abs", 0));
	assert.deepEqual(at("ROUND(x, ¦"), call("round", 1), "names are lower-cased");
	assert.deepEqual(at("round (x, ¦"), call("round", 1), "a space before the parenthesis");
	assert.equal(at("¦"), null);
	assert.equal(at("SELECT 1, ¦"), null);
});

test("right after ( and before )", () => {
	assert.deepEqual(at("abs(¦)"), call("abs", 0));
	assert.deepEqual(at("abs(¦"), call("abs", 0));
	assert.deepEqual(at("max(¦, b)"), call("max", 0));
	assert.deepEqual(at("max(a¦)"), call("max", 0));
	assert.deepEqual(at("max(a, ¦)"), call("max", 1));
	assert.deepEqual(at("max(a,¦)"), call("max", 1));
	assert.equal(at("abs(x)¦"), null, "right after the closing parenthesis the call is over");
	assert.equal(at("abs(x) ¦"), null);
});

test("nested calls", () => {
	assert.deepEqual(at("max(a, min(b, c), ¦)"), call("max", 2));
	assert.deepEqual(at("max(a, min(b, c)¦)"), call("max", 1));
	assert.deepEqual(at("max(a, min(b, ¦"), call("min", 1));
	assert.deepEqual(at("max(a, min(¦"), call("min", 0));
	assert.deepEqual(at("max(a, min(b, abs(c)), ¦"), call("max", 2));
	assert.deepEqual(at("max(min(a, b), min(c, ¦"), call("min", 1));
	assert.deepEqual(at("abs(x)) max(¦"), call("max", 0), "an extra ) closes nothing");
});

test("the innermost parenthesis owns the cursor, call or not", () => {
	assert.equal(at("max(a, (b, ¦"), null);
	assert.equal(at("max(a, (b + ¦"), null);
	assert.deepEqual(at("SELECT x FROM t WHERE x IN (1, ¦"), call("in", 1));
	assert.deepEqual(at("max(a, (b, c), ¦"), call("max", 2), "a closed group is just an argument");
});

test("strings hide their commas and parentheses", () => {
	assert.deepEqual(at("replace(x, ',', ¦"), call("replace", 2));
	assert.deepEqual(at("replace(x, ')', ¦"), call("replace", 2));
	assert.deepEqual(at("replace(x, '(', ¦"), call("replace", 2));
	assert.deepEqual(at("replace('a,b,c', ',', ¦"), call("replace", 2));
	assert.deepEqual(at("replace('it''s, (here', ¦"), call("replace", 1), "a doubled quote stays in the string");
	assert.deepEqual(at("replace('''', ¦"), call("replace", 1));
	assert.deepEqual(at("replace('--', '/*', ¦"), call("replace", 2), "comment marks in strings are text");
});

test("a cursor inside a string or a quoted name is still in its call", () => {
	assert.deepEqual(at("strftime('%Y,¦"), call("strftime", 0));
	assert.deepEqual(at("strftime('%Y-%m', 'now', 'start,¦"), call("strftime", 2));
	assert.deepEqual(at("max('a''¦"), call("max", 0));
	assert.deepEqual(at('max("a,¦'), call("max", 0));
	assert.deepEqual(at("max([a,¦"), call("max", 0));
});

test("quoted names hide their commas and parentheses", () => {
	assert.deepEqual(at('max("a,b", ¦'), call("max", 1));
	assert.deepEqual(at('max("a)", ¦'), call("max", 1));
	assert.deepEqual(at('max("a""b,c", ¦'), call("max", 1), "a doubled quote stays in the name");
	assert.deepEqual(at("max(`a,b`, ¦"), call("max", 1));
	assert.deepEqual(at("max(`a)`, ¦"), call("max", 1));
	assert.deepEqual(at("max([a,b], ¦"), call("max", 1));
	assert.deepEqual(at("max([a)], ¦"), call("max", 1));
	assert.equal(at('"max"(¦'), null, "a quoted name is not read as a function name");
});

test("line comments", () => {
	assert.deepEqual(at("max(a, -- x, (y\n b, ¦"), call("max", 2));
	assert.deepEqual(at("max(a, -- it's\n b, ¦"), call("max", 2), "a quote in a comment is not a string");
	assert.deepEqual(at("max(a, -- x)\n¦"), call("max", 1));
	assert.deepEqual(at("max(a, -- ) (\r\n¦"), call("max", 1));
	assert.equal(at("max(a, -- x, ¦"), null, "the cursor is inside the comment");
	assert.equal(at("max(a, --¦"), null);
	assert.deepEqual(at("max(a - -1, ¦"), call("max", 1), "a minus and a minus are not a comment");
});

test("block comments", () => {
	assert.deepEqual(at("max(a, /* x, y) */ b, ¦"), call("max", 2));
	assert.deepEqual(at("max(a, /* ( */ ¦"), call("max", 1));
	assert.deepEqual(at("max(a, /* x,\n y,\n z) */\n b, ¦"), call("max", 2));
	assert.deepEqual(at("max(a, /* -- */ b, ¦"), call("max", 2), "-- inside a block comment is nothing");
	assert.deepEqual(at("max(a, /* it's */ b, ¦"), call("max", 2));
	assert.deepEqual(at("max(a, /**/ b, ¦"), call("max", 2));
	assert.deepEqual(at("max(a /* ( */, ¦"), call("max", 1));
	assert.deepEqual(at("max /* c */ (a, ¦"), call("max", 1), "a comment between the name and the parenthesis");
	assert.equal(at("max(a, /* x, ¦"), null, "the cursor is inside the comment");
	assert.equal(at("max(a, /*¦"), null);
	assert.deepEqual(at("max(a, */ b, ¦"), call("max", 2), "a stray */ is nothing");
});

test("a semicolon ends the statement", () => {
	assert.equal(at("SELECT max(a; SELECT ¦"), null);
	assert.deepEqual(at("select abs(1); select min(¦"), call("min", 0));
	assert.deepEqual(at("select ';' || max(¦"), call("max", 0), "a semicolon in a string is text");
});

test("names", () => {
	assert.deepEqual(at("my_func2(a, ¦"), call("my_func2", 1));
	assert.deepEqual(at("größe(a, ¦"), call("größe", 1));
	assert.equal(at("1 + (¦"), null);
	assert.equal(at("(¦"), null);
	assert.equal(at("x.(¦"), null);
});

test("a long text is read in one pass", () => {
	const script = "SELECT 'a,b', 1, ".repeat(20000);
	assert.deepEqual(openCall(`${script}max(1, 2, `), call("max", 2));
});

// the active pieces of a signature, and the text they make
const active = (parts) => parts.filter((p) => p.active).map((p) => p.text);
const text = (parts) => parts.map((p) => p.text).join("");

test("the active argument of a signature", () => {
	assert.deepEqual(active(signatureParts("X, Y, Z", 0)), ["X"]);
	assert.deepEqual(active(signatureParts("X, Y, Z", 1)), ["Y"]);
	assert.deepEqual(active(signatureParts("X, Y, Z", 2)), ["Z"]);
	assert.deepEqual(active(signatureParts("X, Y, Z", 3)), [], "past the last argument of a fixed list");
	assert.deepEqual(active(signatureParts("X | *", 0)), ["X | *"]);
	assert.deepEqual(signatureParts("", 0), []);
});

test("brackets are never highlighted with an argument", () => {
	assert.deepEqual(active(signatureParts("X [, Y]", 0)), ["X"]);
	assert.deepEqual(active(signatureParts("X [, Y]", 1)), ["Y"]);
	assert.deepEqual(active(signatureParts("X, START [, LEN]", 1)), ["START"]);
	assert.deepEqual(active(signatureParts("X, START [, LEN]", 2)), ["LEN"]);
	assert.deepEqual(active(signatureParts("X [, OFFSET [, DEFAULT]]", 1)), ["OFFSET"]);
	assert.deepEqual(active(signatureParts("X [, OFFSET [, DEFAULT]]", 2)), ["DEFAULT"]);
	assert.deepEqual(active(signatureParts("[B,] X", 0)), ["B"]);
	assert.deepEqual(active(signatureParts("[B,] X", 1)), ["X"]);
	assert.equal(text(signatureParts("X [, OFFSET [, DEFAULT]]", 2)), "X [, OFFSET [, DEFAULT]]");
});

test("an argument ending in … stands for the rest", () => {
	assert.deepEqual(active(signatureParts("X, Y, …", 0)), ["X"]);
	assert.deepEqual(active(signatureParts("X, Y, …", 1)), ["Y"]);
	assert.deepEqual(active(signatureParts("X, Y, …", 2)), ["…"]);
	assert.deepEqual(active(signatureParts("X, Y, …", 9)), ["…"]);
	assert.deepEqual(active(signatureParts("COL [, 'metric=cos' …]", 1)), ["'metric=cos' …"]);
	assert.deepEqual(active(signatureParts("COL [, 'metric=cos' …]", 4)), ["'metric=cos' …"]);
	assert.deepEqual(active(signatureParts("FTS_TABLE [, WEIGHT, …]", 3)), ["…"]);
});

test("every function's signature marks one argument, and only a name", () => {
	for (const f of FUNCTIONS) {
		const fixed = signatureParts(f.args, 0).filter((p) => "active" in p);
		const variadic = fixed.some((p) => p.text.endsWith("…"));
		for (let arg = 0; arg < 8; arg++) {
			const parts = signatureParts(f.args, arg);
			assert.equal(text(parts), f.args, `${f.name}: the pieces make the list`);
			const marked = active(parts);
			assert.equal(marked.length, arg < fixed.length || variadic ? 1 : 0, `${f.name} at ${arg}`);
			for (const m of marked) assert.ok(!/[,[\]]/.test(m) && m === m.trim(), `${f.name}: ${m}`);
		}
	}
	assert.equal(byName.get("lag").args, "X [, OFFSET [, DEFAULT]]");
});
