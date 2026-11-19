/** Small helpers for S3's XML responses, namespace-agnostic. */

export function parseXml(text: string): Document {
	const doc = new DOMParser().parseFromString(text, 'application/xml');
	const err = doc.getElementsByTagName('parsererror')[0];
	if (err) throw new Error('Malformed XML response');
	return doc;
}

export function children(el: ParentNode | null | undefined, name: string): Element[] {
	if (!el) return [];
	return Array.from(el.children).filter((c) => c.localName === name);
}

export function child(el: ParentNode | null | undefined, name: string): Element | undefined {
	return children(el, name)[0];
}

export function text(el: ParentNode | null | undefined, name: string, fallback = ''): string {
	return child(el, name)?.textContent ?? fallback;
}

export function escapeXml(s: string): string {
	return s.replace(/[<>&'"]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;', "'": '&apos;', '"': '&quot;' })[c] as string);
}
