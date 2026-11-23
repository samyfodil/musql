import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import "./style.css";
import { useEffect, useLayoutEffect, useMemo, useState } from "react";
import { setBaseURL, setTransport } from "./cloud/api.js";
import { configureAuth } from "./cloud/auth.js";
import { Cloud, useCloudState } from "./cloud/useCloud.js";
import { Toasts, useToast } from "./components/Toasts.jsx";
import { ready } from "./db.js";
import { normalize, themeCSS } from "./options.js";
import { Options } from "./slots.js";
import { ThemeProvider } from "./theme.jsx";

/**
 * bindCloud binds the management API and sign-in to one server: cloud is
 * {baseURL, getToken, signIn, signOut, callbackPath, transport}, every field
 * optional, the page's own origin by default; null binds none.
 */
export function bindCloud(cloud) {
	if (!cloud) {
		configureAuth({ server: null });
		setTransport(null);
		return;
	}
	const base = (cloud.baseURL || location.origin).replace(/\/+$/, "");
	configureAuth({ server: base, getToken: cloud.getToken ?? (cloud.transport ? () => Promise.reject(new Error("the transport signs its own calls")) : null), signIn: cloud.signIn, signOut: cloud.signOut, callbackPath: cloud.callbackPath, home: cloud.home });
	if (cloud.transport) setTransport(cloud.transport);
	else setBaseURL(base);
}

function ReadyError() {
	const toast = useToast();
	useEffect(() => {
		ready.catch((e) => toast(e.message, true));
	}, [toast]);
	return null;
}

function CloudState({ children }) {
	return <Cloud.Provider value={useCloudState()}>{children}</Cloud.Provider>;
}

/** ConsoleBase is what every console component needs around it, for options already normalized. */
export function ConsoleBase({ options, children }) {
	const css = useMemo(() => themeCSS(options.theme), [options.theme]);
	useLayoutEffect(() => {
		if (!css) return;
		const style = document.createElement("style");
		style.dataset.musqlConsole = "theme";
		style.textContent = css;
		document.head.append(style);
		return () => style.remove();
	}, [css]);
	const { favicon } = options.branding;
	useLayoutEffect(() => {
		if (!favicon) return;
		let link = document.querySelector('link[rel="icon"]');
		if (!link) {
			link = document.createElement("link");
			link.rel = "icon";
			document.head.append(link);
		}
		link.href = favicon;
	}, [favicon]);
	return (
		<Options.Provider value={options}>
			<ThemeProvider>
				<Toasts>
					<ReadyError />
					<CloudState>{children}</CloudState>
				</Toasts>
			</ThemeProvider>
		</Options.Provider>
	);
}

/**
 * ConsoleProvider holds what the console's components need: the style sheet
 * and a theme over its tokens, toasts, the theme switch, and the cloud API
 * and sign-in bound to one server (see bindCloud; read once, when it mounts).
 */
export function ConsoleProvider({ cloud = {}, branding, theme, children }) {
	useState(() => bindCloud(cloud));
	const options = useMemo(() => normalize({ branding, theme }), [branding, theme]);
	return <ConsoleBase options={options}>{children}</ConsoleBase>;
}
