<script lang="ts">
	import { admin, type Peer, type ReplicationRequest, type ReplicationTarget } from '$lib/api/admin';
	import Dialog from './Dialog.svelte';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let {
		open = $bindable(false),
		bucket = '',
		global = false,
		editing = null,
		onsaved
	}: {
		open?: boolean;
		/** Source bucket; ignored in global mode. */
		bucket?: string;
		/** Server-wide target: every bucket mirrors to a bucket of the same name. */
		global?: boolean;
		/** Existing target to edit; null adds a new one. */
		editing?: ReplicationTarget | null;
		onsaved: () => void;
	} = $props();

	let name = $state('');
	let endpoint = $state('');
	let destBucket = $state('');
	let accessKey = $state('');
	let secretKey = $state('');
	let region = $state('');
	let prefix = $state('');
	let pathStyle = $state(true);
	let enabled = $state(true);
	let syncConfig = $state(false);
	let saving = $state(false);
	let peers = $state<Peer[]>([]);

	// Reset the form whenever the dialog opens.
	$effect(() => {
		if (!open) return;
		const t = editing;
		name = t?.name ?? '';
		endpoint = t?.endpoint ?? '';
		destBucket = t?.bucket ?? bucket;
		accessKey = t?.access_key ?? '';
		secretKey = '';
		region = t?.region ?? '';
		prefix = t?.prefix ?? '';
		pathStyle = t?.path_style ?? true;
		enabled = t?.enabled ?? true;
		syncConfig = t?.sync_config ?? false;
		admin
			.peers()
			.then((p) => (peers = p))
			.catch(() => (peers = []));
	});

	function usePeer(p: Peer) {
		endpoint = p.endpoint;
		if (!name.trim()) name = p.name || '';
		pathStyle = true;
	}

	function hostOf(url: string): string {
		try {
			return new URL(url).host;
		} catch {
			return '';
		}
	}

	const derivedName = $derived(name.trim() || hostOf(endpoint));

	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (!/^https?:\/\//.test(endpoint.trim())) return void toastError('Endpoint must start with http:// or https://');
		if (!global && !destBucket.trim()) return void toastError('Destination bucket is required');
		if (!editing && (!accessKey.trim() || !secretKey)) return void toastError('Access key and secret key are required');
		const req: ReplicationRequest = {
			endpoint: endpoint.trim(),
			bucket: global ? undefined : destBucket.trim(),
			region: region.trim() || undefined,
			path_style: pathStyle,
			prefix: prefix,
			enabled
		};
		if (global) req.sync_config = syncConfig;
		if (accessKey.trim()) req.access_key = accessKey.trim();
		if (secretKey) req.secret_key = secretKey;
		saving = true;
		try {
			const target = editing ? editing.name : name.trim();
			if (global) await admin.replication.global.set(target, req);
			else await admin.replication.set(bucket, target, req);
			toastSuccess(editing ? `Target ${editing.name} updated` : global ? `Replicating every bucket to ${derivedName}` : `Replicating ${bucket} to ${derivedName}`);
			open = false;
			onsaved();
		} catch (err) {
			toastError(err);
		} finally {
			saving = false;
		}
	}
</script>

<Dialog bind:open title={editing ? `Edit target ${editing.name}` : global ? 'Add server-wide target' : 'Add replication target'}>
	<form id="repl-target-form" onsubmit={save}>
		{#if !editing}
			<div class="field">
				<label for="rt-name">Name</label>
				<input id="rt-name" type="text" bind:value={name} placeholder={hostOf(endpoint) || 'defaults to the endpoint host'} />
				<div class="hint">Identifies the target in status output and the CLI (<code>--name</code>).</div>
			</div>
		{/if}
		<div class="field">
			<label for="rt-endpoint">Endpoint URL</label>
			<input id="rt-endpoint" type="text" bind:value={endpoint} placeholder="https://standby.example.com:9000" required />
			{#if peers.length > 0}
				<div class="hint">Servers on this network:
					{#each peers as p (p.id)}
						<button class="peer" type="button" onclick={() => usePeer(p)} title={p.endpoint}>{p.name || p.addr}</button>
					{/each}
				</div>
			{/if}
		</div>
		{#if global}
			<p class="hint" style="margin-top:-0.5rem">Every bucket on this server is mirrored to a bucket of the same name on the standby. Missing buckets are created there on first use, versioned when the source is.</p>
		{:else}
			<div class="field">
				<label for="rt-bucket">Destination bucket</label>
				<input id="rt-bucket" type="text" bind:value={destBucket} required />
				<div class="hint">Created on the standby if missing (versioned when this bucket is).</div>
			</div>
		{/if}
		<div class="grid grid-2">
			<div class="field">
				<label for="rt-ak">Access key</label>
				<input id="rt-ak" type="text" bind:value={accessKey} autocomplete="off" required={!editing} />
			</div>
			<div class="field">
				<label for="rt-sk">Secret key</label>
				<input id="rt-sk" type="password" bind:value={secretKey} autocomplete="new-password" placeholder={editing ? 'unchanged' : ''} required={!editing} />
			</div>
		</div>
		<div class="grid grid-2">
			<div class="field">
				<label for="rt-region">Region</label>
				<input id="rt-region" type="text" bind:value={region} placeholder="us-east-1" />
			</div>
			<div class="field">
				<label for="rt-prefix">Key prefix</label>
				<input id="rt-prefix" type="text" bind:value={prefix} placeholder="(all objects)" />
			</div>
		</div>
		<div class="field">
			<label class="checkbox"><input type="checkbox" bind:checked={pathStyle} /> Path-style addressing</label>
			<div class="hint">Leave on for hogshead and most self-hosted services; turn off for virtual-host style endpoints.</div>
		</div>
		<div class="field">
			<label class="checkbox"><input type="checkbox" bind:checked={enabled} /> Enabled</label>
		</div>
		{#if global}
			<div class="field">
				<label class="checkbox"><input type="checkbox" bind:checked={syncConfig} /> Also mirror users, access keys, passwords and settings</label>
				<div class="hint">The key above must belong to an administrator on the standby. Replication state, the web UI secret and the server identity are never mirrored.</div>
			</div>
		{/if}
	</form>
	{#snippet footer()}
		<button class="btn" type="button" onclick={() => (open = false)} disabled={saving}>Cancel</button>
		<button class="btn btn-primary" type="submit" form="repl-target-form" disabled={saving}>
			{#if saving}<span class="spinner"></span>{/if}
			{editing ? 'Save' : 'Add target'}
		</button>
	{/snippet}
</Dialog>

<style>
	.peer {
		margin: 0.15rem 0.25rem 0 0;
		padding: 0.1rem 0.5rem;
		border: 1px solid var(--border);
		border-radius: 999px;
		background: var(--surface-2);
		color: var(--text);
		font: inherit;
		font-size: 0.8rem;
		cursor: pointer;
	}
	.peer:hover {
		border-color: var(--accent);
	}
</style>
