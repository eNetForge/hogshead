<script lang="ts">
	import { dismissToast, toasts } from '$lib/stores/toast';
	import Icon from './Icon.svelte';
</script>

<div class="toasts" aria-live="polite" aria-relevant="additions">
	{#each $toasts as t (t.id)}
		<div class="toast toast-{t.kind}" role={t.kind === 'error' ? 'alert' : 'status'}>
			<span class="msg">{t.message}</span>
			<button class="btn btn-ghost btn-icon" type="button" aria-label="Dismiss" onclick={() => dismissToast(t.id)}>
				<Icon name="close" size={16} />
			</button>
		</div>
	{/each}
</div>

<style>
	.toasts {
		position: fixed;
		right: 1rem;
		bottom: 1rem;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		z-index: 1000;
		max-width: min(420px, calc(100vw - 2rem));
	}
	.toast {
		display: flex;
		align-items: flex-start;
		gap: 0.5rem;
		padding: 0.6rem 0.6rem 0.6rem 0.9rem;
		border-radius: var(--radius);
		border: 1px solid var(--border);
		border-left-width: 4px;
		background: var(--surface);
		box-shadow: var(--shadow);
		word-break: break-word;
	}
	.toast-error {
		border-left-color: var(--danger);
	}
	.toast-success {
		border-left-color: var(--success);
	}
	.toast-info {
		border-left-color: var(--accent);
	}
	.msg {
		flex: 1;
		padding-top: 0.2rem;
	}
</style>
