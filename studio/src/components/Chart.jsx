import { useMemo, useState } from "react";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Cell, Legend, Line, LineChart, Pie, PieChart, ResponsiveContainer, Scatter, ScatterChart, Tooltip, XAxis, YAxis } from "recharts";

const KINDS = ["bar", "line", "area", "scatter", "pie"];
const COLORS = ["#d9480f", "#1971c2", "#2f9e44", "#ae3ec9", "#f08c00", "#0c8599", "#e03131", "#5c7cfa"];
const MAX = 2000;

const num = (v) => (typeof v === "bigint" ? Number(v) : typeof v === "number" ? v : typeof v === "string" && v.trim() !== "" && !isNaN(+v) ? +v : null);

/**
 * A chart of a result: one column along the x axis, the numeric ones as
 * series. It guesses a first chart (the first text column against the
 * numbers) and every choice can be changed.
 */
export default function Chart({ columns, rows }) {
	const data = rows.slice(0, MAX);
	const numeric = useMemo(() => columns.filter((_, j) => data.length && data.every((r) => r[j] === null || num(r[j]) !== null)), [columns, data]);
	const [kind, setKind] = useState("bar");
	const [x, setX] = useState(() => columns.find((c) => !numeric.includes(c)) ?? columns[0]);
	const [ys, setYs] = useState(() => numeric.filter((c) => c !== x).slice(0, 1));

	const xi = columns.indexOf(x);
	const series = ys.filter((y) => columns.includes(y));
	const points = useMemo(
		() => data.map((r) => Object.fromEntries([["__x", kind === "scatter" ? num(r[xi]) : String(r[xi] ?? "NULL")], ...series.map((y) => [y, num(r[columns.indexOf(y)])])])),
		[data, xi, series.join("\0"), kind],
	);

	if (!numeric.length) return <p className="dim pad">No numeric column to chart. A chart needs at least one column of numbers.</p>;

	const axes = (
		<>
			<CartesianGrid stroke="var(--line)" strokeDasharray="3 3" />
			<XAxis dataKey="__x" type={kind === "scatter" ? "number" : "category"} name={x} stroke="var(--dim)" fontSize={11} />
			<YAxis stroke="var(--dim)" fontSize={11} />
			<Tooltip contentStyle={{ background: "var(--panel)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 12 }} />
			{series.length > 1 && <Legend wrapperStyle={{ fontSize: 12 }} />}
		</>
	);
	let chart;
	if (kind === "bar") chart = <BarChart data={points}>{axes}{series.map((y, i) => <Bar key={y} dataKey={y} fill={COLORS[i % COLORS.length]} radius={[3, 3, 0, 0]} isAnimationActive={false} />)}</BarChart>;
	else if (kind === "line") chart = <LineChart data={points}>{axes}{series.map((y, i) => <Line key={y} dataKey={y} stroke={COLORS[i % COLORS.length]} dot={points.length < 60} strokeWidth={2} isAnimationActive={false} />)}</LineChart>;
	else if (kind === "area") chart = <AreaChart data={points}>{axes}{series.map((y, i) => <Area key={y} dataKey={y} stroke={COLORS[i % COLORS.length]} fill={COLORS[i % COLORS.length]} fillOpacity={0.2} isAnimationActive={false} />)}</AreaChart>;
	else if (kind === "scatter") chart = <ScatterChart>{axes}{series.map((y, i) => <Scatter key={y} name={y} data={points} dataKey={y} fill={COLORS[i % COLORS.length]} isAnimationActive={false} />)}</ScatterChart>;
	else
		chart = (
			<PieChart>
				<Tooltip contentStyle={{ background: "var(--panel)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 12 }} />
				<Pie data={points.slice(0, 50)} dataKey={series[0]} nameKey="__x" outerRadius="80%" label={{ fontSize: 11, fill: "var(--text)" }} isAnimationActive={false}>
					{points.slice(0, 50).map((_, i) => <Cell key={i} fill={COLORS[i % COLORS.length]} />)}
				</Pie>
			</PieChart>
		);

	return (
		<div className="chart">
			<div className="chart-controls">
				<div className="seg">
					{KINDS.map((k) => <button key={k} className={kind === k ? "on" : ""} onClick={() => setKind(k)}>{k}</button>)}
				</div>
				<label className="check">x <select value={x} onChange={(e) => setX(e.target.value)}>{(kind === "scatter" ? numeric : columns).map((c) => <option key={c}>{c}</option>)}</select></label>
				<span className="dim small">{kind === "pie" ? "value" : "y"}</span>
				{numeric.filter((c) => c !== x).map((c) => (
					<label key={c} className="check">
						<input type={kind === "pie" ? "radio" : "checkbox"} checked={kind === "pie" ? series[0] === c : series.includes(c)} onChange={(e) => setYs(kind === "pie" ? [c] : e.target.checked ? [...series, c] : series.filter((y) => y !== c))} />
						{c}
					</label>
				))}
				{rows.length > MAX && <span className="dim small">first {MAX} rows</span>}
			</div>
			<div className="chart-body">
				{series.length ? <ResponsiveContainer width="100%" height="100%">{chart}</ResponsiveContainer> : <p className="dim pad">Pick a column for the y axis.</p>}
			</div>
		</div>
	);
}
