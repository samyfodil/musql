// A small music database for the Sample button: three tables with foreign
// keys, an index, a view, and enough rows (20,000 tracks) to feel the JIT.
export const SAMPLE = `
CREATE TABLE artists(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, country TEXT);
CREATE TABLE albums(id INTEGER PRIMARY KEY, artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE, title TEXT NOT NULL, year INTEGER);
CREATE TABLE tracks(id INTEGER PRIMARY KEY, album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE, title TEXT NOT NULL, seconds INTEGER, plays INTEGER DEFAULT 0, rating REAL);
CREATE INDEX tracks_album ON tracks(album_id);
CREATE VIEW album_stats AS
	SELECT al.title AS album, ar.name AS artist, count(*) AS tracks, sum(t.seconds) / 60 AS minutes, round(avg(t.rating), 2) AS rating
	FROM albums al JOIN artists ar ON ar.id = al.artist_id JOIN tracks t ON t.album_id = al.id
	GROUP BY al.id;
INSERT INTO artists(name, country) VALUES
	('Nina Simone', 'US'), ('Fela Kuti', 'NG'), ('Björk', 'IS'), ('Ryuichi Sakamoto', 'JP'),
	('Stromae', 'BE'), ('Rosalía', 'ES'), ('Air', 'FR'), ('Caetano Veloso', 'BR');
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200)
INSERT INTO albums(artist_id, title, year) SELECT 1 + i % 8, 'Album ' || i, 1965 + i % 58 FROM n;
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
INSERT INTO tracks(album_id, title, seconds, plays, rating)
	SELECT 1 + i % 200, 'Track ' || i, 120 + (i * 37) % 300, (i * 7919) % 100000, round(1 + ((i * 31) % 400) / 100.0, 1) FROM n;
`;
