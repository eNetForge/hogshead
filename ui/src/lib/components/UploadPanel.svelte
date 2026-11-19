<script lang="ts">
	import type { UploadItem } from '$lib/upload';
	import { formatBytes } from '$lib/format';
	import Icon from './Icon.svelte';

	let { items, onclear }: { items: UploadItem[]; onclear: () => void } = $props();

	const active = $derived(items.filter((i) => i.status === 'queued' || i.status === 'uploading').length);
</script>

{#if items.length > 0}
	<section class="card uploads" aria-live="polite">
		<div class="card-title">
			<h3>Uploads {#if active > 0}<span class="muted">({active} in progress)</span>{/if}</h3>
			<button class="btn btn-sm btn-ghost" type="button" onclick={onclear} disabled={active > 0}>Clear</button>
		</div>
		<ul>
			{#each items as u (u.id)}
				<li>
					<div class="line">
						<span class="truncate name" title={u.key}>{u.name}</span>
						<span class="muted small">{formatBytes(u.sent)} / {formatBytes(u.size)}</span>
						{#if u.status === 'done'}<span class="badge badge-success">done</span>
						{:else if u.status === 'error'}<span class="badge badge-danger" title={u.error}>failed</span>
						{:else if u.status === 'aborted'}<span class="badge">cancelled</span>
						{:else}
							<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Cancel upload of {u.name}" onclick={() => u.controller.abort()}>
								<Icon name="close" size={14} />
							</button>
						{/if}
					</div>
					<div class="progress" role="progressbar" aria-valuemin="0" aria-valuemax={u.size} aria-valuenow={u.sent} aria-label="Upload progress for {u.name}">
						<div style="width:{u.size ? (u.sent / u.size) * 100 : u.status === 'done' ? 100 : 0}%"></div>
					</div>
					{#if u.error}<div class="small" style="color:var(--danger)">{u.error}</div>{/if}
				</li>
			{/each}
		</ul>
	</section>
{/if}

<style>
	.uploads {
		margin-bottom: 1rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}
	.line {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		margin-bottom: 0.25rem;
	}
	.name {
		flex: 1;
	}
</style>
