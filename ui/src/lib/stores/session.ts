import { writable, get } from 'svelte/store';
import type { Credentials } from '$lib/api/sigv4';

export type Role = 'admin' | 'read-write' | 'read-only';

export interface SessionUser {
	id: string;
	name: string;
	role: Role;
	buckets: string[];
	disabled: boolean;
	created_at: string;
}

export type SessionMode = 'key' | 'cookie';

export interface Session {
	/** 'key' signs every request in the browser; 'cookie' relies on the server session cookie. */
	mode: SessionMode;
	credentials?: Credentials;
	user: SessionUser;
	remember: boolean;
}

const STORAGE_KEY = 'hogshead.session';

function storageFor(remember: boolean): Storage | null {
	try {
		return remember ? localStorage : sessionStorage;
	} catch {
		return null;
	}
}

function readStored(): Session | null {
	for (const remember of [false, true]) {
		const store = storageFor(remember);
		const raw = store?.getItem(STORAGE_KEY);
		if (!raw) continue;
		try {
			const parsed = JSON.parse(raw) as Session;
			if (!parsed?.user?.name) continue;
			if (parsed.mode === 'cookie') return { ...parsed, remember };
			if (parsed.credentials?.accessKeyId) return { ...parsed, mode: 'key', remember };
		} catch {
			store?.removeItem(STORAGE_KEY);
		}
	}
	return null;
}

function clearStored(): void {
	storageFor(false)?.removeItem(STORAGE_KEY);
	storageFor(true)?.removeItem(STORAGE_KEY);
}

/** The current session, or null when logged out. */
export const session = writable<Session | null>(null);

/** Loads a persisted session from storage; call once on startup in the browser. */
export function loadSession(): Session | null {
	const s = readStored();
	session.set(s);
	return s;
}

/** Persists and activates a session. */
export function setSession(next: Session): void {
	clearStored();
	storageFor(next.remember)?.setItem(STORAGE_KEY, JSON.stringify(next));
	session.set(next);
}

/** Replaces the user part of the session (e.g. after a refresh of /me). */
export function updateSessionUser(user: SessionUser): void {
	const cur = get(session);
	if (!cur) return;
	setSession({ ...cur, user });
}

export function clearSession(): void {
	clearStored();
	session.set(null);
}

/** Returns the active access key credentials or throws when logged out or in cookie mode. */
export function requireCredentials(): Credentials {
	const s = get(session);
	if (!s) throw new Error('not logged in');
	if (s.mode !== 'key' || !s.credentials) throw new Error('this session has no access key; use the server session');
	return s.credentials;
}

/** True when requests are authenticated by the server session cookie. */
export function isCookieSession(): boolean {
	return get(session)?.mode === 'cookie';
}

/** Header that must accompany cookie-authenticated requests. */
export const SESSION_HEADER = 'X-Hogshead-Session';

export function isAdmin(s: Session | null): boolean {
	return s?.user.role === 'admin';
}

export function canWrite(s: Session | null): boolean {
	return s?.user.role === 'admin' || s?.user.role === 'read-write';
}
