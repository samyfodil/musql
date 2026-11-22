import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import { createRoot } from "react-dom/client";
import App from "./App.jsx";
import { setBaseURL } from "./cloud/api.js";
import { normalize, themeCSS } from "./options.js";
import { Options } from "./slots.js";
import "./style.css";

/**
 * createConsole renders the console into root. With no options it is the
 * stock console; see README.md for the slots. It returns {unmount}.
 */
export function createConsole(root, options = {}) {
	const o = normalize(options);
	setBaseURL(o.api.baseURL);
	const css = themeCSS(o.theme);
	let style = null;
	if (css) {
		style = document.createElement("style");
		style.dataset.musqlConsole = "theme";
		style.textContent = css;
		document.head.append(style);
	}
	if (o.branding.favicon) {
		let link = document.querySelector('link[rel="icon"]');
		if (!link) {
			link = document.createElement("link");
			link.rel = "icon";
			document.head.append(link);
		}
		link.href = o.branding.favicon;
	}
	const r = createRoot(root);
	r.render(
		<Options.Provider value={o}>
			<App />
		</Options.Provider>,
	);
	return {
		unmount() {
			r.unmount();
			style?.remove();
		},
	};
}
