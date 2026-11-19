<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import { admin, type UsageResponse } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import StatTile from '$lib/components/StatTile.svelte';
	import UsageTable from '$lib/components/UsageTable.svelte';
	import { formatBytes, formatNumber } from '$lib/format';
	import { toastError } from '$lib/stores/toast';

	let res = $state<UsageResponse | null>(null);
	let loading = $state(true);
	let since = $state('24h');
	let by = $state('bucket');
	let bucket = $state('');
	let user = $state('');
	let key = $state('');
	let scope = $state<'local' | 'cellar'>('local');
	let inCellar = $state(false);

	const ranges = [
		['1h', 'Last hour'],
		['24h', 'Last 24 hours'],
		['7d', 'Last 7 days'],
		['30d', 'Last 30 days']
	] as const;
	const groups = [
		['bucket', 'Bucket'],
		['user', 'User'],
		['key', 'Access key'],
		['class', 'Operation class'],
		['hour', 'Hour'],
		['bucket,class', 'Bucket and class'],
		['user,class', 'User and class'],
		['member', 'Cellar member'],
		['member,bucket', 'Member and bucket']
	] as const;

	async function load() {
		loading = true;
		try {
			res = await admin.usage({ from: since, by, bucket: bucket.trim(), user: user.trim(), key: key.trim(), scope });
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void since;
		void by;
		void scope;
		untrack(() => void load());
	});

	onMount(() => {
		admin.cellar
			.status()
			.then((st) => {
				inCellar = st.active;
				if (st.active) scope = 'cellar';
			})
			.catch(() => (inCellar = false));
		const t = setInterval(() => void load(), 60000);
		return () => clearInterval(t);
	});

	function applyFilters(e: SubmitEvent) {
		e.preventDefault();
		void load();
	}
</script>

<svelte:head><title>Usage · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Usage</h1>
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
	</div>
	<p class="muted small" style="margin-top:0">
		S3 requests counted per hour by bucket, user, access key and operation class, kept for 90 days unless the <code>usage.retention_days</code> setting says otherwise. Each server counts the traffic it served; in cellar scope this server asks every member and sums the answers.
		{#if res?.access_log}Every request is also written to the access log at <span class="mono">{res.access_log}</span>.{:else}Set <span class="mono">access_log</span> in the config to also write one JSON line per request.{/if}
	</p>

	<div class="toolbar">
		{#if inCellar}
			<div class="seg" role="group" aria-label="Scope">
				<button type="button" class:active={scope === 'cellar'} onclick={() => (scope = 'cellar')}>Whole cellar</button>
				<button type="button" class:active={scope === 'local'} onclick={() => (scope = 'local')}>This server</button>
			</div>
		{/if}
		<div class="seg" role="group" aria-label="Time range">
			{#each ranges as [v, label] (v)}
				<button type="button" class:active={since === v} onclick={() => (since = v)}>{label}</button>
			{/each}
		</div>
		<label class="row">
			<span class="muted small">Group by</span>
			<select bind:value={by}>
				{#each groups as [v, label] (v)}<option value={v}>{label}</option>{/each}
			</select>
		</label>
		<form class="row filters" onsubmit={applyFilters}>
			<input type="text" placeholder="bucket" bind:value={bucket} style="width:9rem" />
			<input type="text" placeholder="user" bind:value={user} style="width:8rem" />
			<input type="text" placeholder="access key" bind:value={key} style="width:12rem" />
			<button class="btn btn-sm" type="submit">Filter</button>
		</form>
	</div>

	{#if loading && !res}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if res}
		<div class="grid grid-tiles">
			<StatTile label="Requests" value={formatNumber(res.totals.n)} />
			<StatTile label="Errors (4xx/5xx)" value={formatNumber(res.totals.e)} sub={res.totals.n ? `${((100 * res.totals.e) / res.totals.n).toFixed(1)}%` : ''} />
			<StatTile label="Bytes in" value={formatBytes(res.totals.i)} />
			<StatTile label="Bytes out" value={formatBytes(res.totals.o)} />
		</div>
		{#if res.scope === 'cellar' && res.members?.some((m) => !m.ok)}
			<div class="alert alert-warn" style="margin-top:1rem">
				Not included: {res.members.filter((m) => !m.ok).map((m) => `${m.name} (${m.error ?? 'unreachable'})`).join(', ')}.
			</div>
		{/if}
		<div style="margin-top:1rem">
			<UsageTable rows={res.rows} by={res.by} totals={res.totals} />
		</div>
		{#if res.scope === 'cellar' && res.members}
			<p class="muted small">Summed over {res.members.filter((m) => m.ok).length} of {res.members.length} members: {res.members.filter((m) => m.ok).map((m) => m.name).join(', ')}.</p>
		{/if}
	{/if}
</AdminOnly>

<style>
	.toolbar {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem 1.25rem;
		margin-bottom: 1rem;
	}
	.seg {
		display: inline-flex;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}
	.seg button {
		background: var(--surface);
		border: none;
		border-right: 1px solid var(--border);
		padding: 0.35rem 0.7rem;
		font: inherit;
		color: var(--muted);
		cursor: pointer;
	}
	.seg button:last-child {
		border-right: none;
	}
	.seg button.active {
		background: var(--accent);
		color: #fff;
	}
	.filters input {
		padding: 0.35rem 0.5rem;
	}
</style>
