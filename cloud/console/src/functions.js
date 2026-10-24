// The SQL functions the editor's completion offers, with their signatures and
// one line each: SQLite's built-ins (as C SQLite 3.53 documents them) and
// musql's vector functions. [name, arguments, what it does, kind].

const LIST = [
	// core scalar
	["abs", "X", "The absolute value of X.", "scalar"],
	["changes", "", "Rows changed by the last INSERT, UPDATE or DELETE.", "scalar"],
	["char", "X1, X2, …", "The string of the given Unicode code points.", "scalar"],
	["coalesce", "X, Y, …", "The first argument that is not NULL.", "scalar"],
	["concat", "X, …", "The arguments' text joined; NULLs are skipped.", "scalar"],
	["concat_ws", "SEP, X, …", "The arguments joined with SEP between them; NULLs are skipped.", "scalar"],
	["format", "FORMAT, …", "printf-style formatting.", "scalar"],
	["glob", "PATTERN, X", "Whether X matches the case-sensitive glob PATTERN.", "scalar"],
	["hex", "X", "X as upper-case hexadecimal.", "scalar"],
	["ifnull", "X, Y", "X, or Y when X is NULL.", "scalar"],
	["iif", "COND, X, Y", "X when COND is true, else Y.", "scalar"],
	["if", "COND, X, Y", "Another name for iif.", "scalar"],
	["instr", "X, Y", "The 1-based position of Y in X, or 0.", "scalar"],
	["last_insert_rowid", "", "The rowid of the last row inserted on this connection.", "scalar"],
	["length", "X", "Characters in a string, bytes in a blob.", "scalar"],
	["like", "PATTERN, X [, ESCAPE]", "Whether X matches the LIKE PATTERN.", "scalar"],
	["likelihood", "X, P", "X, with a hint to the planner that it is true with probability P.", "scalar"],
	["likely", "X", "X, hinted to the planner as usually true.", "scalar"],
	["lower", "X", "X in lower case (ASCII).", "scalar"],
	["ltrim", "X [, Y]", "X without the characters of Y (spaces) on the left.", "scalar"],
	["max", "X, Y, …", "The largest argument (with one argument, the aggregate).", "scalar"],
	["min", "X, Y, …", "The smallest argument (with one argument, the aggregate).", "scalar"],
	["nullif", "X, Y", "NULL when X = Y, else X.", "scalar"],
	["octet_length", "X", "The bytes in X's text or blob.", "scalar"],
	["printf", "FORMAT, …", "Another name for format.", "scalar"],
	["quote", "X", "X as a SQL literal.", "scalar"],
	["random", "", "A random 64-bit integer.", "scalar"],
	["randomblob", "N", "N random bytes.", "scalar"],
	["replace", "X, Y, Z", "X with every Y replaced by Z.", "scalar"],
	["round", "X [, Y]", "X rounded to Y digits after the point.", "scalar"],
	["rtrim", "X [, Y]", "X without the characters of Y (spaces) on the right.", "scalar"],
	["sign", "X", "-1, 0 or 1 by X's sign; NULL when X is not a number.", "scalar"],
	["sqlite_version", "", "The SQLite version the engine is compatible with.", "scalar"],
	["substr", "X, START [, LEN]", "LEN characters of X from START (1-based).", "scalar"],
	["substring", "X, START [, LEN]", "Another name for substr.", "scalar"],
	["total_changes", "", "Rows changed since the connection opened.", "scalar"],
	["trim", "X [, Y]", "X without the characters of Y (spaces) at either end.", "scalar"],
	["typeof", "X", "X's storage class: null, integer, real, text or blob.", "scalar"],
	["subtype", "X", "X's subtype, an integer.", "scalar"],
	["unhex", "X [, Y]", "The blob X's hex digits spell, ignoring characters of Y.", "scalar"],
	["unicode", "X", "The code point of X's first character.", "scalar"],
	["unlikely", "X", "X, hinted to the planner as usually false.", "scalar"],
	["upper", "X", "X in upper case (ASCII).", "scalar"],
	["zeroblob", "N", "A blob of N zero bytes.", "scalar"],
	// aggregate
	["avg", "X", "The average of the non-NULL X.", "aggregate"],
	["count", "X | *", "Rows, or rows where X is not NULL.", "aggregate"],
	["group_concat", "X [, SEP]", "The X joined with SEP (a comma).", "aggregate"],
	["string_agg", "X, SEP", "The X joined with SEP.", "aggregate"],
	["sum", "X", "The sum of the non-NULL X; NULL for none.", "aggregate"],
	["total", "X", "The sum of the X as a real; 0.0 for none.", "aggregate"],
	// window
	["row_number", "", "The row's number in its partition, from 1.", "window"],
	["rank", "", "The row's rank, with gaps after ties.", "window"],
	["dense_rank", "", "The row's rank, without gaps.", "window"],
	["percent_rank", "", "(rank - 1) / (rows - 1).", "window"],
	["cume_dist", "", "The cumulative distribution of the row.", "window"],
	["ntile", "N", "The row's bucket of N.", "window"],
	["lag", "X [, OFFSET [, DEFAULT]]", "X from OFFSET rows before.", "window"],
	["lead", "X [, OFFSET [, DEFAULT]]", "X from OFFSET rows after.", "window"],
	["first_value", "X", "X of the frame's first row.", "window"],
	["last_value", "X", "X of the frame's last row.", "window"],
	["nth_value", "X, N", "X of the frame's N-th row.", "window"],
	// date and time
	["date", "TIME, MODIFIER, …", "The date, YYYY-MM-DD.", "date"],
	["time", "TIME, MODIFIER, …", "The time, HH:MM:SS.", "date"],
	["datetime", "TIME, MODIFIER, …", "The date and time, YYYY-MM-DD HH:MM:SS.", "date"],
	["julianday", "TIME, MODIFIER, …", "The Julian day number.", "date"],
	["unixepoch", "TIME, MODIFIER, …", "Seconds since 1970-01-01.", "date"],
	["strftime", "FORMAT, TIME, MODIFIER, …", "The time formatted by FORMAT.", "date"],
	["timediff", "A, B", "The time from B to A, as +YYYY-MM-DD HH:MM:SS.SSS.", "date"],
	["current_date", "", "Today, UTC (also without parentheses).", "date"],
	["current_time", "", "The time now, UTC.", "date"],
	["current_timestamp", "", "The date and time now, UTC.", "date"],
	// JSON
	["json", "X", "X as minified JSON text.", "json"],
	["jsonb", "X", "X as a JSONB blob.", "json"],
	["json_array", "X, …", "A JSON array of the arguments.", "json"],
	["json_array_length", "JSON [, PATH]", "The length of the array at PATH.", "json"],
	["json_error_position", "X", "Where X stops being valid JSON, or 0.", "json"],
	["json_extract", "JSON, PATH, …", "The value at PATH (SQL values; JSON for objects).", "json"],
	["json_insert", "JSON, PATH, VALUE, …", "JSON with VALUE set at PATH where nothing is.", "json"],
	["json_object", "LABEL, VALUE, …", "A JSON object of the pairs.", "json"],
	["json_patch", "TARGET, PATCH", "TARGET with the RFC 7396 merge patch applied.", "json"],
	["json_pretty", "JSON [, INDENT]", "JSON indented for reading.", "json"],
	["json_quote", "X", "X as a JSON value.", "json"],
	["json_remove", "JSON, PATH, …", "JSON without the values at the PATHs.", "json"],
	["json_replace", "JSON, PATH, VALUE, …", "JSON with VALUE set at PATH where something is.", "json"],
	["json_set", "JSON, PATH, VALUE, …", "JSON with VALUE set at PATH.", "json"],
	["json_type", "JSON [, PATH]", "The JSON type at PATH.", "json"],
	["json_valid", "X [, FLAGS]", "Whether X is valid JSON.", "json"],
	["json_group_array", "X", "Aggregate: a JSON array of the X.", "json"],
	["json_group_object", "NAME, VALUE", "Aggregate: a JSON object of the pairs.", "json"],
	["json_each", "JSON [, PATH]", "Table: one row per element at PATH.", "json"],
	["json_tree", "JSON [, PATH]", "Table: one row per element, recursively.", "json"],
	// math
	["acos", "X", "Arccosine, in radians.", "math"],
	["asin", "X", "Arcsine, in radians.", "math"],
	["atan", "X", "Arctangent, in radians.", "math"],
	["atan2", "Y, X", "The arctangent of Y/X, by quadrant.", "math"],
	["ceil", "X", "The smallest integer ≥ X.", "math"],
	["cos", "X", "Cosine of X radians.", "math"],
	["degrees", "X", "X radians in degrees.", "math"],
	["exp", "X", "e to the X.", "math"],
	["floor", "X", "The largest integer ≤ X.", "math"],
	["ln", "X", "Natural logarithm.", "math"],
	["log", "[B,] X", "Logarithm of X, base 10 or B.", "math"],
	["log2", "X", "Base-2 logarithm.", "math"],
	["mod", "X, Y", "The remainder of X / Y.", "math"],
	["pi", "", "π.", "math"],
	["pow", "X, Y", "X to the Y.", "math"],
	["radians", "X", "X degrees in radians.", "math"],
	["sin", "X", "Sine of X radians.", "math"],
	["sqrt", "X", "Square root.", "math"],
	["tan", "X", "Tangent of X radians.", "math"],
	["trunc", "X", "X rounded toward zero.", "math"],
	// FTS5
	["bm25", "FTS_TABLE [, WEIGHT, …]", "FTS5: the row's BM25 relevance (lower is better).", "fts5"],
	["highlight", "FTS_TABLE, COL, OPEN, CLOSE", "FTS5: the column's text with matches wrapped.", "fts5"],
	["snippet", "FTS_TABLE, COL, OPEN, CLOSE, ELLIPSIS, TOKENS", "FTS5: a short fragment around the matches.", "fts5"],
	["fts5_source_id", "", "FTS5's source id.", "fts5"],
	// musql vectors
	["vector32", "TEXT | BLOB", "A float32 vector from '[1, 2, 3]' text or a blob.", "vector"],
	["vector64", "TEXT | BLOB", "A float64 vector.", "vector"],
	["vector", "TEXT | BLOB", "Another name for vector32.", "vector"],
	["vector_extract", "VEC", "A vector blob as '[…]' text.", "vector"],
	["vector_distance_cos", "A, B", "Cosine distance, 0 (same direction) to 2.", "vector"],
	["vector_distance_l2", "A, B", "Euclidean distance.", "vector"],
	["vector_top_k", "INDEX, QUERY, K", "Table: the K rows of INDEX nearest QUERY (their rowids as id).", "vector"],
	["libsql_vector_idx", "COL [, 'metric=cos' …]", "In CREATE INDEX: makes a vector index on COL.", "vector"],
];

/** Every function: {name, args, about, kind}. */
export const FUNCTIONS = LIST.map(([name, args, about, kind]) => ({ name, args, about, kind }));

export const byName = new Map(FUNCTIONS.map((f) => [f.name, f]));

/**
 * signatureParts cuts an argument list as LIST writes it ("X [, Y]", "A, B, …")
 * into the pieces a tooltip shows, {text, active}, in order. The one for the
 * argument at index arg is active; commas, brackets and blanks are pieces of
 * their own and never are. An argument ending in … stands for all the rest.
 */
export function signatureParts(args, arg) {
	const parts = [];
	let k = 0;
	args.split(/([,[\]])/).forEach((piece, i) => {
		if (i % 2) return parts.push({ text: piece });
		const [, lead, core, tail] = /^(\s*)(.*?)(\s*)$/.exec(piece);
		if (lead) parts.push({ text: lead });
		if (core) {
			parts.push({ text: core, active: k === arg || (core.endsWith("…") && arg >= k) });
			k++;
		}
		if (tail) parts.push({ text: tail });
	});
	return parts;
}
