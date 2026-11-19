<script lang="ts">
	import { copyText } from '$lib/clipboard';
	import Icon from './Icon.svelte';

	let { text, label = 'Copy', small = false }: { text: string; label?: string; small?: boolean } = $props();
</script>

<button class="btn" class:btn-sm={small} type="button" onclick={() => copyText(text)} aria-label={label}>
	<Icon name="copy" size={14} />
	{label}
</button>
