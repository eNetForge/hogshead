import { describe, expect, it } from 'vitest';
import { hmacJs, sha256Js } from './sigv4';

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
const enc = new TextEncoder();

describe('pure JS SHA-256 fallback', () => {
	it('matches FIPS test vectors', () => {
		expect(hex(sha256Js(enc.encode('')))).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
		expect(hex(sha256Js(enc.encode('abc')))).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
		expect(hex(sha256Js(enc.encode('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq')))).toBe(
			'248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1'
		);
		// 1,000,000 x 'a' crosses many blocks and exercises the length encoding.
		expect(hex(sha256Js(new Uint8Array(1_000_000).fill(0x61)))).toBe('cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0');
		// 55/56/64 byte messages hit the padding boundaries.
		for (const n of [55, 56, 63, 64, 65]) {
			expect(hex(sha256Js(new Uint8Array(n)))).toHaveLength(64);
		}
	});

	it('matches RFC 4231 HMAC vectors', () => {
		expect(hex(hmacJs(new Uint8Array(20).fill(0x0b), enc.encode('Hi There')))).toBe(
			'b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7'
		);
		expect(hex(hmacJs(enc.encode('Jefe'), enc.encode('what do ya want for nothing?')))).toBe(
			'5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843'
		);
		// Key longer than a block is hashed first.
		expect(hex(hmacJs(new Uint8Array(131).fill(0xaa), enc.encode('Test Using Larger Than Block-Size Key - Hash Key First')))).toBe(
			'60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54'
		);
	});
});
