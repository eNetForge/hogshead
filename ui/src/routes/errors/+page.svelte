<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import { admin, type ErrorGroup, type ErrorsResponse } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatDate, formatNumber, formatRelative } from '$lib/format';
	import { toastError } from '$lib/stores/toast';

	let res = $state<ErrorsResponse | null>(null);
	let loading = $state(true);
	let since = $state('24h');
	let scope = $state<'local' | 'cellar'>('local');
	let inCellar = $state(false);
	let filter = $state<ErrorGroup | null>(null);

	const ranges = [
		['1h', 'Last hour'],
		['24h', 'Last 24 hours'],
		['7d', 'Last 7 days'],
		['30d', 'Last 30 days']
	] as const;

	const badge: Record<string, string> = { error: 'badge-danger', warning: 'badge-warn', info: '' };
	const label: Record<string, string> = { error: 'needs attention', warning: 'worth a look', info: 'normal' };

	const rows = $derived(
		(res?.rows ?? []).filter(
			(r) =>
				!filter ||
				((r.error_code || `HTTP ${r.status}`) === filter.code && r.op === filter.op && (r.bucket ?? '') === (filter.bucket ?? '') && (r.user ?? '') === (filter.user ?? ''))
		)
	);
	const counts = $derived({
		error: (res?.groups ?? []).filter((g) => g.severity === 'error').reduce((n, g) => n + g.count, 0),
		warning: (res?.groups ?? []).filter((g) => g.severity === 'warning').reduce((n, g) => n + g.count, 0),
		info: (res?.groups ?? []).filter((g) => g.severity === 'info').reduce((n, g) => n + g.count, 0)
	});

	async function load() {
		loading = true;
		try {
			res = await admin.errors({ from: since, scope, limit: '500' });
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void since;
		void scope;
		untrack(() => {
			filter = null;
			void load();
		});
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
</script>

<svelte:head><title>Errors · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Errors</h1>
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
	</div>
	<p class="muted small" style="margin-top:0">
		Every S3 request that ended in a 4xx or 5xx, grouped by cause. Most failed requests are routine (a client checking whether an object exists gets a 404), so each group says what it usually means and whether it deserves attention. Each server remembers its latest 10,000 failed requests.
	</p>

	<div class="toolbar">
		{#if inCellar}
			<div class="seg" role="group" aria-label="Scope">
				<button type="button" class:active={scope === 'cellar'} onclick={() => (scope = 'cellar')}>Whole cellar</button>
				<button type="button" class:active={scope === 'local'} onclick={() => (scope = 'local')}>This server</button>
			</div>
		{/if}
		<div class="seg" role="group" aria-label="Time range">
			{#each ranges as [v, l] (v)}
				<button type="button" class:active={since === v} onclick={() => (since = v)}>{l}</button>
			{/each}
		</div>
	</div>

	{#if loading && !res}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if res}
		{#if res.members?.some((m) => !m.ok)}
			<div class="alert alert-warn">Not included: {res.members.filter((m) => !m.ok).map((m) => `${m.name} (${m.error ?? 'unreachable'})`).join(', ')}.</div>
		{/if}
		{#if res.total === 0}
			<div class="empty">No failed requests in this window.</div>
		{:else}
			<div class="summary">
				<span><span class="badge badge-danger">{formatNumber(counts.error)}</span> need attention</span>
				<span><span class="badge badge-warn">{formatNumber(counts.warning)}</span> worth a look</span>
				<span><span class="badge">{formatNumber(counts.info)}</span> normal</span>
			</div>

			<h2 class="section">By cause</h2>
			<div class="groups">
				{#each res.groups as g (g.code + g.status + g.op + (g.bucket ?? '') + (g.user ?? ''))}
					<button type="button" class="group {g.severity}" class:selected={filter === g} onclick={() => (filter = filter === g ? null : g)}>
						<div class="head">
							<span class="count">{formatNumber(g.count)}×</span>
							<strong>{g.code}</strong>
							<span class="muted">HTTP {g.status} on {g.op}</span>
							<span class="badge {badge[g.severity]}">{label[g.severity]}</span>
							<span class="grow"></span>
							<span class="muted small">last {formatRelative(g.last_seen)}</span>
						</div>
						<div class="who small">
							{#if g.bucket}bucket <span class="mono">{g.bucket}</span>{/if}
							{#if g.user} · user <span class="mono">{g.user}</span>{:else} · anonymous{/if}
							{#if g.members && g.members.length > 0 && res.scope === 'cellar'} · on {g.members.join(', ')}{/if}
						</div>
						<div class="hint">{g.hint}</div>
						{#if g.message}<div class="msg small mono">{g.message}</div>{/if}
					</button>
				{/each}
			</div>

			<h2 class="section">
				Recent requests
				{#if filter}<span class="muted small">· {filter.code} on {filter.op} <button class="link" type="button" onclick={() => (filter = null)}>show all</button></span>{/if}
			</h2>
			<div class="table-wrap">
				<table>
					<thead>
						<tr><th>Time</th>{#if res.scope === 'cellar'}<th>Member</th>{/if}<th>Status</th><th>Code</th><th>Operation</th><th>Bucket / key</th><th>User</th><th>Client</th><th>Request id</th></tr>
					</thead>
					<tbody>
						{#each rows.slice(0, 200) as r (r.id + (r.member ?? ''))}
							<tr title={r.error_message ?? ''}>
								<td class="small nowrap">{formatDate(r.time)}</td>
								{#if res.scope === 'cellar'}<td class="small">{r.member ?? '–'}</td>{/if}
								<td>{r.status}</td>
								<td>{r.error_code || '–'}</td>
								<td class="small">{r.op}{#if r.replicated} <span class="badge">replication</span>{/if}</td>
								<td class="mono small key">{r.bucket ?? ''}{r.key ? '/' + r.key : ''}</td>
								<td class="small">{r.user || 'anonymous'}</td>
								<td class="small">{r.ip}<div class="muted ua">{r.user_agent ?? ''}</div></td>
								<td class="mono small">{r.id}</td>
							</tr>
						{/each}
						{#if rows.length === 0}
							<tr><td colspan="9" class="empty">Nothing matches.</td></tr>
						{/if}
					</tbody>
				</table>
			</div>
			<p class="muted small">Showing {Math.min(rows.length, 200)} of {formatNumber(filter ? filter.count : res.total)}. Hover a row for the server's message; search the server log or the access log for the request id to see more.</p>
		{/if}
	{/if}
</AdminOnly>

<style>
	.toolbar {
		display: flex;
		flex-wrap: wrap;
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
	.summary {
		display: flex;
		flex-wrap: wrap;
		gap: 1.25rem;
		margin-bottom: 0.5rem;
	}
	.section {
		margin: 1.25rem 0 0.6rem;
		font-size: 1.05rem;
	}
	.groups {
		display: grid;
		gap: 0.6rem;
	}
	.group {
		text-align: left;
		font: inherit;
		color: var(--text);
		background: var(--surface);
		border: 1px solid var(--border);
		border-left: 4px solid var(--border);
		border-radius: var(--radius);
		padding: 0.6rem 0.8rem;
		cursor: pointer;
	}
	.group.error {
		border-left-color: var(--danger);
	}
	.group.warning {
		border-left-color: #d99a00;
	}
	.group.selected {
		box-shadow: 0 0 0 2px var(--focus);
	}
	.head {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
	}
	.count {
		font-variant-numeric: tabular-nums;
		font-weight: 700;
	}
	.who {
		color: var(--muted);
		margin-top: 0.15rem;
	}
	.hint {
		margin-top: 0.35rem;
	}
	.msg {
		margin-top: 0.3rem;
		color: var(--muted);
	}
	.key {
		max-width: 22rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.ua {
		max-width: 12rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.nowrap {
		white-space: nowrap;
	}
	.link {
		background: none;
		border: none;
		padding: 0;
		color: var(--accent);
		cursor: pointer;
		font: inherit;
		text-decoration: underline;
	}
</style>
