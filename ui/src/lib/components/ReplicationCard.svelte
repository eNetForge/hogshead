<script lang="ts">
	import { untrack } from 'svelte';
	import { admin, type ReplicationStatus, type ReplicationTarget } from '$lib/api/admin';
	import ConfirmDialog from './ConfirmDialog.svelte';
	import Icon from './Icon.svelte';
	import ReplicationTable from './ReplicationTable.svelte';
	import ReplicationTargetDialog from './ReplicationTargetDialog.svelte';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let { bucket }: { bucket: string } = $props();

	let rows = $state<ReplicationStatus[]>([]);
	let loading = $state(true);
	let busy = $state('');
	let dialogOpen = $state(false);
	let editing = $state<ReplicationTarget | null>(null);
	let removing = $state<ReplicationStatus | null>(null);

	async function load() {
		loading = true;
		try {
			rows = (await admin.replication.get(bucket)).filter((r) => !r.target.cellar);
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void bucket;
		untrack(load);
	});

	function add() {
		editing = null;
		dialogOpen = true;
	}

	function edit(r: ReplicationStatus) {
		editing = r.target;
		dialogOpen = true;
	}

	async function toggle(r: ReplicationStatus) {
		busy = `${r.bucket}/${r.target.name}`;
		try {
			rows = await admin.replication.set(bucket, r.target.name, { enabled: !r.target.enabled });
			toastSuccess(r.target.enabled ? `Paused replication to ${r.target.name}` : `Resumed replication to ${r.target.name}`);
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}

	async function confirmRemove() {
		if (!removing) return;
		const r = removing;
		busy = `${r.bucket}/${r.target.name}`;
		try {
			await admin.replication.remove(bucket, r.target.name);
			rows = rows.filter((x) => x.target.name !== r.target.name);
			toastSuccess(`Removed target ${r.target.name}`);
			removing = null;
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}
</script>

<section class="card" style="margin-top:1rem">
	<div class="card-title">
		<h2>Replication</h2>
		<div class="row">
			<button class="btn btn-sm btn-ghost" type="button" onclick={load} disabled={loading} aria-label="Refresh replication status"><Icon name="refresh" size={14} /></button>
			<button class="btn btn-sm btn-primary" type="button" onclick={add}><Icon name="plus" size={14} /> Add target</button>
		</div>
	</div>
	<p class="muted small" style="margin-top:0">
		Every put, delete marker and delete in this bucket is replayed, in order, to each enabled target. Lag is the number of changes a target has not received yet. Server-wide targets apply to every bucket and are managed on the Replication page.
	</p>
	{#if loading && rows.length === 0}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if rows.length === 0}
		<div class="empty">No replication targets. Add a standby server to keep a copy of this bucket elsewhere.</div>
	{:else}
		<ReplicationTable {rows} {busy} onedit={edit} ontoggle={toggle} onremove={(r) => (removing = r)} />
	{/if}
</section>

<ReplicationTargetDialog bind:open={dialogOpen} {bucket} {editing} onsaved={load} />
<ConfirmDialog
	open={removing !== null}
	title="Remove replication target"
	message="Stop replicating “{bucket}” to “{removing?.target.name ?? ''}”? Objects already on the standby are kept."
	confirmLabel="Remove"
	busy={busy !== ''}
	onconfirm={confirmRemove}
/>
