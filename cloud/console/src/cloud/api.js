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
import { Role } from "../gen/cloud/v1/common_pb.js";
import { Access, FileFormat } from "../gen/cloud/v1/database_pb.js";
import { refresh, server, token } from "./auth.js";

export { Access, Code, FileFormat, Role };

const bearer = (next) => async (req) => {
	req.header.set("Authorization", `Bearer ${await token()}`);
	try {
		return await next(req);
	} catch (e) {
		// A token revoked or expired early: one refresh, one retry.
		if (ConnectError.from(e).code !== Code.Unauthenticated) throw e;
		await refresh();
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
