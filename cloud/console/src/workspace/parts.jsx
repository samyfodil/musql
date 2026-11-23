// The pieces of the editor around one open database (useWorkspace): the
// side list, the tab strip, the panes and the table dialogs. The studio and
// DatabaseWorkspace lay them out the same way.
import { Suspense, lazy } from "react";
import { Search } from "lucide-react";
import { fmt } from "../values.js";
import { unsave } from "../library.js";
import TableView from "../components/TableView.jsx";
import { Spinner } from "../components/Busy.jsx";
import { ImportDialog, TableDialog } from "../components/Dialogs.jsx";
import { ICON } from "./useWorkspace.js";

// The editor (CodeMirror) loads with the first query tab, not with the page.
export const LazyQueryView = lazy(() => import("../components/QueryView.jsx"));
export const LazyDiagram = lazy(() => import("../components/Diagram.jsx"));

const GROUP = { table: "Tables", view: "Views", index: "Indexes", trigger: "Triggers" };

/** WorkspaceSide is the side list: the SQL section's queries, or the tables. extra goes under the tables; onPick runs on picking one. */
export function WorkspaceSide({ ws, extra, onPick }) {
	const { name, schema, counts, section, current, library, internal, showInternal, filter } = ws;
	const f = filter.trim().toLowerCase();
	const hidden = showInternal ? 0 : schema.filter((o) => internal.has(o.name)).length;
	if (section === "sql") {
		return (
			<>
				<div className="sidehead">
					<h3>SQL editor</h3>
					<button className="ghost icon" title="New query" disabled={!name} onClick={() => ws.openQuery()}>＋</button>
				</div>
				<nav>
					<div className="group">
						<h4>Open</h4>
						{ws.tabs.filter((t) => t.kind === "query").map((t) => (
							<button key={t.id} className={`item${current?.id === t.id ? " active" : ""}`} onClick={() => ws.setActive(t.id)}>
								<span className="ic">{ICON.query}</span><span className="name">{t.title}</span>
							</button>
						))}
					</div>
					{library.length > 0 && (
						<div className="group">
							<h4>Saved</h4>
							{library.map((q) => (
								<div key={q.name} className="item saved">
									<button className="name" title={q.sql} onClick={() => ws.openQuery(q.sql, q.name)}>
										<span className="ic">★</span>{q.name}
									</button>
									<button className="x" title="Forget this saved query" onClick={() => confirm(`Forget the saved query “${q.name}”?`) && unsave(q.name)}>×</button>
								</div>
							))}
						</div>
					)}
				</nav>
			</>
		);
	}
	return (
		<>
			<div className="sidehead">
				<h3>Tables</h3>
				<button className="ghost icon" title="Create a table" disabled={!name} onClick={() => ws.setDialog("table")}>＋</button>
			</div>
			<input className="filter" type="search" placeholder="Search tables" spellCheck={false} value={filter} onChange={(e) => ws.setFilter(e.target.value)} />
			<nav onClick={(e) => e.target.closest("button.item") && onPick?.()}>
				{["table", "view", "index", "trigger"].map((type) => {
					const items = schema.filter((o) => o.type === type && (showInternal || !internal.has(o.name)) && (!f || o.name.toLowerCase().includes(f)));
					if (!items.length || ((type === "index" || type === "trigger") && !f && !showInternal)) return null;
					return (
						<div className="group" key={type}>
							{type !== "table" && <h4>{GROUP[type]}</h4>}
							{items.map((o) => {
								const own = type === "table" || type === "view";
								return (
									<button
										key={o.name}
										className={`item${current?.table === o.name && section === "tables" ? " active" : ""}`}
										title={o.sql ?? o.name}
										onClick={() => ws.goTable(own ? o.name : o.tbl_name, own ? "data" : "structure")}
										onContextMenu={(e) => ws.openCtx(e, ws.objectMenu(o))}
									>
										<span className="ic">{ICON[type]}</span>
										<span className="name">{o.name}</span>
										{counts[o.name] !== undefined && <span className="count">{fmt(counts[o.name])}</span>}
									</button>
								);
							})}
						</div>
					);
				})}
				{name && !schema.length && (
					<div className="sideempty">
						<p className="dim small">No tables yet.</p>
						<button className="primary" onClick={() => ws.setDialog("table")}>Create a table</button>
						<button onClick={() => ws.setDialog("import")}>Import a CSV</button>
					</div>
				)}
				{name && (hidden > 0 || showInternal || schema.some((o) => o.type === "index" || o.type === "trigger")) && (
					<button className="link small pad" onClick={() => ws.setShowInternal(!showInternal)}>
						{showInternal ? "Show less" : "Show indexes, triggers and internal tables"}
					</button>
				)}
				{extra}
			</nav>
		</>
	);
}

/** WorkspaceTabs is the tab strip of the section in view; extIcon(tab) draws an extension tab's icon. */
export function WorkspaceTabs({ ws, onPalette, extIcon }) {
	const { tabsShown, current, section, schema } = ws;
	return (
		<div className="tabs">
			{tabsShown.map((t) => (
				<div key={t.id} className={`tab${t.id === current?.id ? " active" : ""}`} onClick={() => ws.setActive(t.id)} onAuxClick={(e) => e.button === 1 && ws.closeTab(t.id)}>
					<span className="ic">{t.kind === "table" ? ICON[schema.find((o) => o.name === t.table)?.type ?? "table"] : t.kind === "ext" ? extIcon?.(t) : ICON[t.kind]}</span>
					{t.title}
					<button className="x" title="Close" onClick={(e) => { e.stopPropagation(); ws.closeTab(t.id); }}>×</button>
				</div>
			))}
			{section === "sql" && <button className="ghost icon newtab" title="New query" onClick={() => ws.openQuery()}>＋</button>}
			<span className="grow" />
			<button className="kbdbtn" onClick={onPalette}><Search size={13} /><span className="dim">Search</span> <span className="kbd">⌘K</span></button>
		</div>
	);
}

/** WorkspacePanes holds every open tab, the one in view shown, and empty when there is none. ask is the cloud database AI SQL asks about. */
export function WorkspacePanes({ ws, hidden, ask, renderExt, empty }) {
	const { tabs, current } = ws;
	return (
		<div className="panes" hidden={hidden}>
			{tabs.map((t) => (
				<div key={t.id} className="pane" hidden={t.id !== current?.id}>
					{t.kind === "table" ? (
						<TableView name={t.table} mode={t.mode} tabId={t.id} state={t.state} />
					) : t.kind === "ext" ? (
						renderExt?.(t)
					) : t.kind === "diagram" ? (
						<Suspense fallback={<Spinner label="Loading the diagram…" />}>
							<LazyDiagram onOpen={(n) => ws.goTable(n)} />
						</Suspense>
					) : (
						<Suspense fallback={<Spinner label="Loading the editor…" />}>
							<LazyQueryView initial={t.text} title={t.title} onTitle={(title) => ws.retitle(t.id, title)} ask={ask} />
						</Suspense>
					)}
				</div>
			))}
			{!current && <div className="empty">{empty}</div>}
		</div>
	);
}

/** WorkspaceEmpty is what an open database with no tab in view shows. */
export function WorkspaceEmpty({ ws, onPalette }) {
	const sql = ws.section === "sql";
	return (
		<>
			<h1>{sql ? "Write some SQL" : "Pick a table"}</h1>
			<p className="dim">{sql ? "Open a new query, or a saved one from the list." : "Choose one on the left, or create one."}</p>
			<div className="cta">
				{sql ? <button className="primary" onClick={() => ws.openQuery()}>New query</button> : <button className="primary" onClick={() => ws.setDialog("table")}>Create a table</button>}
				<button onClick={onPalette}>Search <span className="kbd">⌘K</span></button>
			</div>
		</>
	);
}

/** WorkspaceDialogs is the create, edit and import dialogs, when one is open. */
export function WorkspaceDialogs({ ws }) {
	const { dialog, schema, internal, status } = ws;
	return (
		<>
			{(dialog === "table" || dialog?.edit) && (
				<TableDialog
					table={dialog?.edit}
					tables={schema.filter((o) => o.type === "table" && !internal.has(o.name)).map((o) => o.name)}
					status={status}
					onSQL={(sql) => { ws.setSection("sql"); ws.openQuery(sql, dialog?.edit ? `Edit ${dialog.edit}` : "New table"); }}
					onClose={(n) => {
						const was = dialog?.edit;
						ws.setDialog(null);
						if (n) {
							ws.changed();
							ws.goTable(n, "data");
							status(was ? `Saved ${n}.` : `Created ${n}.`);
						}
					}}
				/>
			)}
			{(dialog === "import" || dialog?.import) && (
				<ImportDialog file={dialog?.import} status={status} tables={schema.filter((o) => o.type === "table").map((o) => o.name)} onClose={(n) => {
					ws.setDialog(null);
					if (n) {
						ws.changed();
						ws.goTable(n);
					}
				}} />
			)}
		</>
	);
}
