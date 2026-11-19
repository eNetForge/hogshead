<script lang="ts">
	import { getObjectText, headObject, listObjectVersions, presignGet, putObject, PREVIEW_MAX_BYTES, type ObjectHead, type ObjectVersion } from '$lib/api/s3';
	import { formatBytes, formatDate } from '$lib/format';
	import { previewKind, type PreviewKind } from '$lib/preview';
	import { toastError, toastSuccess } from '$lib/stores/toast';
	import Icon from './Icon.svelte';
	import CopyButton from './CopyButton.svelte';

	let {
		bucket,
		objectKey,
		versioned,
		writable = false,
		editOnOpen = false,
		onclose,
		onchanged
	}: {
		bucket: string;
		objectKey: string;
		versioned: boolean;
		/** Enables the inline text editor. */
		writable?: boolean;
		/** Open straight into the editor when the object is editable text (used by "New file"). */
		editOnOpen?: boolean;
		onclose: () => void;
		/** Called after the object was overwritten from the editor. */
		onchanged?: () => void;
	} = $props();

	let head = $state<ObjectHead | null>(null);
	let versions = $state<ObjectVersion[]>([]);
	let loading = $state(true);
	let panel: HTMLElement | undefined = $state();

	// Preview state.
	let kind = $state<PreviewKind>('none');
	let mediaUrl = $state('');
	let text = $state('');
	let textLoading = $state(false);
	let tooLarge = $state(false);

	// Editor state.
	let editing = $state(false);
	let draft = $state('');
	let saving = $state(false);
	const dirty = $derived(editing && draft !== text);

	const SKIP = new Set(['date', 'server', 'connection', 'accept-ranges', 'x-amz-request-id', 'x-amz-id-2']);

	async function load(key: string) {
		loading = true;
		head = null;
		versions = [];
		editing = false;
		kind = 'none';
		mediaUrl = '';
		text = '';
		tooLarge = false;
		try {
			head = await headObject(bucket, key);
			if (versioned) {
				const r = await listObjectVersions(bucket, { prefix: key, maxKeys: 200 });
				versions = r.versions.filter((v) => v.key === key);
			}
			await loadPreview(key, head);
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	async function loadPreview(key: string, h: ObjectHead) {
		kind = previewKind(key, h.contentType);
		if (kind === 'none') return;
		if (kind === 'text') {
			if (h.contentLength > PREVIEW_MAX_BYTES) {
				tooLarge = true;
				return;
			}
			textLoading = true;
			try {
				text = await getObjectText(bucket, key);
			} finally {
				textLoading = false;
			}
			if (editOnOpen && writable) startEdit();
			return;
		}
		// Images, audio, video and PDFs load straight from a short-lived presigned URL.
		mediaUrl = await presignGet(bucket, key, 600);
	}

	function startEdit() {
		draft = text;
		editing = true;
	}

	function cancelEdit() {
		editing = false;
		draft = '';
	}

	async function save() {
		if (!head) return;
		saving = true;
		try {
			const type = head.contentType || 'text/plain; charset=utf-8';
			await putObject(bucket, objectKey, new Blob([draft], { type }), { contentType: type });
			text = draft;
			editing = false;
			toastSuccess(versioned ? 'Saved as a new version' : 'Saved');
			onchanged?.();
			head = await headObject(bucket, objectKey);
			if (versioned) {
				const r = await listObjectVersions(bucket, { prefix: objectKey, maxKeys: 200 });
				versions = r.versions.filter((v) => v.key === objectKey);
			}
		} catch (e) {
			toastError(e);
		} finally {
			saving = false;
		}
	}


	$effect(() => {
		load(objectKey);
	});

	$effect(() => {
		panel?.focus();
	});

	async function download(versionId?: string) {
		try {
			window.open(await presignGet(bucket, objectKey, 300, versionId), '_blank', 'noopener');
		} catch (e) {
			toastError(e);
		}
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && !editing) onclose();
		if (editing && (e.ctrlKey || e.metaKey) && e.key === 's') {
			e.preventDefault();
			if (dirty && !saving) save();
		}
	}
</script>

<div class="drawer" bind:this={panel} tabindex="-1" role="dialog" aria-label="Object details" {onkeydown}>
	<header>
		<h2 class="truncate" title={objectKey}>{objectKey.split('/').pop()}</h2>
		<button class="btn btn-ghost btn-icon" type="button" aria-label="Close details" onclick={onclose}><Icon name="close" /></button>
	</header>
	<div class="body">
		<div class="row" style="margin-bottom:1rem">
			<button class="btn btn-sm" type="button" onclick={() => download()}><Icon name="download" size={14} /> Download</button>
			<CopyButton text={objectKey} label="Copy key" small />
		</div>
		{#if loading}
			<p class="muted"><span class="spinner"></span> Loading…</p>
		{:else if head}
			{#if kind !== 'none'}
				<section class="preview" aria-label="Preview">
					{#if kind === 'image'}
						{#if mediaUrl}<img src={mediaUrl} alt={objectKey.split('/').pop()} />{/if}
					{:else if kind === 'video'}
						{#if mediaUrl}<video src={mediaUrl} controls preload="metadata"><track kind="captions" /></video>{/if}
					{:else if kind === 'audio'}
						{#if mediaUrl}<audio src={mediaUrl} controls preload="metadata"></audio>{/if}
					{:else if kind === 'pdf'}
						{#if mediaUrl}<iframe src={mediaUrl} title="PDF preview"></iframe>{/if}
					{:else if kind === 'text'}
						{#if tooLarge}
							<p class="muted small">Too large to preview inline ({formatBytes(head.contentLength)}, limit {formatBytes(PREVIEW_MAX_BYTES)}). Download it instead.</p>
						{:else if textLoading}
							<p class="muted"><span class="spinner"></span> Loading…</p>
						{:else if editing}
							<label for="editor" class="sr-only">Edit {objectKey}</label>
							<textarea id="editor" class="editor" bind:value={draft} spellcheck="false" disabled={saving}></textarea>
							<div class="row row-end" style="margin-top:0.5rem">
								<span class="muted small grow">{dirty ? 'Unsaved changes' : 'No changes'} · Ctrl+S saves{versioned ? ' as a new version' : ''}</span>
								<button class="btn btn-sm" type="button" onclick={cancelEdit} disabled={saving}>Cancel</button>
								<button class="btn btn-sm btn-primary" type="button" onclick={save} disabled={!dirty || saving}>
									{#if saving}<span class="spinner"></span>{/if} Save
								</button>
							</div>
						{:else}
							<pre class="text">{text}</pre>
							{#if writable}
								<div class="row row-end" style="margin-top:0.5rem">
									<button class="btn btn-sm" type="button" onclick={startEdit}><Icon name="edit" size={14} /> Edit</button>
								</div>
							{/if}
						{/if}
					{/if}
				</section>
			{/if}
			<h3>Properties</h3>
			<dl class="kv">
				<dt>Key</dt><dd class="mono">{objectKey}</dd>
				<dt>Size</dt><dd>{formatBytes(head.contentLength)}</dd>
				<dt>Content type</dt><dd>{head.contentType || '–'}</dd>
				<dt>ETag</dt><dd class="mono">{head.etag || '–'}</dd>
				<dt>Last modified</dt><dd>{head.lastModified ? formatDate(new Date(head.lastModified).toISOString()) : '–'}</dd>
				{#if head.versionId}<dt>Version</dt><dd class="mono">{head.versionId}</dd>{/if}
			</dl>
			{#if Object.keys(head.metadata).length > 0}
				<h3 style="margin-top:1rem">User metadata</h3>
				<dl class="kv">
					{#each Object.entries(head.metadata) as [k, v] (k)}
						<dt class="mono">{k}</dt><dd>{v}</dd>
					{/each}
				</dl>
			{/if}
			<h3 style="margin-top:1rem">Headers</h3>
			<dl class="kv small">
				{#each Object.entries(head.headers).filter(([k]) => !SKIP.has(k) && !k.startsWith('x-amz-meta-')) as [k, v] (k)}
					<dt class="mono">{k}</dt><dd class="mono">{v}</dd>
				{/each}
			</dl>
			{#if versioned}
				<h3 style="margin-top:1rem">Versions</h3>
				{#if versions.length === 0}
					<p class="muted small">No versions found.</p>
				{:else}
					<ul class="versions">
						{#each versions as v (v.versionId + v.key)}
							<li>
								<div>
									<span class="mono small">{v.versionId || 'null'}</span>
									{#if v.isLatest}<span class="badge badge-success">latest</span>{/if}
									{#if v.deleteMarker}<span class="badge badge-warn">delete marker</span>{/if}
									<div class="muted small">{formatDate(v.lastModified)} · {v.deleteMarker ? '' : formatBytes(v.size)}</div>
								</div>
								{#if !v.deleteMarker}
									<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Download version {v.versionId}" onclick={() => download(v.versionId)}>
										<Icon name="download" size={14} />
									</button>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			{/if}
		{/if}
	</div>
</div>

<style>
	.drawer {
		position: fixed;
		top: 0;
		right: 0;
		bottom: 0;
		width: min(440px, 100vw);
		background: var(--surface);
		border-left: 1px solid var(--border);
		box-shadow: var(--shadow);
		z-index: 40;
		display: flex;
		flex-direction: column;
		outline: none;
	}
	header {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.75rem 1rem;
		border-bottom: 1px solid var(--border);
	}
	header h2 {
		margin: 0;
		flex: 1;
		min-width: 0;
	}
	.body {
		padding: 1rem;
		overflow: auto;
	}
	.preview {
		margin-bottom: 1rem;
	}
	.preview img,
	.preview video {
		display: block;
		max-width: 100%;
		max-height: 320px;
		margin: 0 auto;
		border-radius: var(--radius);
		background: var(--surface-2);
	}
	.preview audio {
		width: 100%;
	}
	.preview iframe {
		width: 100%;
		height: 360px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--surface-2);
	}
	.text {
		max-height: 320px;
		overflow: auto;
		margin: 0;
		padding: 0.6rem 0.75rem;
		font-family: var(--mono);
		font-size: 0.8rem;
		line-height: 1.45;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--surface-2);
		white-space: pre-wrap;
		word-break: break-word;
	}
	.editor {
		min-height: 320px;
		font-size: 0.8rem;
		line-height: 1.45;
	}
	.versions {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.versions li {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 0.5rem;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--border);
	}
</style>
