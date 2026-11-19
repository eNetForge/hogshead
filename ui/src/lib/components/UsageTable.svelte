<script lang="ts">
	import type { UsageRow } from '$lib/api/admin';
	import { formatBytes, formatNumber } from '$lib/format';

	let { rows, by, totals }: { rows: UsageRow[]; by: string[]; totals: UsageRow } = $props();

	const max = $derived(rows.reduce((m, r) => Math.max(m, r.n), 0));
	const labels: Record<string, string> = { hour: 'Hour (UTC)', bucket: 'Bucket', user: 'User', key: 'Access key', class: 'Class', member: 'Member' };

	function cell(r: UsageRow, d: string): string {
		switch (d) {
			case 'hour':
				return new Date(r.h * 1000).toISOString().slice(0, 13).replace('T', ' ') + ':00';
			case 'bucket':
				return r.b || '–';
			case 'user':
				return r.u || '(anonymous)';
			case 'key':
				return r.k || '–';
			case 'class':
				return r.c;
			case 'member':
				return r.m || '–';
		}
		return '';
	}
</script>

<div class="table-wrap">
	<table>
		<thead>
			<tr>
				{#each by as d (d)}<th>{labels[d] ?? d}</th>{/each}
				<th class="num">Requests</th>
				<th class="bar"></th>
				<th class="num">Errors</th>
				<th class="num">Bytes in</th>
				<th class="num">Bytes out</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as r, i (i)}
				<tr>
					{#each by as d (d)}<td class:mono={d === 'key' || d === 'hour'}>{cell(r, d)}</td>{/each}
					<td class="num">{formatNumber(r.n)}</td>
					<td class="bar"><div class="track"><div class="fill" style="width:{max ? (100 * r.n) / max : 0}%"></div></div></td>
					<td class="num" class:danger={r.e > 0}>{formatNumber(r.e)}</td>
					<td class="num">{formatBytes(r.i)}</td>
					<td class="num">{formatBytes(r.o)}</td>
				</tr>
			{/each}
			{#if rows.length === 0}
				<tr><td colspan={by.length + 5} class="empty">No requests in this window.</td></tr>
			{/if}
		</tbody>
		{#if rows.length > 1}
			<tfoot>
				<tr>
					<td colspan={Math.max(by.length, 1)}><strong>Total</strong></td>
					{#if by.length === 0}{/if}
					<td class="num"><strong>{formatNumber(totals.n)}</strong></td>
					<td class="bar"></td>
					<td class="num"><strong>{formatNumber(totals.e)}</strong></td>
					<td class="num"><strong>{formatBytes(totals.i)}</strong></td>
					<td class="num"><strong>{formatBytes(totals.o)}</strong></td>
				</tr>
			</tfoot>
		{/if}
	</table>
</div>

<style>
	.bar {
		width: 22%;
		min-width: 6rem;
	}
	.track {
		height: 0.6rem;
		background: var(--surface-2);
		border-radius: 999px;
		overflow: hidden;
	}
	.fill {
		height: 100%;
		background: var(--accent);
		border-radius: 999px;
	}
	.danger {
		color: var(--danger);
		font-weight: 600;
	}
	tfoot td {
		border-top: 2px solid var(--border);
	}
</style>
