// The cloud pages as the package exports them: each takes the org it shows
// (the one in view by default) and reads the user, toasts and API from the
// ConsoleProvider above it. onNavigate(key) is a page asking for another.
import { useMemo } from "react";
import { BarChart3, Database, KeyRound, MapPin, Settings, Users as UsersIcon, UserCog } from "lucide-react";
import { Spinner } from "../components/Busy.jsx";
import { useToast } from "../components/Toasts.jsx";
import { useCloud } from "./useCloud.js";
import * as body from "./Cloud.jsx";

const none = () => {};

function useOrgCloud(org) {
	const cloud = useCloud();
	if (!cloud) throw new Error("musql console: a cloud page needs a ConsoleProvider around it");
	const slug = org ?? cloud.org;
	return useMemo(() => ({ ...cloud, org: slug, role: cloud.roleIn(slug) }), [cloud, slug]);
}

function Page({ cloud, admin, children }) {
	const toast = useToast();
	if (cloud.auth.status === "none") return <div className="empty"><h1>No cloud server</h1></div>;
	if (cloud.auth.status !== "in") return <div className="empty"><body.SignedOut cloud={cloud} host={false} /></div>;
	if (!cloud.me) return <Spinner label="Loading…" />;
	if (admin && !cloud.admin) return <div className="empty"><h1>Server admins only</h1></div>;
	if (!cloud.org) return <div className="empty"><h1>No organization</h1><p className="dim">You belong to none yet. Accept an invite or ask an admin.</p><body.MyInvites cloud={cloud} toast={toast} /></div>;
	return <div className="cloudpage">{children}</div>;
}

/**
 * Databases lists the org's databases, with create (empty, upload, branch,
 * restore), tokens, rename, delete and restore, export and usage.
 * onOpenDatabase({org, database}, renamedFrom) opens one; current, the
 * {org, database} open in the app, is reopened under its new name when renamed.
 */
export function Databases({ org, onOpenDatabase = none, onNavigate = none, current = null }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud}><body.Databases cloud={cloud} toast={toast} onOpen={onOpenDatabase} setPage={onNavigate} current={current} /></Page>;
}

/** Groups lists the org's groups and their locations, and the server's locations. */
export function Groups({ org }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud}><body.Groups cloud={cloud} toast={toast} /></Page>;
}

/** Members lists the org's members and pending invites, and the user's own invites. */
export function Members({ org }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud}><body.Members cloud={cloud} toast={toast} /></Page>;
}

/** Usage charts the org's (or one database's) rows, statements, errors and latency, and its top queries. */
export function Usage({ org }) {
	const cloud = useOrgCloud(org);
	return <Page cloud={cloud}><body.UsagePage cloud={cloud} /></Page>;
}

/** OrgSettings shows the org and deletes it; after a delete it switches org and calls onNavigate("databases"). */
export function OrgSettings({ org, onNavigate = none }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud}><body.OrgSettings cloud={cloud} toast={toast} setPage={onNavigate} /></Page>;
}

/** PlatformTokens lists, creates and revokes the user's API tokens. */
export function PlatformTokens({ org }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud}><body.PlatformTokens toast={toast} /></Page>;
}

/** Users is the server's users, for a server admin: create, disable, reset a password. */
export function Users({ org }) {
	const cloud = useOrgCloud(org);
	const toast = useToast();
	return <Page cloud={cloud} admin><body.UsersPage toast={toast} /></Page>;
}

/** CLOUD_PAGES is every page with the key, label and icon the stock console lists it by; admin pages are for server admins. */
export const CLOUD_PAGES = [
	{ key: "databases", label: "Databases", icon: Database, component: Databases },
	{ key: "groups", label: "Groups & locations", icon: MapPin, component: Groups },
	{ key: "members", label: "Members", icon: UsersIcon, component: Members },
	{ key: "usage", label: "Usage", icon: BarChart3, component: Usage },
	{ key: "settings", label: "Org settings", icon: Settings, component: OrgSettings },
	{ key: "tokens", label: "Platform tokens", icon: KeyRound, component: PlatformTokens },
	{ key: "users", label: "Users", icon: UserCog, component: Users, admin: true },
];
