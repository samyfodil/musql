// The console's cloud section: the side list (org switcher and pages) and the
// pages of a musql cloud server, over the management API.
import { useCallback, useEffect, useMemo, useState } from "react";
import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { BarChart3, Boxes, Building2, Database, KeyRound, LogOut, MapPin, Users, UserCog } from "lucide-react";
import Drawer from "../components/Drawer.jsx";
import { Field } from "../components/ColumnPanel.jsx";
import { MenuButton } from "../components/Menu.jsx";
import { Spinner } from "../components/Busy.jsx";
import { fmt } from "../values.js";
import { Access, api, databaseToken, date, DatabaseType, FileFormat, message, Role, ROLES, roleName, stamp, typeName, unimplemented } from "./api.js";
import { server, signIn, signOut } from "./auth.js";

export const PAGES = [
	["databases", "Databases", <Database size={14} />],
	["groups", "Groups & locations", <MapPin size={14} />],
	["members", "Members", <Users size={14} />],
	["usage", "Usage", <BarChart3 size={14} />],
	["tokens", "Platform tokens", <KeyRound size={14} />],
	["users", "Users", <UserCog size={14} />, true],
];

const NA = "Not available on this server.";
const when = (ts) => (ts ? date(ts).toLocaleString() : "—");
const day = (ts) => (ts ? date(ts).toLocaleDateString() : "—");
function bytes(n) {
	n = Number(n ?? 0);
	if (n < 1024) return `${n} B`;
	if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
	if (n < 1 << 30) return `${(n / (1 << 20)).toFixed(1)} MB`;
	return `${(n / (1 << 30)).toFixed(2)} GB`;
}
const copy = (text, toast, what) => navigator.clipboard.writeText(text).then(() => toast(`Copied ${what}.`), () => toast("Could not copy; select it and copy by hand.", true));
const can = (cloud, min) => cloud.role >= min;

/** load runs f and keeps its result, with a reload; an error is kept, not thrown. */
function useLoad(f, deps) {
	const [s, setS] = useState({ data: null, error: null, loading: true });
	const run = useCallback(() => {
		setS((p) => ({ ...p, loading: true }));
		return f().then((data) => setS({ data, error: null, loading: false }), (error) => setS({ data: null, error, loading: false }));
	}, deps);
	useEffect(() => {
		run();
	}, [run]);
	return [s, run];
}

// ---- the side list ----

export function CloudSide({ cloud, page, setPage, toast }) {
	const [newOrg, setNewOrg] = useState(false);
	if (!server) return <SignInElsewhere />;
	if (cloud.auth.status !== "in") return <SignedOut cloud={cloud} small />;
	const orgs = cloud.me?.memberships ?? [];
	return (
		<>
			<div className="sidehead">
				<h3>Cloud</h3>
				<button className="ghost icon" title={`Sign out ${cloud.me?.user?.email ?? ""}`} onClick={signOut}><LogOut size={14} /></button>
			</div>
			<div className="orgswitch">
				<MenuButton
					className="dbbtn"
					label={<><Building2 size={14} className="dbkind" /><span className="dbn">{orgs.find((m) => m.org.slug === cloud.org)?.org.name || cloud.org || "No organization"}</span><span className="dim">▾</span></>}
					items={[
						...orgs.map((m) => ({ label: m.org.name || m.org.slug, hint: `${m.org.slug} · ${roleName(m.role)}`, onClick: () => cloud.setOrg(m.org.slug) })),
						"-",
						{ label: "New organization…", onClick: () => setNewOrg(true) },
					]}
				/>
			</div>
			<nav>
				<div className="group">
					{PAGES.filter(([, , , admin]) => !admin || cloud.admin).map(([k, label, ic]) => (
						<button key={k} className={`item${page === k ? " active" : ""}`} onClick={() => setPage(k)}>
							<span className="ic">{ic}</span><span className="name">{label}</span>
						</button>
					))}
				</div>
				<p className="dim small pad">{cloud.me?.user?.email}{cloud.admin ? " · server admin" : ""}<br />{new URL(server).host}</p>
			</nav>
			{newOrg && <NewOrgDrawer cloud={cloud} toast={toast} onClose={() => setNewOrg(false)} />}
		</>
	);
}

function SignInElsewhere() {
	const [url, setUrl] = useState("");
	const ok = /^https?:\/\/[^/\s]+/.test(url.trim());
	return (
		<div className="sideempty">
			<h3>Cloud</h3>
			<p className="dim small">Sign in to a musql cloud server. Its console opens there; local files and remote URLs keep working here without one.</p>
			<form className="form" onSubmit={(e) => { e.preventDefault(); ok && location.assign(`${url.trim().replace(/\/+$/, "")}/console/#cloud`); }}>
				<input spellCheck={false} placeholder="https://cloud.example.com" value={url} onChange={(e) => setUrl(e.target.value)} />
				<button className="primary" disabled={!ok}>Sign in to a musql cloud</button>
			</form>
		</div>
	);
}

function SignedOut({ cloud, small }) {
	return (
		<div className={small ? "sideempty" : "cloud-empty"}>
			{!small && <div className="logo big">μ</div>}
			{small ? <h3>Cloud</h3> : <h1>musql cloud</h1>}
			<p className="dim small">{cloud.auth.status === "starting" ? "Signing in…" : `Sign in to ${new URL(server).host} to manage its databases.`}</p>
			{cloud.auth.error && <p className="err small">{cloud.auth.error}</p>}
			{cloud.auth.status !== "starting" && <button className="primary" onClick={signIn}>Sign in</button>}
		</div>
	);
}

function NewOrgDrawer({ cloud, toast, onClose }) {
	const [slug, setSlug] = useState("");
	const [name, setName] = useState("");
	const [busy, setBusy] = useState(false);
	const go = async () => {
		setBusy(true);
		try {
			const r = await api.orgs.create({ slug: slug.trim(), name: name.trim() || slug.trim() });
			await cloud.reload();
			cloud.setOrg(r.org.slug);
			toast(`Created ${r.org.slug}.`);
			onClose();
		} catch (e) {
			toast(message(e), true);
		} finally {
			setBusy(false);
		}
	};
	return (
		<Drawer title="New organization" onClose={onClose} footer={<><span className="grow" /><button onClick={onClose}>Cancel</button><button className="primary" disabled={!slug.trim() || busy} onClick={go}>Create</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); go(); }}>
				<Field label="Slug" help="In database URLs: /db/<slug>/<name>"><input spellCheck={false} value={slug} onChange={(e) => setSlug(e.target.value)} /></Field>
				<Field label="Name"><input value={name} onChange={(e) => setName(e.target.value)} /></Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

// ---- the pages ----

export function CloudMain({ cloud, page, setPage, toast, onOpen }) {
	if (!server) {
		return (
			<div className="empty">
				<h1>No cloud here</h1>
				<p className="dim">This console is not served by a musql cloud server. Enter a server's URL in the list to sign in there.</p>
			</div>
		);
	}
	if (cloud.auth.status !== "in") return <div className="empty"><SignedOut cloud={cloud} /></div>;
	if (!cloud.me) return <Spinner label="Loading…" />;
	if (!cloud.org) return <div className="empty"><h1>No organization</h1><p className="dim">You belong to none yet. Accept an invite or ask an admin.</p><MyInvites cloud={cloud} toast={toast} /></div>;
	const props = { cloud, toast, setPage };
	return (
		<div className="cloudpage">
			{page === "databases" && <Databases {...props} onOpen={onOpen} />}
			{page === "groups" && <Groups {...props} />}
			{page === "members" && <Members {...props} />}
			{page === "usage" && <UsagePage {...props} />}
			{page === "tokens" && <PlatformTokens {...props} />}
			{page === "users" && cloud.admin && <UsersPage {...props} />}
		</div>
	);
}

function Head({ title, sub, children }) {
	return (
		<header className="cloudhead">
			<div>
				<h1>{title}</h1>
				{sub && <p className="dim small">{sub}</p>}
			</div>
			<span className="grow" />
			{children}
		</header>
	);
}

function Loaded({ s, empty, children }) {
	if (s.loading && !s.data) return <Spinner label="Loading…" />;
	if (s.error) return <p className={unimplemented(s.error) ? "dim pad" : "err pad"}>{unimplemented(s.error) ? NA : message(s.error)}</p>;
	if (empty) return <div className="cloud-none dim">{empty}</div>;
	return children;
}

// ---- databases ----

function Databases({ cloud, toast, onOpen, setPage }) {
	const org = cloud.org;
	const [s, reload] = useLoad(() => api.db.list({ org }).then((r) => r.databases), [org]);
	const [drawer, setDrawer] = useState(null); // {create} | {token: db} | {del: db}
	const dbs = s.data ?? [];
	const member = can(cloud, Role.MEMBER);

	// connection is d as the editor opens it: its URL, and a token that renews itself.
	async function connection(d) {
		const readOnly = !member;
		const first = await databaseToken(org, d.name, readOnly);
		return {
			name: `${org}/${d.name}`,
			// The client resolves "v3/pipeline" against the URL, which drops a
			// last path segment that has no trailing slash.
			url: d.url.replace(/\/?$/, "/"),
			token: first.token,
			renewal: { exp: first.exp, mint: () => databaseToken(org, d.name, readOnly) },
		};
	}
	async function open(d) {
		try {
			await onOpen(await connection(d));
		} catch (e) {
			toast(message(e), true);
		}
	}
	async function exportAs(d, format) {
		try {
			const r = await api.db.export({ org, name: d.name, format });
			const a = document.createElement("a");
			a.href = URL.createObjectURL(new Blob([r.content]));
			a.download = `${d.name}.${format === FileFormat.SQLITE ? "sqlite" : "musq"}`;
			a.click();
			setTimeout(() => URL.revokeObjectURL(a.href), 1000);
		} catch (e) {
			toast(unimplemented(e) ? NA : message(e), true);
		}
	}
	async function invalidate(d) {
		if (!confirm(`Invalidate every token of ${d.name}? Clients using one stop working.`)) return;
		try {
			await api.db.invalidateTokens({ org, name: d.name });
			toast(`Every token of ${d.name} is invalid now.`);
		} catch (e) {
			toast(message(e), true);
		}
	}

	return (
		<>
			<Head title="Databases" sub={`${org} · served over Hrana at ${server}/db/${org}/<name>`}>
				{member && <button className="primary" onClick={() => setDrawer({ create: true })}>New database</button>}
			</Head>
			<Loaded s={s} empty={s.data && !dbs.length && "No databases yet. Create one, or upload a .sqlite or .musq file."}>
				<table className="cloudtable">
					<thead><tr><th>Name</th><th>Type</th><th>Group</th><th className="num">Size</th><th>Created</th><th /></tr></thead>
					<tbody>
						{dbs.map((d) => (
							<tr key={d.id}>
								<td><button className="link mono" title="Open in the editor" onClick={() => open(d)}>{d.name}</button>{d.parent && <span className="badge">branch of {d.parent}</span>}</td>
								<td><span className="chip mono" title={d.type === DatabaseType.CRDT ? "Replicated: every location writes, and restore is per node" : "One file, full SQLite semantics"}>{typeName(d.type)}</span></td>
								<td className="mono">{d.group}</td>
								<td className="num">{bytes(d.sizeBytes)}</td>
								<td title={when(d.createdAt)}>{day(d.createdAt)}</td>
								<td className="rowactions">
									<button onClick={() => open(d)}>Open</button>{" "}
									<MenuButton className="ghost icon" label="⋯" items={[
										{ label: "Copy URL", hint: "Hrana", onClick: () => copy(d.url, toast, "the URL") },
										{ label: "Create a token…", onClick: () => setDrawer({ token: d }) },
										{ label: "Usage", onClick: () => setPage("usage") },
										member && d.type !== DatabaseType.CRDT && { label: "Branch…", onClick: () => setDrawer({ create: { branch: d.name } }) },
										member && d.type !== DatabaseType.CRDT && { label: "Restore to a time…", onClick: () => setDrawer({ create: { restore: d.name } }) },
										"-",
										{ label: "Export as musql", hint: ".musq", disabled: !member, onClick: () => exportAs(d, FileFormat.MUSQL) },
										{ label: "Export as SQLite", hint: ".sqlite", disabled: !member, onClick: () => exportAs(d, FileFormat.SQLITE) },
										"-",
										member && { label: "Invalidate all tokens…", onClick: () => invalidate(d) },
										member && { label: "Delete…", onClick: () => setDrawer({ del: d }) },
									]} />
								</td>
							</tr>
						))}
					</tbody>
				</table>
			</Loaded>
			{drawer?.create && <CreateDb org={org} dbs={dbs} initial={drawer.create} toast={toast} onClose={(made) => { setDrawer(null); made && reload(); }} />}
			{drawer?.token && <DbToken cloud={cloud} db={drawer.token} toast={toast} onClose={() => setDrawer(null)} />}
			{drawer?.del && <DeleteDb org={org} db={drawer.del} toast={toast} onClose={(gone) => { setDrawer(null); gone && reload(); }} />}
		</>
	);
}

function CreateDb({ org, dbs, initial, toast, onClose }) {
	const [name, setName] = useState("");
	const [group, setGroup] = useState("");
	const [source, setSource] = useState(initial.branch ? "branch" : initial.restore ? "restore" : "empty");
	const [file, setFile] = useState(null);
	const [parent, setParent] = useState(initial.branch ?? initial.restore ?? dbs[0]?.name ?? "");
	const [at, setAt] = useState("");
	const [busy, setBusy] = useState(false);
	const [groups] = useLoad(() => api.groups.list({ org }).then((r) => r.groups), [org]);

	const ok = name.trim() && !busy && (source !== "upload" || file) && (source === "empty" || source === "upload" || parent) && (source !== "restore" || at);
	async function go() {
		setBusy(true);
		try {
			let r;
			if (source === "restore") {
				r = await api.db.restore({ org, name: parent, at: stamp(new Date(at)), newName: name.trim() });
			} else {
				const req = { org, name: name.trim(), group };
				if (source === "upload") {
					const up = await api.db.upload({ org, content: new Uint8Array(await file.arrayBuffer()) });
					req.source = { case: "upload", value: up.upload };
				} else if (source === "branch") {
					req.source = { case: "branchOf", value: parent };
					if (at) req.branchAt = stamp(new Date(at));
				}
				r = await api.db.create(req);
			}
			toast(`Created ${r.database.name}.`);
			onClose(true);
		} catch (e) {
			toast(unimplemented(e) ? `${source === "restore" ? "Restore" : source === "branch" ? "Branching" : "That"} is not available on this server.` : message(e), true);
		} finally {
			setBusy(false);
		}
	}
	return (
		<Drawer title="New database" subtitle={org} onClose={() => onClose(false)} footer={<><span className="grow" /><button onClick={() => onClose(false)}>Cancel</button><button className="primary" disabled={!ok} onClick={go}>{busy ? <><span className="spinner inline" /> Creating</> : "Create"}</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); ok && go(); }}>
				<Field label="Name"><input spellCheck={false} placeholder="my-db" value={name} onChange={(e) => setName(e.target.value)} /></Field>
				<div className="ffield">
					<span className="flabel2">Start from</span>
					<div className="seg">
						{[["empty", "Empty"], ["upload", "Upload a file"], ["branch", "Branch"], ["restore", "Restore to a time"]].map(([k, l]) => (
							<button type="button" key={k} className={source === k ? "pressed" : ""} onClick={() => setSource(k)}>{l}</button>
						))}
					</div>
				</div>
				{source === "upload" && <Field label="File" help=".sqlite, .db or .musq; a C SQLite file is converted on the server"><input type="file" accept=".musq,.sqlite,.sqlite3,.db" onChange={(e) => setFile(e.target.files[0] ?? null)} /></Field>}
				{(source === "branch" || source === "restore") && (
					<Field label={source === "branch" ? "Branch of" : "Restore"}>
						<select value={parent} onChange={(e) => setParent(e.target.value)}>{dbs.map((d) => <option key={d.id}>{d.name}</option>)}</select>
					</Field>
				)}
				{(source === "branch" || source === "restore") && (
					<Field label={source === "branch" ? "At (optional)" : "To"} help={source === "branch" ? "Empty for now" : "The new database holds the data as it was then"}>
						<input type="datetime-local" value={at} onChange={(e) => setAt(e.target.value)} />
					</Field>
				)}
				{source !== "restore" && (
					<Field label="Group">
						<select value={group} onChange={(e) => setGroup(e.target.value)}>
							<option value="">default</option>
							{(groups.data ?? []).filter((g) => g.name !== "default").map((g) => <option key={g.name}>{g.name}</option>)}
						</select>
					</Field>
				)}
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

const EXPIRY = [["", "Never"], ["1", "1 hour"], ["24", "1 day"], ["168", "7 days"], ["720", "30 days"]];

function DbToken({ cloud, db, toast, onClose }) {
	const [readOnly, setReadOnly] = useState(!can(cloud, Role.MEMBER));
	const [hours, setHours] = useState("168");
	const [tok, setTok] = useState(null);
	const go = async () => {
		try {
			const r = await api.db.createToken({ org: cloud.org, name: db.name, access: readOnly ? Access.READ_ONLY : Access.FULL, expiresAt: hours ? stamp(new Date(Date.now() + hours * 3600e3)) : undefined });
			setTok(r.token);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="Create a database token" subtitle={`${cloud.org}/${db.name}`} onClose={onClose} footer={<><span className="grow" />{tok ? <button className="primary" onClick={onClose}>Done</button> : <><button onClick={onClose}>Cancel</button><button className="primary" onClick={go}>Create token</button></>}</>}>
			{tok ? <SecretOnce secret={tok} toast={toast} extra={<Field label="URL"><div className="secret mono">{db.url}</div></Field>} /> : (
				<div className="form">
					<label className="check"><input type="checkbox" checked={readOnly} disabled={!can(cloud, Role.MEMBER)} onChange={(e) => setReadOnly(e.target.checked)} /> Read-only</label>
					<Field label="Expires"><select value={hours} onChange={(e) => setHours(e.target.value)}>{EXPIRY.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></Field>
				</div>
			)}
		</Drawer>
	);
}

function SecretOnce({ secret, toast, extra }) {
	return (
		<div className="form">
			<p className="small">Copy it now: it is shown only this once.</p>
			<div className="secret mono">{secret}</div>
			<div><button className="primary" onClick={() => copy(secret, toast, "the token")}>Copy</button></div>
			{extra}
		</div>
	);
}

function DeleteDb({ org, db, toast, onClose }) {
	const [typed, setTyped] = useState("");
	const go = async () => {
		try {
			await api.db.delete({ org, name: db.name });
			toast(`Deleted ${db.name}.`);
			onClose(true);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title={`Delete ${db.name}`} subtitle={org} onClose={() => onClose(false)} footer={<><span className="grow" /><button onClick={() => onClose(false)}>Cancel</button><button className="danger" disabled={typed !== db.name} onClick={go}>Delete forever</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); typed === db.name && go(); }}>
				<p>This deletes the database and its data. It cannot be undone.</p>
				<Field label={<>Type <b className="mono">{db.name}</b> to confirm</>}><input spellCheck={false} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} /></Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

// ---- groups & locations ----

function Groups({ cloud, toast }) {
	const org = cloud.org;
	const [s, reload] = useLoad(() => api.groups.list({ org }).then((r) => r.groups), [org]);
	const [locs] = useLoad(() => api.groups.listLocations({}).then((r) => r.locations), []);
	const [create, setCreate] = useState(false);
	const admin = can(cloud, Role.ADMIN);
	const codes = (locs.data ?? []).map((l) => l.code);
	const act = async (f, done) => {
		try {
			await f();
			if (done) toast(done);
			reload();
		} catch (e) {
			toast(unimplemented(e) ? NA : message(e), true);
		}
	};
	return (
		<>
			<Head title="Groups & locations" sub="A group places its databases in a set of locations. A crdt group's databases are written at every one of them.">
				{admin && <button className="primary" onClick={() => setCreate(true)}>New group</button>}
			</Head>
			<Loaded s={s}>
				<table className="cloudtable">
					<thead><tr><th>Group</th><th>Type</th><th>Primary</th><th>Locations</th><th>Created</th><th /></tr></thead>
					<tbody>
						{(s.data ?? []).map((g) => (
							<tr key={g.name}>
								<td className="mono">{g.name}</td>
								<td className="mono">{typeName(g.type)}</td>
								<td className="mono">{g.primary}</td>
								<td>
									{g.locations.map((l) => (
										<span key={l} className="chip mono">{l}{admin && l !== g.primary && <button className="x" title={`Remove ${l}`} onClick={() => act(() => api.groups.removeLocation({ org, group: g.name, location: l }), `Removed ${l} from ${g.name}.`)}>×</button>}</span>
									))}
									{admin && codes.some((c) => !g.locations.includes(c)) && (
										<MenuButton className="ghost icon" label="＋" title="Add a location" items={codes.filter((c) => !g.locations.includes(c)).map((c) => ({ label: c, onClick: () => act(() => api.groups.addLocation({ org, group: g.name, location: c }), `Added ${c} to ${g.name}.`) }))} />
									)}
								</td>
								<td>{day(g.createdAt)}</td>
								<td className="rowactions">{admin && g.name !== "default" && <button className="ghost" onClick={() => confirm(`Delete the group ${g.name}?`) && act(() => api.groups.delete({ org, name: g.name }), `Deleted ${g.name}.`)}>Delete</button>}</td>
							</tr>
						))}
					</tbody>
				</table>
			</Loaded>
			<h2 className="cloudsub">Locations</h2>
			<Loaded s={locs} empty={locs.data && !locs.data.length && "This server reports no locations."}>
				<table className="cloudtable"><thead><tr><th>Code</th><th>Node</th></tr></thead><tbody>{(locs.data ?? []).map((l) => <tr key={l.code}><td className="mono">{l.code}</td><td className="mono">{l.node}</td></tr>)}</tbody></table>
			</Loaded>
			{create && <NewGroup org={org} codes={codes} toast={toast} onClose={(made) => { setCreate(false); made && reload(); }} />}
		</>
	);
}

function NewGroup({ org, codes, toast, onClose }) {
	const [name, setName] = useState("");
	const [loc, setLoc] = useState(codes[0] ?? "");
	const go = async () => {
		try {
			await api.groups.create({ org, name: name.trim(), location: loc });
			toast(`Created the group ${name.trim()}.`);
			onClose(true);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="New group" subtitle={org} onClose={() => onClose(false)} footer={<><span className="grow" /><button onClick={() => onClose(false)}>Cancel</button><button className="primary" disabled={!name.trim()} onClick={go}>Create</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); name.trim() && go(); }}>
				<Field label="Name"><input spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} /></Field>
				<Field label="Primary location"><select value={loc} onChange={(e) => setLoc(e.target.value)}>{codes.map((c) => <option key={c}>{c}</option>)}</select></Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

// ---- members ----

function Members({ cloud, toast }) {
	const org = cloud.org;
	const admin = can(cloud, Role.ADMIN);
	const [s, reload] = useLoad(() => api.orgs.listMembers({ org }).then((r) => r.members), [org]);
	const [inv, reloadInv] = useLoad(() => (admin ? api.orgs.listInvites({ org }).then((r) => r.invites) : Promise.resolve([])), [org, admin]);
	const [invite, setInvite] = useState(false);
	const grantable = ROLES.filter(([r]) => r < Role.OWNER || cloud.role === Role.OWNER);
	const act = async (f, done, undo) => {
		try {
			await f();
			toast(done, false, undo && { label: "Undo", run: () => act(undo, "Undone.") });
			reload();
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<>
			<Head title="Members" sub={org}>{admin && <button className="primary" onClick={() => setInvite(true)}>Invite</button>}</Head>
			<Loaded s={s}>
				<table className="cloudtable">
					<thead><tr><th>Member</th><th>Email</th><th>Role</th><th /></tr></thead>
					<tbody>
						{(s.data ?? []).map((m) => (
							<tr key={m.user.id}>
								<td>{m.user.name || "—"}{m.user.id === cloud.me.user.id && <span className="badge">you</span>}</td>
								<td className="mono">{m.user.email}</td>
								<td>
									{admin ? (
										<select value={m.role} onChange={(e) => {
											const was = m.role;
											const to = Number(e.target.value);
											act(() => api.orgs.updateMember({ org, userId: m.user.id, role: to }), `${m.user.email} is ${roleName(to)} now.`, () => api.orgs.updateMember({ org, userId: m.user.id, role: was }));
										}}>
											{(m.role === Role.OWNER && cloud.role !== Role.OWNER ? ROLES : grantable).map(([r, l]) => <option key={r} value={r}>{l}</option>)}
										</select>
									) : roleName(m.role)}
								</td>
								<td className="rowactions">{admin && <button className="ghost" onClick={() => confirm(`Remove ${m.user.email} from ${org}?`) && act(() => api.orgs.removeMember({ org, userId: m.user.id }), `Removed ${m.user.email}.`)}>Remove</button>}</td>
							</tr>
						))}
					</tbody>
				</table>
			</Loaded>
			{admin && (
				<>
					<h2 className="cloudsub">Pending invites</h2>
					<Loaded s={inv} empty={inv.data && !inv.data.length && "No pending invites."}>
						<table className="cloudtable"><thead><tr><th>Email</th><th>Role</th><th>Sent</th></tr></thead><tbody>{(inv.data ?? []).map((i) => <tr key={i.id}><td className="mono">{i.email}</td><td>{roleName(i.role)}</td><td>{day(i.createdAt)}</td></tr>)}</tbody></table>
					</Loaded>
				</>
			)}
			<MyInvites cloud={cloud} toast={toast} />
			{invite && <InviteDrawer org={org} roles={grantable} toast={toast} onClose={(sent) => { setInvite(false); sent && reloadInv(); }} />}
		</>
	);
}

function MyInvites({ cloud, toast }) {
	const [s, reload] = useLoad(() => api.orgs.listInvites({ org: "" }).then((r) => r.invites), []);
	if (!s.data?.length) return null;
	const accept = async (i) => {
		try {
			const r = await api.orgs.acceptInvite({ id: i.id });
			toast(`You joined ${r.org.slug}.`);
			await cloud.reload();
			reload();
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<>
			<h2 className="cloudsub">Invites for you</h2>
			<table className="cloudtable"><tbody>{s.data.map((i) => <tr key={i.id}><td className="mono">{i.org}</td><td>{roleName(i.role)}</td><td className="rowactions"><button className="primary" onClick={() => accept(i)}>Accept</button></td></tr>)}</tbody></table>
		</>
	);
}

function InviteDrawer({ org, roles, toast, onClose }) {
	const [email, setEmail] = useState("");
	const [role, setRole] = useState(Role.MEMBER);
	const go = async () => {
		try {
			await api.orgs.invite({ org, email: email.trim(), role });
			toast(`Invited ${email.trim()}.`);
			onClose(true);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="Invite a member" subtitle={org} onClose={() => onClose(false)} footer={<><span className="grow" /><button onClick={() => onClose(false)}>Cancel</button><button className="primary" disabled={!email.includes("@")} onClick={go}>Invite</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); email.includes("@") && go(); }}>
				<Field label="Email" help="They join when they next sign in with this address."><input type="email" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} /></Field>
				<Field label="Role"><select value={role} onChange={(e) => setRole(Number(e.target.value))}>{roles.map(([r, l]) => <option key={r} value={r}>{l}</option>)}</select></Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

// ---- users (server admins) ----

function UsersPage({ toast }) {
	const [s, reload] = useLoad(() => api.users.list({}).then((r) => r.users), []);
	const [drawer, setDrawer] = useState(null); // "create" | {reset: user}
	const setDisabled = async (u, disabled, quiet) => {
		try {
			await api.users.update({ id: u.id, disabled });
			if (!quiet) toast(`${u.email} is ${disabled ? "disabled" : "enabled"}.`, false, { label: "Undo", run: () => setDisabled(u, !disabled, true) });
			reload();
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<>
			<Head title="Users" sub="Everyone who can sign in to this server.">
				<button className="primary" onClick={() => setDrawer("create")}>New user</button>
			</Head>
			<Loaded s={s}>
				<table className="cloudtable">
					<thead><tr><th>Email</th><th>Name</th><th>Created</th><th /></tr></thead>
					<tbody>
						{(s.data ?? []).map((u) => (
							<tr key={u.id} className={u.disabled ? "off" : ""}>
								<td className="mono">{u.email}{u.admin && <span className="badge key">admin</span>}{u.disabled && <span className="badge">disabled</span>}</td>
								<td>{u.name || "—"}</td>
								<td>{day(u.createdAt)}</td>
								<td className="rowactions">
									<button className="ghost" onClick={() => setDisabled(u, !u.disabled)}>{u.disabled ? "Enable" : "Disable"}</button>
									<button className="ghost" onClick={() => setDrawer({ reset: u })}>Reset password</button>
								</td>
							</tr>
						))}
					</tbody>
				</table>
			</Loaded>
			{drawer === "create" && <NewUser toast={toast} onClose={(made) => { setDrawer(null); made && reload(); }} />}
			{drawer?.reset && <ResetPassword user={drawer.reset} toast={toast} onClose={() => setDrawer(null)} />}
		</>
	);
}

function NewUser({ toast, onClose }) {
	const [f, setF] = useState({ email: "", name: "", password: "", admin: false });
	const ok = f.email.includes("@") && f.password.length >= 8;
	const go = async () => {
		try {
			await api.users.create(f);
			toast(`Created ${f.email}.`);
			onClose(true);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="New user" onClose={() => onClose(false)} footer={<><span className="grow" /><button onClick={() => onClose(false)}>Cancel</button><button className="primary" disabled={!ok} onClick={go}>Create</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); ok && go(); }}>
				<Field label="Email"><input type="email" spellCheck={false} value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} /></Field>
				<Field label="Name"><input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
				<Field label="Password" help="At least 8 characters; they can sign in with it right away."><input type="password" autoComplete="new-password" value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} /></Field>
				<label className="check"><input type="checkbox" checked={f.admin} onChange={(e) => setF({ ...f, admin: e.target.checked })} /> Server admin</label>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

function ResetPassword({ user, toast, onClose }) {
	const [pw, setPw] = useState("");
	const go = async () => {
		try {
			await api.users.resetPassword({ id: user.id, password: pw });
			toast(`New password set for ${user.email}.`);
			onClose();
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="Reset password" subtitle={user.email} onClose={onClose} footer={<><span className="grow" /><button onClick={onClose}>Cancel</button><button className="primary" disabled={pw.length < 8} onClick={go}>Set password</button></>}>
			<form className="form" onSubmit={(e) => { e.preventDefault(); pw.length >= 8 && go(); }}>
				<Field label="New password" help="At least 8 characters."><input type="password" autoComplete="new-password" value={pw} onChange={(e) => setPw(e.target.value)} /></Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

// ---- platform tokens ----

function PlatformTokens({ toast }) {
	const [s, reload] = useLoad(() => api.auth.listPlatformTokens({}).then((r) => r.tokens), []);
	const [mint, setMint] = useState(false);
	const revoke = async (t) => {
		if (!confirm(`Revoke ${t.name}? Anything using it stops working.`)) return;
		try {
			await api.auth.revokePlatformToken({ id: t.id });
			toast(`Revoked ${t.name}.`);
			reload();
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<>
			<Head title="Platform tokens" sub="API tokens for CI and the CLI. Each acts as you."><button className="primary" onClick={() => setMint(true)}>New token</button></Head>
			<Loaded s={s} empty={s.data && !s.data.length && "No platform tokens."}>
				<table className="cloudtable">
					<thead><tr><th>Name</th><th>Created</th><th>Last used</th><th /></tr></thead>
					<tbody>{(s.data ?? []).map((t) => <tr key={t.id}><td className="mono">{t.name}</td><td>{when(t.createdAt)}</td><td>{t.lastUsedAt ? when(t.lastUsedAt) : "never"}</td><td className="rowactions"><button className="ghost" onClick={() => revoke(t)}>Revoke</button></td></tr>)}</tbody>
				</table>
			</Loaded>
			{mint && <MintToken toast={toast} onClose={(made) => { setMint(false); made && reload(); }} />}
		</>
	);
}

function MintToken({ toast, onClose }) {
	const [name, setName] = useState("");
	const [secret, setSecret] = useState(null);
	const go = async () => {
		try {
			const r = await api.auth.createPlatformToken({ name: name.trim() });
			setSecret(r.secret);
		} catch (e) {
			toast(message(e), true);
		}
	};
	return (
		<Drawer title="New platform token" onClose={() => onClose(!!secret)} footer={<><span className="grow" />{secret ? <button className="primary" onClick={() => onClose(true)}>Done</button> : <><button onClick={() => onClose(false)}>Cancel</button><button className="primary" disabled={!name.trim()} onClick={go}>Create token</button></>}</>}>
			{secret ? <SecretOnce secret={secret} toast={toast} /> : (
				<form className="form" onSubmit={(e) => { e.preventDefault(); name.trim() && go(); }}>
					<Field label="Name" help="What uses it, e.g. ci-deploy"><input spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} /></Field>
					<button type="submit" hidden />
				</form>
			)}
		</Drawer>
	);
}

// ---- usage ----

const SERIES = [
	["rowsRead", "rows read", "#1971c2"],
	["rowsWritten", "rows written", "#d9480f"],
	["statements", "statements", "#2f9e44"],
	["errors", "errors", "#e03131"],
];

function UsagePage({ cloud }) {
	const org = cloud.org;
	const [dbs] = useLoad(() => api.db.list({ org }).then((r) => r.databases), [org]);
	const [pick, setPick] = useState(""); // "" = the whole org
	const [days, setDays] = useState(7);
	const names = useMemo(() => (dbs.data ?? []).map((d) => d.name), [dbs.data]);
	const [s] = useLoad(async () => {
		const to = new Date();
		const from = new Date(to - days * 864e5);
		const one = (name) => api.db.usage({ org, name, from: stamp(from), to: stamp(to) }).then((r) => r.points);
		const lists = await Promise.all((pick ? [pick] : names).map(one));
		// Per-org totals: the databases' points summed per hour.
		const byHour = new Map();
		for (const pts of lists)
			for (const p of pts) {
				const k = date(p.hour).getTime();
				const o = byHour.get(k) ?? { t: k, rowsRead: 0, rowsWritten: 0, statements: 0, errors: 0, bytes: 0, p50: 0, p95: 0 };
				o.rowsRead += Number(p.rowsRead);
				o.rowsWritten += Number(p.rowsWritten);
				o.statements += Number(p.statements);
				o.errors += Number(p.errors);
				o.bytes += Number(p.bytesStored);
				o.p50 = Math.max(o.p50, p.p50Ms);
				o.p95 = Math.max(o.p95, p.p95Ms);
				byHour.set(k, o);
			}
		const stats = pick ? await api.db.stats({ org, name: pick }).then((r) => r.topQueries, (e) => (unimplemented(e) ? null : Promise.reject(e))) : null;
		return { points: [...byHour.values()].sort((a, b) => a.t - b.t), stats };
	}, [org, pick, days, names.join("\0")]);

	const pts = s.data?.points ?? [];
	const total = (k) => pts.reduce((a, p) => a + p[k], 0);
	const tick = (t) => new Date(t).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit" });
	return (
		<>
			<Head title="Usage" sub={pick ? `${org}/${pick}` : `${org}, every database`}>
				<select value={pick} onChange={(e) => setPick(e.target.value)}><option value="">All databases</option>{names.map((n) => <option key={n}>{n}</option>)}</select>
				<select value={days} onChange={(e) => setDays(Number(e.target.value))}><option value={1}>24 hours</option><option value={7}>7 days</option><option value={30}>30 days</option></select>
			</Head>
			{s.loading && !s.data ? <Spinner label="Loading…" /> : s.error ? (
				<div className="cloud-none">
					<BarChart3 size={26} className="dim" />
					<p>{unimplemented(s.error) ? "Usage analytics are not available on this server yet." : message(s.error)}</p>
					<p className="dim small">Rows read and written, statements, errors and latency per hour appear here once the server counts them.</p>
				</div>
			) : !names.length ? <div className="cloud-none dim">No databases, so no usage yet.</div> : (
				<>
					<div className="tiles">
						{SERIES.map(([k, l]) => <div key={k} className="tile"><span className="dim small">{l}</span><b>{fmt(total(k))}</b></div>)}
						<div className="tile"><span className="dim small">stored</span><b>{bytes(pts.at(-1)?.bytes ?? 0)}</b></div>
					</div>
					{!pts.length ? <div className="cloud-none dim">No activity in this window.</div> : (
						<>
							<UsageChart data={pts} tick={tick} lines={SERIES} />
							<UsageChart data={pts} tick={tick} lines={[["p50", "p50 ms", "#1971c2"], ["p95", "p95 ms", "#ae3ec9"]]} />
						</>
					)}
					{pick && (
						<>
							<h2 className="cloudsub">Top queries</h2>
							{s.data.stats === null ? <p className="dim pad">Top queries are not available on this server yet.</p> : !s.data.stats.length ? <p className="dim pad">None yet.</p> : (
								<table className="cloudtable">
									<thead><tr><th>Statement</th><th className="num">Count</th><th className="num">Total ms</th><th className="num">Rows read</th><th className="num">Rows written</th></tr></thead>
									<tbody>{s.data.stats.map((q, i) => <tr key={i}><td className="mono sqlcell" title={q.sql}>{q.sql}</td><td className="num">{fmt(Number(q.count))}</td><td className="num">{q.totalMs.toFixed(1)}</td><td className="num">{fmt(Number(q.rowsRead))}</td><td className="num">{fmt(Number(q.rowsWritten))}</td></tr>)}</tbody>
								</table>
							)}
						</>
					)}
				</>
			)}
		</>
	);
}

function UsageChart({ data, tick, lines }) {
	return (
		<div className="usagechart">
			<ResponsiveContainer width="100%" height={220}>
				<LineChart data={data}>
					<CartesianGrid stroke="var(--line)" strokeDasharray="3 3" />
					<XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={tick} stroke="var(--dim)" fontSize={11} />
					<YAxis stroke="var(--dim)" fontSize={11} />
					<Tooltip labelFormatter={tick} contentStyle={{ background: "var(--panel)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 12 }} />
					<Legend wrapperStyle={{ fontSize: 12 }} />
					{lines.map(([k, l, c]) => <Line key={k} dataKey={k} name={l} stroke={c} dot={false} strokeWidth={2} isAnimationActive={false} />)}
				</LineChart>
			</ResponsiveContainer>
		</div>
	);
}

export const ICON = <Boxes size={18} />;
