// Regression pins for bare columns in aggregate queries over indexed tables.
// Four real mined shapes: some moved to parity gates, the last answered by a
// run-time anchor certificate.
package compat

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// assertStaysDeclined verifies the oracle answers while this engine declines.
func assertStaysDeclined(t *testing.T, ddl []string, query string) {
	t.Helper()
	cdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range ddl {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("oracle Exec(%s): %v", s, err)
		}
	}
	if rows, err := cdb.Query(query); err != nil {
		t.Fatalf("expected the real oracle to ANSWER this (the point of the decline), got %v", err)
	} else {
		rows.Close()
	}

	path := t.TempDir() + "/anchor_order_r16.sqlite"
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ddl {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine Exec(%s): %v", s, err)
		}
	}
	if err := edb.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.QueryArgs(query, nil); err == nil {
		t.Errorf("expected a clean decline, got an answer -- verify it against the real oracle and move this pin to a parity gate")
	}
}


// TestTransitiveMultiViewAggregateAnchorCertificate tests a real-world query from
// transitive1.test: a six-way JOIN of views and base tables. The run-time anchor
// certificate answers when rows of each group agree on bare columns, or declines when they differ.
func TestTransitiveMultiViewAggregateAnchorCertificate(t *testing.T) {
	ddl := []string{
			`CREATE TABLE episode ( idEpisode integer primary key, idFile integer, c12 varchar(24), c13 varchar(24), idShow integer)`,
			`CREATE TABLE tvshow ( idShow integer primary key, c00 text, c01 text, c05 text, c08 text, c13 text, c14 text, c16 text, c17 text)`,
			`CREATE TABLE files ( idFile integer primary key, idPath integer, strFilename text, playCount integer, lastPlayed text, dateAdded text)`,
			`CREATE TABLE path ( idPath integer primary key, strPath text, dateAdded text)`,
			`CREATE TABLE bookmark ( idFile integer, type integer, timeInSeconds integer, totalTimeInSeconds integer)`,
			`CREATE TABLE seasons ( idSeason integer primary key, idShow integer, season integer)`,
			`CREATE TABLE tvshowlinkpath (idShow integer, idPath integer)`,
			`CREATE INDEX ix_bookmark ON bookmark (idFile, type)`,
			`CREATE INDEX ix_path ON path ( strPath )`,
			`CREATE INDEX ix_files ON files ( idPath, strFilename )`,
			`CREATE UNIQUE INDEX ix_episode_file_1 on episode (idEpisode, idFile)`,
			`CREATE UNIQUE INDEX id_episode_file_2 on episode (idFile, idEpisode)`,
			`CREATE INDEX ix_episode_season_episode on episode (c12, c13)`,
			`CREATE INDEX ix_episode_show1 on episode(idEpisode,idShow)`,
			`CREATE INDEX ix_episode_show2 on episode(idShow,idEpisode)`,
			`CREATE UNIQUE INDEX ix_tvshowlinkpath_1 ON tvshowlinkpath ( idShow, idPath )`,
			`CREATE UNIQUE INDEX ix_tvshowlinkpath_2 ON tvshowlinkpath ( idPath, idShow )`,
			`CREATE INDEX ixTVShowBasePath on tvshow ( c17 )`,
			`CREATE INDEX ix_seasons ON seasons (idShow, season)`,
			`CREATE VIEW episodeview AS
    SELECT episode.*,
           files.strfilename           AS strFileName,
           path.strpath                AS strPath,
           files.playcount             AS playCount,
           files.lastplayed            AS lastPlayed,
           files.dateadded             AS dateAdded,
           tvshow.c00                  AS strTitle,
           tvshow.c14                  AS strStudio,
           tvshow.c05                  AS premiered,
           tvshow.c13                  AS mpaa,
           tvshow.c16                  AS strShowPath,
           bookmark.timeinseconds      AS resumeTimeInSeconds,
           bookmark.totaltimeinseconds AS totalTimeInSeconds,
           seasons.idseason            AS idSeason
    FROM   episode
           JOIN files ON files.idfile = episode.idfile
           JOIN tvshow ON tvshow.idshow = episode.idshow
           LEFT JOIN seasons ON seasons.idshow = episode.idshow
                     AND seasons.season = episode.c12
           JOIN path ON files.idpath = path.idpath
           LEFT JOIN bookmark ON bookmark.idfile = episode.idfile
                     AND bookmark.type = 1`,
			`CREATE VIEW tvshowview AS
    SELECT tvshow.*,
           path.strpath                              AS strPath,
           path.dateadded                            AS dateAdded,
           Max(files.lastplayed)                     AS lastPlayed,
           NULLIF(Count(episode.c12), 0)             AS totalCount,
           Count(files.playcount)                    AS watchedcount,
           NULLIF(Count(DISTINCT( episode.c12 )), 0) AS totalSeasons
    FROM   tvshow
           LEFT JOIN tvshowlinkpath ON tvshowlinkpath.idshow = tvshow.idshow
           LEFT JOIN path ON path.idpath = tvshowlinkpath.idpath
           LEFT JOIN episode ON episode.idshow = tvshow.idshow
           LEFT JOIN files ON files.idfile = episode.idfile
    GROUP  BY tvshow.idshow`,
	}
	const query = `SELECT
    episodeview.c12,
    path.strPath,
    tvshowview.c00,
    tvshowview.c01,
    tvshowview.c05,
    tvshowview.c08,
    tvshowview.c14,
    tvshowview.c13,
    seasons.idSeason,
    count(1),
    count(files.playCount)
  FROM episodeview
      JOIN tvshowview ON tvshowview.idShow = episodeview.idShow
      JOIN seasons ON (seasons.idShow = tvshowview.idShow
                       AND seasons.season = episodeview.c12)
      JOIN files ON files.idFile = episodeview.idFile
      JOIN tvshowlinkpath ON tvshowlinkpath.idShow = tvshowview.idShow
      JOIN path ON path.idPath = tvshowlinkpath.idPath
  WHERE tvshowview.idShow = 1
  GROUP BY episodeview.c12`
	oneRowPerGroup := []string{
		`INSERT INTO tvshow VALUES(1, 'The Big Bang Theory', 'Leonard and Sheldon', '2007-09-24', 'Comedy', 'TV-PG', 'CBS', '/tmp/tvshows/', '/tmp/tvshows/')`,
		`INSERT INTO path VALUES(1, '/tmp/tvshows/The.Big.Bang.Theory/', '2013-10-23')`,
		`INSERT INTO tvshowlinkpath VALUES(1, 1)`,
		`INSERT INTO files VALUES(1, 1, 'e1.avi', NULL, NULL, '2013-10-23')`,
		`INSERT INTO episode VALUES(1, 1, '1', '1', 1)`,
		`INSERT INTO seasons VALUES(3, 1, 1)`,
	}
	stmts := append(append(append([]string(nil), ddl...), oneRowPerGroup...), query)
	oracle := run(t, "cgo", stmts)
	if last := oracle[len(oracle)-1]; last["kind"] != "rows" || last["rows"] == nil {
		t.Fatalf("fixture drift: the oracle returned no rows for the mined query: %v", last)
	}
	differ(t, "one joined row per group", stmts)

	// Multi-path variant: anchor-dependent strPath must match or decline, never differ.
	multi := append(append(append([]string(nil), ddl...), oneRowPerGroup...),
		`INSERT INTO path VALUES(2, '/tmp/tvshows/alt/', '2013-10-24')`,
		`INSERT INTO tvshowlinkpath VALUES(1, 2)`,
		query)
	want := run(t, "cgo", multi)
	got := run(t, "musql", multi)
	switch g, w := got[len(got)-1], want[len(want)-1]; {
	case g["kind"] == "error":
		// The certificate's run-time decline.
	case fmt.Sprint(g) != fmt.Sprint(w):
		t.Errorf("rows of one group disagree on a bare column and musql answered differently from the oracle:\n  cgo:    %v\n  musql: %v", w, g)
	}
}
