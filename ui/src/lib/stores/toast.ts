import { writable } from 'svelte/store';

export type ToastKind = 'info' | 'success' | 'error';

export interface Toast {
	id: number;
	kind: ToastKind;
	message: string;
}

const DEFAULT_TTL_MS = 6000;
let seq = 0;

export const toasts = writable<Toast[]>([]);

export function pushToast(kind: ToastKind, message: string, ttl = DEFAULT_TTL_MS): number {
	const id = ++seq;
	toasts.update((list) => [...list, { id, kind, message }]);
	if (ttl > 0) setTimeout(() => dismissToast(id), ttl);
	return id;
}

export function dismissToast(id: number): void {
	toasts.update((list) => list.filter((t) => t.id !== id));
}

export const toastError = (e: unknown): number => pushToast('error', errorMessage(e));
export const toastSuccess = (message: string): number => pushToast('success', message);

export function errorMessage(e: unknown): string {
	if (e instanceof Error) return e.message;
	if (typeof e === 'string') return e;
	return 'Unexpected error';
}
