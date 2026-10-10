import { useEffect, useRef, useState } from "react";

/** The new-row form. onClose(values) on Insert, onClose(null) otherwise. */
export default function RowDialog({ name, cols, onClose }) {
	const ref = useRef(null);
	const [values, setValues] = useState(() => Object.fromEntries(cols.map((c) => [c.name, ""])));
	useEffect(() => ref.current.showModal(), []);
	return (
		<dialog ref={ref} onClose={() => onClose(null)}>
			<form
				onSubmit={(e) => {
					e.preventDefault();
					onClose(values);
				}}
			>
				<h2>New row in {name}</h2>
				<p className="dim small">Leave a field empty for the column's default.</p>
				<div className="fields">
					{cols.map((c, i) => (
						<label key={c.name} className="field">
							<span>
								{c.name}
								{c.type && <span className="dim"> {c.type.toLowerCase()}</span>}
							</span>
							<input
								autoFocus={i === 0}
								spellCheck={false}
								placeholder={c.dflt_value != null ? `default ${c.dflt_value}` : c.pk ? "auto" : c.notnull ? "required" : "NULL"}
								value={values[c.name]}
								onChange={(e) => setValues({ ...values, [c.name]: e.target.value })}
							/>
						</label>
					))}
				</div>
				<div className="dialog-actions">
					<button type="button" onClick={() => onClose(null)}>Cancel</button>
					<button type="submit" className="primary">Insert</button>
				</div>
			</form>
		</dialog>
	);
}
