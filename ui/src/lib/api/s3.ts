import { isCookieSession, requireCredentials, SESSION_HEADER } from '$lib/stores/session';
import { ApiError, origin, s3Error, signedFetch, uiApi } from './http';
import { encodeKeyPath, presignUrl, signRequest, UNSIGNED_PAYLOAD } from './sigv4';
import { child, children, escapeXml, parseXml, text } from './xml';

export interface S3Bucket {
	name: string;
	creationDate: string;
}

export interface S3Object {
	key: string;
	size: number;
	lastModified: string;
	etag: string;
	storageClass: string;
}

export interface ListObjectsResult {
	prefixes: string[];
	objects: S3Object[];
	nextToken?: string;
	truncated: boolean;
}

export interface ObjectVersion {
	key: string;
	versionId: string;
	isLatest: boolean;
	lastModified: string;
	etag: string;
	size: number;
	deleteMarker: boolean;
}

export interface ListVersionsResult {
	versions: ObjectVersion[];
	/** Common prefixes ("folders") when a delimiter was given. */
	prefixes: string[];
	truncated: boolean;
	nextKeyMarker?: string;
	nextVersionIdMarker?: string;
}

export interface ObjectHead {
	contentType: string;
	contentLength: number;
	etag: string;
	lastModified: string;
	versionId?: string;
	/** x-amz-meta-* headers with the prefix stripped. */
	metadata: Record<string, string>;
	/** All response headers, lowercase. */
	headers: Record<string, string>;
}

export interface UploadOptions {
	contentType?: string;
	onProgress?: (sentBytes: number) => void;
	signal?: AbortSignal;
}

export const MULTIPART_THRESHOLD = 16 * 1024 * 1024;
export const PART_SIZE = 16 * 1024 * 1024;
const PART_CONCURRENCY = 3;

/** Path-style URL of a bucket or object. `key` is the raw object key. */
export function objectUrl(bucket: string, key = '', query: Record<string, string> = {}): URL {
	const u = new URL(`${origin()}/${encodeURIComponent(bucket)}${key ? '/' + encodeKeyPath(key) : ''}`);
	for (const [k, v] of Object.entries(query)) u.searchParams.set(k, v);
	return u;
}

async function okOrThrow(res: Response): Promise<Response> {
	if (!res.ok) throw await s3Error(res);
	return res;
}

// ---- buckets ---------------------------------------------------------------

export async function listBuckets(): Promise<S3Bucket[]> {
	const res = await okOrThrow(await signedFetch('GET', origin() + '/'));
	const doc = parseXml(await res.text());
	return children(child(doc.documentElement, 'Buckets'), 'Bucket').map((b) => ({
		name: text(b, 'Name'),
		creationDate: text(b, 'CreationDate')
	}));
}

// ---- listing --------------------------------------------------------------

export async function listObjects(
	bucket: string,
	opts: { prefix?: string; delimiter?: string; token?: string; maxKeys?: number } = {}
): Promise<ListObjectsResult> {
	const query: Record<string, string> = { 'list-type': '2' };
	if (opts.prefix) query.prefix = opts.prefix;
	if (opts.delimiter) query.delimiter = opts.delimiter;
	if (opts.token) query['continuation-token'] = opts.token;
	if (opts.maxKeys) query['max-keys'] = String(opts.maxKeys);
	const res = await okOrThrow(await signedFetch('GET', objectUrl(bucket, '', query)));
	const root = parseXml(await res.text()).documentElement;
	return {
		prefixes: children(root, 'CommonPrefixes').map((p) => text(p, 'Prefix')),
		objects: children(root, 'Contents').map((c) => ({
			key: text(c, 'Key'),
			size: Number(text(c, 'Size', '0')),
			lastModified: text(c, 'LastModified'),
			etag: text(c, 'ETag').replace(/^"|"$/g, ''),
			storageClass: text(c, 'StorageClass')
		})),
		nextToken: text(root, 'NextContinuationToken') || undefined,
		truncated: text(root, 'IsTruncated') === 'true'
	};
}

export async function listObjectVersions(
	bucket: string,
	opts: { prefix?: string; delimiter?: string; keyMarker?: string; versionIdMarker?: string; maxKeys?: number } = {}
): Promise<ListVersionsResult> {
	const query: Record<string, string> = { versions: '' };
	if (opts.prefix) query.prefix = opts.prefix;
	if (opts.delimiter) query.delimiter = opts.delimiter;
	if (opts.keyMarker) query['key-marker'] = opts.keyMarker;
	if (opts.versionIdMarker) query['version-id-marker'] = opts.versionIdMarker;
	if (opts.maxKeys) query['max-keys'] = String(opts.maxKeys);
	const res = await okOrThrow(await signedFetch('GET', objectUrl(bucket, '', query)));
	const root = parseXml(await res.text()).documentElement;
	const parse = (el: Element, deleteMarker: boolean): ObjectVersion => ({
		key: text(el, 'Key'),
		versionId: text(el, 'VersionId'),
		isLatest: text(el, 'IsLatest') === 'true',
		lastModified: text(el, 'LastModified'),
		etag: text(el, 'ETag').replace(/^"|"$/g, ''),
		size: Number(text(el, 'Size', '0')),
		deleteMarker
	});
	const versions = [
		...children(root, 'Version').map((v) => parse(v, false)),
		...children(root, 'DeleteMarker').map((v) => parse(v, true))
	].sort((a, b) => (a.key !== b.key ? (a.key < b.key ? -1 : 1) : a.lastModified < b.lastModified ? 1 : a.lastModified > b.lastModified ? -1 : 0));
	return {
		versions,
		prefixes: children(root, 'CommonPrefixes').map((p) => text(p, 'Prefix')),
		truncated: text(root, 'IsTruncated') === 'true',
		nextKeyMarker: text(root, 'NextKeyMarker') || undefined,
		nextVersionIdMarker: text(root, 'NextVersionIdMarker') || undefined
	};
}

// ---- single object ops ----------------------------------------------------

function versionQuery(versionId?: string): Record<string, string> {
	return versionId ? { versionId } : {};
}

export async function headObject(bucket: string, key: string, versionId?: string): Promise<ObjectHead> {
	const query = versionQuery(versionId);
	const res = await okOrThrow(await signedFetch('HEAD', objectUrl(bucket, key, query)));
	const headers: Record<string, string> = {};
	const metadata: Record<string, string> = {};
	res.headers.forEach((v, k) => {
		headers[k] = v;
		if (k.startsWith('x-amz-meta-')) metadata[k.slice('x-amz-meta-'.length)] = v;
	});
	return {
		contentType: headers['content-type'] ?? '',
		contentLength: Number(headers['content-length'] ?? '0'),
		etag: (headers['etag'] ?? '').replace(/^"|"$/g, ''),
		lastModified: headers['last-modified'] ?? '',
		versionId: headers['x-amz-version-id'],
		metadata,
		headers
	};
}

/** Largest object the UI previews or edits inline. */
export const PREVIEW_MAX_BYTES = 1024 * 1024;

/** Fetches an object's body as text (for previews and the inline editor). */
export async function getObjectText(bucket: string, key: string, versionId?: string): Promise<string> {
	const res = await okOrThrow(await signedFetch('GET', objectUrl(bucket, key, versionQuery(versionId))));
	return res.text();
}

export async function deleteObject(bucket: string, key: string, versionId?: string): Promise<void> {
	const query = versionQuery(versionId);
	await okOrThrow(await signedFetch('DELETE', objectUrl(bucket, key, query)));
}

export interface DeleteError {
	key: string;
	code: string;
	message: string;
}

/** Deletes many keys in one request; returns per-key errors (empty on full success). */
export async function deleteObjects(bucket: string, keys: string[]): Promise<DeleteError[]> {
	if (keys.length === 0) return [];
	const body = `<?xml version="1.0" encoding="UTF-8"?><Delete><Quiet>true</Quiet>${keys
		.map((k) => `<Object><Key>${escapeXml(k)}</Key></Object>`)
		.join('')}</Delete>`;
	const res = await okOrThrow(
		await signedFetch('POST', objectUrl(bucket, '', { delete: '' }), {
			headers: { 'content-type': 'application/xml' },
			body
		})
	);
	const root = parseXml(await res.text()).documentElement;
	return children(root, 'Error').map((e) => ({ key: text(e, 'Key'), code: text(e, 'Code'), message: text(e, 'Message') }));
}

export async function copyObject(bucket: string, srcKey: string, dstKey: string): Promise<void> {
	const res = await okOrThrow(
		await signedFetch('PUT', objectUrl(bucket, dstKey), {
			headers: { 'x-amz-copy-source': `/${encodeURIComponent(bucket)}/${encodeKeyPath(srcKey)}` }
		})
	);
	// CopyObject can return 200 with an <Error> body.
	const body = await res.text();
	if (body.includes('<Error>')) {
		const root = parseXml(body).documentElement;
		throw new ApiError(200, text(root, 'Code') || 'CopyFailed', text(root, 'Message') || 'copy failed');
	}
}

/** Copies a specific version onto a key (restoring an old version makes it the new current one). */
export async function copyObjectVersion(bucket: string, srcKey: string, versionId: string, dstKey: string): Promise<void> {
	const res = await okOrThrow(
		await signedFetch('PUT', objectUrl(bucket, dstKey), {
			headers: { 'x-amz-copy-source': `/${encodeURIComponent(bucket)}/${encodeKeyPath(srcKey)}?versionId=${encodeURIComponent(versionId)}` }
		})
	);
	const body = await res.text();
	if (body.includes('<Error>')) {
		const root = parseXml(body).documentElement;
		throw new ApiError(200, text(root, 'Code') || 'CopyFailed', text(root, 'Message') || 'copy failed');
	}
}

export async function renameObject(bucket: string, srcKey: string, dstKey: string): Promise<void> {
	await copyObject(bucket, srcKey, dstKey);
	await deleteObject(bucket, srcKey);
}

/** Creates an empty `prefix/` placeholder object so the "folder" shows up in listings. */
export async function createFolder(bucket: string, prefix: string): Promise<void> {
	const key = prefix.endsWith('/') ? prefix : prefix + '/';
	await okOrThrow(await signedFetch('PUT', objectUrl(bucket, key), { body: new Blob([]) }));
}

export async function presignGet(bucket: string, key: string, expires: number, versionId?: string): Promise<string> {
	if (isCookieSession()) {
		const r = await uiApi<{ url: string }>('POST', '/presign', { bucket, key, version_id: versionId ?? '', expires });
		return r.url;
	}
	const query = versionQuery(versionId);
	return presignUrl({ method: 'GET', url: objectUrl(bucket, key, query), expires }, requireCredentials());
}

// ---- uploads --------------------------------------------------------------

/** Signed PUT via XMLHttpRequest so that upload progress is observable. Resolves with the ETag. */
function xhrPut(url: URL, body: Blob, headers: Record<string, string>, opts: UploadOptions): Promise<string> {
	return new Promise((resolve, reject) => {
		const xhr = new XMLHttpRequest();
		xhr.open('PUT', url.toString(), true);
		for (const [k, v] of Object.entries(headers)) {
			if (k === 'host' || k === 'content-length') continue;
			xhr.setRequestHeader(k, v);
		}
		xhr.upload.onprogress = (ev) => opts.onProgress?.(ev.loaded);
		xhr.onerror = () => reject(new ApiError(0, 'NetworkError', 'Upload failed: network error'));
		xhr.onabort = () => reject(new DOMException('aborted', 'AbortError'));
		xhr.onload = () => {
			if (xhr.status >= 200 && xhr.status < 300) {
				resolve((xhr.getResponseHeader('ETag') ?? '').replace(/^"|"$/g, ''));
				return;
			}
			try {
				const root = parseXml(xhr.responseText).documentElement;
				reject(new ApiError(xhr.status, text(root, 'Code') || `HTTP${xhr.status}`, text(root, 'Message') || xhr.statusText));
			} catch {
				reject(new ApiError(xhr.status, `HTTP${xhr.status}`, xhr.responseText || xhr.statusText));
			}
		};
		opts.signal?.addEventListener('abort', () => xhr.abort(), { once: true });
		xhr.send(body);
	});
}

async function signedPut(url: URL, body: Blob, contentType: string | undefined, opts: UploadOptions): Promise<string> {
	const headers: Record<string, string> = {};
	if (contentType) headers['content-type'] = contentType;
	if (isCookieSession()) {
		headers[SESSION_HEADER] = '1';
		return xhrPut(url, body, headers, opts);
	}
	const signed = await signRequest({ method: 'PUT', url, headers, payloadHash: UNSIGNED_PAYLOAD }, requireCredentials());
	return xhrPut(url, body, signed.headers, opts);
}

export async function putObject(bucket: string, key: string, body: Blob, opts: UploadOptions = {}): Promise<string> {
	return signedPut(objectUrl(bucket, key), body, opts.contentType || (body as File).type || undefined, opts);
}

async function initiateMultipart(bucket: string, key: string, contentType?: string): Promise<string> {
	const headers: Record<string, string> = {};
	if (contentType) headers['content-type'] = contentType;
	const res = await okOrThrow(await signedFetch('POST', objectUrl(bucket, key, { uploads: '' }), { headers }));
	const root = parseXml(await res.text()).documentElement;
	const id = text(root, 'UploadId');
	if (!id) throw new ApiError(res.status, 'NoUploadId', 'Server did not return an UploadId');
	return id;
}

async function completeMultipart(bucket: string, key: string, uploadId: string, parts: { n: number; etag: string }[]): Promise<void> {
	const body = `<CompleteMultipartUpload>${parts
		.sort((a, b) => a.n - b.n)
		.map((p) => `<Part><PartNumber>${p.n}</PartNumber><ETag>"${escapeXml(p.etag)}"</ETag></Part>`)
		.join('')}</CompleteMultipartUpload>`;
	const res = await okOrThrow(
		await signedFetch('POST', objectUrl(bucket, key, { uploadId }), { headers: { 'content-type': 'application/xml' }, body })
	);
	const txt = await res.text();
	if (txt.includes('<Error>')) {
		const root = parseXml(txt).documentElement;
		throw new ApiError(200, text(root, 'Code') || 'CompleteFailed', text(root, 'Message') || 'complete failed');
	}
}

export async function abortMultipart(bucket: string, key: string, uploadId: string): Promise<void> {
	await signedFetch('DELETE', objectUrl(bucket, key, { uploadId }));
}

export async function multipartUpload(bucket: string, key: string, file: Blob, opts: UploadOptions = {}): Promise<void> {
	const contentType = opts.contentType || (file as File).type || undefined;
	const uploadId = await initiateMultipart(bucket, key, contentType);
	const total = Math.ceil(file.size / PART_SIZE);
	const sent: number[] = new Array(total).fill(0);
	const report = () => opts.onProgress?.(sent.reduce((a, b) => a + b, 0));
	const results: { n: number; etag: string }[] = [];
	let next = 0;
	let failed: unknown = null;

	const worker = async () => {
		while (next < total && !failed) {
			const i = next++;
			const n = i + 1;
			const blob = file.slice(i * PART_SIZE, Math.min(file.size, (i + 1) * PART_SIZE));
			try {
				const etag = await signedPut(objectUrl(bucket, key, { partNumber: String(n), uploadId }), blob, undefined, {
					signal: opts.signal,
					onProgress: (b) => {
						sent[i] = b;
						report();
					}
				});
				sent[i] = blob.size;
				report();
				results.push({ n, etag });
			} catch (e) {
				failed = e;
			}
		}
	};
	await Promise.all(Array.from({ length: Math.min(PART_CONCURRENCY, total) }, worker));
	if (failed) {
		await abortMultipart(bucket, key, uploadId).catch(() => undefined);
		throw failed;
	}
	await completeMultipart(bucket, key, uploadId, results);
}

/** Uploads a file, choosing single PUT or multipart by size. */
export async function uploadFile(bucket: string, key: string, file: Blob, opts: UploadOptions = {}): Promise<void> {
	if (file.size > MULTIPART_THRESHOLD) {
		await multipartUpload(bucket, key, file, opts);
	} else {
		await putObject(bucket, key, file, opts);
	}
}

/** Returns "Enabled", "Suspended" or "" (never enabled). */
export async function getBucketVersioning(bucket: string): Promise<string> {
	const res = await okOrThrow(await signedFetch('GET', objectUrl(bucket, '', { versioning: '' })));
	const root = parseXml(await res.text()).documentElement;
	return text(root, 'Status');
}
