<script lang="ts">
	import { base } from '$app/paths';
	import { onMount } from 'svelte';
	import { admin, type ReplicationStatus, type Status, type UsageResponse } from '$lib/api/admin';
	import { listBuckets, type S3Bucket } from '$lib/api/s3';
	import StatTile from '$lib/components/StatTile.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatBytes, formatDate, formatNumber, formatRelative } from '$lib/format';
	import { isAdmin, session } from '$lib/stores/session';
	import { serverName } from '$lib/stores/server';
	import { toastError } from '$lib/stores/toast';

	let status = $state<Status | null>(null);
	let buckets = $state<S3Bucket[]>([]);
	let replication = $state<ReplicationStatus[]>([]);
	let traffic = $state<UsageResponse | null>(null);
	let trafficScope = $state<'local' | 'cellar'>('local');
	const trafficMax = $derived(traffic ? traffic.rows.reduce((m, r) => Math.max(m, r.n), 0) : 0);
	const trafficHours = $derived.by(() => {
		const rows = traffic?.rows ?? [];
		return Array.from({ length: 24 }, (_, i) => {
			const h = Math.floor(Date.now() / 3600000 - 23 + i) * 3600;
			return rows.find((r) => r.h === h) ?? { h, n: 0, e: 0, i: 0, o: 0, b: '', u: '', k: '', c: '' };
		});
	});
	let loading = $state(true);

	async function load() {
		loading = true;
		try {
			if (isAdmin($session)) {
				[status, replication] = await Promise.all([admin.status(), admin.replication.list()]);
				serverName.set(status.node_name ?? '');
				try {
					const cs = await admin.cellar.status();
					trafficScope = cs.active ? 'cellar' : 'local';
				} catch {
					trafficScope = 'local';
				}
				traffic = await admin.usage({ from: '24h', by: 'hour', scope: trafficScope }).catch(() => null);
			} else {
				buckets = await listBuckets();
			}
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	onMount(load);
</script>

<svelte:head><title>Dashboard · hogshead</title></svelte:head>

<div class="page-header">
	<h1>Dashboard</h1>
	<button class="btn btn-sm" type="button" onclick={load} disabled={loading}>
		<Icon name="refresh" size={14} />
		Refresh
	</button>
</div>

{#if loading && !status && buckets.length === 0}
	<p class="muted"><span class="spinner"></span> Loading…</p>
{:else if isAdmin($session)}
	{#if status}
		{#if status.pending_restart.length > 0}
			<div class="alert alert-warn" role="status">
				<strong>Restart required.</strong> Changed settings take effect after a restart: {status.pending_restart.join(', ')}
			</div>
		{/if}

		<div class="grid grid-tiles">
			<StatTile label="Buckets" value={formatNumber(status.stats.buckets)} />
			<StatTile label="Objects" value={formatNumber(status.stats.objects)} />
			<StatTile label="Stored" value={formatBytes(status.stats.bytes)} sub="{formatNumber(status.stats.chunks)} chunks" />
			<StatTile label="Uploads in progress" value={formatNumber(status.stats.uploads)} />
		</div>

		{#if traffic}
			<section class="card" style="margin-top:1rem">
				<div class="card-title">
					<h2>Traffic, last 24 hours{trafficScope === 'cellar' ? ' · whole cellar' : ''}</h2>
					<a href="{base}/usage">Usage →</a>
				</div>
				<div class="traffic">
					<dl class="kv">
						<dt>Requests</dt><dd>{formatNumber(traffic.totals.n)}</dd>
						<dt>Errors</dt><dd class:danger-text={traffic.totals.e > 0}>{formatNumber(traffic.totals.e)} {#if traffic.totals.e > 0}<a class="small" href="{base}/errors">explain →</a>{/if}</dd>
						<dt>Bytes in</dt><dd>{formatBytes(traffic.totals.i)}</dd>
						<dt>Bytes out</dt><dd>{formatBytes(traffic.totals.o)}</dd>
					</dl>
					<div class="spark" role="img" aria-label="Requests per hour over the last 24 hours">
						{#each trafficHours as h (h.h)}
							<div class="col" title="{new Date(h.h * 1000).toLocaleString()}: {formatNumber(h.n)} requests, {formatNumber(h.e)} errors">
								<div class="bar" style="height:{trafficMax ? Math.max(2, (100 * h.n) / trafficMax) : 2}%"></div>
								{#if h.e > 0}<div class="err" style="height:{trafficMax ? Math.max(2, (100 * h.e) / trafficMax) : 2}%"></div>{/if}
							</div>
						{/each}
					</div>
				</div>
			</section>
		{/if}

		<div class="grid grid-2" style="margin-top:1rem">
			<section class="card">
				<div class="card-title"><h2>Replication</h2><a href="{base}/replication">Details →</a></div>
				{#if replication.length === 0}
					<p class="muted small" style="margin:0">No bucket replicates to a standby server. Add targets from a bucket's settings.</p>
				{:else}
					{@const buckets = new Set(replication.map((r) => r.bucket)).size}
					{@const failing = replication.filter((r) => r.last_error).length}
					<dl class="kv">
						<dt>Targets</dt><dd>{formatNumber(replication.length)} across {formatNumber(buckets)} bucket{buckets === 1 ? '' : 's'}</dd>
						<dt>Pending changes</dt><dd>{formatNumber(replication.reduce((n, r) => n + r.lag, 0))}</dd>
						<dt>Paused</dt><dd>{formatNumber(replication.filter((r) => !r.target.enabled).length)}</dd>
						<dt>Errors</dt>
						<dd>{#if failing > 0}<span class="badge badge-danger">{formatNumber(failing)} target{failing === 1 ? '' : 's'} failing</span>{:else}none{/if}</dd>
					</dl>
				{/if}
			</section>
			<section class="card">
				<div class="card-title"><h2>This server</h2></div>
				<dl class="kv">
					<dt>Name</dt><dd>{status.node_name || '–'}</dd>
					{#if status.id}<dt>Server id</dt><dd class="mono small">{status.id}</dd>{/if}
					<dt>Version</dt><dd>{status.version}</dd>
					<dt>Uptime</dt><dd>{status.uptime} <span class="muted">(since {formatDate(status.started_at)})</span></dd>
					<dt>Listen</dt><dd class="mono">{status.listen?.join(', ') || '–'}</dd>
					<dt>TLS</dt><dd>{status.tls_mode || 'off'}</dd>
				</dl>
			</section>
		</div>

		<section class="card" style="margin-top:1rem">
			<div class="card-title"><h2>Background workers</h2></div>
			{#if status.workers.last_error}
				<div class="alert alert-danger">Last error: {status.workers.last_error}</div>
			{/if}
			<div class="grid grid-2">
				<dl class="kv">
					<dt>Last GC</dt><dd>{formatRelative(status.workers.last_gc)}</dd>
					<dt>Chunks released</dt><dd>{formatNumber(status.workers.chunks_released)}</dd>
					<dt>Orphans removed</dt><dd>{formatNumber(status.workers.orphans_removed)}</dd>
					<dt>Bytes reclaimed</dt><dd>{formatBytes(status.workers.bytes_reclaimed)}</dd>
				</dl>
				<dl class="kv">
					<dt>Last lifecycle run</dt><dd>{formatRelative(status.workers.last_lifecycle)}</dd>
					<dt>Objects expired</dt><dd>{formatNumber(status.workers.objects_expired)}</dd>
					<dt>Versions expired</dt><dd>{formatNumber(status.workers.versions_expired)}</dd>
					<dt>Markers removed</dt><dd>{formatNumber(status.workers.markers_removed)}</dd>
					<dt>Uploads aborted</dt><dd>{formatNumber(status.workers.uploads_aborted)}</dd>
				</dl>
			</div>
		</section>
	{/if}
{:else}
	<section class="card">
		<div class="card-title"><h2>Your buckets</h2><a href="{base}/buckets">All buckets →</a></div>
		{#if buckets.length === 0}
			<div class="empty">No buckets are accessible with this key.</div>
		{:else}
			<ul class="bucket-list">
				{#each buckets as b (b.name)}
					<li><a href="{base}/buckets/{encodeURIComponent(b.name)}"><Icon name="bucket" size={16} /> {b.name}</a> <span class="muted small">created {formatDate(b.creationDate)}</span></li>
				{/each}
			</ul>
		{/if}
	</section>
	<p class="muted small">Server statistics are only available to administrators.</p>
{/if}

<style>
	.traffic {
		display: grid;
		grid-template-columns: minmax(12rem, 1fr) 3fr;
		gap: 1rem;
		align-items: end;
	}
	@media (max-width: 720px) {
		.traffic {
			grid-template-columns: 1fr;
		}
	}
	.spark {
		display: flex;
		align-items: flex-end;
		gap: 3px;
		height: 90px;
		padding-top: 0.5rem;
	}
	.col {
		flex: 1;
		position: relative;
		height: 100%;
		display: flex;
		align-items: flex-end;
	}
	.bar {
		width: 100%;
		background: var(--accent);
		border-radius: 2px 2px 0 0;
		opacity: 0.85;
	}
	.err {
		position: absolute;
		left: 0;
		right: 0;
		bottom: 0;
		background: var(--danger);
		border-radius: 2px 2px 0 0;
	}
	.danger-text {
		color: var(--danger);
		font-weight: 600;
	}
	.bucket-list {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.bucket-list li {
		display: flex;
		gap: 0.75rem;
		align-items: center;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--border);
	}
	.bucket-list li:last-child {
		border-bottom: none;
	}
	.bucket-list a {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		font-weight: 500;
	}
</style>
