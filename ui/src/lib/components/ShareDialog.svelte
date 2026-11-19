<script lang="ts">
	import { presignGet } from '$lib/api/s3';
	import { toastError } from '$lib/stores/toast';
	import CopyButton from './CopyButton.svelte';
	import Dialog from './Dialog.svelte';

	let { open = $bindable(false), bucket, objectKey, versionId }: { open?: boolean; bucket: string; objectKey: string; versionId?: string } = $props();

	const options = [
		{ label: '1 hour', seconds: 3600 },
		{ label: '1 day', seconds: 86400 },
		{ label: '7 days', seconds: 7 * 86400 }
	];
	let expires = $state(3600);
	let url = $state('');

	$effect(() => {
		if (!open) {
			url = '';
			return;
		}
		const e = expires;
		presignGet(bucket, objectKey, e, versionId)
			.then((u) => (url = u))
			.catch(toastError);
	});
</script>

<Dialog bind:open title="Share link">
	<p class="muted small" style="margin-top:0">Anyone with this link can download <strong class="mono">{objectKey}</strong> until it expires.</p>
	<div class="field">
		<label for="share-exp">Expires in</label>
		<select id="share-exp" bind:value={expires}>
			{#each options as o (o.seconds)}<option value={o.seconds}>{o.label}</option>{/each}
		</select>
	</div>
	<div class="field">
		<label for="share-url">Presigned URL</label>
		<textarea id="share-url" readonly rows="4" value={url} style="min-height:5rem"></textarea>
	</div>
	{#snippet footer()}
		<a class="btn" href={url || undefined} target="_blank" rel="noopener" aria-disabled={!url}>Open</a>
		<CopyButton text={url} label="Copy link" />
	{/snippet}
</Dialog>
