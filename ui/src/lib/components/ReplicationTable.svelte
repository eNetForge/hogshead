<script lang="ts">
	import { base } from '$app/paths';
	import type { ReplicationStatus } from '$lib/api/admin';
	import Icon from './Icon.svelte';
	import { formatNumber, formatRelative } from '$lib/format';

	let {
		rows,
		showBucket = false,
		busy = '',
		onedit,
		ontoggle,
		onremove
	}: {
		rows: ReplicationStatus[];
		showBucket?: boolean;
		busy?: string;
		onedit?: (r: ReplicationStatus) => void;
		ontoggle?: (r: ReplicationStatus) => void;
		onremove?: (r: ReplicationStatus) => void;
	} = $props();

	const key = (r: ReplicationStatus) => `${r.bucket}/${r.target.name}`;
</script>

<div class="table-wrap">
	<table>
		<thead>
			<tr>
				{#if showBucket}<th>Bucket</th>{/if}
				<th>Target</th>
				<th>Destination</th>
				<th>State</th>
				<th class="num">Lag</th>
				<th class="num">Pushed</th>
				<th>Last sync</th>
				{#if onedit || ontoggle || onremove}<th></th>{/if}
			</tr>
		</thead>
		<tbody>
			{#each rows as r (key(r))}
				<tr>
					{#if showBucket}
						<td>{#if r.bucket}<a href="{base}/buckets/{encodeURIComponent(r.bucket)}/settings">{r.bucket}</a>{:else}<span class="muted">users &amp; settings</span>{/if}</td>
					{/if}
					<td>
						<strong>{r.target.name}</strong>
						{#if r.target.inherited}<span class="badge" title="Comes from a server-wide target; manage it on the Replication page">server-wide</span>{/if}
						{#if r.target.prefix}<div class="muted small mono">prefix {r.target.prefix}</div>{/if}
					</td>
					<td class="mono small">{r.target.endpoint}/{r.bucket ? r.target.bucket : '_admin'}</td>
					<td>
						{#if r.last_error}
							<span class="badge badge-danger" title={r.last_error}>error</span>
						{:else if !r.target.enabled}
							<span class="badge badge-warn">paused</span>
						{:else if r.lag > 0}
							<span class="badge badge-accent">syncing</span>
						{:else}
							<span class="badge badge-success">in sync</span>
						{/if}
						{#if r.last_error}<div class="error small" title={r.last_error}>{r.last_error}</div>{/if}
						{#if r.last_warning}
							<span class="badge badge-warn" title={r.last_warning}>warning</span>
							<div class="muted small" title={r.last_warning}>{r.last_warning}</div>
						{/if}
					</td>
					<td class="num">{formatNumber(r.lag)}</td>
					<td class="num">{formatNumber(r.pushed)}</td>
					<td>{formatRelative(r.last_sync)}</td>
					{#if onedit || ontoggle || onremove}
						<td class="actions">
							{#if r.target.inherited && !showBucket}
								<a class="btn btn-sm btn-ghost" href="{base}/replication">Manage</a>
							{:else}
							{#if ontoggle}
								<button class="btn btn-sm btn-ghost" type="button" disabled={busy === key(r)} onclick={() => ontoggle(r)} title={r.target.enabled ? 'Pause replication to this target' : 'Resume replication to this target'}>
									<Icon name={r.target.enabled ? 'pause' : 'play'} size={14} />
									{r.target.enabled ? 'Pause' : 'Resume'}
								</button>
							{/if}
							{#if onedit}
								<button class="btn btn-sm btn-ghost" type="button" disabled={busy === key(r)} onclick={() => onedit(r)}><Icon name="edit" size={14} /> Edit</button>
							{/if}
							{#if onremove}
								<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Remove target {r.target.name}" disabled={busy === key(r)} onclick={() => onremove(r)}><Icon name="trash" size={14} /></button>
							{/if}
							{/if}
						</td>
					{/if}
				</tr>
			{/each}
		</tbody>
	</table>
</div>

<style>
	.error {
		color: var(--danger);
		max-width: 24rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		margin-top: 0.2rem;
	}
</style>
