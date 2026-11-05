// The management API: Connect clients generated from proto/cloud/v1, over
// connect-web, each call carrying the signed-in user's access token.
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { AIService } from "../gen/cloud/v1/ai_pb.js";
import { AuthService } from "../gen/cloud/v1/auth_pb.js";
import { DatabaseService } from "../gen/cloud/v1/database_pb.js";
import { GroupService } from "../gen/cloud/v1/group_pb.js";
import { OrgService } from "../gen/cloud/v1/org_pb.js";
import { UserService } from "../gen/cloud/v1/user_pb.js";
import { DatabaseType, Durability, Role } from "../gen/cloud/v1/common_pb.js";
import { Access, FileFormat } from "../gen/cloud/v1/database_pb.js";
import { authState, refresh, server, token } from "./auth.js";

export { Access, Code, DatabaseType, Durability, FileFormat, Role };

// typeName is a database type as the API and the docs spell it.
export const typeName = (t) => ({ [DatabaseType.PLAIN]: "plain", [DatabaseType.CRDT]: "crdt", [DatabaseType.RAFT]: "raft" })[t] ?? "plain";

const bearer = (next) => async (req) => {
	req.header.set("Authorization", `Bearer ${await token()}`);
	try {
		return await next(req);
	} catch (e) {
		// A token revoked or expired early: one refresh, one retry. A stream's
		// request is spent once sent, so its caller is the one to try again.
		if (ConnectError.from(e).code !== Code.Unauthenticated) throw e;
		await refresh();
		if (req.stream) throw e;
		req.header.set("Authorization", `Bearer ${await token()}`);
		return next(req);
	}
};

const transport = server && createConnectTransport({ baseUrl: server, interceptors: [bearer] });

export const api = server && {
	ai: createClient(AIService, transport),
	auth: createClient(AuthService, transport),
	db: createClient(DatabaseService, transport),
	groups: createClient(GroupService, transport),
	orgs: createClient(OrgService, transport),
	users: createClient(UserService, transport),
};

/** Whether e says the server does not implement the call (yet). */
export const unimplemented = (e) => ConnectError.from(e).code === Code.Unimplemented;

/** A call's error as a sentence for a toast. */
export const message = (e) => ConnectError.from(e).rawMessage || String(e.message ?? e);

export const date = (ts) => (ts ? timestampDate(ts) : null);
export const stamp = (d) => timestampFromDate(d);

export const ROLES = [
	[Role.VIEWER, "viewer"],
	[Role.MEMBER, "member"],
	[Role.ADMIN, "admin"],
	[Role.OWNER, "owner"],
];
export const roleName = (r) => ROLES.find(([v]) => v === r)?.[1] ?? "none";

/** The {org, database} a connection is to, when it is a database of this cloud server; else null. */
export function cloudDatabase(conn) {
	if (!server || conn?.kind !== "remote" || !conn.url?.startsWith(`${server}/db/`)) return null;
	const [org, database] = conn.url.slice(server.length + 4).split("/");
	return org && database ? { org, database } : null;
}

const DB_TOKEN_LIFETIME = 3600e3;

/** databaseToken mints an hour's token for a database: {token, exp}, exp in ms; the editor renews it (renew.js). */
export async function databaseToken(org, name, readOnly) {
	const exp = Date.now() + DB_TOKEN_LIFETIME;
	try {
		const r = await api.db.createToken({ org, name, access: readOnly ? Access.READ_ONLY : Access.FULL, expiresAt: stamp(new Date(exp)) });
		return { token: r.token, exp };
	} catch (e) {
		if (authState().status === "out") throw new Error(`Signed out: sign in again to reach ${org}/${name}.`);
		throw e;
	}
}
