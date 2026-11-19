<script lang="ts">
	import { onMount } from 'svelte';
	import { admin, type AuditEntry } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatDate } from '$lib/format';
	import { toastError } from '$lib/stores/toast';

	const PAGE = 100;
	let entries = $state<AuditEntry[]>([]);
	let loading = $state(false);
	let exhausted = $state(false);

	async function load(reset: boolean) {
		loading = true;
		try {
			const before = reset ? undefined : entries[entries.length - 1]?.id;
			const batch = await admin.audit(PAGE, before);
			entries = reset ? batch : [...entries, ...batch];
			exhausted = batch.length < PAGE;
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => load(true));
</script>

<svelte:head><title>Audit log · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Audit log</h1>
		<button class="btn btn-sm" type="button" onclick={() => load(true)} disabled={loading}><Icon name="refresh" size={14} /> Refresh</button>
	</div>

	<div class="table-wrap">
		<table>
			<thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Target</th><th>Detail</th></tr></thead>
			<tbody>
				{#each entries as e (e.id)}
					<tr>
						<td class="small" style="white-space:nowrap">{formatDate(e.at)}</td>
						<td>{e.actor}</td>
						<td class="mono small">{e.action}</td>
						<td class="mono small">{e.target}</td>
						<td class="small muted" style="word-break:break-all">{e.detail || ''}</td>
					</tr>
				{:else}
					{#if !loading}<tr><td colspan="5" class="empty">No audit entries.</td></tr>{/if}
				{/each}
				{#if loading}<tr><td colspan="5" class="empty"><span class="spinner"></span></td></tr>{/if}
			</tbody>
		</table>
	</div>
	{#if !exhausted}
		<div class="row" style="justify-content:center;margin-top:1rem">
			<button class="btn" type="button" onclick={() => load(false)} disabled={loading}>Older</button>
		</div>
	{/if}
</AdminOnly>
