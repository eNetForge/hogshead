<script lang="ts">
	import { base } from '$app/paths';
	import { page } from '$app/state';
	import { isAdmin, session } from '$lib/stores/session';
	import { serverName } from '$lib/stores/server';
	import Icon from './Icon.svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	type Item = { href: string; label: string; icon: 'dashboard' | 'bucket' | 'cellar' | 'replicate' | 'transfer' | 'usage' | 'warning' | 'users' | 'settings' | 'audit'; admin?: boolean };
	const items: Item[] = [
		{ href: '/', label: 'Dashboard', icon: 'dashboard' },
		{ href: '/buckets', label: 'Buckets', icon: 'bucket' },
		{ href: '/cellar', label: 'Cellar', icon: 'cellar', admin: true },
		{ href: '/replication', label: 'Replication', icon: 'replicate', admin: true },
		{ href: '/transfers', label: 'Transfers', icon: 'transfer', admin: true },
		{ href: '/usage', label: 'Usage', icon: 'usage', admin: true },
		{ href: '/errors', label: 'Errors', icon: 'warning', admin: true },
		{ href: '/users', label: 'Users', icon: 'users', admin: true },
		{ href: '/settings', label: 'Settings', icon: 'settings', admin: true },
		{ href: '/audit', label: 'Audit log', icon: 'audit', admin: true }
	];

	const visible = $derived(items.filter((i) => !i.admin || isAdmin($session)));

	function active(href: string): boolean {
		const p = page.url.pathname.replace(base, '') || '/';
		return href === '/' ? p === '/' : p === href || p.startsWith(href + '/');
	}
</script>

<nav class="sidebar" class:open aria-label="Main">
	<div class="brand">
		<svg width="26" height="26" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="7" fill="var(--accent)" /><path d="M8 11.5a8 2.5 0 0 1 16 0v9a8 2.5 0 0 1-16 0z" fill="none" stroke="#fff" stroke-width="2" /><ellipse cx="16" cy="11.5" rx="8" ry="2.5" fill="none" stroke="#fff" stroke-width="2" /><path d="M8 16a8 2.5 0 0 0 16 0" fill="none" stroke="#fff" stroke-width="2" /></svg>
		<span class="brand-text">hogshead{#if $serverName}<small>{$serverName}</small>{/if}</span>
		<button class="btn btn-ghost btn-icon close" type="button" aria-label="Close menu" onclick={() => (open = false)}>
			<Icon name="close" />
		</button>
	</div>
	<ul>
		{#each visible as item (item.href)}
			<li>
				<a href="{base}{item.href}" class:active={active(item.href)} aria-current={active(item.href) ? 'page' : undefined} onclick={() => (open = false)}>
					<Icon name={item.icon} />
					{item.label}
				</a>
			</li>
		{/each}
	</ul>
</nav>
{#if open}
	<button class="scrim" type="button" aria-label="Close menu" onclick={() => (open = false)}></button>
{/if}

<style>
	.sidebar {
		position: fixed;
		inset: 0 auto 0 0;
		width: var(--sidebar-w);
		background: var(--surface);
		border-right: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		z-index: 30;
		transition: transform 0.18s ease;
	}
	.brand {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		padding: 0.9rem 1rem;
		font-weight: 700;
		font-size: 1.05rem;
		border-bottom: 1px solid var(--border);
	}
	.brand span {
		flex: 1;
	}
	.brand-text {
		display: flex;
		flex-direction: column;
		line-height: 1.1;
	}
	.brand-text small {
		font-weight: 500;
		font-size: 0.75rem;
		color: var(--muted);
	}
	.close {
		display: none;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0.5rem;
	}
	a {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		padding: 0.55rem 0.7rem;
		border-radius: var(--radius);
		color: var(--text);
		font-weight: 500;
	}
	a:hover {
		background: var(--surface-2);
		text-decoration: none;
	}
	a.active {
		background: var(--accent);
		color: var(--accent-text);
	}
	.scrim {
		display: none;
	}
	@media (max-width: 860px) {
		.sidebar {
			transform: translateX(-100%);
			box-shadow: var(--shadow);
		}
		.sidebar.open {
			transform: translateX(0);
		}
		.close {
			display: inline-flex;
		}
		.scrim {
			display: block;
			position: fixed;
			inset: 0;
			background: rgba(0, 0, 0, 0.35);
			border: 0;
			z-index: 25;
			cursor: default;
		}
	}
</style>
