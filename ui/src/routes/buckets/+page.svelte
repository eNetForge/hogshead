<script lang="ts">
	import { base } from '$app/paths';
	import { onMount } from 'svelte';
	import { admin, type Bucket } from '$lib/api/admin';
	import { listBuckets } from '$lib/api/s3';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatBytes, formatDate, formatNumber, isValidBucketName } from '$lib/format';
	import { isAdmin, session } from '$lib/stores/session';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	interface Row {
		name: string;
		created_at: string;
		admin?: Bucket;
	}

	let rows = $state<Row[]>([]);
	let loading = $state(true);
	let filter = $state('');
	let createOpen = $state(false);
	let creating = $state(false);
	let form = $state({ name: '', versioning: false, object_lock: false });

	const filtered = $derived(rows.filter((r) => r.name.includes(filter.trim())));
	const nameOk = $derived(isValidBucketName(form.name));

	async function load() {
		loading = true;
		try {
			if (isAdmin($session)) {
				rows = (await admin.buckets.list()).map((b) => ({ name: b.name, created_at: b.created_at, admin: b }));
			} else {
				rows = (await listBuckets()).map((b) => ({ name: b.name, created_at: b.creationDate }));
			}
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	async function create(e: SubmitEvent) {
		e.preventDefault();
		if (!nameOk) return;
		creating = true;
		try {
			await admin.buckets.create({
				name: form.name,
				versioning: form.versioning || form.object_lock ? 'Enabled' : undefined,
				object_lock: form.object_lock || undefined
			});
			toastSuccess(`Bucket ${form.name} created`);
			createOpen = false;
			form = { name: '', versioning: false, object_lock: false };
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			creating = false;
		}
	}

	onMount(load);
</script>

<svelte:head><title>Buckets · hogshead</title></svelte:head>

<div class="page-header">
	<h1>Buckets</h1>
	<div class="row">
		<label class="sr-only" for="filter">Filter buckets</label>
		<input id="filter" type="search" placeholder="Filter…" bind:value={filter} style="width:200px" />
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
		{#if isAdmin($session)}
			<button class="btn btn-primary btn-sm" type="button" onclick={() => (createOpen = true)}>
				<Icon name="plus" size={14} />
				New bucket
			</button>
		{/if}
	</div>
</div>

<div class="table-wrap">
	<table>
		<thead>
			<tr>
				<th>Name</th>
				{#if isAdmin($session)}
					<th>Owner</th>
					<th class="num">Objects</th>
					<th class="num">Size</th>
					<th>Versioning</th>
				{/if}
				<th>Created</th>
				{#if isAdmin($session)}<th></th>{/if}
			</tr>
		</thead>
		<tbody>
			{#if loading && rows.length === 0}
				<tr><td colspan="7" class="empty"><span class="spinner"></span></td></tr>
			{:else}
				{#each filtered as b (b.name)}
					<tr>
						<td>
							<a href="{base}/buckets/{encodeURIComponent(b.name)}" class="row" style="display:inline-flex;gap:.4rem;font-weight:500">
								<Icon name="bucket" size={16} />
								{b.name}
							</a>
							{#if b.admin?.object_lock}<span class="badge" style="margin-left:.4rem">object lock</span>{/if}
						</td>
						{#if isAdmin($session)}
							<td>{b.admin?.owner_name || b.admin?.owner || '–'}</td>
							<td class="num">{formatNumber(b.admin?.objects)}</td>
							<td class="num">{formatBytes(b.admin?.bytes)}</td>
							<td>{b.admin?.versioning || 'Off'}</td>
						{/if}
						<td class="small">{formatDate(b.created_at)}</td>
						{#if isAdmin($session)}
							<td class="actions">
								<a class="btn btn-sm" href="{base}/buckets/{encodeURIComponent(b.name)}/settings"><Icon name="settings" size={14} /> Settings</a>
							</td>
						{/if}
					</tr>
				{:else}
					<tr><td colspan="7" class="empty">{filter ? 'No buckets match the filter.' : 'No buckets yet.'}</td></tr>
				{/each}
			{/if}
		</tbody>
	</table>
</div>

<Dialog bind:open={createOpen} title="New bucket">
	<form onsubmit={create} id="create-bucket">
		<div class="field">
			<label for="bname">Name</label>
			<input id="bname" type="text" bind:value={form.name} autocapitalize="off" spellcheck="false" placeholder="my-bucket" required aria-invalid={form.name.length > 0 && !nameOk} />
			<div class="hint">3–63 characters: lowercase letters, digits, dots and hyphens.</div>
		</div>
		<div class="field">
			<label class="checkbox"><input type="checkbox" bind:checked={form.versioning} disabled={form.object_lock} /> Enable versioning</label>
		</div>
		<div class="field">
			<label class="checkbox"><input type="checkbox" bind:checked={form.object_lock} onchange={() => form.object_lock && (form.versioning = true)} /> Enable object lock</label>
			<div class="hint">Object lock requires versioning and cannot be turned off later.</div>
		</div>
	</form>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (createOpen = false)}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="create-bucket" disabled={creating || !nameOk}>
			{#if creating}<span class="spinner"></span>{/if}
			Create
		</button>
	{/snippet}
</Dialog>
