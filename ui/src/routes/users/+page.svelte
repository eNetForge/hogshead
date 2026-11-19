<script lang="ts">
	import { onMount } from 'svelte';
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { admin, type AccessKey, type CreatedKey, type User } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import PasswordDialog from '$lib/components/PasswordDialog.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import RoleBadge from '$lib/components/RoleBadge.svelte';
	import { formatDate } from '$lib/format';
	import { clearSession, session } from '$lib/stores/session';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	const ROLES = ['admin', 'read-write', 'read-only'];

	let users = $state<User[]>([]);
	let loading = $state(true);
	let busy = $state(false);

	let createOpen = $state(false);
	let createForm = $state({ name: '', role: 'read-write', buckets: '' });

	let editOpen = $state(false);
	let editUser = $state<User | null>(null);
	let editForm = $state({ role: '', buckets: '', disabled: false });

	let deleteUser = $state<User | null>(null);
	let deleteOpen = $state(false);

	let keysUser = $state<User | null>(null);
	let keys = $state<AccessKey[]>([]);
	let keysLoading = $state(false);
	let keyLabel = $state('');
	let createdKey = $state<CreatedKey | null>(null);
	let passwordUser = $state<string | null>(null);
	let inCellar = $state(false);
	const CELLAR_USER = 'cellar';
	const managed = (name: string) => inCellar && name === CELLAR_USER;
	let passwordOpen = $state(false);
	let revokeId = $state<string | null>(null);
	let revokeOpen = $state(false);
	let logoutAllOpen = $state(false);

	const parseBuckets = (s: string) => s.split(',').map((b) => b.trim()).filter(Boolean);

	async function load() {
		admin.cellar.status().then((st) => (inCellar = st.active)).catch(() => (inCellar = false));
		loading = true;
		try {
			users = await admin.users.list();
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	async function create(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		try {
			await admin.users.create({ name: createForm.name.trim(), role: createForm.role, buckets: parseBuckets(createForm.buckets) });
			toastSuccess(`User ${createForm.name} created`);
			createOpen = false;
			createForm = { name: '', role: 'read-write', buckets: '' };
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	function openEdit(u: User) {
		editUser = u;
		editForm = { role: u.role, buckets: (u.buckets ?? []).join(', '), disabled: u.disabled };
		editOpen = true;
	}

	async function saveEdit(e: SubmitEvent) {
		e.preventDefault();
		if (!editUser) return;
		busy = true;
		try {
			await admin.users.update(editUser.name, { role: editForm.role, buckets: parseBuckets(editForm.buckets), disabled: editForm.disabled });
			toastSuccess(`User ${editUser.name} updated`);
			editOpen = false;
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	async function confirmDelete() {
		if (!deleteUser) return;
		busy = true;
		try {
			await admin.users.remove(deleteUser.name);
			toastSuccess(`User ${deleteUser.name} deleted`);
			deleteOpen = false;
			if (keysUser?.name === deleteUser.name) keysUser = null;
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	async function showKeys(u: User) {
		keysUser = u;
		createdKey = null;
		keysLoading = true;
		try {
			keys = await admin.keys.listForUser(u.name);
		} catch (e) {
			toastError(e);
		} finally {
			keysLoading = false;
		}
	}

	async function createKey(e: SubmitEvent) {
		e.preventDefault();
		if (!keysUser) return;
		busy = true;
		try {
			createdKey = await admin.keys.create(keysUser.name, keyLabel.trim() || undefined);
			keyLabel = '';
			keys = await admin.keys.listForUser(keysUser.name);
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	async function toggleKey(k: AccessKey) {
		busy = true;
		try {
			const updated = await admin.keys.setDisabled(k.access_key_id, !k.disabled);
			keys = keys.map((x) => (x.access_key_id === k.access_key_id ? updated : x));
			toastSuccess(`Key ${k.access_key_id} ${updated.disabled ? 'disabled' : 'enabled'}`);
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	// Replacing the session secret signs out every password login on this
	// server, this one included, so the page reloads to the login screen.
	async function confirmLogoutAll() {
		busy = true;
		try {
			await admin.users.logoutAll();
			logoutAllOpen = false;
			if ($session?.mode === 'cookie') {
				clearSession();
				goto(`${base}/login`);
				return;
			}
			toastSuccess('Every web UI session was signed out');
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	async function confirmRevoke() {
		if (!revokeId) return;
		busy = true;
		try {
			await admin.keys.revoke(revokeId);
			keys = keys.filter((k) => k.access_key_id !== revokeId);
			toastSuccess('Key revoked');
			revokeOpen = false;
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			busy = false;
		}
	}

	onMount(load);
</script>

<svelte:head><title>Users · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Users</h1>
		<div class="row">
			<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
			<button class="btn btn-sm" type="button" onclick={() => (logoutAllOpen = true)} title="Replace the session secret so every password login to the web UI has to sign in again"><Icon name="logout" size={14} /> Sign everyone out</button>
			<button class="btn btn-primary btn-sm" type="button" onclick={() => (createOpen = true)}><Icon name="plus" size={14} /> New user</button>
		</div>
	</div>

	<div class="table-wrap">
		<table>
			<thead>
				<tr><th>Name</th><th>Role</th><th>Bucket scope</th><th class="num">Keys</th><th>Status</th><th>Created</th><th></th></tr>
			</thead>
			<tbody>
				{#if loading && users.length === 0}
					<tr><td colspan="7" class="empty"><span class="spinner"></span></td></tr>
				{:else}
					{#each users as u (u.id)}
						<tr>
							<td><strong>{u.name}</strong>{#if u.name === $session?.user.name}<span class="muted small"> (you)</span>{/if}</td>
							<td><RoleBadge role={u.role} />{#if managed(u.name)}<span class="badge" style="margin-left:0.4rem" title="The cellar's service account: rotated and removed by the cellar itself">cellar service account</span>{/if}</td>
							<td class="small">{u.buckets?.length ? u.buckets.join(', ') : u.role === 'admin' ? 'all' : 'all buckets'}</td>
							<td class="num">{u.keys ?? 0}</td>
							<td>{#if u.disabled}<span class="badge badge-danger">disabled</span>{:else}<span class="badge badge-success">active</span>{/if}</td>
							<td class="small">{formatDate(u.created_at)}</td>
							<td class="actions">
								<button class="btn btn-sm" type="button" onclick={() => { passwordUser = u.name; passwordOpen = true; }} disabled={managed(u.name)} title={managed(u.name) ? 'Managed by the cellar' : u.has_password ? 'Change or remove the web UI password' : 'Set a web UI password'}><Icon name="key" size={14} /> {u.has_password ? 'Password' : 'Set password'}</button>
								<button class="btn btn-sm" type="button" onclick={() => showKeys(u)}><Icon name="key" size={14} /> Keys</button>
								<button class="btn btn-sm" type="button" onclick={() => openEdit(u)} disabled={managed(u.name)}><Icon name="edit" size={14} /> Edit</button>
								<button class="btn btn-sm btn-danger" type="button" onclick={() => { deleteUser = u; deleteOpen = true; }} disabled={u.name === $session?.user.name || managed(u.name)} aria-label="Delete {u.name}"><Icon name="trash" size={14} /></button>
							</td>
						</tr>
					{:else}
						<tr><td colspan="7" class="empty">No users.</td></tr>
					{/each}
				{/if}
			</tbody>
		</table>
	</div>

	{#if keysUser}
		<section class="card" style="margin-top:1rem">
			<div class="card-title">
				<h2>Access keys for {keysUser.name}</h2>
				<button class="btn btn-ghost btn-icon" type="button" aria-label="Close keys panel" onclick={() => (keysUser = null)}><Icon name="close" /></button>
			</div>

			{#if createdKey}
				<div class="alert alert-warn" role="alert">
					<strong>Save this secret now.</strong> It is shown only once and cannot be retrieved later.
					<dl class="kv" style="margin-top:.5rem">
						<dt>Access key ID</dt><dd class="mono row">{createdKey.access_key_id} <CopyButton text={createdKey.access_key_id} small /></dd>
						<dt>Secret access key</dt><dd class="mono row">{createdKey.secret_access_key} <CopyButton text={createdKey.secret_access_key} small /></dd>
					</dl>
				</div>
			{/if}

			<form class="row" onsubmit={createKey} style="margin-bottom:1rem">
				<label for="key-label" class="sr-only">Key label</label>
				<input id="key-label" type="text" placeholder="Label (optional)" bind:value={keyLabel} style="max-width:260px" />
				<button class="btn btn-primary btn-sm" type="submit" disabled={busy}><Icon name="plus" size={14} /> Create key</button>
			</form>

			<div class="table-wrap">
				<table>
					<thead><tr><th>Access key ID</th><th>Label</th><th>Status</th><th>Created</th><th></th></tr></thead>
					<tbody>
						{#if keysLoading}
							<tr><td colspan="5" class="empty"><span class="spinner"></span></td></tr>
						{:else}
							{#each keys as k (k.access_key_id)}
								<tr>
									<td class="mono">{k.access_key_id}</td>
									<td>{k.label || '–'}</td>
									<td>{#if k.disabled}<span class="badge badge-danger">disabled</span>{:else}<span class="badge badge-success">active</span>{/if}</td>
									<td class="small">{formatDate(k.created_at)}</td>
									<td class="actions">
										<button class="btn btn-sm" type="button" onclick={() => toggleKey(k)} disabled={busy}>{k.disabled ? 'Enable' : 'Disable'}</button>
										<button class="btn btn-sm btn-danger" type="button" onclick={() => { revokeId = k.access_key_id; revokeOpen = true; }} disabled={busy}>Revoke</button>
									</td>
								</tr>
							{:else}
								<tr><td colspan="5" class="empty">No keys.</td></tr>
							{/each}
						{/if}
					</tbody>
				</table>
			</div>
		</section>
	{/if}

	<Dialog bind:open={createOpen} title="New user">
		<form id="create-user" onsubmit={create}>
			<div class="field">
				<label for="cu-name">Name</label>
				<input id="cu-name" type="text" bind:value={createForm.name} required autocapitalize="off" spellcheck="false" pattern="[A-Za-z0-9._@\-]{'{'}1,64{'}'}" />
				<div class="hint">1–64 characters: letters, digits, - _ . @</div>
			</div>
			<div class="field">
				<label for="cu-role">Role</label>
				<select id="cu-role" bind:value={createForm.role}>{#each ROLES as r (r)}<option value={r}>{r}</option>{/each}</select>
			</div>
			<div class="field">
				<label for="cu-buckets">Bucket scope</label>
				<input id="cu-buckets" type="text" bind:value={createForm.buckets} placeholder="bucket-a, bucket-b" />
				<div class="hint">Comma-separated bucket names. Empty means all buckets.</div>
			</div>
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (createOpen = false)} disabled={busy}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="create-user" disabled={busy || !createForm.name.trim()}>Create</button>
		{/snippet}
	</Dialog>

	<Dialog bind:open={editOpen} title="Edit {editUser?.name ?? 'user'}">
		<form id="edit-user" onsubmit={saveEdit}>
			<div class="field">
				<label for="eu-role">Role</label>
				<select id="eu-role" bind:value={editForm.role}>{#each ROLES as r (r)}<option value={r}>{r}</option>{/each}</select>
			</div>
			<div class="field">
				<label for="eu-buckets">Bucket scope</label>
				<input id="eu-buckets" type="text" bind:value={editForm.buckets} placeholder="bucket-a, bucket-b" />
				<div class="hint">Comma-separated bucket names. Empty means all buckets.</div>
			</div>
			<div class="field">
				<label class="checkbox"><input type="checkbox" bind:checked={editForm.disabled} disabled={editUser?.name === $session?.user.name} /> Disabled</label>
			</div>
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (editOpen = false)} disabled={busy}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="edit-user" disabled={busy}>Save</button>
		{/snippet}
	</Dialog>

	{#if passwordUser}
		<PasswordDialog bind:open={passwordOpen} user={passwordUser} onsaved={load} />
	{/if}
	<ConfirmDialog bind:open={deleteOpen} title="Delete user" message="Delete user “{deleteUser?.name}” and all of their access keys?" {busy} onconfirm={confirmDelete} />
	<ConfirmDialog bind:open={logoutAllOpen} title="Sign everyone out" message="Sign out every web UI session on this server, including yours if you logged in with a password? Access keys and passwords are not changed." confirmLabel="Sign everyone out" {busy} onconfirm={confirmLogoutAll} />
	<ConfirmDialog bind:open={revokeOpen} title="Revoke key" message="Revoke access key {revokeId}? Clients using it will stop working immediately." confirmLabel="Revoke" {busy} onconfirm={confirmRevoke} />
</AdminOnly>
