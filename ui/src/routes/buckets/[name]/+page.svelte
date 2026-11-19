<script lang="ts">
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { untrack } from 'svelte';
	import { admin } from '$lib/api/admin';
	import { createFolder, deleteObject, deleteObjects, getBucketVersioning, listObjects, presignGet, renameObject, type S3Object, putObject, copyObjectVersion, listObjectVersions, type ObjectVersion } from '$lib/api/s3';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import ObjectDrawer from '$lib/components/ObjectDrawer.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import UploadPanel from '$lib/components/UploadPanel.svelte';
	import { baseName, formatBytes, formatDate } from '$lib/format';
	import { canWrite, isAdmin, session } from '$lib/stores/session';
	import { pushToast, toastError, toastSuccess } from '$lib/stores/toast';
	import { newUploadItem, runUpload, type UploadItem } from '$lib/upload';

	const PAGE_SIZE = 200;
	const DELIMITER = '/';

	const bucket = $derived(page.params.name ?? '');
	const prefix = $derived(page.url.searchParams.get('prefix') ?? '');
	const writable = $derived(canWrite($session));

	let prefixes = $state<string[]>([]);
	let objects = $state<S3Object[]>([]);
	let nextToken = $state<string | undefined>();
	let loading = $state(false);
	let versioned = $state(false);
	let search = $state('');
	let selected = $state<Set<string>>(new Set());
	let uploads = $state<UploadItem[]>([]);

	let detailKey = $state<string | null>(null);
	let shareKey = $state<string | null>(null);
	let shareOpen = $state(false);
	let renameOpen = $state(false);
	let renameFrom = $state('');
	let renameTo = $state('');
	let folderOpen = $state(false);
	let folderName = $state('');
	let deleteOpen = $state(false);
	let deleteKeys = $state<string[]>([]);
	let busy = $state(false);
	let fileInput: HTMLInputElement | undefined = $state();
	let dragging = $state(false);
	let dragDepth = 0;
	let newFileOpen = $state(false);
	let newFileName = $state('');
	let editKey = $state<string | null>(null);

	// Versions view: every version and delete marker under the prefix.
	let showVersions = $state(false);
	let versions = $state<ObjectVersion[]>([]);
	let versionMarkers = $state<{ key: string; vid: string } | undefined>();
	let versionAction = $state<{ kind: 'purge' | 'undelete' | 'restore'; v: ObjectVersion } | null>(null);

	const crumbs = $derived.by(() => {
		const parts = prefix.split('/').filter(Boolean);
		return parts.map((p, i) => ({ label: p, prefix: parts.slice(0, i + 1).join('/') + '/' }));
	});
	const listPrefix = $derived(prefix + search.trim());
	const allSelected = $derived(objects.length > 0 && objects.every((o) => selected.has(o.key)));

	// The placeholder object for the current folder itself is hidden.
	const visibleObjects = $derived(objects.filter((o) => o.key !== prefix));

	async function load(reset = true) {
		loading = true;
		try {
			if (showVersions) {
				const r = await listObjectVersions(bucket, {
					prefix: listPrefix,
					delimiter: DELIMITER,
					maxKeys: PAGE_SIZE,
					keyMarker: reset ? undefined : versionMarkers?.key,
					versionIdMarker: reset ? undefined : versionMarkers?.vid
				});
				prefixes = reset ? r.prefixes : [...prefixes, ...r.prefixes];
				versions = reset ? r.versions : [...versions, ...r.versions];
				versionMarkers = r.truncated && r.nextKeyMarker ? { key: r.nextKeyMarker, vid: r.nextVersionIdMarker ?? '' } : undefined;
				nextToken = versionMarkers ? 'more' : undefined;
				objects = [];
			} else {
				const r = await listObjects(bucket, { prefix: listPrefix, delimiter: DELIMITER, maxKeys: PAGE_SIZE, token: reset ? undefined : nextToken });
				prefixes = reset ? r.prefixes : [...prefixes, ...r.prefixes];
				objects = reset ? r.objects : [...objects, ...r.objects];
				nextToken = r.truncated ? r.nextToken : undefined;
				versions = [];
			}
			if (reset) selected = new Set();
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	function toggleVersions() {
		showVersions = !showVersions;
		void load(true);
	}

	const visibleVersions = $derived(versions.filter((v) => v.key !== prefix));

	async function downloadVersion(v: ObjectVersion) {
		try {
			window.open(await presignGet(bucket, v.key, 300, v.versionId), '_blank', 'noopener');
		} catch (e) {
			toastError(e);
		}
	}

	async function confirmVersionAction() {
		if (!versionAction) return;
		const { kind, v } = versionAction;
		busy = true;
		try {
			switch (kind) {
				case 'purge':
					await deleteObject(bucket, v.key, v.versionId);
					toastSuccess(v.deleteMarker ? 'Delete marker removed' : 'Version permanently deleted');
					break;
				case 'undelete':
					await deleteObject(bucket, v.key, v.versionId);
					toastSuccess(`${baseName(v.key)} is visible again`);
					break;
				case 'restore':
					await copyObjectVersion(bucket, v.key, v.versionId, v.key);
					toastSuccess(`Restored ${baseName(v.key)} as the current version`);
					break;
			}
			versionAction = null;
			await load(true);
		} catch (e) {
			toastError(e);
		} finally {
			busy = false;
		}
	}

	function shortId(id: string): string {
		return id.length > 12 ? id.slice(0, 12) + '…' : id;
	}

	async function loadVersioning() {
		try {
			if (isAdmin($session)) {
				versioned = (await admin.buckets.get(bucket)).versioning === 'Enabled';
			} else {
				versioned = (await getBucketVersioning(bucket)) === 'Enabled';
			}
		} catch {
			versioned = false;
		}
	}

	let searchTimer: ReturnType<typeof setTimeout> | undefined;
	$effect(() => {
		// Reload on bucket/prefix change (and debounced search changes).
		void bucket;
		void listPrefix;
		clearTimeout(searchTimer);
		searchTimer = setTimeout(() => untrack(() => load(true)), untrack(() => search) ? 250 : 0);
		return () => clearTimeout(searchTimer);
	});

	$effect(() => {
		void bucket;
		untrack(loadVersioning);
	});

	function navigate(p: string) {
		search = '';
		detailKey = null;
		goto(`${base}/buckets/${encodeURIComponent(bucket)}${p ? `?prefix=${encodeURIComponent(p)}` : ''}`);
	}

	function toggle(key: string) {
		const next = new Set(selected);
		if (next.has(key)) next.delete(key);
		else next.add(key);
		selected = next;
	}

	function toggleAll() {
		selected = allSelected ? new Set() : new Set(visibleObjects.map((o) => o.key));
	}

	async function download(key: string) {
		try {
			window.open(await presignGet(bucket, key, 300), '_blank', 'noopener');
		} catch (e) {
			toastError(e);
		}
	}

	function share(key: string) {
		shareKey = key;
		shareOpen = true;
	}

	function askDelete(keys: string[]) {
		deleteKeys = keys;
		deleteOpen = true;
	}

	async function confirmDelete() {
		busy = true;
		try {
			if (deleteKeys.length === 1) {
				await deleteObject(bucket, deleteKeys[0]);
			} else {
				const errs = await deleteObjects(bucket, deleteKeys);
				for (const e of errs) pushToast('error', `${e.key}: ${e.message || e.code}`);
			}
			toastSuccess(`Deleted ${deleteKeys.length} object${deleteKeys.length === 1 ? '' : 's'}`);
			deleteOpen = false;
			if (detailKey && deleteKeys.includes(detailKey)) detailKey = null;
			await load(true);
		} catch (e) {
			toastError(e);
		} finally {
			busy = false;
		}
	}

	function askRename(key: string) {
		renameFrom = key;
		renameTo = baseName(key);
		renameOpen = true;
	}

	async function confirmRename(e: SubmitEvent) {
		e.preventDefault();
		const to = prefix + renameTo.trim();
		if (!renameTo.trim() || renameTo.includes('/') || to === renameFrom) return;
		busy = true;
		try {
			await renameObject(bucket, renameFrom, to);
			toastSuccess(`Renamed to ${renameTo}`);
			renameOpen = false;
			if (detailKey === renameFrom) detailKey = to;
			await load(true);
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	async function confirmFolder(e: SubmitEvent) {
		e.preventDefault();
		const name = folderName.trim().replace(/^\/+|\/+$/g, '');
		if (!name) return;
		busy = true;
		try {
			await createFolder(bucket, prefix + name);
			toastSuccess(`Folder ${name} created`);
			folderOpen = false;
			folderName = '';
			await load(true);
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	function patchUpload(next: UploadItem) {
		uploads = uploads.map((u) => (u.id === next.id ? next : u));
	}

	async function uploadFiles(files: File[]) {
		if (files.length === 0) return;
		// Files dropped from a folder carry a relative path; keep it under the current prefix.
		const items = files.map((f) => newUploadItem(f, prefix + ((f as File & { relativePath?: string }).relativePath ?? f.name)));
		uploads = [...uploads, ...items];
		const results = await Promise.all(items.map((item, i) => runUpload(bucket, files[i], item, patchUpload)));
		const ok = results.filter((r) => r.status === 'done').length;
		if (ok > 0) toastSuccess(`Uploaded ${ok} file${ok === 1 ? '' : 's'}`);
		await load(true);
	}

	function onFiles(e: Event) {
		const input = e.currentTarget as HTMLInputElement;
		const files = Array.from(input.files ?? []);
		input.value = '';
		void uploadFiles(files);
	}

	// Drag and drop. Directory entries are walked so a dropped folder keeps its layout.
	function hasFiles(e: DragEvent): boolean {
		return Array.from(e.dataTransfer?.types ?? []).includes('Files');
	}

	function onDragEnter(e: DragEvent) {
		if (!writable || !hasFiles(e)) return;
		e.preventDefault();
		dragDepth++;
		dragging = true;
	}

	function onDragOver(e: DragEvent) {
		if (!writable || !hasFiles(e)) return;
		e.preventDefault();
		if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
	}

	function onDragLeave(e: DragEvent) {
		if (!dragging) return;
		e.preventDefault();
		dragDepth = Math.max(0, dragDepth - 1);
		if (dragDepth === 0) dragging = false;
	}

	async function onDrop(e: DragEvent) {
		if (!writable || !hasFiles(e)) return;
		e.preventDefault();
		dragDepth = 0;
		dragging = false;
		const files = await filesFromDrop(e.dataTransfer);
		await uploadFiles(files);
	}

	type EntryFile = File & { relativePath?: string };

	async function filesFromDrop(dt: DataTransfer | null): Promise<EntryFile[]> {
		if (!dt) return [];
		const items = Array.from(dt.items ?? []);
		const entries = items.map((i) => (i.webkitGetAsEntry ? i.webkitGetAsEntry() : null));
		if (entries.length === 0 || entries.every((e) => e === null)) return Array.from(dt.files);
		const out: EntryFile[] = [];
		for (const entry of entries) {
			if (entry) await walkEntry(entry, '', out);
		}
		return out;
	}

	async function walkEntry(entry: FileSystemEntry, dir: string, out: EntryFile[]): Promise<void> {
		if (entry.isFile) {
			const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject));
			const tagged = file as EntryFile;
			tagged.relativePath = dir + file.name;
			out.push(tagged);
			return;
		}
		if (entry.isDirectory) {
			const reader = (entry as FileSystemDirectoryEntry).createReader();
			for (;;) {
				const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
				if (batch.length === 0) break;
				for (const child of batch) await walkEntry(child, dir + entry.name + '/', out);
			}
		}
	}

	async function confirmNewFile(e: SubmitEvent) {
		e.preventDefault();
		const name = newFileName.trim();
		if (!name || name.endsWith('/')) return void toastError('Enter a file name');
		busy = true;
		try {
			const key = prefix + name;
			await putObject(bucket, key, new Blob([''], { type: 'text/plain; charset=utf-8' }), { contentType: 'text/plain; charset=utf-8' });
			newFileOpen = false;
			newFileName = '';
			await load(true);
			editKey = key;
			detailKey = key;
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{bucket} · hogshead</title></svelte:head>

<div class="page-header">
	<nav class="crumbs" aria-label="Breadcrumb">
		<a href="{base}/buckets">Buckets</a>
		<Icon name="chevron-right" size={14} />
		<button class="crumb" type="button" onclick={() => navigate('')} aria-current={prefix === '' ? 'page' : undefined}>{bucket}</button>
		{#each crumbs as c (c.prefix)}
			<Icon name="chevron-right" size={14} />
			<button class="crumb" type="button" onclick={() => navigate(c.prefix)} aria-current={c.prefix === prefix ? 'page' : undefined}>{c.label}</button>
		{/each}
	</nav>
	<div class="row">
		{#if versioned}
			<span class="badge badge-success">versioned</span>
			<button class="btn btn-sm" class:btn-primary={showVersions} type="button" onclick={toggleVersions} aria-pressed={showVersions} title="List every version and delete marker, including deleted files">
				<Icon name="audit" size={14} /> {showVersions ? 'Hide versions' : 'Show all versions'}
			</button>
		{/if}
		{#if isAdmin($session)}
			<a class="btn btn-sm" href="{base}/buckets/{encodeURIComponent(bucket)}/settings"><Icon name="settings" size={14} /> Settings</a>
		{/if}
	</div>
</div>

<UploadPanel items={uploads} onclear={() => (uploads = uploads.filter((u) => u.status === 'queued' || u.status === 'uploading'))} />

<div class="toolbar">
	<div class="search">
		<label class="sr-only" for="search">Filter by name prefix</label>
		<Icon name="search" size={16} />
		<input id="search" type="search" placeholder="Filter by prefix…" bind:value={search} />
	</div>
	<div class="grow"></div>
	{#if selected.size > 0}
		<span class="muted small">{selected.size} selected</span>
		<button class="btn btn-sm btn-danger" type="button" onclick={() => askDelete([...selected])} disabled={!writable}>
			<Icon name="trash" size={14} /> Delete selected
		</button>
	{/if}
	<button class="btn btn-sm" type="button" onclick={() => load(true)} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
	{#if writable}
		<button class="btn btn-sm" type="button" onclick={() => (folderOpen = true)}><Icon name="folder" size={14} /> New folder</button>
		<button class="btn btn-sm" type="button" onclick={() => (newFileOpen = true)}><Icon name="file" size={14} /> New file</button>
		<button class="btn btn-sm btn-primary" type="button" onclick={() => fileInput?.click()}><Icon name="upload" size={14} /> Upload</button>
		<input bind:this={fileInput} type="file" multiple class="sr-only" onchange={onFiles} aria-label="Choose files to upload" tabindex="-1" />
	{/if}
</div>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="table-wrap dropzone" class:dragging ondragenter={onDragEnter} ondragover={onDragOver} ondragleave={onDragLeave} ondrop={onDrop}>
	{#if dragging}
		<div class="drop-hint" aria-live="polite"><Icon name="upload" size={28} /> Drop to upload into <strong>{bucket}{prefix ? '/' + prefix : ''}</strong></div>
	{/if}
	<table class="objects">
		<thead>
			<tr>
				<th style="width:2rem">
					<input type="checkbox" checked={allSelected} onchange={toggleAll} aria-label="Select all objects" disabled={visibleObjects.length === 0} />
				</th>
				<th>Name</th>
				<th class="num">Size</th>
				<th>Last modified</th>
				<th></th>
			</tr>
		</thead>
		<tbody>
			{#each prefixes as p (p)}
				<tr>
					<td></td>
					<td>
						<button class="name folder" type="button" onclick={() => navigate(p)}>
							<Icon name="folder" size={16} />
							{baseName(p)}/
						</button>
					</td>
					<td class="num muted">–</td>
					<td class="muted">–</td>
					<td class="actions"></td>
				</tr>
			{/each}
			{#if showVersions}
				{#each visibleVersions as v (v.key + '@' + v.versionId)}
					<tr class:deleted={v.deleteMarker}>
						<td></td>
						<td>
							<button class="name" type="button" onclick={() => (detailKey = v.key)} title={v.versionId}>
								<Icon name={v.deleteMarker ? 'trash' : 'file'} size={16} />
								<span class="truncate">{baseName(v.key)}</span>
							</button>
							<div class="small version-meta">
								{#if v.deleteMarker}
									<span class="badge badge-warn">delete marker</span>
								{:else if v.isLatest}
									<span class="badge badge-success">current</span>
								{:else}
									<span class="badge">old version</span>
								{/if}
								<span class="mono muted">{shortId(v.versionId)}</span>
							</div>
						</td>
						<td class="num">{v.deleteMarker ? '–' : formatBytes(v.size)}</td>
						<td class="small">{formatDate(v.lastModified)}</td>
						<td class="actions">
							{#if !v.deleteMarker}
								<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Download this version of {baseName(v.key)}" title="Download this version" onclick={() => downloadVersion(v)}><Icon name="download" size={15} /></button>
							{/if}
							{#if writable}
								{#if v.deleteMarker && v.isLatest}
									<button class="btn btn-sm btn-ghost" type="button" title="Remove the delete marker so the previous version is current again" onclick={() => (versionAction = { kind: 'undelete', v })}>Undelete</button>
								{:else if !v.deleteMarker && !v.isLatest}
									<button class="btn btn-sm btn-ghost" type="button" title="Copy this version on top as the new current version" onclick={() => (versionAction = { kind: 'restore', v })}>Restore</button>
								{/if}
								<button class="btn btn-sm btn-ghost btn-icon danger" type="button" aria-label="Permanently delete this version of {baseName(v.key)}" title="Permanently delete this version" onclick={() => (versionAction = { kind: 'purge', v })}><Icon name="trash" size={15} /></button>
							{/if}
						</td>
					</tr>
				{/each}
				{#if !loading && prefixes.length === 0 && visibleVersions.length === 0}
					<tr><td colspan="5" class="empty">{search ? 'Nothing matches this prefix.' : 'No versions here.'}</td></tr>
				{/if}
			{/if}
			{#each visibleObjects as o (o.key)}
				<tr class:selected={selected.has(o.key)}>
					<td><input type="checkbox" checked={selected.has(o.key)} onchange={() => toggle(o.key)} aria-label="Select {baseName(o.key)}" /></td>
					<td>
						<button class="name" type="button" onclick={() => (detailKey = o.key)} title="ETag: {o.etag}">
							<Icon name="file" size={16} />
							<span class="truncate">{baseName(o.key)}</span>
						</button>
					</td>
					<td class="num">{formatBytes(o.size)}</td>
					<td class="small">{formatDate(o.lastModified)}</td>
					<td class="actions">
						<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Download {baseName(o.key)}" title="Download" onclick={() => download(o.key)}><Icon name="download" size={15} /></button>
						<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Share {baseName(o.key)}" title="Share" onclick={() => share(o.key)}><Icon name="share" size={15} /></button>
						{#if writable}
							<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Rename {baseName(o.key)}" title="Rename" onclick={() => askRename(o.key)}><Icon name="edit" size={15} /></button>
							<button class="btn btn-sm btn-ghost btn-icon danger" type="button" aria-label="Delete {baseName(o.key)}" title="Delete" onclick={() => askDelete([o.key])}><Icon name="trash" size={15} /></button>
						{/if}
					</td>
				</tr>
			{/each}
			{#if !showVersions && !loading && prefixes.length === 0 && visibleObjects.length === 0}
				<tr><td colspan="5" class="empty">{search ? 'Nothing matches this prefix.' : 'This folder is empty.'}</td></tr>
			{/if}
			{#if loading}
				<tr><td colspan="5" class="empty"><span class="spinner"></span></td></tr>
			{/if}
		</tbody>
	</table>
</div>
{#if nextToken}
	<div class="row" style="justify-content:center;margin-top:1rem">
		<button class="btn" type="button" onclick={() => load(false)} disabled={loading}>Load more</button>
	</div>
{/if}

{#if detailKey}
	<ObjectDrawer {bucket} objectKey={detailKey} {versioned} {writable} editOnOpen={editKey === detailKey} onclose={() => ((detailKey = null), (editKey = null))} onchanged={() => load(true)} />
{/if}

{#if shareKey}
	<ShareDialog bind:open={shareOpen} {bucket} objectKey={shareKey} />
{/if}

<ConfirmDialog
	bind:open={deleteOpen}
	title="Delete {deleteKeys.length === 1 ? 'object' : `${deleteKeys.length} objects`}"
	message={deleteKeys.length === 1 ? `Delete "${deleteKeys[0]}"? ${versioned ? 'A delete marker will be created.' : 'This cannot be undone.'}` : `Delete ${deleteKeys.length} objects? ${versioned ? 'Delete markers will be created.' : 'This cannot be undone.'}`}
	{busy}
	onconfirm={confirmDelete}
/>

<ConfirmDialog
	open={versionAction !== null}
	title={versionAction?.kind === 'purge' ? 'Permanently delete version' : versionAction?.kind === 'undelete' ? 'Undelete object' : 'Restore version'}
	message={versionAction?.kind === 'purge'
		? `Permanently delete ${versionAction.v.deleteMarker ? 'the delete marker' : 'this version'} of "${versionAction.v.key}"? This cannot be undone.`
		: versionAction?.kind === 'undelete'
			? `Remove the delete marker on "${versionAction.v.key}" so the previous version becomes current again?`
			: `Make this version of "${versionAction?.v.key ?? ''}" the current one? A copy of it is written as a new version; nothing is lost.`}
	confirmLabel={versionAction?.kind === 'purge' ? 'Delete permanently' : versionAction?.kind === 'undelete' ? 'Undelete' : 'Restore'}
	danger={versionAction?.kind === 'purge'}
	{busy}
	onconfirm={confirmVersionAction}
/>

<Dialog bind:open={renameOpen} title="Rename object">
	<form id="rename-form" onsubmit={confirmRename}>
		<p class="muted small" style="margin-top:0">Renaming copies the object to the new key and deletes the old one.</p>
		<div class="field">
			<label for="rename-to">New name</label>
			<input id="rename-to" type="text" bind:value={renameTo} required />
			<div class="hint mono">{prefix}{renameTo}</div>
		</div>
	</form>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (renameOpen = false)} disabled={busy}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="rename-form" disabled={busy || !renameTo.trim() || renameTo.includes('/')}>Rename</button>
	{/snippet}
</Dialog>

<Dialog bind:open={folderOpen} title="New folder">
	<form id="folder-form" onsubmit={confirmFolder}>
		<div class="field">
			<label for="folder-name">Folder name</label>
			<input id="folder-name" type="text" bind:value={folderName} required />
			<div class="hint">Creates an empty <span class="mono">{prefix}{folderName || 'name'}/</span> placeholder object.</div>
		</div>
	</form>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (folderOpen = false)} disabled={busy}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="folder-form" disabled={busy || !folderName.trim()}>Create</button>
	{/snippet}
</Dialog>

<Dialog bind:open={newFileOpen} title="New file">
	<form id="newfile-form" onsubmit={confirmNewFile}>
		<div class="field">
			<label for="newfile-name">File name</label>
			<input id="newfile-name" type="text" bind:value={newFileName} placeholder="notes.md" required />
			<div class="hint">Creates an empty text object at <span class="mono">{prefix}{newFileName || 'name'}</span> and opens it in the editor.</div>
		</div>
	</form>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (newFileOpen = false)} disabled={busy}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="newfile-form" disabled={busy || !newFileName.trim()}>Create</button>
	{/snippet}
</Dialog>

<style>
	.crumbs {
		display: flex;
		align-items: center;
		gap: 0.25rem;
		flex-wrap: wrap;
		font-size: 1.05rem;
	}
	.crumb {
		background: none;
		border: 0;
		padding: 0.1rem 0.3rem;
		font: inherit;
		color: var(--accent);
		cursor: pointer;
		border-radius: 4px;
	}
	.crumb[aria-current='page'] {
		color: var(--text);
		font-weight: 600;
		cursor: default;
	}
	tr.deleted td {
		color: var(--muted);
	}
	.version-meta {
		display: flex;
		gap: 0.4rem;
		align-items: center;
		margin-top: 0.15rem;
		margin-left: 1.6rem;
	}
	.dropzone {
		position: relative;
		transition: box-shadow 0.12s ease;
	}
	.dropzone.dragging {
		box-shadow: 0 0 0 3px var(--focus);
		border-color: var(--accent);
	}
	.drop-hint {
		position: absolute;
		inset: 0;
		z-index: 2;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 0.5rem;
		background: color-mix(in srgb, var(--surface) 85%, transparent);
		color: var(--accent);
		font-size: 1.05rem;
		pointer-events: none;
		border-radius: var(--radius);
	}
	.toolbar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		margin-bottom: 0.75rem;
	}
	.search {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		flex: 1 1 220px;
		max-width: 360px;
		color: var(--muted);
	}
	.name {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
		background: none;
		border: 0;
		padding: 0;
		font: inherit;
		color: var(--text);
		cursor: pointer;
		max-width: 100%;
		text-align: left;
	}
	.name:hover {
		color: var(--accent);
	}
	.folder {
		font-weight: 500;
	}
	tr.selected td {
		background: var(--surface-2);
	}
	.danger {
		color: var(--danger);
	}
	.objects td.actions .btn {
		opacity: 0.6;
	}
	.objects tr:hover td.actions .btn,
	.objects td.actions .btn:focus-visible {
		opacity: 1;
	}
</style>
