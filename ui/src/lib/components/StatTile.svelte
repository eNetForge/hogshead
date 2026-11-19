<script lang="ts">
	let { label, value, sub = '' }: { label: string; value: string; sub?: string } = $props();
</script>

<div class="tile card">
	<div class="label">{label}</div>
	<div class="value">{value}</div>
	{#if sub}<div class="sub muted small">{sub}</div>{/if}
</div>

<style>
	.label {
		color: var(--muted);
		font-size: 0.8rem;
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}
	.value {
		font-size: 1.6rem;
		font-weight: 600;
		font-variant-numeric: tabular-nums;
		margin-top: 0.2rem;
	}
</style>
