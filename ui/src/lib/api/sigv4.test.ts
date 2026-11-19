import { describe, expect, it } from 'vitest';
import { amzDate, canonicalQuery, canonicalUri, encodeKeyPath, encodeRfc3986, presignUrl, signRequest } from './sigv4';

const creds = { accessKeyId: 'AKIAIOSFODNN7EXAMPLE', secretAccessKey: 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY' };
const EMPTY = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';

describe('signRequest', () => {
	it('matches the AWS S3 GET object example', async () => {
		const r = await signRequest(
			{
				method: 'GET',
				url: 'https://examplebucket.s3.amazonaws.com/test.txt',
				headers: { Range: 'bytes=0-9', 'x-amz-content-sha256': EMPTY, 'x-amz-date': '20130524T000000Z' }
			},
			creds
		);
		expect(r.signature).toBe('f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41');
		expect(r.headers.authorization).toBe(
			'AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41'
		);
		expect(r.headers.host).toBeUndefined();
		expect(r.canonicalRequest).toBe(
			[
				'GET',
				'/test.txt',
				'',
				'host:examplebucket.s3.amazonaws.com',
				'range:bytes=0-9',
				`x-amz-content-sha256:${EMPTY}`,
				'x-amz-date:20130524T000000Z',
				'',
				'host;range;x-amz-content-sha256;x-amz-date',
				EMPTY
			].join('\n')
		);
	});

	it('matches the AWS S3 list objects example (query string)', async () => {
		const r = await signRequest(
			{
				method: 'GET',
				url: 'https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J',
				headers: { 'x-amz-content-sha256': EMPTY, 'x-amz-date': '20130524T000000Z' }
			},
			creds
		);
		expect(r.signature).toBe('34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7');
	});

	it('defaults to UNSIGNED-PAYLOAD and a current x-amz-date', async () => {
		const now = new Date('2024-02-03T04:05:06Z');
		const r = await signRequest({ method: 'PUT', url: 'http://localhost:9000/b/k' }, creds, { now });
		expect(r.headers['x-amz-content-sha256']).toBe('UNSIGNED-PAYLOAD');
		expect(r.headers['x-amz-date']).toBe('20240203T040506Z');
		expect(r.canonicalRequest).toContain('host:localhost:9000\n');
	});
});

describe('canonical query', () => {
	it('sorts by name then value and encodes with RFC 3986', () => {
		const q = canonicalQuery([
			['prefix', 'a b/c'],
			['list-type', '2'],
			['delimiter', '/'],
			['continuation-token', 'x+y=z'],
			['A', '2'],
			['A', '1'],
			['versions', '']
		]);
		expect(q).toBe('A=1&A=2&continuation-token=x%2By%3Dz&delimiter=%2F&list-type=2&prefix=a%20b%2Fc&versions=');
	});

	it('round-trips through URLSearchParams', () => {
		const u = new URL('http://h/b?uploads&prefix=%C3%BC');
		expect(canonicalQuery(u.searchParams)).toBe('prefix=%C3%BC&uploads=');
	});
});

describe('canonical URI', () => {
	it('encodes spaces, unicode and reserved characters per segment', () => {
		expect(encodeKeyPath('my folder/ü ñ/file (1)+x.txt')).toBe('my%20folder/%C3%BC%20%C3%B1/file%20%281%29%2Bx.txt');
		expect(encodeRfc3986('a/b', true)).toBe('a/b');
		expect(encodeRfc3986('a/b')).toBe('a%2Fb');
		expect(encodeRfc3986('-_.~')).toBe('-_.~');
	});

	it('does not double-encode an already encoded path', () => {
		const encoded = '/bucket/' + encodeKeyPath('my folder/ü.txt');
		expect(canonicalUri(encoded)).toBe('/bucket/my%20folder/%C3%BC.txt');
		expect(canonicalUri('/bucket/my%20folder/%C3%BC.txt')).toBe('/bucket/my%20folder/%C3%BC.txt');
		// URL objects leave some reserved characters bare; they must still be encoded once.
		const u = new URL("http://h/bucket/it's (1)+two.txt");
		expect(canonicalUri(u.pathname)).toBe('/bucket/it%27s%20%281%29%2Btwo.txt');
		expect(canonicalUri('')).toBe('/');
		expect(canonicalUri('/')).toBe('/');
	});
});

describe('presignUrl', () => {
	it('matches the AWS presigned GET example', async () => {
		const url = await presignUrl(
			{ method: 'GET', url: 'https://examplebucket.s3.amazonaws.com/test.txt', expires: 86400 },
			creds,
			{ now: new Date('2013-05-24T00:00:00Z') }
		);
		expect(url).toBe(
			'https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404'
		);
	});

	it('rejects out-of-range expiry', async () => {
		await expect(presignUrl({ method: 'GET', url: 'http://h/b/k', expires: 0 }, creds)).rejects.toThrow();
		await expect(presignUrl({ method: 'GET', url: 'http://h/b/k', expires: 700000 }, creds)).rejects.toThrow();
	});
});

describe('amzDate', () => {
	it('formats without separators or millis', () => {
		expect(amzDate(new Date('2013-05-24T00:00:00.123Z'))).toBe('20130524T000000Z');
	});
});
