import { createContext, useContext, useEffect, useMemo, useState } from "react";

const Theme = createContext(null);

// The chosen theme (kept in this browser) and whether the page is dark: the
// system's choice unless one is picked.
function useThemeState() {
	const mq = useMemo(() => matchMedia("(prefers-color-scheme: dark)"), []);
	const [systemDark, setSystemDark] = useState(mq.matches);
	const [theme, setTheme] = useState(() => {
		try {
			return localStorage.getItem("musql-theme") ?? "system";
		} catch {
			return "system";
		}
	});
	useEffect(() => {
		const on = (e) => setSystemDark(e.matches);
		mq.addEventListener("change", on);
		return () => mq.removeEventListener("change", on);
	}, [mq]);
	useEffect(() => {
		if (theme === "system") delete document.documentElement.dataset.theme;
		else document.documentElement.dataset.theme = theme;
		try {
			localStorage.setItem("musql-theme", theme);
		} catch {
			// a private window keeps the choice for this page only
		}
	}, [theme]);
	return [theme, setTheme, theme === "dark" || (theme === "system" && systemDark)];
}

export function ThemeProvider({ children }) {
	const t = useThemeState();
	return <Theme.Provider value={t}>{children}</Theme.Provider>;
}

/** useTheme is [theme, setTheme, dark]: "system" | "light" | "dark", and whether the page is dark now. */
export const useTheme = () => useContext(Theme);
