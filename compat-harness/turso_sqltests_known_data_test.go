package compat

// Generated from a whole-corpus run of TestTursoSqltests; edit by re-running the gate,
// not by hand. Every entry is a divergence from C SQLite 3.53.3 that is understood and
// deferred; see tsKnownEntry.

func init() {
	tsKnownGroup("2026-10-14: CTE corners: INSERT ... RETURNING (SELECT ... FROM cte) is declined (this parser merges a leading WITH into the INSERT's SELECT, so the RETURNING scope cannot be told apart)",
		"cte.sqltest::memory::cte-insert-returning-multi-subquery",
		"cte.sqltest::memory::cte-insert-returning-subquery",
		"cte.sqltest::memory::cte-insert-values-and-returning-subquery",
	)
	tsKnownGroup("2026-10-12: GROUP BY without FROM combined with HAVING is declined",
		"aggregate-filter-in-subquery-grouped.sqltest::memory::having-filter-in-subquery-no-from",
		"aggregate-filter-in-subquery-grouped.sqltest::memory::having-filter-in-subquery-no-from-match",
	)
	tsKnownGroup("2026-10-12: CHECK diagnostics: wording for aggregates, bound parameters and quoted expression names (C echoes the token), ALTER ADD COLUMN wrappers, and an INSERT ... SELECT rowid check that musql reports where C stores",
		"check_constraint.sqltest::memory::check_constraint_insert_select_validates_destination_rowid",
		"check_constraint.sqltest::memory::check_constraint_aggregate_function",
		"check_constraint.sqltest::memory::check_constraint_aggregate_function_sum",
		"check_constraint.sqltest::memory::check_constraint_alter_add_column_aggregate",
		"check_constraint.sqltest::memory::check_constraint_alter_add_column_nonexistent_column",
		"check_constraint.sqltest::memory::check_constraint_bind_parameter",
		"check_constraint.sqltest::memory::check_constraint_drop_column_qualified_ref",
		"check_constraint.sqltest::memory::check_constraint_error_dequotes_leading_quoted_token",
		"check_constraint.sqltest::memory::check_constraint_rename_column_to_keyword_violation",
		"check_constraint.sqltest::memory::check_constraint_rename_table_qualified_ref_still_enforced",
	)
	tsKnownGroup("2026-10-12: generated columns: GLOB in a generated expression is declined, ALTER/CHECK/DEFAULT diagnostics are worded differently, window-function misuse text, and the oracle's driver returns a REAL generated column as an integer",
		"gencol.sqltest::memory::gencol_glob_in_expression",
		"gencol.sqltest::memory::gencol_glob_pattern_column",
		"gencol.sqltest::memory::gencol_alter_add_column_default_after_generated_rejected",
		"gencol.sqltest::memory::gencol_alter_add_column_generated_default_rejected",
		"gencol.sqltest::memory::gencol_alter_add_column_generated_pk_rejected",
		"gencol.sqltest::memory::gencol_alter_add_current_timestamp_rejected",
		"gencol.sqltest::memory::gencol_alter_add_virtual_random_error",
		"gencol.sqltest::memory::gencol_error_nested_window_function",
		"gencol.sqltest::memory::gencol_error_nondeterministic_datetime",
		"gencol.sqltest::memory::gencol_error_nondeterministic_strftime",
		"gencol.sqltest::memory::gencol_error_nondeterministic_unixepoch",
		"gencol.sqltest::memory::gencol_error_window_function",
		"gencol.sqltest::memory::gencol_issue7058_alter_add_default_on_gencol",
		"gencol.sqltest::memory::gencol_issue7072_upsert_expression_does_not_match_gencol_index",
	)
	tsKnownGroup("2026-10-12: INSERT corners: an AUTOINCREMENT-less rowid overflow picks random rowids (musql reports a unique violation), STRICT multi-statement text, and PRIMARY KEY conflict wording",
		"insert.sqltest::memory::rowid-overflow-random-generation",
		"insert.sqltest::memory::strict-int-vs-integer-pk",
		"insert.sqltest::memory::strict-update-pk-constraint",
	)
	tsKnownGroup("2026-10-12: PRAGMA surface: max_page_count has no meaning on this format (declined by design), pragma_* table-valued functions for pragmas musql does not expose as functions, page_size / wal_checkpoint / user_version on an attached or temp database, and pragma_table_list rows",
		"integrity_check/memory.sqltest::memory::integrity-check-qualified-empty-attached-database",
		"pragma/memory.sqltest::memory::pragma-function-update-journal-mode",
		"pragma/memory.sqltest::memory::pragma-max-page-count-clamping-with-data",
		"pragma/memory.sqltest::memory::pragma-max-page-count-default",
		"pragma/memory.sqltest::memory::pragma-max-page-count-enforcement-error",
		"pragma/memory.sqltest::memory::pragma-max-page-count-set-large",
		"pragma/memory.sqltest::memory::pragma-max-page-count-set-zero-ignored",
		"pragma/page_size.sqltest::memory::pragma-attached-page-size-independent-of-main",
		"pragma/synchronous.sqltest::memory::pragma-synchronous-tvf-schema-hidden-column-reads-back-null",
		"pragma/synchronous.sqltest::memory::pragma-synchronous-tvf-schema-qualified",
		"pragma/synchronous.sqltest::memory::pragma-synchronous-tvf-unqualified-defaults-to-main",
		"sql-error-message-prefix.sqltest::memory::database-full-error-has-no-prefix",
		"temp_tables.sqltest::memory::pragma-temp-page-size-independent-of-main",
		"temp_tables.sqltest::memory::pragma-temp-wal-checkpoint-without-temp-db-matches-sqlite",
		"pragma/synchronous.sqltest::memory::pragma-synchronous-tvf-too-many-arguments",
	)
	tsKnownGroup("2026-10-12: NOT BETWEEN with a subquery bound and an ORDER BY/OFFSET inside is declined",
		"not_between.sqltest::memory::not-between-subquery-bound-orderby-offset",
	)
	tsKnownGroup("2026-10-12: ORDER BY terms holding a multi-column IN subquery or a whole-table aggregate expression are declined",
		"orderby/memory.sqltest::memory::orderby_rowid_truncation",
		"orderby/memory.sqltest::memory::orderby_rowid_truncation_explicit_rowid",
		"orderby/memory.sqltest::memory::orderby_single_row_aggregate_optimization",
		"orderby/memory.sqltest::memory::orderby_single_row_aggregate_with_data",
	)
	tsKnownGroup("2026-10-12: partial-index corners: INDEXED BY a partial index whose BETWEEN predicate C proves, and an UPSERT target with a qualified WHERE, are declined",
		"partial_idx.sqltest::memory::partial-index-indexed-by-between-predicate-can-prove-index",
		"partial_idx.sqltest::memory::upsert-partial-target-qualified-where-doupdate",
	)
	tsKnownGroup("2026-10-12: REINDEX on an attached database, an INDEXED BY a partial index, and index_xinfo for an expression index with an outer COLLATE",
		"reindex.sqltest::memory::reindex-attached-qualified-targets",
		"reindex.sqltest::memory::reindex-table-with-multiple-index-types",
	)
	tsKnownGroup("2026-10-12: INSERT ... SELECT into a STRICT BLOB column from an ANY column: C's transfer optimisation stores the value, musql checks the type",
		"strict.sqltest::memory::strict-insert-select-any-to-blob-rejects-xfer",
	)
	tsKnownGroup("2026-10-12: subquery corners: a correlated scalar subquery in GROUP BY over a join and exists() inside an aggregate are declined, and a row value in an aggregate argument or HAVING is worded differently",
		"subquery/memory.sqltest::memory::subquery-exists-false-in-aggregate",
		"subquery/memory.sqltest::memory::subquery-vector-in-aggregate-arg",
		"subquery/memory.sqltest::memory::subquery-vector-in-having",
	)
	tsKnownGroup("2026-10-12: triggers: RAISE shorthand, RETURNING and system-table diagnostics are worded differently, TEMP trigger firing order, triggers over attached databases, NEW references inside CTE subqueries",
		"temp_trigger.sqltest::memory::unqualified-temp-trigger-stays-with-attached-table",
		"trigger-before-insert-affinity.sqltest::memory::old-columns-have-no-affinity",
		"trigger.sqltest::memory::trigger-new-reference-in-subquery-cte",
		"trigger_attach_regression.sqltest::memory::drop-trigger-finds-attached-trigger-without-qualifier",
		"trigger_attach_regression.sqltest::memory::trigger-attached-db-cte-same-db",
		"trigger.sqltest::memory::raise-shorthand-normal-query",
		"trigger.sqltest::memory::trigger-cannot-create-on-system-table",
		"trigger.sqltest::memory::trigger-cannot-create-on-system-table-2",
		"trigger.sqltest::memory::trigger-raise-shorthand",
		"trigger.sqltest::memory::trigger-returning-delete-rejected",
		"trigger.sqltest::memory::trigger-returning-insert-rejected",
		"trigger.sqltest::memory::trigger-returning-update-rejected",
		"trigger_attach_regression.sqltest::memory::trigger-doubly-qualified-expr-runtime-error",
		"trigger_attach_regression.sqltest::memory::trigger-doubly-qualified-when-runtime-error",
	)
	tsKnownGroup("2026-10-12: correlated EXISTS whose inequality compares a json_extract() value is declined",
		"unnest-correlated.sqltest::memory::correlated-exists-inequality-expression-errors[exists][json-extract-i-json-value-o-limit-value][and-i-value-o-wanted]",
		"unnest-correlated.sqltest::memory::correlated-exists-inequality-expression-errors[exists][o-limit-value-json-extract-i-json-value][and-i-value-o-wanted]",
	)
	tsKnownGroup("2026-10-12: UPDATE: a PRIMARY KEY conflict is worded \"UNIQUE constraint failed: duplicate rowid N\" instead of naming the column, and column-list SET from a star or volatile subquery is declined",
		"update.sqltest::memory::row-values-from-subquery-star",
		"update.sqltest::memory::row-values-from-volatile-subquery-project-same-row",
		"update.sqltest::memory::row-values-from-subquery-star-width-mismatch",
		"update.sqltest::memory::update_primary_key_constraint_error",
		"update.sqltest::memory::update_primary_key_constraint_error_2",
		"update.sqltest::memory::update_primary_key_constraint_error_3",
	)
	tsKnownGroup("2026-10-12: UPSERT conflict targets that are expressions (ON CONFLICT(lower(x))) are declined",
		"upsert-expr-index.sqltest::memory::upsert-expr-index-basic",
		"upsert-expr-index.sqltest::memory::upsert-expr-index-binary-op",
		"upsert-expr-index.sqltest::memory::upsert-expr-index-do-nothing",
		"upsert-expr-index.sqltest::memory::upsert-expr-index-mixed-columns",
		"upsert-expr-index.sqltest::memory::upsert-expr-index-no-conflict",
	)
	tsKnownGroup("2026-10-12: views: a circular definition is reported through the VDBE-only wrapper, and pragma_table_info over views built from USING/NATURAL/CTE stars lists different columns or is declined",
		"views.sqltest::memory::view-mutual-circle-detection",
		"views.sqltest::memory::view-self-circle-detection",
	)
	tsKnownGroup("2026-10-12: an aggregate over an outer column inside a subquery is \"misuse of aggregate\" in C; musql reports a name-resolution decline",
		"aggregate-of-outer-column.sqltest::memory::outer-aggregate-in-a-table-function-argument-is-an-error",
		"aggregate-of-outer-column.sqltest::memory::outer-aggregate-in-a-where-subquery-is-an-error",
		"aggregate-of-outer-column.sqltest::memory::outer-aggregate-in-an-on-subquery-is-an-error",
	)
	tsKnownGroup("2026-10-12: \"table/view/trigger X already exists\" echoes the object name token as written, quotes included; musql prints the bare name",
		"already-exists-error-message.sqltest::memory::create-table-duplicate-backtick-quoted-name",
		"already-exists-error-message.sqltest::memory::create-table-duplicate-bracket-quoted-name",
		"already-exists-error-message.sqltest::memory::create-table-duplicate-double-quoted-name",
		"already-exists-error-message.sqltest::memory::create-table-duplicate-escaped-quote-name",
		"already-exists-error-message.sqltest::memory::create-trigger-duplicate-quoted-name",
		"already-exists-error-message.sqltest::memory::create-view-duplicate-quoted-name",
	)
	tsKnownGroup("2026-10-12: an ORDER BY term on a compound SELECT that matches no result column is declined instead of reported",
		"compound-select-orderby.sqltest::memory::compound-order-by-integer-beyond-i32-does-not-match",
		"compound-select-orderby.sqltest::memory::union-order-by-expr-no-match",
	)
	tsKnownGroup("2026-10-12: CREATE INDEX: a parenthesised string in the key list is a missing column in C, and the non-deterministic-function diagnostics are worded differently",
		"create_index.sqltest::memory::create-index-sqlite-source-id",
		"create_index.sqltest::memory::create-index-sqlite-version",
	)
	tsKnownGroup("2026-10-12: NATURAL JOIN with ON/USING diagnostic wording and RIGHT then FULL JOIN USING values",
		"join/memory.sqltest::memory::join-3-1",
		"join/memory.sqltest::memory::join-3-2",
	)
	tsKnownGroup("2026-10-12: RETURNING: a multi-column scalar subquery and an aggregate are worded through declines, and the function name is echoed in lowercase",
		"returning.sqltest::memory::delete-returning-multi-column-subquery",
		"returning.sqltest::memory::insert-returning-multi-column-subquery",
		"returning.sqltest::memory::update-returning-multi-column-subquery",
	)
	tsKnownGroup("2026-10-12: a row value under an aggregate result expression is declined instead of \"row value misused\"",
		"row-value-arity-mismatch-error.sqltest::memory::binary-op-row-value-arity-mismatch-error-message",
	)
	tsKnownGroup("2026-10-12: SELECT corners: a trailing -- comment is part of C's result column name, doubly qualified column errors are hidden behind LIMIT 0 planning, SELECT * with no FROM in a subquery",
		"select/default.sqltest::default-no-rowidalias::select-star-subquery",
		"select/default.sqltest::default::select-star-subquery",
	)
	tsKnownGroup("2026-10-12: SELECT corners: a double-quoted name in VALUES names the column in C, the self-join ambiguity names the schema-qualified column, and the bound-parameter count error wording",
		"select/memory.sqltest::memory::ambiguous-self-join",
		"select/memory.sqltest::memory::where-variable-predicate",
	)
	tsKnownGroup("2026-10-12: REGEXP is reported as \"no such function: regexp\" (lower case) where C echoes REGEXP",
		"simple-count-optimization.sqltest::memory::simple-count-regexp-invalid-pattern-correctness",
	)
	tsKnownGroup("2026-10-12: UPSERT: a partial-index target written with a qualified WHERE, NULLS FIRST in a target and invalid-target wording",
		"upsert.sqltest::memory::upsert-conflict-target-nulls-first-rejected",
		"upsert.sqltest::memory::upsert-invalid-qualified-lhs",
		"upsert.sqltest::memory::upsert-invalid-target-expression",
		"upsert.sqltest::memory::upsert-update-causes-second-unique-violation",
	)
	tsKnownGroup("2026-10-12: C 3.53.3 planner bug (Turso's own @skip-if sqlite documents it): GROUP BY sort elision swallows the outer ORDER BY direction when a CTE supplies pre-ordered rows; musql returns the ordered result",
		"subquery/cte_materialization_adversarial.sqltest::memory::sort-elision-group-by-opposite-dir",
	)
}
