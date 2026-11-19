<script lang="ts">
	import { base } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { untrack } from 'svelte';
	import { admin, type Bucket, type UpdateBucketRequest } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import ReplicationCard from '$lib/components/ReplicationCard.svelte';
	import UsageTable from '$lib/components/UsageTable.svelte';
	import { type UsageResponse } from '$lib/api/admin';
	import { formatBytes, formatDate, formatNumber } from '$lib/format';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	const name = $derived(page.params.name ?? '');

	let bucket = $state<Bucket | null>(null);
	let loading = $state(true);
	let saving = $state('');
	let policy = $state('');
	let cors = $state('');
	let lifecycle = $state('');
	let lcDays = $state(30);
	let lcPrefix = $state('');
	let deleteOpen = $state(false);
	let purgeOpen = $state(false);
	let usage = $state<UsageResponse | null>(null);
	let usageSince = $state('7d');

	async function loadUsage() {
		try {
			usage = await admin.usage({ from: usageSince, by: 'class', bucket: name });
		} catch {
			usage = null;
		}
	}
	$effect(() => {
		void name;
		void usageSince;
		untrack(() => void loadUsage());
	});
	let purgeConfirm = $state('');

	const publicRead = $derived(isPublicRead(policy));

	function isPublicRead(p: string): boolean {
		try {
			const doc = JSON.parse(p);
			const stmts: unknown[] = Array.isArray(doc?.Statement) ? doc.Statement : [];
			return stmts.some((s) => {
				const st = s as { Effect?: string; Principal?: unknown; Action?: unknown };
				const principal = st.Principal === '*' || (typeof st.Principal === 'object' && st.Principal !== null && (st.Principal as { AWS?: unknown }).AWS === '*');
				const actions = Array.isArray(st.Action) ? st.Action : [st.Action];
				return st.Effect === 'Allow' && principal && actions.some((a) => a === 's3:GetObject' || a === 's3:*');
			});
		} catch {
			return false;
		}
	}

	async function load() {
		loading = true;
		try {
			bucket = await admin.buckets.get(name);
			policy = bucket.policy ?? '';
			cors = bucket.cors ?? '';
			lifecycle = bucket.lifecycle ?? '';
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void name;
		untrack(load);
	});

	async function update(what: string, req: UpdateBucketRequest, message: string) {
		saving = what;
		try {
			bucket = await admin.buckets.update(name, req);
			policy = bucket.policy ?? '';
			cors = bucket.cors ?? '';
			lifecycle = bucket.lifecycle ?? '';
			toastSuccess(message);
		} catch (e) {
			toastError(e);
		} finally {
			saving = '';
		}
	}

	function savePolicy() {
		const trimmed = policy.trim();
		if (trimmed) {
			try {
				const parsed = JSON.parse(trimmed);
				if (typeof parsed !== 'object' || parsed === null || !Array.isArray(parsed.Statement)) {
					toastError('Policy must be a JSON object with a Statement array');
					return;
				}
			} catch (e) {
				toastError(`Policy is not valid JSON: ${e instanceof Error ? e.message : e}`);
				return;
			}
		}
		update('policy', { policy: trimmed }, trimmed ? 'Policy saved' : 'Policy removed');
	}

	function validXml(s: string, rootName: string): string | null {
		const doc = new DOMParser().parseFromString(s, 'application/xml');
		if (doc.getElementsByTagName('parsererror').length) return 'Not well-formed XML';
		if (doc.documentElement.localName !== rootName) return `Root element must be <${rootName}>`;
		return null;
	}

	function saveCors() {
		const trimmed = cors.trim();
		if (trimmed) {
			const err = validXml(trimmed, 'CORSConfiguration');
			if (err) return void toastError(err);
		}
		update('cors', { cors: trimmed }, trimmed ? 'CORS saved' : 'CORS removed');
	}

	function saveLifecycle() {
		const trimmed = lifecycle.trim();
		if (trimmed) {
			const err = validXml(trimmed, 'LifecycleConfiguration');
			if (err) return void toastError(err);
		}
		update('lifecycle', { lifecycle: trimmed }, trimmed ? 'Lifecycle saved' : 'Lifecycle removed');
	}

	function generateLifecycle() {
		const days = Math.max(1, Math.floor(lcDays));
		const esc = (s: string) => s.replace(/[<>&]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;' })[c] as string);
		const filter = lcPrefix ? `<Filter><Prefix>${esc(lcPrefix)}</Prefix></Filter>` : '<Filter></Filter>';
		lifecycle = `<LifecycleConfiguration>
  <Rule>
    <ID>expire-${lcPrefix ? lcPrefix.replace(/[^a-z0-9-]/gi, '-') + '-' : ''}after-${days}d</ID>
    <Status>Enabled</Status>
    ${filter}
    <Expiration><Days>${days}</Days></Expiration>
  </Rule>
</LifecycleConfiguration>`;
	}

	async function remove(force = false) {
		saving = 'delete';
		try {
			await admin.buckets.remove(name, force);
			toastSuccess(force ? `Bucket ${name} and all its contents deleted` : `Bucket ${name} deleted`);
			goto(`${base}/buckets`);
		} catch (e) {
			toastError(e);
			deleteOpen = false;
			purgeOpen = false;
		} finally {
			saving = '';
		}
	}

	const hidden = $derived((bucket?.versions ?? 0) - (bucket?.objects ?? 0));
</script>

<svelte:head><title>{name} settings · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<nav class="row" aria-label="Breadcrumb" style="font-size:1.05rem">
			<a href="{base}/buckets">Buckets</a>
			<Icon name="chevron-right" size={14} />
			<a href="{base}/buckets/{encodeURIComponent(name)}">{name}</a>
			<Icon name="chevron-right" size={14} />
			<strong>Settings</strong>
		</nav>
	</div>

	{#if loading && !bucket}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if bucket}
		<div class="grid grid-2">
			<section class="card">
				<div class="card-title"><h2>Overview</h2></div>
				<dl class="kv">
					<dt>Owner</dt><dd>{bucket.owner_name || bucket.owner || '–'}</dd>
					<dt>Created</dt><dd>{formatDate(bucket.created_at)}</dd>
					<dt>Objects</dt><dd>{formatNumber(bucket.objects)}{#if hidden > 0} <span class="muted small">+ {formatNumber(hidden)} non-current version{hidden === 1 ? '' : 's'} or delete marker{hidden === 1 ? '' : 's'} ({formatNumber(bucket.delete_markers ?? 0)} marker{(bucket.delete_markers ?? 0) === 1 ? '' : 's'})</span>{/if}</dd>
					<dt>Size</dt><dd>{formatBytes(bucket.bytes)}</dd>
					<dt>Object lock</dt><dd>{bucket.object_lock ? 'enabled' : 'off'}</dd>
				</dl>
			</section>

			<section class="card">
				<div class="card-title"><h2>Access & consistency</h2></div>
				<div class="field">
					<label for="versioning">Versioning</label>
					<div class="row">
						<select id="versioning" value={bucket.versioning || ''} disabled={bucket.object_lock || saving === 'versioning'} onchange={(e) => update('versioning', { versioning: e.currentTarget.value }, 'Versioning updated')}>
							{#if !bucket.versioning}<option value="">Off</option>{/if}
							<option value="Enabled">Enabled</option>
							<option value="Suspended">Suspended</option>
						</select>
					</div>
					{#if bucket.object_lock}<div class="hint">Versioning is required by object lock.</div>
					{:else if !bucket.versioning}<div class="hint">Once enabled, versioning can only be suspended, not turned off.</div>{/if}
				</div>
				<div class="field">
					<label class="checkbox">
						<input type="checkbox" checked={publicRead} disabled={saving === 'public'} onchange={(e) => update('public', { public_read: e.currentTarget.checked }, e.currentTarget.checked ? 'Bucket is now publicly readable' : 'Public read removed')} />
						Public read (anonymous GetObject)
					</label>
					<div class="hint">Replaces the bucket policy with a public-read policy, or clears it.</div>
				</div>
			</section>
		</div>

		<section class="card" style="margin-top:1rem">
			<div class="card-title"><h2>Bucket policy</h2><span class="muted small">JSON · empty removes the policy</span></div>
			<div class="field">
				<label for="policy" class="sr-only">Policy JSON</label>
				<textarea id="policy" bind:value={policy} rows="10" spellcheck="false" placeholder={'{"Version":"2012-10-17","Statement":[...]}'}></textarea>
			</div>
			<div class="row row-end">
				<button class="btn btn-primary" type="button" onclick={savePolicy} disabled={saving === 'policy'}>Save policy</button>
			</div>
		</section>

		<section class="card" style="margin-top:1rem">
			<div class="card-title"><h2>CORS</h2><span class="muted small">XML · empty removes the configuration</span></div>
			<div class="field">
				<label for="cors" class="sr-only">CORS XML</label>
				<textarea id="cors" bind:value={cors} rows="8" spellcheck="false" placeholder="<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>"></textarea>
			</div>
			<div class="row row-end">
				<button class="btn btn-primary" type="button" onclick={saveCors} disabled={saving === 'cors'}>Save CORS</button>
			</div>
		</section>

		<section class="card" style="margin-top:1rem">
			<div class="card-title"><h2>Lifecycle</h2><span class="muted small">XML · empty removes the configuration</span></div>
			<div class="helper">
				<span class="muted small">Generate a rule:</span>
				<label for="lc-days" class="sr-only">Expire after days</label>
				<span class="row"><span>expire after</span><input id="lc-days" type="number" min="1" bind:value={lcDays} style="width:5rem" /><span>days</span></span>
				<label for="lc-prefix" class="sr-only">Key prefix</label>
				<span class="row"><span>with prefix</span><input id="lc-prefix" type="text" bind:value={lcPrefix} placeholder="(all objects)" style="width:12rem" /></span>
				<button class="btn btn-sm" type="button" onclick={generateLifecycle}>Generate</button>
			</div>
			<div class="field">
				<label for="lifecycle" class="sr-only">Lifecycle XML</label>
				<textarea id="lifecycle" bind:value={lifecycle} rows="10" spellcheck="false"></textarea>
			</div>
			<div class="row row-end">
				<button class="btn btn-primary" type="button" onclick={saveLifecycle} disabled={saving === 'lifecycle'}>Save lifecycle</button>
			</div>
		</section>

		<section class="card" style="margin-top:1rem">
			<div class="card-title">
				<h2>Usage</h2>
				<div class="row">
					<select bind:value={usageSince} aria-label="Usage window">
						<option value="24h">Last 24 hours</option>
						<option value="7d">Last 7 days</option>
						<option value="30d">Last 30 days</option>
					</select>
					<a class="btn btn-sm" href="{base}/usage?bucket={encodeURIComponent(name)}">All usage →</a>
				</div>
			</div>
			{#if usage}
				<UsageTable rows={usage.rows} by={usage.by} totals={usage.totals} />
			{:else}
				<p class="muted small">No usage data.</p>
			{/if}
		</section>

		<ReplicationCard bucket={name} />

		<section class="card danger-zone" style="margin-top:1rem">
			<div class="card-title"><h2>Danger zone</h2></div>
			<div class="row">
				<div class="grow">
					<strong>Delete this bucket</strong>
					<div class="muted small">
						The bucket must be empty{bucket.versioning ? ', including all object versions and delete markers' : ''}.
						{#if bucket.objects === 0 && hidden > 0}
							It shows no objects, but still holds {formatNumber(hidden)} non-current version{hidden === 1 ? '' : 's'} or delete marker{hidden === 1 ? '' : 's'}, which the browser does not list.
						{/if}
					</div>
				</div>
				<button class="btn btn-danger" type="button" onclick={() => (deleteOpen = true)} disabled={(bucket.versions ?? bucket.objects) > 0}>
					<Icon name="trash" size={14} /> Delete bucket
				</button>
			</div>
			{#if (bucket.versions ?? bucket.objects) > 0}
				<div class="row" style="margin-top:0.75rem">
					<div class="grow">
						<strong>Delete bucket and all its contents</strong>
						<div class="muted small">Removes every object, every version and every delete marker, then the bucket. Objects under an unexpired compliance retention or a legal hold block this. In a cellar the deletion reaches every member.</div>
					</div>
					<button class="btn btn-danger" type="button" onclick={() => { purgeConfirm = ''; purgeOpen = true; }}>
						<Icon name="trash" size={14} /> Delete everything
					</button>
				</div>
			{/if}
		</section>

		<ConfirmDialog bind:open={deleteOpen} title="Delete bucket" message="Delete bucket “{name}”? This cannot be undone." busy={saving === 'delete'} onconfirm={() => remove(false)} />
		<Dialog bind:open={purgeOpen} title="Delete bucket and all its contents">
			<p>This permanently removes <strong>{formatNumber(bucket.versions ?? 0)}</strong> stored version{(bucket.versions ?? 0) === 1 ? '' : 's'} ({formatBytes(bucket.bytes)} of current data) and the bucket <span class="mono">{name}</span>. It cannot be undone.</p>
			<div class="field">
				<label for="purge-confirm">Type the bucket name to confirm</label>
				<input id="purge-confirm" type="text" bind:value={purgeConfirm} autocomplete="off" spellcheck="false" />
			</div>
			{#snippet footer()}
				<button class="btn" type="button" onclick={() => (purgeOpen = false)} disabled={saving === 'delete'}>Cancel</button>
				<button class="btn btn-danger" type="button" onclick={() => remove(true)} disabled={saving === 'delete' || purgeConfirm !== name}>
					{#if saving === 'delete'}<span class="spinner"></span>{/if}
					Delete everything
				</button>
			{/snippet}
		</Dialog>
	{/if}
</AdminOnly>

<style>
	.helper {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		flex-wrap: wrap;
		margin-bottom: 0.75rem;
	}
	.danger-zone {
		border-color: var(--danger);
	}
</style>
