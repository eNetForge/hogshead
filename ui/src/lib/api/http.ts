import { isCookieSession, requireCredentials, SESSION_HEADER } from '$lib/stores/session';
import { signRequest, UNSIGNED_PAYLOAD, type Credentials } from './sigv4';

/** An error returned by the server (admin JSON or S3 XML) or a transport failure. */
export class ApiError extends Error {
	constructor(
		public readonly status: number,
		public readonly code: string,
		message: string
	) {
		super(message);
		this.name = 'ApiError';
	}
}

export interface RequestInit2 {
	headers?: Record<string, string>;
	body?: BodyInit | null;
	signal?: AbortSignal;
	credentials?: Credentials;
}

/** The server origin the UI talks to. Same origin as the page. */
export function origin(): string {
	return typeof location !== 'undefined' ? location.origin : 'http://localhost';
}

/**
 * Performs an authenticated fetch: SigV4 header-signed with UNSIGNED-PAYLOAD
 * when the session holds an access key, or cookie-authenticated with the
 * session header when the user logged in with a password.
 */
export async function signedFetch(method: string, url: string | URL, init: RequestInit2 = {}): Promise<Response> {
	let headers: Record<string, string>;
	let credentials: RequestCredentials = 'omit';
	if (!init.credentials && isCookieSession()) {
		headers = { ...(init.headers ?? {}), [SESSION_HEADER]: '1' };
		credentials = 'same-origin';
	} else {
		const creds = init.credentials ?? requireCredentials();
		headers = (await signRequest({ method, url, headers: init.headers ?? {}, payloadHash: UNSIGNED_PAYLOAD }, creds)).headers;
	}
	try {
		return await fetch(url, { method, headers, body: init.body ?? undefined, signal: init.signal, credentials });
	} catch (e) {
		if (e instanceof DOMException && e.name === 'AbortError') throw e;
		throw new ApiError(0, 'NetworkError', `Network error: ${e instanceof Error ? e.message : String(e)}`);
	}
}

/** The web UI's own JSON endpoints (password login, logout, presign). */
export const UI_API = '/_ui/api';

interface UIErrorBody {
	error?: { code?: string; message?: string };
}

export async function uiApi<T>(method: string, path: string, body?: unknown): Promise<T> {
	const headers: Record<string, string> = { accept: 'application/json', [SESSION_HEADER]: '1' };
	if (body !== undefined) headers['content-type'] = 'application/json';
	let res: Response;
	try {
		res = await fetch(origin() + UI_API + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin' });
	} catch (e) {
		throw new ApiError(0, 'NetworkError', `Network error: ${e instanceof Error ? e.message : String(e)}`);
	}
	if (res.status === 204) return undefined as T;
	const text = await res.text();
	let parsed: unknown;
	try {
		parsed = text ? JSON.parse(text) : undefined;
	} catch {
		if (res.ok) throw new ApiError(res.status, 'BadResponse', 'Server returned malformed JSON');
	}
	if (!res.ok) {
		const err = (parsed as UIErrorBody | undefined)?.error;
		throw new ApiError(res.status, err?.code ?? `HTTP${res.status}`, err?.message ?? text ?? res.statusText);
	}
	return parsed as T;
}

/** Parses an S3 XML error body into an ApiError. */
export async function s3Error(res: Response): Promise<ApiError> {
	const text = await res.text().catch(() => '');
	try {
		const doc = new DOMParser().parseFromString(text, 'application/xml');
		const code = doc.querySelector('Error > Code')?.textContent ?? '';
		const message = doc.querySelector('Error > Message')?.textContent ?? '';
		if (code || message) return new ApiError(res.status, code || 'Error', message || code);
	} catch {
		// fall through
	}
	return new ApiError(res.status, 'HTTP' + res.status, text || res.statusText || `HTTP ${res.status}`);
}
