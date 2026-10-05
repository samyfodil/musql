package compat

import "testing"

// This file tests that BEFORE triggers on the same table do not incorrectly
// affect the outer statement's row visibility.
func TestBeforeTriggerSelfMutateOtherRow(t *testing.T) {
	differ(t, "before-trigger-self-mutate-other-row", []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, x INTEGER)",
		"INSERT INTO t VALUES (1, 1), (2, 100)",
		"CREATE TRIGGER trg BEFORE DELETE ON t WHEN OLD.id = 1 BEGIN UPDATE t SET x = 5 WHERE id = 2; END",
		"DELETE FROM t WHERE x < 50",
		"SELECT * FROM t ORDER BY id",
	})
}

// TestBeforeTriggerSelfMutateWidensOuterWhere tests a variant where the trigger's mutation widens the outer WHERE.
func TestBeforeTriggerSelfMutateWidensOuterWhere(t *testing.T) {
	differ(t, "before-trigger-self-mutate-widens-outer-where", []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, x INTEGER)",
		"INSERT INTO t VALUES (1, 1), (2, 100)",
		"CREATE TRIGGER trg BEFORE DELETE ON t WHEN OLD.id = 1 BEGIN UPDATE t SET x = 1 WHERE id = 2; END",
		"DELETE FROM t WHERE x < 50",
		"SELECT * FROM t ORDER BY id",
	})
}
