import { useMemo } from "react";
import { Background, Controls, ReactFlow } from "@xyflow/react";
import "@xyflow/react/dist/style.css";

const KIND = [
	[/^SEARCH\b.*\bUSING\b.*\bINDEX\b|^SEARCH\b.*PRIMARY KEY/i, "seek"],
	[/^SCAN\b/i, "scan"],
	[/TEMP B-TREE/i, "sort"],
	[/SUBQUERY|CO-ROUTINE|MATERIALIZE|COMPOUND|UNION|MULTI-INDEX/i, "group"],
];

/**
 * EXPLAIN QUERY PLAN's rows (id, parent, notused, detail) as a tree that
 * flows top-down: scans in one color, index seeks in another, sorts and
 * subqueries in a third, so a full scan stands out.
 */
export default function PlanFlow({ rows, dark }) {
	const { nodes, edges } = useMemo(() => {
		const kids = new Map();
		for (const [id, parent, , detail] of rows) {
			if (!kids.has(parent)) kids.set(parent, []);
			kids.get(parent).push({ id: Number(id), detail: String(detail) });
		}
		const nodes = [{ id: "0", position: { x: 0, y: 0 }, data: { label: "QUERY" }, className: "plan-node root", type: "input" }];
		const edges = [];
		let leaf = 0;
		// A node sits over the middle of its leaves.
		const place = (parent, depth) => {
			const list = kids.get(parent) ?? [];
			const xs = [];
			for (const n of list) {
				const sub = place(n.id, depth + 1);
				const x = sub.length ? (sub[0] + sub[sub.length - 1]) / 2 : leaf++ * 260;
				const kind = KIND.find(([re]) => re.test(n.detail))?.[1] ?? "other";
				nodes.push({ id: String(n.id), position: { x, y: depth * 90 }, data: { label: n.detail }, className: `plan-node ${kind}` });
				edges.push({ id: `${parent}-${n.id}`, source: String(parent), target: String(n.id) });
				xs.push(x);
			}
			return xs;
		};
		const top = place(0, 1);
		if (top.length) nodes[0].position.x = (top[0] + top[top.length - 1]) / 2;
		return { nodes, edges };
	}, [rows]);
	return (
		<div className="flow plan">
			<ReactFlow nodes={nodes} edges={edges} colorMode={dark ? "dark" : "light"} fitView nodesConnectable={false} proOptions={{ hideAttribution: true }}>
				<Background gap={18} size={1} />
				<Controls showInteractive={false} />
			</ReactFlow>
			<p className="flow-hint small">
				<span className="lg seek">index seek</span> <span className="lg scan">full scan</span> <span className="lg sort">sort</span> <span className="lg group">subquery</span>
			</p>
		</div>
	);
}
