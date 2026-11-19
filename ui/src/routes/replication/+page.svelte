<script lang="ts">
	import { base } from '$app/paths';
	import { onMount } from 'svelte';
	import { admin, type ReplicationStatus, type ReplicationTarget } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import ReplicationTable from '$lib/components/ReplicationTable.svelte';
	import ReplicationTargetDialog from '$lib/components/ReplicationTargetDialog.svelte';
	import StatTile from '$lib/components/StatTile.svelte';
	import { formatNumber } from '$lib/format';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let rows = $state<ReplicationStatus[]>([]);
	let globals = $state<ReplicationTarget[]>([]);
	let loading = $state(true);
	let busy = $state('');
	let dialogOpen = $state(false);
	let editing = $state<ReplicationTarget | null>(null);
	let removing = $state<ReplicationTarget | null>(null);

	const errors = $derived(rows.filter((r) => r.last_error).length);
	const paused = $derived(rows.filter((r) => !r.target.enabled).length);
	const lag = $derived(rows.reduce((n, r) => n + r.lag, 0));

	async function load() {
		loading = true;
		try {
			const [allRows, allGlobals] = await Promise.all([admin.replication.list(), admin.replication.global.list()]);
			// Cellar membership manages its own targets; they live on the Cellar page.
			rows = allRows.filter((r) => !r.target.cellar);
			globals = allGlobals.filter((g) => !g.cellar);
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	async function toggleRow(r: ReplicationStatus) {
		if (r.target.inherited) return toggleGlobal(r.target);
		busy = `${r.bucket}/${r.target.name}`;
		try {
			await admin.replication.set(r.bucket, r.target.name, { enabled: !r.target.enabled });
			toastSuccess(r.target.enabled ? `Paused ${r.bucket} → ${r.target.name}` : `Resumed ${r.bucket} → ${r.target.name}`);
			await load();
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}

	async function toggleGlobal(t: ReplicationTarget) {
		busy = `global/${t.name}`;
		try {
			await admin.replication.global.set(t.name, { enabled: !t.enabled });
			toastSuccess(t.enabled ? `Paused server-wide target ${t.name}` : `Resumed server-wide target ${t.name}`);
			await load();
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}

	async function confirmRemove() {
		if (!removing) return;
		const t = removing;
		busy = `global/${t.name}`;
		try {
			await admin.replication.global.remove(t.name);
			toastSuccess(`Removed server-wide target ${t.name}`);
			removing = null;
			await load();
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 15000);
		return () => clearInterval(t);
	});
</script>

<svelte:head><title>Replication · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Replication</h1>
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading}><Icon name="refresh" size={14} /> Refresh</button>
	</div>

	<section class="card">
		<div class="card-title">
			<h2>Server-wide targets</h2>
			<button class="btn btn-sm btn-primary" type="button" onclick={() => ((editing = null), (dialogOpen = true))}><Icon name="plus" size={14} /> Add target</button>
		</div>
		<p class="muted small" style="margin-top:0">
			A server-wide target replicates every bucket on this server to a bucket of the same name on the standby, including buckets created later. A bucket with its own target of the same name overrides it. Targets managed by a cellar are shown on the <a href="{base}/cellar">Cellar</a> page instead.
		</p>
		{#if loading && globals.length === 0}
			<p class="muted"><span class="spinner"></span> Loading…</p>
		{:else if globals.length === 0}
			<div class="empty">No server-wide target. Add one to mirror the whole server to another hogshead instance.</div>
		{:else}
			<div class="table-wrap">
				<table>
					<thead><tr><th>Target</th><th>Endpoint</th><th>State</th><th>Prefix</th><th></th></tr></thead>
					<tbody>
						{#each globals as t (t.name)}
							<tr>
								<td><strong>{t.name}</strong></td>
								<td class="mono small">{t.endpoint}{t.path_style ? '' : ' (virtual-host)'}</td>
								<td>{#if t.enabled}<span class="badge badge-success">enabled</span>{:else}<span class="badge badge-warn">paused</span>{/if}{#if t.sync_config}<span class="badge" style="margin-left:0.3rem" title="Users, keys, passwords and settings are mirrored too">users &amp; settings</span>{/if}</td>
								<td class="mono small">{t.prefix || '–'}</td>
								<td class="actions">
									<button class="btn btn-sm btn-ghost" type="button" disabled={busy === `global/${t.name}`} onclick={() => toggleGlobal(t)}>
										<Icon name={t.enabled ? 'pause' : 'play'} size={14} /> {t.enabled ? 'Pause' : 'Resume'}
									</button>
									<button class="btn btn-sm btn-ghost" type="button" disabled={busy === `global/${t.name}`} onclick={() => ((editing = t), (dialogOpen = true))}><Icon name="edit" size={14} /> Edit</button>
									<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Remove server-wide target {t.name}" disabled={busy === `global/${t.name}`} onclick={() => (removing = t)}><Icon name="trash" size={14} /></button>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<section style="margin-top:1.5rem">
		<h2 style="margin:0 0 0.75rem">Per-bucket status</h2>
		{#if loading && rows.length === 0}
			<p class="muted"><span class="spinner"></span> Loading…</p>
		{:else if rows.length === 0}
			<div class="empty">
				No bucket replicates anywhere yet. Add a server-wide target above, or open a bucket's settings to add a target for that bucket only.
			</div>
		{:else}
			<div class="grid grid-tiles">
				<StatTile label="Bucket targets" value={formatNumber(rows.length)} />
				<StatTile label="Pending changes" value={formatNumber(lag)} />
				<StatTile label="Paused" value={formatNumber(paused)} />
				<StatTile label="With errors" value={formatNumber(errors)} />
			</div>
			<div style="margin-top:1rem">
				<ReplicationTable {rows} showBucket {busy} ontoggle={toggleRow} />
			</div>
			<p class="muted small">Per-bucket targets are added, edited and removed from each bucket's settings page. Status refreshes every 15 seconds.</p>
		{/if}
	</section>

	<ReplicationTargetDialog bind:open={dialogOpen} global {editing} onsaved={load} />
	<ConfirmDialog
		open={removing !== null}
		title="Remove server-wide target"
		message="Stop replicating every bucket to “{removing?.name ?? ''}”? Objects already on the standby are kept."
		confirmLabel="Remove"
		busy={busy !== ''}
		onconfirm={confirmRemove}
	/>
</AdminOnly>
