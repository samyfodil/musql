// The signed-in user, their orgs and the org in view, shared by the cloud
// side list and pages.
import { useCallback, useEffect, useState } from "react";
import { api, Role } from "./api.js";
import { authState, start, subscribe } from "./auth.js";

const ORG = "musql-cloud:org";

let started = false;

export function useCloud() {
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

	const setOrg = (slug) => {
		setOrgState(slug);
		try {
			localStorage.setItem(ORG, slug);
		} catch {
			// remembered for this page only
		}
	};

	const slugs = me?.memberships.map((m) => m.org.slug) ?? [];
	const current = org && slugs.includes(org) ? org : (me?.memberships.find((m) => m.org.personal)?.org.slug ?? slugs[0] ?? null);
	const role = me?.memberships.find((m) => m.org.slug === current)?.role ?? Role.UNSPECIFIED;
	return { auth, me, org: current, role, setOrg, reload, admin: !!me?.user?.admin };
}
