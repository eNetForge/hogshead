<script lang="ts">
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { admin } from '$lib/api/admin';
	import { clearSession, session } from '$lib/stores/session';
	import Icon from './Icon.svelte';
	import PasswordDialog from './PasswordDialog.svelte';
	import RoleBadge from './RoleBadge.svelte';

	let { onmenu }: { onmenu: () => void } = $props();
	let passwordOpen = $state(false);

	async function logout() {
		if ($session?.mode === 'cookie') {
			try {
				await admin.login.logout();
			} catch {
				// the cookie is dropped locally either way
			}
		}
		clearSession();
		goto(`${base}/login`);
	}
</script>

<header class="topbar">
	<button class="btn btn-ghost btn-icon menu" type="button" aria-label="Open menu" onclick={onmenu}>
		<Icon name="menu" />
	</button>
	<div class="grow"></div>
	{#if $session}
		<span class="user">
			<span class="name">{$session.user.name}</span>
			<RoleBadge role={$session.user.role} />
		</span>
		<button class="btn btn-ghost btn-sm" type="button" onclick={() => (passwordOpen = true)} title="Change your web UI password">
			<Icon name="key" size={16} />
			Password
		</button>
		<button class="btn btn-ghost btn-sm" type="button" onclick={logout}>
			<Icon name="logout" size={16} />
			Log out
		</button>
		<PasswordDialog bind:open={passwordOpen} user={$session.user.name} self />
	{/if}
</header>

<style>
	.topbar {
		position: sticky;
		top: 0;
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 1rem;
		background: var(--surface);
		border-bottom: 1px solid var(--border);
		z-index: 20;
		min-height: 50px;
	}
	.menu {
		display: none;
	}
	.user {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
	}
	.name {
		font-weight: 600;
	}
	@media (max-width: 860px) {
		.menu {
			display: inline-flex;
		}
	}
</style>
