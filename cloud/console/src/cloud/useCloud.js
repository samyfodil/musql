// The signed-in user, their orgs and the org in view, shared by everything
// under one ConsoleProvider (or createConsole).
import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { api, Role } from "./api.js";
import { authState, refresh, server, signIn, signOut, start, subscribe } from "./auth.js";

const ORG = "musql-cloud:org";

let started = false;

export const Cloud = createContext(null);

/** useCloudState is the state useCloud() reads, kept once by the provider. */
export function useCloudState() {
	const [auth, setAuth] = useState(authState);
	const [me, setMe] = useState(null); // {user, memberships}
	const [org, setOrgState] = useState(() => {
		try {
			return localStorage.getItem(ORG);
		} catch {
			return null;
		}
	});

	useEffect(() => {
		const off = subscribe(setAuth);
		if (!started) {
			started = true;
			start();
		}
		return off;
	}, []);

	const reload = useCallback(async () => {
		if (!api) return;
		const r = await api.auth.whoAmI({});
		// A server admin sees every org; WhoAmI lists only the ones it belongs to.
		let memberships = r.memberships;
		if (r.user?.admin) {
			const all = await api.orgs.list({});
			memberships = all.orgs.map((o) => ({ org: o, role: Role.OWNER }));
		}
		setMe({ user: r.user, memberships });
	}, []);

	useEffect(() => {
		if (auth.status === "in") reload().catch(() => {});
		else setMe(null);
	}, [auth.status, reload]);

	const setOrg = useCallback((slug) => {
		setOrgState(slug);
		try {
			localStorage.setItem(ORG, slug);
		} catch {
			// remembered for this page only
		}
	}, []);

	return useMemo(() => {
		const slugs = me?.memberships.map((m) => m.org.slug) ?? [];
		const current = org && slugs.includes(org) ? org : (me?.memberships.find((m) => m.org.personal)?.org.slug ?? slugs[0] ?? null);
		const roleIn = (slug) => me?.memberships.find((m) => m.org.slug === slug)?.role ?? Role.UNSPECIFIED;
		return {
			auth,
			me,
			user: me?.user ?? null,
			orgs: me?.memberships.map((m) => m.org) ?? [],
			org: current,
			role: roleIn(current),
			roleIn,
			setOrg,
			reload,
			refresh,
			signIn,
			signOut,
			server,
			admin: !!me?.user?.admin,
		};
	}, [auth, me, org, setOrg, reload]);
}

/**
 * useCloud is the one server the console is bound to: auth ({status, error}),
 * user, orgs, org (the slug in view) and setOrg, role and roleIn(slug), admin,
 * reload, signIn, signOut and refresh.
 */
export const useCloud = () => useContext(Cloud);
