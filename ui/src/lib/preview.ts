export type PreviewKind = 'none' | 'image' | 'video' | 'audio' | 'pdf' | 'text';

const TEXT_TYPES = new Set([
	'application/json',
	'application/xml',
	'application/javascript',
	'application/x-javascript',
	'application/typescript',
	'application/x-yaml',
	'application/yaml',
	'application/toml',
	'application/x-sh',
	'application/x-ndjson',
	'application/sql',
	'application/graphql',
	'application/ld+json',
	'application/xhtml+xml',
	'image/svg+xml'
]);

const TEXT_EXTENSIONS = new Set([
	'txt', 'md', 'markdown', 'csv', 'tsv', 'log', 'json', 'jsonl', 'ndjson', 'xml', 'yaml', 'yml', 'toml', 'ini', 'cfg', 'conf', 'env',
	'js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'css', 'scss', 'html', 'htm', 'svelte', 'vue', 'go', 'py', 'rb', 'rs', 'java', 'kt', 'c', 'h',
	'cpp', 'hpp', 'cs', 'php', 'sh', 'bash', 'zsh', 'fish', 'ps1', 'sql', 'graphql', 'gql', 'proto', 'dockerfile', 'makefile', 'gitignore',
	'editorconfig', 'lock', 'tf', 'hcl', 'nix', 'lua', 'r', 'swift', 'm', 'pl', 'ex', 'exs', 'erl', 'hs', 'scala', 'clj', 'dart', 'tex', 'bib',
	'rst', 'adoc', 'org', 'srt', 'vtt', 'ics', 'pem', 'crt', 'key', 'pub'
]);

/** Decides how the drawer previews an object from its key and content type. */
export function previewKind(key: string, contentType: string): PreviewKind {
	const type = (contentType || '').split(';')[0].trim().toLowerCase();
	if (type.startsWith('image/') && type !== 'image/svg+xml') return 'image';
	if (type.startsWith('video/')) return 'video';
	if (type.startsWith('audio/')) return 'audio';
	if (type === 'application/pdf') return 'pdf';
	if (type.startsWith('text/') || TEXT_TYPES.has(type)) return 'text';
	const name = key.split('/').pop() ?? '';
	const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : name.toLowerCase();
	if (type === '' || type === 'application/octet-stream' || type === 'binary/octet-stream') {
		if (TEXT_EXTENSIONS.has(ext)) return 'text';
		if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico'].includes(ext)) return 'image';
		if (['mp4', 'webm', 'mov', 'm4v'].includes(ext)) return 'video';
		if (['mp3', 'wav', 'ogg', 'oga', 'flac', 'm4a'].includes(ext)) return 'audio';
		if (ext === 'pdf') return 'pdf';
	}
	return 'none';
}
