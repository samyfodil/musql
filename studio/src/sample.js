// A small database for the Sample button: a music library (three tables with
// foreign keys, an index, a view, and 20,000 tracks to feel the JIT) and a
// table of short notes on varied subjects, to try embedding and semantic search.
export const SAMPLE = `
CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT NOT NULL);
INSERT INTO notes(body) VALUES
	('The ocean absorbs about a quarter of the carbon dioxide humans emit.'),
	('Sourdough rises from wild yeast and lactic acid bacteria living in the starter.'),
	('A black hole''s event horizon is the boundary beyond which light cannot escape.'),
	('Espresso is brewed by forcing hot water through finely ground coffee under pressure.'),
	('The Rosetta Stone carried the same decree in hieroglyphic, Demotic and Greek scripts.'),
	('Octopuses have three hearts and blue, copper-based blood.'),
	('A sonnet has fourteen lines, often ending in a rhyming couplet.'),
	('Interest compounds when earnings are reinvested to earn more earnings.'),
	('Mount Everest grows a few millimetres each year as the Indian plate pushes north.'),
	('Jazz improvisation builds melodies over a song''s chord changes in real time.'),
	('Vaccines train the immune system to recognize a pathogen before an infection.'),
	('The Great Barrier Reef is the largest structure built by living organisms.'),
	('Photosynthesis turns sunlight, water and carbon dioxide into sugar and oxygen.'),
	('A binary search halves the remaining range at every step.'),
	('Bees communicate the direction of flowers with a waggle dance.'),
	('Tides are caused mostly by the Moon''s gravity pulling on the oceans.'),
	('Bread goes stale as its starch recrystallizes, not just by drying out.'),
	('The violin''s sound comes from a bow''s horsehair gripping and releasing the string.'),
	('Inflation erodes the purchasing power of cash held over time.'),
	('Penguins huddle together to survive the Antarctic winter.'),
	('A database index trades extra writes for much faster lookups.'),
	('Volcanic eruptions can cool the planet by filling the stratosphere with sulfate.'),
	('Marathon runners rely on stored glycogen and hit the wall when it runs out.'),
	('The printing press made books cheap enough to spread literacy across Europe.'),
	('Coral bleaching happens when warm water makes coral expel its algae.'),
	('Green tea and black tea come from the same plant, processed differently.'),
	('A haiku has three lines of five, seven and five syllables.'),
	('Stock prices react to expectations about future earnings more than past ones.'),
	('Whales sing songs that can travel hundreds of kilometres underwater.'),
	('Cast iron pans need seasoning, a layer of polymerized oil, to resist rust.'),
	('Neutron stars are so dense a teaspoon would weigh billions of tonnes.'),
	('Caching keeps recently used data close so it can be served without recomputing it.'),
	('Monarch butterflies migrate thousands of kilometres to overwinter in Mexico.'),
	('The Silk Road carried paper, gunpowder and ideas between China and the West.'),
	('Rain forests produce much of their own rainfall through evaporation from leaves.'),
	('Fermentation turns cabbage into sauerkraut and kimchi.'),
	('A haircut and a budget have something in common: both are easier to trim than to grow.'),
	('Diversifying a portfolio reduces the risk of any single investment.'),
	('Earthquakes release stress built up where tectonic plates lock together.'),
	('Chess engines search millions of positions per second to choose a move.');
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
