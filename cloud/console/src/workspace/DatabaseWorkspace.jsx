// A cloud database in the editor, for an app that is its own shell:
// DatabaseProvider opens it over Hrana with a token it mints and renews, and
// DatabaseWorkspace is the editor the stock console shows for it.
import { Suspense, createContext, useContext, useEffect, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import { Globe, Keyboard, Network, Search, SquareTerminal, Table2 } from "lucide-react";
import { close, connect } from "../db.js";
import { api, cloudConnection, message, Role } from "../cloud/api.js";
import { useCloud } from "../cloud/useCloud.js";
import { dumpSQL } from "../dump.js";
import { Studio } from "../studio.js";
import { useTheme } from "../theme.jsx";
import { download } from "../values.js";
import { viewState } from "../view.js";
import { BusyBar, Spinner } from "../components/Busy.jsx";
import { MenuButton } from "../components/Menu.jsx";
import Palette from "../components/Palette.jsx";
import Shortcuts from "../components/Shortcuts.jsx";
import { useToast } from "../components/Toasts.jsx";
import { useWorkspace } from "./useWorkspace.js";
import { LazyDiagram, LazyQueryView, WorkspaceDialogs, WorkspaceEmpty, WorkspacePanes, WorkspaceSide, WorkspaceTabs } from "./parts.jsx";

const Db = createContext(null);

/**
 * DatabaseProvider opens org/database (one at a time on a page) for the
 * editor's parts under it: TableView, QueryView, Diagram, or a whole
 * DatabaseWorkspace. A viewer of the org gets a read-only token.
 */
export function DatabaseProvider({ org, database, children }) {
	const cloud = useCloud();
	const toast = useToast();
	const [, , dark] = useTheme();
	const ws = useWorkspace({ status: toast, dark });
	const [state, setState] = useState({ phase: "opening", error: null });
	const signedIn = !!cloud?.me;
	const readOnly = signedIn && cloud.roleIn(org) < Role.MEMBER;

	useEffect(() => {
		if (!signedIn) return;
		let live = true;
		setState({ phase: "opening", error: null });
		(async () => {
			try {
				const view = viewState(`${org}/${database}`);
				const { database: d } = await api.db.get({ org, name: database });
				const c = await cloudConnection(org, d.name, d.url, readOnly);
				if (!live) return;
				// A run the effect has left behind must not replace the
				// connection a newer run opened.
				if (!(await connect(c.url, c.token, c.renewal, () => live))) return;
				const s = await ws.loadSchema();
				if (!live) return;
				if (!ws.show(c.name, s, view)) ws.openFirst(s);
				setState({ phase: "open", error: null });
			} catch (e) {
				if (live) setState({ phase: "error", error: message(e) });
			}
		})();
		return () => {
			live = false;
		};
	}, [org, database, signedIn, readOnly]);
	useEffect(() => () => close(), []);

	return (
		<Db.Provider value={{ ws, org, database, readOnly, ...state }}>
			<Studio.Provider value={ws.ctx}>
				{children}
				<WorkspaceDialogs ws={ws} />
				{ws.ctxMenu}
			</Studio.Provider>
		</Db.Provider>
	);
}

/**
 * useDatabase is the database the DatabaseProvider above has open: org,
 * database, phase ("opening" | "open" | "error") and error, readOnly, schema
 * and counts, reload(), and the workspace's own openTable(name, mode),
 * openQuery(sql, title), openDiagram() and section/setSection.
 */
export function useDatabase() {
	const db = useContext(Db);
	if (!db) return null;
	const { ws } = db;
	return { org: db.org, database: db.database, phase: db.phase, error: db.error, readOnly: db.readOnly, schema: ws.schema, counts: ws.counts, reload: ws.loadSchema, openTable: ws.goTable, openQuery: ws.openQuery, openDiagram: ws.openDiagram, section: ws.section, setSection: ws.setSection };
}

/** QueryView is the SQL editor (CodeMirror, loaded with the first one): initial is its text; AI SQL asks about the open database. */
export function QueryView(props) {
	const db = useContext(Db);
	return (
		<Suspense fallback={<Spinner label="Loading the editor…" />}>
			<LazyQueryView ask={db ? { org: db.org, database: db.database } : null} {...props} />
		</Suspense>
	);
}

/** Diagram is the schema diagram; onOpen(table) runs on opening a table from it. */
export function Diagram(props) {
	const db = useContext(Db);
	return (
		<Suspense fallback={<Spinner label="Loading the diagram…" />}>
			<LazyDiagram onOpen={(n) => db?.ws.goTable(n)} {...props} />
		</Suspense>
	);
}

const SECTIONS = [
	["tables", <Table2 size={18} />, "Table editor"],
	["sql", <SquareTerminal size={18} />, "SQL editor"],
	["diagram", <Network size={18} />, "Schema diagram"],
];

/**
 * DatabaseWorkspace is what the stock console shows for a cloud database: the
 * table list, table editor and grid, SQL editor with AI SQL, schema diagram,
 * and the row and column panels. It fills its container. Given controls, an
 * element of the host's (its header, say), the section and search buttons go
 * there instead of the rail on the left. title false drops the database's
 * name and menu above the table list, for a host that already shows which
 * database is open; its download is in the command palette too.
 */
export function DatabaseWorkspace({ org, database, controls, title = true }) {
	return (
		<DatabaseProvider org={org} database={database}>
			<Workspace controls={controls} title={title} />
		</DatabaseProvider>
	);
}

function Workspace({ controls, title }) {
	const { ws, org, database, phase, error } = useContext(Db);
	const cloud = useCloud();
	const [palette, setPalette] = useState(false);
	const [keys, setKeys] = useState(false);
	const [sideOpen, setSideOpen] = useState(false); // the sidebar, on a narrow screen

	useEffect(() => {
		const key = (e) => {
			if (e.key === "?" && !e.target.closest?.("input, textarea, select, .cm-editor, [contenteditable]")) {
				e.preventDefault();
				setKeys((k) => !k);
			} else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
				e.preventDefault();
				setPalette((p) => !p);
			}
		};
		addEventListener("keydown", key);
		return () => removeEventListener("keydown", key);
	}, []);

	const dump = async () => {
		try {
			download(`${database}.sql`, await dumpSQL(), "application/sql");
			ws.status(`Exported ${database}.sql`);
		} catch (e) {
			ws.status(`Export failed: ${e.message}`, true);
		}
	};

	const commands = useMemo(() => {
		const out = ws.commands();
		if (ws.name) out.push({ group: "Actions", label: "Download as SQL dump", run: dump, hint: ".sql", icon: "⇩" });
		out.push({ group: "Actions", label: "Keyboard shortcuts", run: () => setKeys(true), hint: "?", icon: "⌨" });
		return out;
	}, [ws.schema, ws.internal, ws.counts, ws.library, ws.name]);

	function goSection(sec) {
		// On a narrow screen the list is an overlay a rail button opens and closes.
		setSideOpen(matchMedia("(max-width: 760px)").matches && (sec !== ws.section || !sideOpen) && sec !== "diagram");
		ws.enter(sec);
	}

	const buttons = (
		<>
			{SECTIONS.map(([k, ic, label]) => (
				<button key={k} className={`railbtn${ws.section === k ? " on" : ""}`} title={label} disabled={!ws.name && k !== "tables"} onClick={() => goSection(k)}>
					{ic}
				</button>
			))}
			<span className="grow" />
			<button className="railbtn" title="Search everything (⌘K)" onClick={() => setPalette(true)}><Search size={17} /></button>
			<button className="railbtn" title="Keyboard shortcuts (?)" onClick={() => setKeys(true)}><Keyboard size={17} /></button>
		</>
	);

	return (
		<div className={`app workspace${sideOpen ? " sideopen" : ""}${controls ? " norail" : ""}`}>
			<BusyBar />
			{controls ? createPortal(<nav className="wsctl">{buttons}</nav>, controls) : <nav className="rail">{buttons}</nav>}
			<aside className="side">
				{title && <div className="dbswitch">
					<MenuButton
						className="dbbtn"
						label={<>
							<Globe size={14} className="dbkind" />
							<span className="dbn">{database}</span>
							<span className="savestate remote" title="Every change is written to the database as it is made.">{org}</span>
							<span className="dim">▾</span>
						</>}
						items={[{ label: "Download as SQL dump", hint: ".sql", disabled: !ws.name, onClick: dump }]}
					/>
				</div>}
				<WorkspaceSide ws={ws} onPick={() => setSideOpen(false)} />
			</aside>
			<main className="main">
				{ws.name && <WorkspaceTabs ws={ws} onPalette={() => setPalette(true)} />}
				{cloud?.auth.status === "out" && (
					<div className="sessionbar" role="alert">
						<b>Signed out</b>
						<span title={cloud.auth.error ?? undefined}>{database} stops working when its access expires.</span>
						<span className="grow" />
						<button className="primary" onClick={cloud.signIn}>Sign in again</button>
					</div>
				)}
				<WorkspacePanes
					ws={ws}
					ask={{ org, database }}
					empty={phase === "open" ? <WorkspaceEmpty ws={ws} onPalette={() => setPalette(true)} /> : phase === "error" ? (
						<>
							<h1>Could not open {database}</h1>
							<p className="err">{error}</p>
						</>
					) : <Spinner label={`Opening ${database}…`} />}
				/>
			</main>
			{palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
			{keys && <Shortcuts onClose={() => setKeys(false)} />}
		</div>
	);
}
