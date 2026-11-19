/**
 * Live integration test against a running hogshead node. Skipped unless
 * HOGSHEAD_URL, HOGSHEAD_AK and HOGSHEAD_SK are set, e.g.
 *
 *   HOGSHEAD_URL=http://127.0.0.1:9000 HOGSHEAD_AK=... HOGSHEAD_SK=... npm test
 *
 * It exercises exactly the request shapes the UI sends (header SigV4 with
 * UNSIGNED-PAYLOAD, presigned GETs, path-style URLs, XML bodies) using only
 * Node's fetch, so a failure here means the browser code would fail too.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { encodeKeyPath, presignUrl, signRequest } from './sigv4';

// Node globals without pulling in @types/node for a browser project.
declare const process: { env: Record<string, string | undefined> };

const URL_ = process.env.HOGSHEAD_URL;
const creds = { accessKeyId: process.env.HOGSHEAD_AK ?? '', secretAccessKey: process.env.HOGSHEAD_SK ?? '' };
const enabled = Boolean(URL_ && creds.accessKeyId && creds.secretAccessKey);

const BUCKET = `ui-smoke-${Date.now().toString(36)}`;
const KEY = 'my folder/ü ñ (1)+x.txt';

async function call(method: string, path: string, init: { headers?: Record<string, string>; body?: string | Uint8Array } = {}) {
	const url = new URL(URL_ + path);
	const headers: Record<string, string> = { ...(init.headers ?? {}) };
	const signed = await signRequest({ method, url, headers }, creds);
	const res = await fetch(url, { method, headers: signed.headers, body: init.body as BodyInit | undefined });
	const text = method === 'HEAD' ? '' : await res.text();
	return { res, text };
}

describe.skipIf(!enabled)('live node', () => {
	beforeAll(async () => {
		const { res, text } = await call('POST', '/_admin/v1/buckets', {
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ name: BUCKET })
		});
		expect(res.status, text).toBe(201);
	});

	afterAll(async () => {
		await call('DELETE', `/${BUCKET}`).catch(() => undefined);
		await call('DELETE', `/_admin/v1/buckets/${BUCKET}`);
	});

	it('GET /_admin/v1/me and /status', async () => {
		const me = await call('GET', '/_admin/v1/me');
		expect(me.res.status, me.text).toBe(200);
		expect(JSON.parse(me.text).user.role).toBe('admin');
		const st = await call('GET', '/_admin/v1/status');
		expect(st.res.status, st.text).toBe(200);
		expect(JSON.parse(st.text)).toHaveProperty('stats.buckets');
	});

	it('PUT, HEAD, list, presigned GET and DeleteObjects with an awkward key', async () => {
		const path = `/${BUCKET}/${encodeKeyPath(KEY)}`;
		const put = await call('PUT', path, { headers: { 'content-type': 'text/plain' }, body: 'hello' });
		expect(put.res.status, put.text).toBe(200);

		const head = await call('HEAD', path);
		expect(head.res.status).toBe(200);
		expect(head.res.headers.get('content-length')).toBe('5');

		const list = await call('GET', `/${BUCKET}?list-type=2&prefix=${encodeURIComponent('my folder/')}&delimiter=%2F&max-keys=10`);
		expect(list.res.status, list.text).toBe(200);
		expect(list.text).toContain('<Key>my folder/ü ñ (1)+x.txt</Key>');

		const top = await call('GET', `/${BUCKET}?list-type=2&delimiter=%2F`);
		expect(top.text).toContain('<Prefix>my folder/</Prefix>');

		const url = await presignUrl({ method: 'GET', url: URL_ + path, expires: 300 }, creds);
		const got = await fetch(url);
		expect(got.status, await got.clone().text()).toBe(200);
		expect(await got.text()).toBe('hello');

		const copy = await call('PUT', `/${BUCKET}/renamed.txt`, { headers: { 'x-amz-copy-source': `/${BUCKET}/${encodeKeyPath(KEY)}` } });
		expect(copy.res.status, copy.text).toBe(200);

		const del = await call('POST', `/${BUCKET}?delete`, {
			headers: { 'content-type': 'application/xml' },
			body: `<Delete><Quiet>true</Quiet><Object><Key>my folder/ü ñ (1)+x.txt</Key></Object><Object><Key>renamed.txt</Key></Object></Delete>`
		});
		expect(del.res.status, del.text).toBe(200);
		expect(del.text).not.toContain('<Error>');
	});

	it('multipart upload round trip', async () => {
		const path = `/${BUCKET}/big.bin`;
		const init = await call('POST', `${path}?uploads`);
		expect(init.res.status, init.text).toBe(200);
		const uploadId = /<UploadId>([^<]+)<\/UploadId>/.exec(init.text)?.[1];
		expect(uploadId).toBeTruthy();

		const part = new Uint8Array(6 * 1024 * 1024).fill(7);
		const up = await call('PUT', `${path}?partNumber=1&uploadId=${encodeURIComponent(uploadId!)}`, { body: part });
		expect(up.res.status, up.text).toBe(200);
		const etag = up.res.headers.get('etag');
		expect(etag).toBeTruthy();

		const done = await call('POST', `${path}?uploadId=${encodeURIComponent(uploadId!)}`, {
			headers: { 'content-type': 'application/xml' },
			body: `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>${etag}</ETag></Part></CompleteMultipartUpload>`
		});
		expect(done.res.status, done.text).toBe(200);
		expect(done.text).not.toContain('<Error>');

		const head = await call('HEAD', path);
		expect(head.res.headers.get('content-length')).toBe(String(part.length));

		const rm = await call('DELETE', path);
		expect(rm.res.status, rm.text).toBe(204);
	});

	it('versioning via S3 and admin', async () => {
		const v0 = await call('GET', `/${BUCKET}?versioning`);
		expect(v0.res.status, v0.text).toBe(200);
		const patch = await call('PATCH', `/_admin/v1/buckets/${BUCKET}`, {
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ versioning: 'Enabled' })
		});
		expect(patch.res.status, patch.text).toBe(200);
		const v1 = await call('GET', `/${BUCKET}?versioning`);
		expect(v1.text).toContain('<Status>Enabled</Status>');

		await call('PUT', `/${BUCKET}/v.txt`, { body: 'one' });
		await call('PUT', `/${BUCKET}/v.txt`, { body: 'two' });
		const versions = await call('GET', `/${BUCKET}?versions&prefix=v.txt`);
		expect(versions.res.status, versions.text).toBe(200);
		expect((versions.text.match(/<Version>/g) ?? []).length).toBe(2);
		const vid = /<VersionId>([^<]+)<\/VersionId>/.exec(versions.text)?.[1];
		const one = await call('GET', `/${BUCKET}/v.txt?versionId=${encodeURIComponent(vid!)}`);
		expect(one.res.status).toBe(200);

		// Clean up all versions so the bucket can be deleted.
		for (const m of versions.text.matchAll(/<VersionId>([^<]+)<\/VersionId>/g)) {
			await call('DELETE', `/${BUCKET}/v.txt?versionId=${encodeURIComponent(m[1])}`);
		}
	});
});
