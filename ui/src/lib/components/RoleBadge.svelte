<script lang="ts">
	let { role }: { role: string } = $props();
	const cls = $derived(role === 'admin' ? 'badge-accent' : role === 'read-write' ? 'badge-success' : '');
</script>

<span class="badge {cls}">{role}</span>
