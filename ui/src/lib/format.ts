const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

export function formatBytes(n: number | undefined | null): string {
	if (n === undefined || n === null || !Number.isFinite(n)) return '–';
	if (n < 1024) return `${n} B`;
	let v = n;
	let i = 0;
	while (v >= 1024 && i < UNITS.length - 1) {
		v /= 1024;
		i++;
	}
	return `${v < 10 ? v.toFixed(2) : v < 100 ? v.toFixed(1) : Math.round(v)} ${UNITS[i]}`;
}

export function formatNumber(n: number | undefined | null): string {
	if (n === undefined || n === null || !Number.isFinite(n)) return '–';
	return new Intl.NumberFormat().format(n);
}

const ZERO_TIME = '0001-01-01T00:00:00Z';

export function isZeroTime(s: string | undefined | null): boolean {
	return !s || s.startsWith('0001-01-01');
}

export function formatDate(s: string | undefined | null): string {
	if (isZeroTime(s) || s === ZERO_TIME) return '–';
	const d = new Date(s as string);
	if (Number.isNaN(d.getTime())) return s as string;
	return d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function formatRelative(s: string | undefined | null): string {
	if (isZeroTime(s)) return 'never';
	const d = new Date(s as string);
	if (Number.isNaN(d.getTime())) return s as string;
	const diff = (d.getTime() - Date.now()) / 1000;
	const abs = Math.abs(diff);
	const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
	if (abs < 60) return rtf.format(Math.round(diff), 'second');
	if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute');
	if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour');
	return rtf.format(Math.round(diff / 86400), 'day');
}

/** The last path component of a key or prefix, e.g. "a/b/c/" -> "c", "a/b.txt" -> "b.txt". */
export function baseName(key: string): string {
	const trimmed = key.endsWith('/') ? key.slice(0, -1) : key;
	const i = trimmed.lastIndexOf('/');
	return i >= 0 ? trimmed.slice(i + 1) : trimmed;
}

export function isValidBucketName(name: string): boolean {
	return /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(name) && !/\.\./.test(name) && !/^\d+\.\d+\.\d+\.\d+$/.test(name);
}
