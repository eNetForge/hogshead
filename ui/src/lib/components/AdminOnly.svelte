<script lang="ts">
	import type { Snippet } from 'svelte';
	import { isAdmin, session } from '$lib/stores/session';

	let { children }: { children: Snippet } = $props();
</script>

{#if isAdmin($session)}
	{@render children()}
{:else}
	<div class="alert alert-warn">This page requires the administrator role.</div>
{/if}
