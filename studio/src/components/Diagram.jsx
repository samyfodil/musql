import { useEffect, useMemo, useState } from "react";
import { Background, Controls, Handle, MiniMap, Position, ReactFlow, useEdgesState, useNodesState } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { ident, rows } from "../db.js";
import { useStudio } from "../studio.js";
import { Spinner } from "./Busy.jsx";

const ROW = 22;
const W = 230;

// TableNode is one table or view: its name, then its columns, each with a
// handle on either side so a foreign key's edge meets the column it names.
function TableNode({ data }) {
	return (
		<div className={`er-node${data.view ? " view" : ""}`}>
			<div className="er-head" onDoubleClick={data.open} title="Double-click to open">
				<span>{data.view ? "◇" : "▦"}</span> {data.name}
				{data.count != null && <span className="dim small"> {data.count}</span>}
			</div>
			{data.cols.map((c) => (
				<div key={c.name} className="er-col">
					<Handle type="target" position={Position.Left} id={`in-${c.name}`} />
					<span className={c.pk ? "pk" : c.fk ? "fk" : ""}>{c.pk ? "🔑 " : c.fk ? "↗ " : ""}{c.name}</span>
					<span className="dim">{c.type.toLowerCase()}</span>
					<Handle type="source" position={Position.Right} id={`out-${c.name}`} />
				</div>
			))}
		</div>
	);
}
const nodeTypes = { table: TableNode };

/** The schema as a diagram: tables and views, and an edge for every foreign key. */
export default function Diagram({ onOpen }) {
	const { schema, dark } = useStudio();
	const [graph, setGraph] = useState(null);
	const [nodes, setNodes, onNodesChange] = useNodesState([]);
	const [edges, setEdges, onEdgesChange] = useEdgesState([]);
	const objs = useMemo(() => schema.filter((o) => (o.type === "table" || o.type === "view") && !/_(data|idx|content|docsize|config|segments|segdir|stat|node|parent|rowid)$/.test(o.name) && !/^musql_studio_/.test(o.name)), [schema]);

	useEffect(() => {
		let gone = false;
		(async () => {
			const out = [];
			for (const o of objs) {
				try {
					const cols = await rows(`PRAGMA table_info(${ident(o.name)})`);
					const fks = o.type === "table" ? await rows(`PRAGMA foreign_key_list(${ident(o.name)})`) : [];
					out.push({ ...o, cols, fks });
				} catch {
					// a virtual table whose module is missing is left out
				}
			}
			if (!gone) setGraph(out);
		})();
		return () => { gone = true; };
	}, [objs]);

	useEffect(() => {
		if (!graph) return;
		// Columns of the layout by depth: a table sits right of every table it references.
		const depth = new Map();
		const by = new Map(graph.map((t) => [t.name.toLowerCase(), t]));
		const d = (t, seen = new Set()) => {
			if (depth.has(t.name)) return depth.get(t.name);
			if (seen.has(t.name)) return 0;
			seen.add(t.name);
			const parents = t.fks.map((f) => by.get(f.table.toLowerCase())).filter((p) => p && p !== t);
			const v = t.type === "view" ? 0 : parents.length ? 1 + Math.max(...parents.map((p) => d(p, seen))) : 0;
			depth.set(t.name, v);
			return v;
		};
		graph.forEach((t) => d(t));
		const maxDepth = Math.max(0, ...depth.values());
		const colY = new Map();
		const ns = graph.map((t) => {
			const c = t.type === "view" ? maxDepth + 1 : depth.get(t.name);
			const y = colY.get(c) ?? 0;
			colY.set(c, y + 60 + (t.cols.length + 1) * ROW);
			const fkCols = new Set(t.fks.map((f) => f.from));
			return {
				id: t.name,
				type: "table",
				position: { x: c * (W + 110), y },
				data: {
					name: t.name,
					view: t.type === "view",
					open: () => onOpen(t.name),
					cols: t.cols.map((col) => ({ name: col.name, type: col.type || "", pk: col.pk > 0, fk: fkCols.has(col.name) })),
				},
			};
		});
		const es = graph.flatMap((t) =>
			t.fks.filter((f) => by.has(f.table.toLowerCase())).map((f, i) => {
				const parent = by.get(f.table.toLowerCase());
				const to = f.to ?? parent.cols.find((c) => c.pk > 0)?.name ?? parent.cols[0]?.name;
				return {
					id: `${t.name}-${f.id}-${i}`,
					// parent key → the column that references it, left to right
					source: parent.name,
					sourceHandle: `out-${to}`,
					target: t.name,
					targetHandle: `in-${f.from}`,
					label: f.on_delete !== "NO ACTION" ? `on delete ${f.on_delete.toLowerCase()}` : undefined,
					animated: true,
					style: { stroke: "var(--accent)" },
					labelStyle: { fontSize: 10, fill: "var(--dim)" },
					labelBgStyle: { fill: "var(--panel)" },
				};
			}),
		);
		setNodes(ns);
		setEdges(es);
	}, [graph]);

	if (!graph) return <Spinner label="Reading the schema…" />;
	if (!graph.length) return <p className="dim pad">No tables yet.</p>;
	return (
		<div className="flow">
			<ReactFlow nodes={nodes} edges={edges} onNodesChange={onNodesChange} onEdgesChange={onEdgesChange} nodeTypes={nodeTypes} colorMode={dark ? "dark" : "light"} fitView minZoom={0.1} proOptions={{ hideAttribution: true }}>
				<Background gap={18} size={1} />
				<Controls showInteractive={false} />
				<MiniMap pannable zoomable nodeColor={(n) => (n.data.view ? "#888" : "var(--accent)")} />
			</ReactFlow>
			<p className="flow-hint dim small">Drag tables to arrange them · double-click one to open it · edges are foreign keys</p>
		</div>
	);
}
