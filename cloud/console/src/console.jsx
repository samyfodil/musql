import { createRoot } from "react-dom/client";
import App from "./App.jsx";
import { setBaseURL } from "./cloud/api.js";
import { normalize } from "./options.js";
import { ConsoleBase } from "./provider.jsx";

/**
 * createConsole renders the stock console into root, built from the same
 * pieces the package exports. With no options it is the stock console; see
 * README.md for the slots. It returns {unmount}.
 */
export function createConsole(root, options = {}) {
	const o = normalize(options);
	setBaseURL(o.api.baseURL);
	const r = createRoot(root);
	r.render(
		<ConsoleBase options={o}>
			<App />
		</ConsoleBase>,
	);
	return {
		unmount() {
			r.unmount();
		},
	};
}
