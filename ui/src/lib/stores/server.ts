import { writable } from 'svelte/store';

/** The server's configured name, loaded from /status for administrators. Empty when unknown. */
export const serverName = writable('');
