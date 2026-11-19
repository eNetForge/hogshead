<script lang="ts">
	import type { Snippet } from 'svelte';
	import Icon from './Icon.svelte';

	let {
		open = $bindable(false),
		title,
		wide = false,
		onclose,
		children,
		footer
	}: {
		open?: boolean;
		title: string;
		wide?: boolean;
		onclose?: () => void;
		children: Snippet;
		footer?: Snippet;
	} = $props();

	let el: HTMLDialogElement | undefined = $state();

	$effect(() => {
		if (!el) return;
		if (open && !el.open) el.showModal();
		else if (!open && el.open) el.close();
	});

	function close() {
		open = false;
		onclose?.();
	}

	function onBackdrop(e: MouseEvent) {
		if (e.target === el) close();
	}
</script>

<dialog bind:this={el} class:wide onclose={close} onclick={onBackdrop} aria-labelledby="dlg-title">
	<div class="body">
		<header>
			<h2 id="dlg-title">{title}</h2>
			<button class="btn btn-ghost btn-icon" type="button" aria-label="Close dialog" onclick={close}>
				<Icon name="close" />
			</button>
		</header>
		<div class="content">
			{@render children()}
		</div>
		{#if footer}
			<footer>{@render footer()}</footer>
		{/if}
	</div>
</dialog>

<style>
	dialog {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--text);
		padding: 0;
		width: min(480px, calc(100vw - 2rem));
		box-shadow: var(--shadow);
	}
	dialog.wide {
		width: min(760px, calc(100vw - 2rem));
	}
	dialog::backdrop {
		background: rgba(0, 0, 0, 0.45);
	}
	.body {
		display: flex;
		flex-direction: column;
		max-height: calc(100vh - 4rem);
	}
	header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 0.9rem 1rem 0.5rem;
	}
	header h2 {
		margin: 0;
	}
	.content {
		padding: 0.5rem 1rem 1rem;
		overflow: auto;
	}
	footer {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
		padding: 0.75rem 1rem;
		border-top: 1px solid var(--border);
	}
</style>
