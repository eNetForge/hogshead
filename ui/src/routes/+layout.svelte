<script lang="ts">
	import '../app.css';
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import Sidebar from '$lib/components/Sidebar.svelte';
	import Toasts from '$lib/components/Toasts.svelte';
	import TopBar from '$lib/components/TopBar.svelte';
	import { clearSession, loadSession, session, updateSessionUser } from '$lib/stores/session';
	import { admin } from '$lib/api/admin';
	import { serverName } from '$lib/stores/server';
	import { isAdmin } from '$lib/stores/session';

	let { children } = $props();
	let ready = $state(false);
	let menuOpen = $state(false);

	const isLogin = $derived(page.url.pathname.replace(base, '') === '/login');

	onMount(async () => {
		const s = loadSession();
		if (s?.mode === 'cookie') {
			// The cookie may have expired or been cleared server-side; check before showing the app.
			try {
				const me = await admin.login.me();
				updateSessionUser(me.user);
			} catch {
				clearSession();
			}
		}
		ready = true;
		if (isAdmin($session)) {
			admin.status().then((st) => serverName.set(st.node_name ?? '')).catch(() => {});
		}
	});

	$effect(() => {
		if (!ready) return;
		if (!$session && !isLogin) {
			const next = page.url.pathname.replace(base, '') + page.url.search;
			goto(`${base}/login${next && next !== '/' ? `?next=${encodeURIComponent(next)}` : ''}`, { replaceState: true });
		}
	});
</script>

<Toasts />

{#if !ready}
	<div class="boot"><span class="spinner"></span></div>
{:else if isLogin || !$session}
	{@render children()}
{:else}
	<div class="shell">
		<Sidebar bind:open={menuOpen} />
		<div class="main">
			<TopBar onmenu={() => (menuOpen = true)} />
			<main>
				{@render children()}
			</main>
		</div>
	</div>
{/if}

<style>
	.boot {
		display: grid;
		place-items: center;
		height: 100vh;
	}
	.shell {
		min-height: 100vh;
	}
	.main {
		margin-left: var(--sidebar-w);
		min-height: 100vh;
		display: flex;
		flex-direction: column;
	}
	main {
		padding: 1.25rem;
		max-width: 1280px;
		width: 100%;
		flex: 1;
	}
	@media (max-width: 860px) {
		.main {
			margin-left: 0;
		}
		main {
			padding: 1rem;
		}
	}
</style>
