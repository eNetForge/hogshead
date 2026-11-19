<script lang="ts">
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { admin } from '$lib/api/admin';
	import { ApiError } from '$lib/api/http';
	import { setSession } from '$lib/stores/session';

	let mode = $state<'password' | 'key'>('password');
	let username = $state('');
	let password = $state('');
	let accessKeyId = $state('');
	let secretAccessKey = $state('');
	let remember = $state(false);
	let busy = $state(false);
	let error = $state('');

	function finish() {
		const next = page.url.searchParams.get('next');
		goto(`${base}${next && next.startsWith('/') ? next : '/'}`, { replaceState: true });
	}

	async function submitPassword(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		if (!username.trim() || !password) {
			error = 'User name and password are required.';
			return;
		}
		busy = true;
		try {
			const me = await admin.login.password(username.trim(), password);
			setSession({ mode: 'cookie', user: me.user, remember: true });
			finish();
		} catch (err) {
			if (err instanceof ApiError && err.status === 401) error = 'Invalid user name or password.';
			else if (err instanceof ApiError && err.status === 429) error = 'Too many failed attempts. Try again in a few minutes.';
			else error = err instanceof Error ? err.message : 'Login failed';
		} finally {
			busy = false;
		}
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		const creds = { accessKeyId: accessKeyId.trim(), secretAccessKey: secretAccessKey.trim() };
		if (!creds.accessKeyId || !creds.secretAccessKey) {
			error = 'Both the access key ID and the secret are required.';
			return;
		}
		busy = true;
		try {
			const me = await admin.me(creds);
			if (me.user.disabled) {
				error = 'This user is disabled.';
				return;
			}
			setSession({ mode: 'key', credentials: creds, user: me.user, remember });
			finish();
		} catch (err) {
			if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
				error = 'Invalid access key or secret.';
			} else {
				error = err instanceof Error ? err.message : 'Login failed';
			}
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Log in · hogshead</title></svelte:head>

<div class="login">
	<form class="card" onsubmit={mode === 'password' ? submitPassword : submit} aria-describedby={error ? 'login-error' : undefined}>
		<div class="brand">
			<svg width="36" height="36" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="7" fill="var(--accent)" /><path d="M8 11.5a8 2.5 0 0 1 16 0v9a8 2.5 0 0 1-16 0z" fill="none" stroke="#fff" stroke-width="2" /><ellipse cx="16" cy="11.5" rx="8" ry="2.5" fill="none" stroke="#fff" stroke-width="2" /><path d="M8 16a8 2.5 0 0 0 16 0" fill="none" stroke="#fff" stroke-width="2" /></svg>
			<div>
				<h1>hogshead</h1>
				<div class="muted small">{mode === 'password' ? 'Sign in with your user name and password' : 'Sign in with an access key'}</div>
			</div>
		</div>

		<div class="tabs" role="tablist" aria-label="Sign-in method">
			<button type="button" role="tab" class:active={mode === 'password'} aria-selected={mode === 'password'} onclick={() => ((mode = 'password'), (error = ''))}>Password</button>
			<button type="button" role="tab" class:active={mode === 'key'} aria-selected={mode === 'key'} onclick={() => ((mode = 'key'), (error = ''))}>Access key</button>
		</div>

		{#if error}
			<div class="alert alert-danger" id="login-error" role="alert">{error}</div>
		{/if}

		{#if mode === 'password'}
			<div class="field">
				<label for="user">User name</label>
				<input id="user" type="text" bind:value={username} autocomplete="username" autocapitalize="off" spellcheck="false" required />
			</div>
			<div class="field">
				<label for="pw">Password</label>
				<input id="pw" type="password" bind:value={password} autocomplete="current-password" required />
				<div class="hint">Stays signed in on this browser for 7 days. An administrator sets passwords with <span class="mono">hogshead user passwd</span> or on the Users page.</div>
			</div>
			<button class="btn btn-primary" type="submit" disabled={busy} style="width:100%;justify-content:center">
				{#if busy}<span class="spinner"></span>{/if}
				Log in
			</button>
		{:else}
		<div class="field">
			<label for="ak">Access key ID</label>
			<input id="ak" type="text" bind:value={accessKeyId} autocomplete="username" autocapitalize="off" spellcheck="false" required />
		</div>
		<div class="field">
			<label for="sk">Secret access key</label>
			<input id="sk" type="password" bind:value={secretAccessKey} autocomplete="current-password" required />
		</div>
		<div class="field">
			<label class="checkbox">
				<input type="checkbox" bind:checked={remember} />
				Remember me on this device
			</label>
			<div class="hint">Keys are stored in this browser only and used to sign requests; they are never sent as plain text.</div>
		</div>
		<button class="btn btn-primary" type="submit" disabled={busy} style="width:100%;justify-content:center">
			{#if busy}<span class="spinner"></span>{/if}
			Log in
		</button>
		{/if}
	</form>
</div>

<style>
	.login {
		min-height: 100vh;
		display: grid;
		place-items: center;
		padding: 1rem;
	}
	form {
		width: min(400px, 100%);
		padding: 1.5rem;
	}
	.brand {
		display: flex;
		gap: 0.8rem;
		align-items: center;
		margin-bottom: 1.25rem;
	}
	.brand h1 {
		margin: 0;
	}
	.tabs {
		display: flex;
		gap: 0.25rem;
		margin-bottom: 1rem;
		border-bottom: 1px solid var(--border);
	}
	.tabs button {
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		padding: 0.5rem 0.75rem;
		color: var(--muted);
		font: inherit;
		cursor: pointer;
	}
	.tabs button.active {
		color: var(--text);
		border-bottom-color: var(--accent);
		font-weight: 600;
	}
</style>
