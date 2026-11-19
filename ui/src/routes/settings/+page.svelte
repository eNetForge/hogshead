<script lang="ts">
	import { onMount } from 'svelte';
	import { admin, type ConfigView, type Setting } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let settings = $state<Setting[]>([]);
	let config = $state<ConfigView | null>(null);
	let loading = $state(true);
	let editing = $state<string | null>(null);
	let draft = $state('');
	let busy = $state(false);

	async function load() {
		loading = true;
		try {
			[settings, config] = await Promise.all([admin.settings.list(), admin.config.get()]);
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	function edit(s: Setting) {
		editing = s.key;
		draft = s.value;
	}

	async function save(key: string) {
		busy = true;
		try {
			const r = await admin.settings.set(key, draft.trim());
			settings = settings.map((s) => (s.key === key ? { ...s, value: r.value } : s));
			toastSuccess(r.value ? `${key} = ${r.value}` : `${key} reset to default`);
			editing = null;
		} catch (e) {
			toastError(e);
		} finally {
			busy = false;
		}
	}

	async function reload() {
		busy = true;
		try {
			config = await admin.config.reload();
			toastSuccess('Configuration reloaded');
		} catch (e) {
			toastError(e);
		} finally {
			busy = false;
		}
	}

	function onKey(e: KeyboardEvent, key: string) {
		if (e.key === 'Enter') {
			e.preventDefault();
			save(key);
		} else if (e.key === 'Escape') {
			editing = null;
		}
	}

	onMount(load);
</script>

<svelte:head><title>Settings · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Settings</h1>
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
	</div>

	<section>
		<div class="card-title"><h2>Server settings</h2><span class="muted small">Empty value restores the default</span></div>
		<div class="table-wrap">
			<table>
				<thead><tr><th>Key</th><th>Value</th><th>Description</th><th></th></tr></thead>
				<tbody>
					{#if loading && settings.length === 0}
						<tr><td colspan="4" class="empty"><span class="spinner"></span></td></tr>
					{:else}
						{#each settings as s (s.key)}
							<tr>
								<td class="mono">{s.key}</td>
								<td>
									{#if editing === s.key}
										<label class="sr-only" for="edit-{s.key}">Value for {s.key}</label>
										<!-- svelte-ignore a11y_autofocus -->
										<input id="edit-{s.key}" type="text" bind:value={draft} onkeydown={(e) => onKey(e, s.key)} autofocus style="max-width:240px" />
									{:else if s.value}
										<span class="mono">{s.value}</span>
									{:else}
										<span class="muted">default</span>
									{/if}
								</td>
								<td class="small muted">{s.description || ''}</td>
								<td class="actions">
									{#if editing === s.key}
										<button class="btn btn-sm btn-primary" type="button" onclick={() => save(s.key)} disabled={busy}>Save</button>
										<button class="btn btn-sm" type="button" onclick={() => (editing = null)} disabled={busy}>Cancel</button>
									{:else}
										<button class="btn btn-sm" type="button" onclick={() => edit(s)}><Icon name="edit" size={14} /> Edit</button>
									{/if}
								</td>
							</tr>
						{:else}
							<tr><td colspan="4" class="empty">No settings.</td></tr>
						{/each}
					{/if}
				</tbody>
			</table>
		</div>
	</section>

	<section class="card" style="margin-top:1.25rem">
		<div class="card-title">
			<h2>Node configuration <span class="muted small mono">{config?.path || ''}</span></h2>
			<div class="row">
				{#if config && config.pending_restart.length > 0}
					<span class="badge badge-warn" title={config.pending_restart.join(', ')}>restart required</span>
				{/if}
				<button class="btn btn-sm" type="button" onclick={reload} disabled={busy || !config}><Icon name="refresh" size={14} /> Reload from disk</button>
			</div>
		</div>
		{#if config && config.pending_restart.length > 0}
			<div class="alert alert-warn">These settings take effect after a restart: <span class="mono">{config.pending_restart.join(', ')}</span></div>
		{/if}
		<p class="muted small" style="margin-top:0">Read-only view of the node-local TOML file with secrets redacted. Edit the file on the node and use reload to apply changes that do not require a restart.</p>
		{#if config}
			<pre class="small">{config.toml}</pre>
		{:else if !loading}
			<div class="empty">Configuration unavailable.</div>
		{/if}
	</section>
</AdminOnly>
