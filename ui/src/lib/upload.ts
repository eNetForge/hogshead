import { uploadFile } from '$lib/api/s3';

export type UploadStatus = 'queued' | 'uploading' | 'done' | 'error' | 'aborted';

export interface UploadItem {
	id: number;
	name: string;
	key: string;
	size: number;
	sent: number;
	status: UploadStatus;
	error?: string;
	controller: AbortController;
}

let seq = 0;

export function newUploadItem(file: File, key: string): UploadItem {
	return { id: ++seq, name: file.name, key, size: file.size, sent: 0, status: 'queued', controller: new AbortController() };
}

/**
 * Runs one upload and reports immutable state updates through `update`.
 * Resolves when finished regardless of outcome; the final item carries the status.
 */
export async function runUpload(bucket: string, file: File, item: UploadItem, update: (next: UploadItem) => void): Promise<UploadItem> {
	let cur: UploadItem = { ...item, status: 'uploading' };
	update(cur);
	try {
		await uploadFile(bucket, item.key, file, {
			signal: item.controller.signal,
			onProgress: (sent) => {
				cur = { ...cur, sent: Math.min(sent, item.size) };
				update(cur);
			}
		});
		cur = { ...cur, sent: item.size, status: 'done' };
	} catch (e) {
		const aborted = e instanceof DOMException && e.name === 'AbortError';
		cur = { ...cur, status: aborted ? 'aborted' : 'error', error: aborted ? undefined : e instanceof Error ? e.message : String(e) };
	}
	update(cur);
	return cur;
}
