<script lang="ts">
	import { onMount } from 'svelte';
	import { admin, type CellarMember, type CellarPending, type CellarStatus, type Peer } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatNumber, formatRelative } from '$lib/format';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let status = $state<CellarStatus | null>(null);
	let peers = $state<Peer[]>([]);
	let loading = $state(true);
	let busy = $state('');

	// Create form.
	let createOpen = $state(false);
	let newName = $state('');
	let newAck = $state('one');
	// Join form.
	let joinOpen = $state(false);
	let joinAddress = $state('');
	let joinAdvertise = $state('');
	let joinMerge = $state(false);
	let localBuckets = $state<string[]>([]);
	// Settings.
	let settingsOpen = $state(false);
	let editName = $state('');
	let editAck = $state('one');
	// Confirmations.
	let removing = $state<CellarMember | null>(null);
	let leaveOpen = $state(false);

	const cellarsSeen = $derived.by(() => {
		const seen = new Map<string, { id: string; name: string; peers: Peer[] }>();
		for (const p of peers) {
			if (!p.cellar) continue;
			const c = seen.get(p.cellar) ?? { id: p.cellar, name: p.cellar_name ?? p.cellar, peers: [] };
			c.peers.push(p);
			seen.set(p.cellar, c);
		}
		return [...seen.values()];
	});

	async function load() {
		try {
			const [st, ps] = await Promise.all([admin.cellar.status(), admin.peers().catch(() => [] as Peer[])]);
			// Never trust list fields to be present: a missing one must not take the page down.
			status = { ...st, members: st.members ?? [], pending: st.pending ?? [] };
			peers = ps ?? [];
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
		const t = setInterval(() => void load(), 5000);
		return () => clearInterval(t);
	});

	async function run(label: string, fn: () => Promise<unknown>, ok: string) {
		busy = label;
		try {
			await fn();
			toastSuccess(ok);
			await load();
			return true;
		} catch (e) {
			toastError(e);
			return false;
		} finally {
			busy = '';
		}
	}

	async function create(e: SubmitEvent) {
		e.preventDefault();
		if (!newName.trim()) return void toastError('Give the cellar a name');
		if (await run('create', () => admin.cellar.init(newName.trim(), newAck), `Cellar “${newName.trim()}” created`)) createOpen = false;
	}

	async function openJoin(address = '') {
		joinAddress = address;
		joinMerge = false;
		try {
			localBuckets = (await admin.buckets.list()).map((b) => b.name);
		} catch {
			localBuckets = [];
		}
		joinOpen = true;
	}

	async function join(e: SubmitEvent) {
		e.preventDefault();
		if (!joinAddress.trim()) return void toastError('Enter the address of a cellar member');
		if (localBuckets.length > 0 && !joinMerge) return void toastError('This server has buckets. Tick "merge" to add them to the cellar, or delete them first.');
		if (await run('join', () => admin.cellar.join(joinAddress.trim(), joinAdvertise.trim(), joinMerge), 'Join requested. An administrator of the cellar must approve this server.')) joinOpen = false;
	}

	function joinPeer(p: Peer) {
		void openJoin(p.endpoint);
	}

	function openSettings() {
		editName = status?.name ?? '';
		editAck = status?.write_ack ?? 'one';
		settingsOpen = true;
	}

	async function saveSettings(e: SubmitEvent) {
		e.preventDefault();
		if (await run('settings', () => admin.cellar.update({ name: editName.trim() || undefined, write_ack: editAck }), 'Cellar settings saved')) settingsOpen = false;
	}

	function memberState(m: CellarMember): { label: string; badge: string } {
		if (m.self) return { label: 'this server', badge: 'badge-accent' };
		if (m.joining) return { label: 'joining', badge: 'badge-accent' };
		if (m.last_error) return { label: 'unreachable', badge: 'badge-danger' };
		if (m.backfilling) return { label: 'backfilling', badge: 'badge-accent' };
		if (!m.enabled) return { label: 'paused', badge: 'badge-warn' };
		if (m.lag > 0) return { label: 'syncing', badge: 'badge-accent' };
		return { label: 'in sync', badge: 'badge-success' };
	}

	const ackLabel: Record<string, string> = {
		local: 'Local: a write is acknowledged once stored here (fastest, a dead node can lose its last seconds of writes).',
		one: 'One: a write is acknowledged once at least one other member holds it (recommended).',
		all: 'All: every member must hold a write before it is acknowledged (a down member stalls writes).'
	};
</script>

<svelte:head><title>Cellar · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Cellar</h1>
		<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
	</div>

	{#if loading && !status}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if status && !status.active}
		<p class="muted small" style="margin-top:0">
			A cellar is a group of hogshead servers that keep a full copy of each other's buckets, users and settings, with no leader and no quorum. Every member serves reads and writes; changes travel to the others immediately and a member that was down catches up when it returns.
		</p>

		{#if status.joining}
			<section class="card">
				<div class="card-title"><h2>Join requested</h2></div>
				<p>
					This server asked <span class="mono">{status.joining.address}</span> to join {formatRelative(status.joining.requested_at)}. An administrator of that cellar must approve it on their Cellar page; this page updates automatically once that happens.
				</p>
				{#if status.joining.last_error}<div class="alert alert-danger">{status.joining.last_error}</div>{/if}
				<div class="row row-end">
					<button class="btn btn-danger" type="button" disabled={busy === 'cancel'} onclick={() => run('cancel', () => admin.cellar.cancelJoin(), 'Join request withdrawn')}>Withdraw request</button>
				</div>
			</section>
		{:else}
			<div class="grid grid-2">
				<section class="card">
					<div class="card-title"><h2>Create a cellar</h2></div>
					<p class="muted small">Start a new cellar with this server as its first member. Other servers then ask to join and you approve them here.</p>
					<button class="btn btn-primary" type="button" onclick={() => (createOpen = true)}><Icon name="plus" size={14} /> Create cellar</button>
				</section>
				<section class="card">
					<div class="card-title"><h2>Join a cellar</h2></div>
					{#if cellarsSeen.length > 0}
						<p class="muted small">Cellars seen on this network:</p>
						<ul class="cellars">
							{#each cellarsSeen as c (c.id)}
								<li>
									<div>
										<strong>{c.name}</strong>
										<div class="muted small">{c.peers.map((p) => p.name || p.addr).join(', ')}</div>
									</div>
									<button class="btn btn-sm btn-primary" type="button" onclick={() => joinPeer(c.peers[0])}>Join</button>
								</li>
							{/each}
						</ul>
					{:else}
						<p class="muted small">No cellar heard on this network yet. You can still join one by address.</p>
					{/if}
					<button class="btn" type="button" onclick={() => openJoin()}>Join by address…</button>
				</section>
			</div>
		{/if}
	{:else if status}
		{#if !status.ready}
			<div class="alert alert-warn" role="status">
				<strong>Catching up.</strong> {formatNumber(status.backlog)} change{status.backlog === 1 ? '' : 's'} are still on the way from other members. Reads and writes work; the readiness check reports not ready until the backlog is drained.
				{#if status.check_error}<div class="small">{status.check_error}</div>{/if}
			</div>
		{/if}

		<section class="card">
			<div class="card-title">
				<h2>{status.name}</h2>
				<div class="row">
					<button class="btn btn-sm" type="button" onclick={openSettings}><Icon name="settings" size={14} /> Settings</button>
					<button class="btn btn-sm btn-danger" type="button" onclick={() => (leaveOpen = true)}>Leave cellar</button>
				</div>
			</div>
			<dl class="kv">
				<dt>Cellar id</dt><dd class="mono small">{status.id}</dd>
				<dt>Write acknowledgement</dt><dd>{status.write_ack} <span class="muted small">— {ackLabel[status.write_ack ?? 'one']}</span></dd>
				<dt>Members</dt><dd>{formatNumber(status.members.length)}</dd>
				<dt>State</dt><dd>{#if status.ready}<span class="badge badge-success">ready</span>{:else}<span class="badge badge-warn">catching up</span>{/if} {#if status.last_check}<span class="muted small">checked {formatRelative(status.last_check)}</span>{/if}</dd>
			</dl>
		</section>

		{#if status.pending.length > 0}
			<section class="card" style="margin-top:1rem">
				<div class="card-title"><h2>Join requests</h2></div>
				<p class="muted small">Approving a server gives it the cellar's admin credentials and a full copy of all data. Check the id and address match the server you expect.</p>
				<div class="table-wrap">
					<table>
						<thead><tr><th>Server</th><th>Id</th><th>Endpoint</th><th>Version</th><th>Brings</th><th>Requested</th><th></th></tr></thead>
						<tbody>
							{#each status.pending as p (p.id)}
								<tr>
									<td><strong>{p.name || '(unnamed)'}</strong></td>
									<td class="mono small">{p.id}</td>
									<td class="mono small">{p.endpoint}</td>
									<td class="small">{p.version || '–'}</td>
									<td class="small">{#if p.buckets}<span class="badge badge-warn" title="This server already has data; its buckets will be merged into the cellar">{formatNumber(p.buckets)} bucket{p.buckets === 1 ? '' : 's'} (merge)</span>{:else}<span class="muted">empty</span>{/if}</td>
									<td class="small">{formatRelative(p.requested_at)}</td>
									<td class="actions">
										<button class="btn btn-sm btn-ghost" type="button" disabled={busy === p.id} onclick={() => run(p.id, () => admin.cellar.reject(p.id), `Rejected ${p.name || p.id}`)}>Reject</button>
										<button class="btn btn-sm btn-primary" type="button" disabled={busy === p.id} onclick={() => run(p.id, () => admin.cellar.approve(p.id), `${p.name || p.id} approved; it joins within seconds`)}>Approve</button>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</section>
		{/if}

		<section class="card" style="margin-top:1rem">
			<div class="card-title"><h2>Members</h2></div>
			<div class="table-wrap">
				<table>
					<thead><tr><th>Member</th><th>Endpoint</th><th>Version</th><th>Joined</th><th class="num">Pending to it</th><th>State</th><th></th></tr></thead>
					<tbody>
						{#each status.members as m (m.id)}
							{@const st = memberState(m)}
							<tr>
								<td><strong>{m.name}</strong><div class="mono small muted">{m.id}</div></td>
								<td class="mono small">{m.endpoint}</td>
								<td class="small">{m.version || '–'}</td>
								<td class="small">{formatRelative(m.joined_at)}</td>
								<td class="num">{m.self ? '–' : formatNumber(m.lag)}</td>
								<td><span class="badge {st.badge}" title={m.last_error ?? ''}>{st.label}</span>{#if m.last_error}<div class="small error" title={m.last_error}>{m.last_error}</div>{/if}</td>
								<td class="actions">
									{#if !m.self}
										<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Remove {m.name}" disabled={busy === m.id} onclick={() => (removing = m)}><Icon name="trash" size={14} /></button>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			<p class="muted small">"Pending to it" counts changes made here that the member has not received yet. A member that was down receives everything it missed when it returns.</p>
		</section>

		{#if status.notices && status.notices.length > 0}
			<section class="card" style="margin-top:1rem">
				<div class="card-title"><h2>Notices</h2></div>
				<ul class="notices">
					{#each [...status.notices].reverse() as n (n.at + n.message)}
						<li><span class="muted small">{formatRelative(n.at)}</span> {n.message}</li>
					{/each}
				</ul>
			</section>
		{/if}

		{#if cellarsSeen.some((c) => c.id !== status?.id)}
			<p class="muted small" style="margin-top:1rem">Other cellars on this network: {cellarsSeen.filter((c) => c.id !== status?.id).map((c) => c.name).join(', ')}.</p>
		{/if}
	{/if}

	<Dialog bind:open={createOpen} title="Create a cellar">
		<form id="cellar-create" onsubmit={create}>
			<div class="field">
				<label for="cl-name">Cellar name</label>
				<input id="cl-name" type="text" bind:value={newName} placeholder="office" required />
			</div>
			<div class="field">
				<label for="cl-ack">Write acknowledgement</label>
				<select id="cl-ack" bind:value={newAck}>
					<option value="one">One other member (recommended)</option>
					<option value="all">All members</option>
					<option value="local">Local only</option>
				</select>
				<div class="hint">{ackLabel[newAck]}</div>
			</div>
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (createOpen = false)} disabled={busy === 'create'}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="cellar-create" disabled={busy === 'create'}>Create</button>
		{/snippet}
	</Dialog>

	<Dialog bind:open={joinOpen} title="Join a cellar">
		<form id="cellar-join" onsubmit={join}>
			<div class="field">
				<label for="cl-addr">Address of any member</label>
				<input id="cl-addr" type="text" bind:value={joinAddress} placeholder="192.0.2.10:9000 or http://hogshead1:9000" required />
			</div>
			<div class="field">
				<label for="cl-adv">How members reach this server</label>
				<input id="cl-adv" type="text" bind:value={joinAdvertise} placeholder="(derived from this server's address)" />
				<div class="hint">Set it when this server has several addresses or sits behind a proxy.</div>
			</div>
			{#if localBuckets.length > 0}
				<div class="field">
					<label class="checkbox"><input type="checkbox" bind:checked={joinMerge} /> Merge my {localBuckets.length} existing bucket{localBuckets.length === 1 ? '' : 's'} into the cellar</label>
					<div class="hint">
						<span class="mono">{localBuckets.slice(0, 6).join(', ')}{localBuckets.length > 6 ? ', …' : ''}</span>.
						A server normally joins empty. If you merge: buckets only this server has are added to the cellar with their settings; for buckets that already exist in the cellar the cellar's configuration wins and both sides' objects are kept, the newest version of a key being current; same-named users take the cellar's definition (keys from both sides stay); this server's settings are replaced by the cellar's. A bucket with Object Lock on one side only cannot be merged.
					</div>
				</div>
			{:else}
				<div class="alert alert-info small">Joining imports the cellar's users, keys and settings, replacing same-named users here, and copies all of the cellar's buckets to this server.</div>
			{/if}
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (joinOpen = false)} disabled={busy === 'join'}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="cellar-join" disabled={busy === 'join'}>Request to join</button>
		{/snippet}
	</Dialog>

	<Dialog bind:open={settingsOpen} title="Cellar settings">
		<form id="cellar-settings" onsubmit={saveSettings}>
			<div class="field">
				<label for="cl-ename">Name</label>
				<input id="cl-ename" type="text" bind:value={editName} />
			</div>
			<div class="field">
				<label for="cl-eack">Write acknowledgement</label>
				<select id="cl-eack" bind:value={editAck}>
					<option value="one">One other member</option>
					<option value="all">All members</option>
					<option value="local">Local only</option>
				</select>
				<div class="hint">{ackLabel[editAck]} Applies to every member.</div>
			</div>
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (settingsOpen = false)} disabled={busy === 'settings'}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="cellar-settings" disabled={busy === 'settings'}>Save</button>
		{/snippet}
	</Dialog>

	<ConfirmDialog
		open={removing !== null}
		title="Remove member"
		message="Remove “{removing?.name ?? ''}” from the cellar? The cellar key is rotated so it loses access; its copy of the data stays on it."
		confirmLabel="Remove"
		busy={busy !== ''}
		onconfirm={() => {
			if (removing) void run(removing.id, () => admin.cellar.remove(removing!.id), `Removed ${removing!.name}`).then(() => (removing = null));
		}}
	/>
	<ConfirmDialog
		bind:open={leaveOpen}
		title="Leave the cellar"
		message="Leave “{status?.name ?? ''}”? This server stops exchanging data with the members. Everything already copied stays where it is."
		confirmLabel="Leave"
		busy={busy === 'leave'}
		onconfirm={() => {
			void run('leave', () => admin.cellar.leave(), 'Left the cellar').then(() => (leaveOpen = false));
		}}
	/>
</AdminOnly>

<style>
	.cellars {
		list-style: none;
		margin: 0 0 0.75rem;
		padding: 0;
	}
	.cellars li {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--border);
	}
	.cellars li:last-child {
		border-bottom: none;
	}
	.notices {
		margin: 0;
		padding-left: 1.2rem;
	}
	.notices li {
		padding: 0.2rem 0;
	}
	.error {
		color: var(--danger);
		max-width: 22rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
