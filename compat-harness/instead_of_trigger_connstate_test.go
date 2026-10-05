package compat

// changes() and last_insert_rowid() in an INSTEAD OF trigger's VALUES clause,
// including nested and multi-level trigger chains.
import "testing"

func TestInsteadOfTriggerConnState(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"single-instead-of-changes-and-lirid", []string{
			"CREATE TABLE u1(k INTEGER PRIMARY KEY)",
			"CREATE VIEW uv1 AS SELECT * FROM u1",
			"CREATE TRIGGER ur1 INSTEAD OF INSERT ON uv1 BEGIN INSERT INTO u1 VALUES(NEW.k*10); END",
			"INSERT INTO uv1 VALUES(3)",
			"SELECT last_insert_rowid(), changes()",
		}},
		{"nested-instead-of-lastinsert-7.1", []string{
			"drop table if exists t1", "drop table if exists t2",
			"create temp table t1 (k integer primary key)",
			"create temp table t2 (k integer primary key)",
			"create temp view v1 as select * from t1",
			"create temp view v2 as select * from t2",
			"create temp table rid (k integer primary key, rin, rout)",
			"insert into rid values (1, NULL, NULL)",
			"insert into rid values (2, NULL, NULL)",
			`create temp trigger r1 instead of insert on v1 for each row begin
				update rid set rin=last_insert_rowid() where k=1;
				insert into t1 values (100+NEW.k);
				insert into v2 values (100+last_insert_rowid());
				update rid set rout=last_insert_rowid() where k=1;
			end`,
			`create temp trigger r2 instead of insert on v2 for each row begin
				update rid set rin=last_insert_rowid() where k=2;
				insert into t2 values (1000+NEW.k);
				update rid set rout=last_insert_rowid() where k=2;
			end`,
			"insert into t1 values (77)",
			"select last_insert_rowid()",
		}},
		{"nested-instead-of-lastinsert-7.2", []string{
			"drop table if exists t1", "drop table if exists t2",
			"create temp table t1 (k integer primary key)",
			"create temp table t2 (k integer primary key)",
			"create temp view v1 as select * from t1",
			"create temp view v2 as select * from t2",
			"create temp table rid (k integer primary key, rin, rout)",
			"insert into rid values (1, NULL, NULL)",
			"insert into rid values (2, NULL, NULL)",
			`create temp trigger r1 instead of insert on v1 for each row begin
				update rid set rin=last_insert_rowid() where k=1;
				insert into t1 values (100+NEW.k);
				insert into v2 values (100+last_insert_rowid());
				update rid set rout=last_insert_rowid() where k=1;
			end`,
			`create temp trigger r2 instead of insert on v2 for each row begin
				update rid set rin=last_insert_rowid() where k=2;
				insert into t2 values (1000+NEW.k);
				update rid set rout=last_insert_rowid() where k=2;
			end`,
			"insert into t1 values (77)",
			"insert into v1 values (5)",
			"select last_insert_rowid()",
			"select rin, rout from rid",
		}},
		{"triple-nested-instead-of", []string{
			"CREATE TABLE w1(k INTEGER PRIMARY KEY)",
			"CREATE TABLE w2(k INTEGER PRIMARY KEY)",
			"CREATE TABLE w3(k INTEGER PRIMARY KEY)",
			"CREATE VIEW wv1 AS SELECT * FROM w1",
			"CREATE VIEW wv2 AS SELECT * FROM w2",
			"CREATE VIEW wv3 AS SELECT * FROM w3",
			"CREATE TRIGGER wr1 INSTEAD OF INSERT ON wv1 BEGIN INSERT INTO w1 VALUES(NEW.k); INSERT INTO wv2 VALUES(NEW.k+1); END",
			"CREATE TRIGGER wr2 INSTEAD OF INSERT ON wv2 BEGIN INSERT INTO w2 VALUES(NEW.k); INSERT INTO wv3 VALUES(last_insert_rowid()+1); END",
			"CREATE TRIGGER wr3 INSTEAD OF INSERT ON wv3 BEGIN INSERT INTO w3 VALUES(last_insert_rowid()); END",
			"INSERT INTO wv1 VALUES(1)",
			"SELECT * FROM w1",
			"SELECT * FROM w2",
			"SELECT * FROM w3",
			"SELECT last_insert_rowid()",
		}},
		{"instead-of-with-changes-mid-body", []string{
			"CREATE TABLE x1(k INTEGER PRIMARY KEY)",
			"CREATE TABLE x2(k INTEGER PRIMARY KEY)",
			"CREATE VIEW xv1 AS SELECT * FROM x1",
			"CREATE VIEW xv2 AS SELECT * FROM x2",
			"CREATE TRIGGER xr1 INSTEAD OF INSERT ON xv1 BEGIN INSERT INTO x1 VALUES(NEW.k); INSERT INTO xv2 VALUES(changes()+NEW.k); END",
			"CREATE TRIGGER xr2 INSTEAD OF INSERT ON xv2 BEGIN INSERT INTO x2 VALUES(NEW.k); END",
			"INSERT INTO xv1 VALUES(5)",
			"SELECT * FROM x2",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
