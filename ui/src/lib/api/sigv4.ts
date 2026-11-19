/**
 * AWS Signature Version 4 for the browser, built on Web Crypto.
 *
 * Only what the UI needs: header auth for same-origin requests and query
 * auth (presigned URLs) for download/share links. The payload is never
 * hashed by the signer: callers pass `UNSIGNED-PAYLOAD` (the default) or a
 * precomputed hex SHA-256.
 */

export interface Credentials {
	accessKeyId: string;
	secretAccessKey: string;
}

export interface SignOptions {
	region?: string;
	service?: string;
	/** Clock override, mainly for tests. */
	now?: Date;
}

export interface SignRequestInput {
	method: string;
	url: string | URL;
	/** Headers to send; `host` is derived from the URL. Names are case-insensitive. */
	headers?: Record<string, string>;
	/** Hex SHA-256 of the body or `UNSIGNED-PAYLOAD`. */
	payloadHash?: string;
}

export interface SignedRequest {
	/** All headers to send, including Authorization and x-amz-* ones. */
	headers: Record<string, string>;
	canonicalRequest: string;
	stringToSign: string;
	signature: string;
}

export interface PresignInput {
	method: string;
	url: string | URL;
	/** Lifetime in seconds, 1..604800. */
	expires: number;
}

export const UNSIGNED_PAYLOAD = 'UNSIGNED-PAYLOAD';
export const EMPTY_SHA256 = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';

const ALGORITHM = 'AWS4-HMAC-SHA256';
const DEFAULT_REGION = 'us-east-1';
const DEFAULT_SERVICE = 's3';
const MAX_PRESIGN_SECONDS = 7 * 24 * 3600;

/** Headers that must never be part of the signature (hop-by-hop or browser-managed). */
const UNSIGNABLE = new Set([
	'authorization',
	'connection',
	'content-length',
	'expect',
	'keep-alive',
	'proxy-authorization',
	'te',
	'trailer',
	'transfer-encoding',
	'upgrade',
	'user-agent',
	'x-amzn-trace-id'
]);

const encoder = new TextEncoder();

// ---- encoding -------------------------------------------------------------

function isUnreserved(c: number): boolean {
	return (
		(c >= 0x41 && c <= 0x5a) || // A-Z
		(c >= 0x61 && c <= 0x7a) || // a-z
		(c >= 0x30 && c <= 0x39) || // 0-9
		c === 0x2d || // -
		c === 0x5f || // _
		c === 0x2e || // .
		c === 0x7e // ~
	);
}

/** Percent-encodes `s` with the RFC 3986 unreserved set; `/` is encoded unless `keepSlash`. */
export function encodeRfc3986(s: string, keepSlash = false): string {
	const bytes = encoder.encode(s);
	let out = '';
	for (const b of bytes) {
		if (isUnreserved(b) || (keepSlash && b === 0x2f)) {
			out += String.fromCharCode(b);
		} else {
			out += '%' + b.toString(16).toUpperCase().padStart(2, '0');
		}
	}
	return out;
}

/** Encodes one raw (unencoded) path segment, e.g. an object key segment. */
export function encodePathSegment(segment: string): string {
	return encodeRfc3986(segment, false);
}

/** Encodes a raw object key for use in a URL path, keeping `/` separators. */
export function encodeKeyPath(key: string): string {
	return key.split('/').map(encodePathSegment).join('/');
}

function safeDecode(seg: string): string {
	try {
		return decodeURIComponent(seg);
	} catch {
		return seg;
	}
}

/**
 * Canonical URI: each segment of an already-URL-encoded path is decoded and
 * re-encoded exactly once with AWS rules, so pre-encoded input is never
 * double-encoded. An empty path becomes "/".
 */
export function canonicalUri(encodedPath: string): string {
	if (encodedPath === '') return '/';
	return encodedPath
		.split('/')
		.map((seg) => encodeRfc3986(safeDecode(seg)))
		.join('/');
}

/** Canonical query string: pairs sorted by name then value, each encoded. */
export function canonicalQuery(params: Iterable<[string, string]>): string {
	const pairs = Array.from(params, ([k, v]) => [encodeRfc3986(k), encodeRfc3986(v)] as const);
	pairs.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0));
	return pairs.map(([k, v]) => `${k}=${v}`).join('&');
}

function trimHeaderValue(v: string): string {
	return v.trim().split(/\s+/).join(' ');
}

// ---- crypto ---------------------------------------------------------------

function toHex(buf: ArrayBuffer): string {
	return Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, '0')).join('');
}

/** Web Crypto exists only in secure contexts (HTTPS, localhost); fall back to JS elsewhere. */
function subtle(): SubtleCrypto | null {
	return typeof globalThis.crypto !== 'undefined' && globalThis.crypto.subtle ? globalThis.crypto.subtle : null;
}

export async function sha256Hex(data: string | ArrayBuffer | Uint8Array): Promise<string> {
	const bytes = typeof data === 'string' ? encoder.encode(data) : data;
	const s = subtle();
	if (s) return toHex(await s.digest('SHA-256', bytes as BufferSource));
	return toHex(sha256Js(toBytes(bytes)).buffer as ArrayBuffer);
}

async function hmac(key: ArrayBuffer | Uint8Array, data: string): Promise<ArrayBuffer> {
	const s = subtle();
	if (s) {
		const k = await s.importKey('raw', key as BufferSource, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
		return s.sign('HMAC', k, encoder.encode(data));
	}
	return hmacJs(toBytes(key), encoder.encode(data)).buffer as ArrayBuffer;
}

function toBytes(b: ArrayBuffer | Uint8Array): Uint8Array {
	return b instanceof Uint8Array ? b : new Uint8Array(b);
}

// ---- pure JS SHA-256 / HMAC (FIPS 180-4), used only without Web Crypto ----

const K = new Uint32Array([
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
	0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
	0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
	0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
]);

export function sha256Js(msg: Uint8Array): Uint8Array {
	const h = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
	const bitLen = msg.length * 8;
	const padded = new Uint8Array(((msg.length + 9 + 63) >> 6) << 6);
	padded.set(msg);
	padded[msg.length] = 0x80;
	const dv = new DataView(padded.buffer);
	dv.setUint32(padded.length - 8, Math.floor(bitLen / 0x100000000));
	dv.setUint32(padded.length - 4, bitLen >>> 0);
	const w = new Uint32Array(64);
	for (let off = 0; off < padded.length; off += 64) {
		for (let i = 0; i < 16; i++) w[i] = dv.getUint32(off + i * 4);
		for (let i = 16; i < 64; i++) {
			const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
			const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
			w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
		}
		let [a, b, c, d, e, f, g, hh] = h;
		for (let i = 0; i < 64; i++) {
			const S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
			const ch = (e & f) ^ (~e & g);
			const t1 = (hh + S1 + ch + K[i] + w[i]) >>> 0;
			const S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
			const maj = (a & b) ^ (a & c) ^ (b & c);
			const t2 = (S0 + maj) >>> 0;
			hh = g;
			g = f;
			f = e;
			e = (d + t1) >>> 0;
			d = c;
			c = b;
			b = a;
			a = (t1 + t2) >>> 0;
		}
		h[0] = (h[0] + a) >>> 0;
		h[1] = (h[1] + b) >>> 0;
		h[2] = (h[2] + c) >>> 0;
		h[3] = (h[3] + d) >>> 0;
		h[4] = (h[4] + e) >>> 0;
		h[5] = (h[5] + f) >>> 0;
		h[6] = (h[6] + g) >>> 0;
		h[7] = (h[7] + hh) >>> 0;
	}
	const out = new Uint8Array(32);
	const odv = new DataView(out.buffer);
	for (let i = 0; i < 8; i++) odv.setUint32(i * 4, h[i]);
	return out;
}

function rotr(x: number, n: number): number {
	return ((x >>> n) | (x << (32 - n))) >>> 0;
}

export function hmacJs(key: Uint8Array, msg: Uint8Array): Uint8Array {
	if (key.length > 64) key = sha256Js(key);
	const k = new Uint8Array(64);
	k.set(key);
	const ipad = new Uint8Array(64 + msg.length);
	const opad = new Uint8Array(64 + 32);
	for (let i = 0; i < 64; i++) {
		ipad[i] = k[i] ^ 0x36;
		opad[i] = k[i] ^ 0x5c;
	}
	ipad.set(msg, 64);
	opad.set(sha256Js(ipad), 64);
	return sha256Js(opad);
}

async function signingKey(secret: string, date: string, region: string, service: string): Promise<ArrayBuffer> {
	const kDate = await hmac(encoder.encode('AWS4' + secret), date);
	const kRegion = await hmac(kDate, region);
	const kService = await hmac(kRegion, service);
	return hmac(kService, 'aws4_request');
}

// ---- dates ----------------------------------------------------------------

/** Formats a date as `YYYYMMDDTHHMMSSZ`. */
export function amzDate(d: Date): string {
	return d.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
}

// ---- signing --------------------------------------------------------------

function hostOf(u: URL): string {
	return u.host; // includes a non-default port, as the browser sends it
}

function scopeOf(dateStamp: string, region: string, service: string): string {
	return `${dateStamp}/${region}/${service}/aws4_request`;
}

/** Signs a request with header auth and returns the full header set to send. */
export async function signRequest(input: SignRequestInput, creds: Credentials, opts: SignOptions = {}): Promise<SignedRequest> {
	const region = opts.region ?? DEFAULT_REGION;
	const service = opts.service ?? DEFAULT_SERVICE;
	const now = opts.now ?? new Date();
	const url = typeof input.url === 'string' ? new URL(input.url) : input.url;
	const payloadHash = input.payloadHash ?? UNSIGNED_PAYLOAD;

	// Lowercase, deduplicate and drop unsignable headers.
	const headers: Record<string, string> = {};
	for (const [k, v] of Object.entries(input.headers ?? {})) {
		const lk = k.toLowerCase();
		if (!UNSIGNABLE.has(lk)) headers[lk] = v;
	}
	headers['host'] = hostOf(url);
	if (!headers['x-amz-date']) headers['x-amz-date'] = amzDate(now);
	if (!headers['x-amz-content-sha256']) headers['x-amz-content-sha256'] = payloadHash;

	const longDate = headers['x-amz-date'];
	const dateStamp = longDate.slice(0, 8);
	const signedNames = Object.keys(headers).sort();
	const canonicalHeaders = signedNames.map((n) => `${n}:${trimHeaderValue(headers[n])}\n`).join('');
	const signedHeaders = signedNames.join(';');

	const canonicalRequest = [
		input.method.toUpperCase(),
		canonicalUri(url.pathname),
		canonicalQuery(url.searchParams),
		canonicalHeaders,
		signedHeaders,
		headers['x-amz-content-sha256']
	].join('\n');

	const scope = scopeOf(dateStamp, region, service);
	const stringToSign = [ALGORITHM, longDate, scope, await sha256Hex(canonicalRequest)].join('\n');
	const key = await signingKey(creds.secretAccessKey, dateStamp, region, service);
	const signature = toHex(await hmac(key, stringToSign));

	const out: Record<string, string> = { ...headers };
	delete out['host']; // the browser sets it
	out['authorization'] = `${ALGORITHM} Credential=${creds.accessKeyId}/${scope}, SignedHeaders=${signedHeaders}, Signature=${signature}`;
	return { headers: out, canonicalRequest, stringToSign, signature };
}

/** Builds a presigned URL (query auth). Only `host` is signed; payload is unsigned. */
export async function presignUrl(input: PresignInput, creds: Credentials, opts: SignOptions = {}): Promise<string> {
	const region = opts.region ?? DEFAULT_REGION;
	const service = opts.service ?? DEFAULT_SERVICE;
	const now = opts.now ?? new Date();
	const expires = Math.floor(input.expires);
	if (!Number.isFinite(expires) || expires < 1 || expires > MAX_PRESIGN_SECONDS) {
		throw new Error(`presign: expiry must be between 1 and ${MAX_PRESIGN_SECONDS} seconds`);
	}
	const url = new URL(typeof input.url === 'string' ? input.url : input.url.toString());
	const longDate = amzDate(now);
	const dateStamp = longDate.slice(0, 8);
	const scope = scopeOf(dateStamp, region, service);

	const params: [string, string][] = Array.from(url.searchParams.entries()).filter(([k]) => !k.toLowerCase().startsWith('x-amz-'));
	params.push(
		['X-Amz-Algorithm', ALGORITHM],
		['X-Amz-Credential', `${creds.accessKeyId}/${scope}`],
		['X-Amz-Date', longDate],
		['X-Amz-Expires', String(expires)],
		['X-Amz-SignedHeaders', 'host']
	);
	const cq = canonicalQuery(params);
	const canonicalRequest = [
		input.method.toUpperCase(),
		canonicalUri(url.pathname),
		cq,
		`host:${hostOf(url)}\n`,
		'host',
		UNSIGNED_PAYLOAD
	].join('\n');
	const stringToSign = [ALGORITHM, longDate, scope, await sha256Hex(canonicalRequest)].join('\n');
	const key = await signingKey(creds.secretAccessKey, dateStamp, region, service);
	const signature = toHex(await hmac(key, stringToSign));
	return `${url.origin}${url.pathname}?${cq}&X-Amz-Signature=${signature}`;
}
