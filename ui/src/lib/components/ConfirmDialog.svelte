<script lang="ts">
	import Dialog from './Dialog.svelte';

	let {
		open = $bindable(false),
		title = 'Are you sure?',
		message,
		confirmLabel = 'Delete',
		danger = true,
		busy = false,
		onconfirm
	}: {
		open?: boolean;
		title?: string;
		message: string;
		confirmLabel?: string;
		danger?: boolean;
		busy?: boolean;
		onconfirm: () => void | Promise<void>;
	} = $props();
</script>

<Dialog bind:open {title}>
	<p style="margin:0">{message}</p>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (open = false)} disabled={busy}>Cancel</button>
		<button class="btn" class:btn-danger={danger} class:btn-primary={!danger} type="button" onclick={onconfirm} disabled={busy}>
			{#if busy}<span class="spinner"></span>{/if}
			{confirmLabel}
		</button>
	{/snippet}
</Dialog>
