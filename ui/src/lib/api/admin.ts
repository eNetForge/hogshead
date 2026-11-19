import type { SessionUser } from '$lib/stores/session';
import { ApiError, origin, signedFetch, uiApi } from './http';
import type { Credentials } from './sigv4';

export const ADMIN_PREFIX = '/_admin/v1';

export interface Me {
	user: SessionUser;
	via: string;
}

export interface Stats {
	buckets: number;
	objects: number;
	bytes: number;
	uploads: number;
	chunks: number;
}

export interface WorkerStats {
	last_gc: string;
	chunks_released: number;
	orphans_removed: number;
	bytes_reclaimed: number;
	last_lifecycle: string;
	objects_expired: number;
	versions_expired: number;
	markers_removed: number;
	uploads_aborted: number;
	last_error?: string;
}

export interface Status {
	version: string;
	node_name: string;
	id?: string;
	uptime: string;
	started_at: string;
	stats: Stats;
	workers: WorkerStats;
	pending_restart: string[];
	settings: Record<string, string>;
	listen: string[];
	tls_mode: string;
}

export interface User extends SessionUser {
	keys?: number;
	has_password?: boolean;
}

export interface AccessKey {
	access_key_id: string;
	user: string;
	label: string;
	disabled: boolean;
	created_at: string;
}

export interface CreatedKey extends AccessKey {
	secret_access_key: string;
}

export interface Bucket {
	name: string;
	owner: string;
	owner_name: string;
	created_at: string;
	versioning: string;
	object_lock: boolean;
	policy?: string;
	cors?: string;
	lifecycle?: string;
	objects: number;
	bytes: number;
	/** Every stored version, non-current ones and delete markers included. */
	versions?: number;
	delete_markers?: number;
}

export interface UpdateBucketRequest {
	versioning?: string;
	policy?: string;
	cors?: string;
	lifecycle?: string;
	public_read?: boolean;
}

export interface AuditEntry {
	id: number;
	at: string;
	actor: string;
	action: string;
	target: string;
	detail?: string;
}

export interface Setting {
	key: string;
	value: string;
	description?: string;
}

export interface ReplicationTarget {
	name: string;
	enabled: boolean;
	prefix?: string;
	endpoint: string;
	bucket: string;
	access_key: string;
	secret_key: string;
	region?: string;
	path_style?: boolean;
	/** Comes from the server-wide rule; managed on the Replication page, not per bucket. */
	inherited?: boolean;
	/** Server-wide only: also mirrors users, keys, passwords and settings. */
	sync_config?: boolean;
	/** Managed by cellar membership; shown under Cellar, not Replication. */
	cellar?: boolean;
}

export interface Peer {
	id: string;
	name: string;
	addr: string;
	endpoint: string;
	version: string;
	last_seen: string;
	cellar?: string;
	cellar_name?: string;
}

export interface CellarMember {
	id: string;
	name: string;
	endpoint: string;
	version?: string;
	joined_at: string;
	self: boolean;
	enabled: boolean;
	lag: number;
	last_error?: string;
	backfilling: boolean;
	backfilled: boolean;
	/** Approved but has not collected its credentials yet. */
	joining?: boolean;
}

export interface CellarPending {
	id: string;
	name: string;
	endpoint: string;
	version?: string;
	requested_at: string;
	/** Buckets the joiner brings along (a merge); 0 for an empty server. */
	buckets?: number;
}

export interface CellarStatus {
	active: boolean;
	id?: string;
	name?: string;
	write_ack?: string;
	self_id: string;
	members: CellarMember[];
	pending: CellarPending[];
	ready: boolean;
	backlog: number;
	joining?: { address: string; endpoint: string; requested_at: string; last_error?: string };
	last_check?: string;
	check_error?: string;
	notices?: { at: string; message: string }[];
}

export interface ReplicationStatus {
	bucket: string;
	target: ReplicationTarget;
	cursor: number;
	head: number;
	lag: number;
	last_sync?: string;
	last_error?: string;
	/** A bucket configuration change the destination refused; objects still flow. */
	last_warning?: string;
	pushed: number;
}

/** Fields left undefined keep their current value on an existing target. */
export interface ReplicationRequest {
	name?: string;
	endpoint?: string;
	bucket?: string;
	access_key?: string;
	secret_key?: string;
	region?: string;
	path_style?: boolean;
	prefix?: string;
	enabled?: boolean;
	sync_config?: boolean;
}

export type TransferDirection = 'import' | 'export';
export type TransferState = 'queued' | 'running' | 'done' | 'failed' | 'cancelled' | 'interrupted';

export interface TransferSpec {
	direction: TransferDirection;
	bucket: string;
	prefix?: string;
	remote: { endpoint: string; bucket: string; access_key: string; region?: string; path_style: boolean };
	remote_prefix?: string;
	overwrite?: string;
	dry_run?: boolean;
	concurrency?: number;
}

export interface TransferJob {
	id: string;
	actor: string;
	spec: TransferSpec;
	state: TransferState;
	listed: number;
	copied: number;
	skipped: number;
	failed: number;
	bytes: number;
	created_at: string;
	started_at?: string;
	finished_at?: string;
	error?: string;
	errors?: string[];
}

export interface TransferRequest {
	direction: TransferDirection;
	bucket: string;
	prefix?: string;
	endpoint: string;
	remote_bucket: string;
	remote_prefix?: string;
	access_key: string;
	secret_key: string;
	region?: string;
	path_style?: boolean;
	overwrite?: string;
	dry_run?: boolean;
	concurrency?: number;
}

export interface UsageRow {
	h: number; // unix seconds, hour
	b: string; // bucket
	u: string; // user
	k: string; // access key ("session" for password logins)
	c: string; // class: read, write, delete, list, bucket, other
	m?: string; // member name, when grouped by member
	n: number; // requests
	e: number; // errors
	i: number; // bytes in
	o: number; // bytes out
}

export interface UsageResponse {
	from: string;
	to: string;
	by: string[];
	rows: UsageRow[];
	totals: UsageRow;
	access_log?: string;
	scope: 'local' | 'cellar';
	members?: { id: string; name: string; self: boolean; ok: boolean; error?: string }[];
}

export interface ErrorGroup {
	code: string;
	status: number;
	op: string;
	bucket?: string;
	user?: string;
	count: number;
	last_seen: string;
	message?: string;
	hint: string;
	severity: 'info' | 'warning' | 'error';
	members?: string[];
}

export interface ErrorEntry {
	time: string;
	id: string;
	op: string;
	method: string;
	bucket?: string;
	key?: string;
	status: number;
	user?: string;
	access_key?: string;
	ip: string;
	user_agent?: string;
	error_code?: string;
	error_message?: string;
	replicated?: boolean;
	member?: string;
}

export interface ErrorsResponse {
	from: string;
	scope: 'local' | 'cellar';
	total: number;
	groups: ErrorGroup[];
	rows: ErrorEntry[];
	members?: { id: string; name: string; self: boolean; ok: boolean; error?: string }[];
}

export interface ConfigView {
	path: string;
	toml: string;
	pending_restart: string[];
}

interface ErrorBody {
	error?: { code?: string; message?: string };
}

async function call<T>(method: string, path: string, body?: unknown, credentials?: Credentials): Promise<T> {
	const headers: Record<string, string> = { accept: 'application/json' };
	let payload: string | undefined;
	if (body !== undefined) {
		headers['content-type'] = 'application/json';
		payload = JSON.stringify(body);
	}
	const res = await signedFetch(method, origin() + ADMIN_PREFIX + path, { headers, body: payload, credentials });
	if (res.status === 204) return undefined as T;
	const text = await res.text();
	let parsed: unknown = undefined;
	if (text) {
		try {
			parsed = JSON.parse(text);
		} catch {
			if (res.ok) throw new ApiError(res.status, 'BadResponse', 'Server returned malformed JSON');
		}
	}
	if (!res.ok) {
		const err = (parsed as ErrorBody | undefined)?.error;
		throw new ApiError(res.status, err?.code ?? `HTTP${res.status}`, err?.message ?? text ?? res.statusText);
	}
	return parsed as T;
}

const enc = encodeURIComponent;

export const admin = {
	me: (credentials?: Credentials) => call<Me>('GET', '/me', undefined, credentials),
	status: () => call<Status>('GET', '/status'),

	users: {
		list: () => call<User[]>('GET', '/users'),
		get: (name: string) => call<User>('GET', `/users/${enc(name)}`),
		create: (req: { name: string; role: string; buckets?: string[] }) => call<User>('POST', '/users', req),
		update: (name: string, req: { role?: string; buckets?: string[]; disabled?: boolean }) =>
			call<User>('PATCH', `/users/${enc(name)}`, req),
		remove: (name: string) => call<void>('DELETE', `/users/${enc(name)}`),
		/** Sets a web UI password; '' removes password login. Admins for anyone, users for themselves. */
		setPassword: (name: string, password: string) => call<User>('PUT', `/users/${enc(name)}/password`, { password }),
		/** Replaces the session secret: every web UI session on this server is signed out, the caller's included. */
		logoutAll: () => call<{ rotated: boolean }>('POST', '/sessions/rotate')
	},

	peers: () => call<Peer[]>('GET', '/peers'),

	errors: (params: { from?: string; bucket?: string; user?: string; code?: string; scope?: string; limit?: string }) => {
		const q = new URLSearchParams();
		for (const [k, v] of Object.entries(params)) if (v) q.set(k, v);
		const s = q.toString();
		return call<ErrorsResponse>('GET', `/errors${s ? '?' + s : ''}`);
	},

	usage: (params: { from?: string; to?: string; by?: string; bucket?: string; user?: string; key?: string; scope?: string }) => {
		const q = new URLSearchParams();
		for (const [k, v] of Object.entries(params)) if (v) q.set(k, v);
		const s = q.toString();
		return call<UsageResponse>('GET', `/usage${s ? '?' + s : ''}`);
	},

	cellar: {
		status: () => call<CellarStatus>('GET', '/cellar'),
		init: (name: string, write_ack: string) => call<CellarStatus>('POST', '/cellar/init', { name, write_ack }),
		update: (req: { name?: string; write_ack?: string }) => call<CellarStatus>('PUT', '/cellar', req),
		join: (address: string, advertise?: string, merge = false) =>
			call<CellarStatus>('POST', '/cellar/join', { address, advertise: advertise || undefined, merge: merge || undefined }),
		cancelJoin: () => call<void>('DELETE', '/cellar/join'),
		approve: (id: string) => call<CellarStatus>('POST', `/cellar/pending/${enc(id)}/approve`),
		reject: (id: string) => call<void>('DELETE', `/cellar/pending/${enc(id)}`),
		remove: (id: string) => call<void>('DELETE', `/cellar/members/${enc(id)}`),
		leave: () => call<void>('POST', '/cellar/leave')
	},

	transfers: {
		list: () => call<TransferJob[]>('GET', '/transfers'),
		start: (req: TransferRequest) => call<TransferJob>('POST', '/transfers', req),
		get: (id: string) => call<TransferJob>('GET', `/transfers/${enc(id)}`),
		/** Cancels a running job or removes a finished job's record. */
		cancel: (id: string) => call<void>('DELETE', `/transfers/${enc(id)}`)
	},

	/** Password login against the server session endpoints. */
	login: {
		password: (user: string, password: string) => uiApi<Me>('POST', '/login', { user, password }),
		me: () => uiApi<Me>('GET', '/me'),
		logout: () => uiApi<void>('POST', '/logout')
	},

	keys: {
		listForUser: (name: string) => call<AccessKey[]>('GET', `/users/${enc(name)}/keys`),
		create: (name: string, label?: string) => call<CreatedKey>('POST', `/users/${enc(name)}/keys`, { label: label ?? '' }),
		listAll: () => call<AccessKey[]>('GET', '/keys'),
		setDisabled: (id: string, disabled: boolean) => call<AccessKey>('PATCH', `/keys/${enc(id)}`, { disabled }),
		revoke: (id: string) => call<void>('DELETE', `/keys/${enc(id)}`)
	},

	buckets: {
		list: () => call<Bucket[]>('GET', '/buckets'),
		get: (name: string) => call<Bucket>('GET', `/buckets/${enc(name)}`),
		create: (req: { name: string; versioning?: string; object_lock?: boolean; owner?: string }) =>
			call<Bucket>('POST', '/buckets', req),
		update: (name: string, req: UpdateBucketRequest) => call<Bucket>('PATCH', `/buckets/${enc(name)}`, req),
		remove: (name: string, force = false) => call<void>('DELETE', `/buckets/${enc(name)}${force ? '?force=true' : ''}`)
	},

	audit: (limit: number, before?: number) =>
		call<AuditEntry[]>('GET', `/audit?limit=${limit}${before ? `&before=${before}` : ''}`),

	settings: {
		list: () => call<Setting[]>('GET', '/settings'),
		set: (key: string, value: string) => call<{ key: string; value: string }>('PUT', `/settings/${enc(key)}`, { value })
	},

	config: {
		get: () => call<ConfigView>('GET', '/config'),
		reload: () => call<ConfigView>('POST', '/config/reload')
	},

	replication: {
		list: () => call<ReplicationStatus[]>('GET', '/replication'),
		/** Returns [] when the bucket has no targets. */
		get: async (bucket: string): Promise<ReplicationStatus[]> => {
			try {
				return await call<ReplicationStatus[]>('GET', `/buckets/${enc(bucket)}/replication`);
			} catch (e) {
				if (e instanceof ApiError && e.code === 'NoReplicationRule') return [];
				throw e;
			}
		},
		set: (bucket: string, target: string, req: ReplicationRequest) =>
			call<ReplicationStatus[]>('PUT', `/buckets/${enc(bucket)}/replication${target ? `?target=${enc(target)}` : ''}`, req),
		remove: (bucket: string, target?: string) =>
			call<void>('DELETE', `/buckets/${enc(bucket)}/replication${target ? `?target=${enc(target)}` : ''}`),
		global: {
			list: () => call<ReplicationTarget[]>('GET', '/replication/global'),
			set: (target: string, req: ReplicationRequest) =>
				call<ReplicationTarget[]>('PUT', `/replication/global${target ? `?target=${enc(target)}` : ''}`, req),
			remove: (target?: string) => call<void>('DELETE', `/replication/global${target ? `?target=${enc(target)}` : ''}`)
		}
	}
};
