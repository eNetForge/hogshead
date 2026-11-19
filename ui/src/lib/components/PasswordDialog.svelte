<script lang="ts">
	import { admin } from '$lib/api/admin';
	import Dialog from './Dialog.svelte';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let {
		open = $bindable(false),
		user,
		self = false,
		onsaved
	}: {
		open?: boolean;
		/** The user whose password is set. */
		user: string;
		/** Wording for changing your own password. */
		self?: boolean;
		onsaved?: () => void;
	} = $props();

	let password = $state('');
	let repeat = $state('');
	let saving = $state(false);

	$effect(() => {
		if (open) {
			password = '';
			repeat = '';
		}
	});

	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (password.length < 8) return void toastError('The password must be at least 8 characters');
		if (password !== repeat) return void toastError('The passwords do not match');
		saving = true;
		try {
			await admin.users.setPassword(user, password);
			toastSuccess(self ? 'Password changed' : `Password set for ${user}`);
			open = false;
			onsaved?.();
		} catch (err) {
			toastError(err);
		} finally {
			saving = false;
		}
	}

	async function remove() {
		saving = true;
		try {
			await admin.users.setPassword(user, '');
			toastSuccess(`Password login removed for ${user}`);
			open = false;
			onsaved?.();
		} catch (err) {
			toastError(err);
		} finally {
			saving = false;
		}
	}
</script>

<Dialog bind:open title={self ? 'Change password' : `Set password for ${user}`}>
	<form id="password-form" onsubmit={save}>
		<div class="field">
			<label for="pw-new">New password</label>
			<input id="pw-new" type="password" bind:value={password} autocomplete="new-password" minlength="8" required />
			<div class="hint">At least 8 characters. Used only to sign in to this web UI; S3 clients keep using access keys.</div>
		</div>
		<div class="field">
			<label for="pw-repeat">Repeat password</label>
			<input id="pw-repeat" type="password" bind:value={repeat} autocomplete="new-password" required />
		</div>
	</form>
	{#snippet footer()}
		{#if !self}
			<button class="btn btn-danger grow-start" type="button" onclick={remove} disabled={saving}>Remove password login</button>
		{/if}
		<button class="btn" type="button" onclick={() => (open = false)} disabled={saving}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="password-form" disabled={saving || !password || !repeat}>
			{#if saving}<span class="spinner"></span>{/if}
			Save
		</button>
	{/snippet}
</Dialog>

<style>
	.grow-start {
		margin-right: auto;
	}
</style>
