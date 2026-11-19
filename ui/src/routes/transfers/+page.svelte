<script lang="ts">
	import { onMount } from 'svelte';
	import { admin, type Bucket, type TransferDirection, type TransferJob, type TransferRequest } from '$lib/api/admin';
	import AdminOnly from '$lib/components/AdminOnly.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import Dialog from '$lib/components/Dialog.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import { formatBytes, formatNumber, formatRelative } from '$lib/format';
	import { toastError, toastSuccess } from '$lib/stores/toast';

	let jobs = $state<TransferJob[]>([]);
	let buckets = $state<Bucket[]>([]);
	let loading = $state(true);
	let busy = $state('');
	let formOpen = $state(false);
	let detail = $state<TransferJob | null>(null);
	let removing = $state<TransferJob | null>(null);

	// Form.
	let direction = $state<TransferDirection>('export');
	let bucket = $state('');
	let prefix = $state('');
	let endpoint = $state('');
	let remoteBucket = $state('');
	let remotePrefix = $state('');
	let accessKey = $state('');
	let secretKey = $state('');
	let region = $state('');
	let pathStyle = $state(true);
	let overwrite = $state('if-changed');
	let dryRun = $state(false);
	let concurrency = $state(4);
	let starting = $state(false);

	const active = $derived(jobs.some((j) => j.state === 'queued' || j.state === 'running'));

	async function load() {
		try {
			[jobs, buckets] = await Promise.all([admin.transfers.list(), admin.buckets.list()]);
			if (!bucket && buckets.length > 0) bucket = buckets[0].name;
			if (detail) detail = jobs.find((j) => j.id === detail?.id) ?? detail;
		} catch (e) {
			toastError(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
		const t = setInterval(() => void load(), 3000);
		return () => clearInterval(t);
	});

	function openForm(dir: TransferDirection) {
		direction = dir;
		formOpen = true;
	}

	async function start(e: SubmitEvent) {
		e.preventDefault();
		if (!/^https?:\/\//.test(endpoint.trim())) return void toastError('Endpoint must start with http:// or https://');
		if (!bucket || !remoteBucket.trim() || !accessKey.trim() || !secretKey) return void toastError('Bucket, remote bucket and remote credentials are required');
		const req: TransferRequest = {
			direction,
			bucket,
			prefix: prefix.trim(),
			endpoint: endpoint.trim(),
			remote_bucket: remoteBucket.trim(),
			remote_prefix: remotePrefix.trim(),
			access_key: accessKey.trim(),
			secret_key: secretKey,
			region: region.trim() || undefined,
			path_style: pathStyle,
			overwrite,
			dry_run: dryRun,
			concurrency
		};
		starting = true;
		try {
			const j = await admin.transfers.start(req);
			toastSuccess(`${direction === 'export' ? 'Export' : 'Import'} ${j.id} started${dryRun ? ' (dry run)' : ''}`);
			formOpen = false;
			secretKey = '';
			await load();
		} catch (err) {
			toastError(err);
		} finally {
			starting = false;
		}
	}

	async function cancel(j: TransferJob) {
		busy = j.id;
		try {
			await admin.transfers.cancel(j.id);
			toastSuccess(isActive(j) ? `Cancelling ${j.id}` : `Removed ${j.id}`);
			removing = null;
			if (detail?.id === j.id && !isActive(j)) detail = null;
			await load();
		} catch (e) {
			toastError(e);
		} finally {
			busy = '';
		}
	}

	function isActive(j: TransferJob): boolean {
		return j.state === 'queued' || j.state === 'running';
	}

	function badge(state: string): string {
		switch (state) {
			case 'done':
				return 'badge-success';
			case 'running':
			case 'queued':
				return 'badge-accent';
			case 'failed':
			case 'interrupted':
				return 'badge-danger';
			default:
				return 'badge-warn';
		}
	}

	function remoteOf(j: TransferJob): string {
		return `${j.spec.remote.endpoint}/${j.spec.remote.bucket}/${j.spec.remote_prefix ?? ''}`;
	}
</script>

<svelte:head><title>Transfers · hogshead</title></svelte:head>

<AdminOnly>
	<div class="page-header">
		<h1>Transfers</h1>
		<div class="row">
			<button class="btn btn-sm" type="button" onclick={load} disabled={loading} aria-label="Refresh"><Icon name="refresh" size={14} /></button>
			<button class="btn btn-sm" type="button" onclick={() => openForm('import')}><Icon name="download" size={14} /> Import</button>
			<button class="btn btn-sm btn-primary" type="button" onclick={() => openForm('export')}><Icon name="upload" size={14} /> Export</button>
		</div>
	</div>
	<p class="muted small" style="margin-top:0">
		One-shot copies between a bucket here and a bucket on any S3-compatible service: export to migrate away or seed a standby, import to migrate in. Unchanged objects (same size and ETag) are skipped, so a job can be re-run to catch up.
		{#if active}<span class="badge badge-accent">running, refreshing every 3 s</span>{/if}
	</p>

	{#if loading && jobs.length === 0}
		<p class="muted"><span class="spinner"></span> Loading…</p>
	{:else if jobs.length === 0}
		<div class="empty">No transfers yet. Start an export or import above.</div>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr><th>Job</th><th>Direction</th><th>Local</th><th>Remote</th><th>State</th><th class="num">Listed</th><th class="num">Copied</th><th class="num">Skipped</th><th class="num">Failed</th><th class="num">Bytes</th><th>Started</th><th></th></tr>
				</thead>
				<tbody>
					{#each jobs as j (j.id)}
						<tr>
							<td><button class="link mono" type="button" onclick={() => (detail = j)}>{j.id}</button>{#if j.spec.dry_run}<span class="badge" style="margin-left:0.4rem">dry run</span>{/if}</td>
							<td>{j.spec.direction}</td>
							<td class="mono small">{j.spec.bucket}/{j.spec.prefix ?? ''}</td>
							<td class="mono small">{remoteOf(j)}</td>
							<td><span class="badge {badge(j.state)}" title={j.error ?? ''}>{j.state}</span></td>
							<td class="num">{formatNumber(j.listed)}</td>
							<td class="num">{formatNumber(j.copied)}</td>
							<td class="num">{formatNumber(j.skipped)}</td>
							<td class="num" class:danger={j.failed > 0}>{formatNumber(j.failed)}</td>
							<td class="num">{formatBytes(j.bytes)}</td>
							<td class="small">{formatRelative(j.started_at)}</td>
							<td class="actions">
								{#if isActive(j)}
									<button class="btn btn-sm btn-ghost" type="button" disabled={busy === j.id} onclick={() => cancel(j)}>Cancel</button>
								{:else}
									<button class="btn btn-sm btn-ghost btn-icon" type="button" aria-label="Remove record {j.id}" disabled={busy === j.id} onclick={() => (removing = j)}><Icon name="trash" size={14} /></button>
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}

	<Dialog bind:open={formOpen} title={direction === 'export' ? 'Export a bucket to another S3 service' : 'Import a bucket from another S3 service'} wide>
		<form id="transfer-form" onsubmit={start}>
			<div class="grid grid-2">
				<div class="field">
					<label for="tf-bucket">{direction === 'export' ? 'Source bucket (here)' : 'Destination bucket (here)'}</label>
					<select id="tf-bucket" bind:value={bucket} required>
						{#each buckets as b (b.name)}<option value={b.name}>{b.name}</option>{/each}
					</select>
					{#if direction === 'import'}<div class="hint">Must already exist; create it on the Buckets page first.</div>{/if}
				</div>
				<div class="field">
					<label for="tf-prefix">Local key prefix</label>
					<input id="tf-prefix" type="text" bind:value={prefix} placeholder="(whole bucket)" />
					<div class="hint">Keys are mapped by swapping the local and remote prefixes.</div>
				</div>
			</div>
			<div class="field">
				<label for="tf-endpoint">Remote endpoint URL</label>
				<input id="tf-endpoint" type="text" bind:value={endpoint} placeholder="https://s3.eu-central-1.amazonaws.com or https://other-server:9000" required />
			</div>
			<div class="grid grid-2">
				<div class="field">
					<label for="tf-rbucket">Remote bucket</label>
					<input id="tf-rbucket" type="text" bind:value={remoteBucket} required />
					{#if direction === 'export'}<div class="hint">Created if missing (credentials must allow it).</div>{/if}
				</div>
				<div class="field">
					<label for="tf-rprefix">Remote key prefix</label>
					<input id="tf-rprefix" type="text" bind:value={remotePrefix} placeholder="(whole bucket)" />
				</div>
			</div>
			<div class="grid grid-2">
				<div class="field">
					<label for="tf-ak">Remote access key</label>
					<input id="tf-ak" type="text" bind:value={accessKey} autocomplete="off" required />
				</div>
				<div class="field">
					<label for="tf-sk">Remote secret key</label>
					<input id="tf-sk" type="password" bind:value={secretKey} autocomplete="new-password" required />
					<div class="hint">Used only for this job; it is not stored with the job record.</div>
				</div>
			</div>
			<div class="grid grid-2">
				<div class="field">
					<label for="tf-region">Remote region</label>
					<input id="tf-region" type="text" bind:value={region} placeholder="us-east-1" />
				</div>
				<div class="field">
					<label for="tf-overwrite">Existing objects</label>
					<select id="tf-overwrite" bind:value={overwrite}>
						<option value="if-changed">Copy only if size or ETag differ</option>
						<option value="always">Always overwrite</option>
						<option value="never">Never overwrite</option>
					</select>
				</div>
			</div>
			<div class="grid grid-2">
				<div class="field">
					<label for="tf-conc">Parallel copies</label>
					<input id="tf-conc" type="number" min="1" max="16" bind:value={concurrency} />
				</div>
				<div class="field" style="padding-top:1.6rem">
					<label class="checkbox"><input type="checkbox" bind:checked={pathStyle} /> Path-style addressing (off for AWS)</label>
					<label class="checkbox" style="margin-top:0.4rem"><input type="checkbox" bind:checked={dryRun} /> Dry run: count only, write nothing</label>
				</div>
			</div>
		</form>
		{#snippet footer()}
			<button class="btn" type="button" onclick={() => (formOpen = false)} disabled={starting}>Cancel</button>
			<button class="btn btn-primary" type="submit" form="transfer-form" disabled={starting}>
				{#if starting}<span class="spinner"></span>{/if}
				{dryRun ? 'Start dry run' : direction === 'export' ? 'Start export' : 'Start import'}
			</button>
		{/snippet}
	</Dialog>

	{#if detail}
		<Dialog open={true} title="Transfer {detail.id}" onclose={() => (detail = null)}>
			<dl class="kv">
				<dt>Direction</dt><dd>{detail.spec.direction}{detail.spec.dry_run ? ' (dry run)' : ''}</dd>
				<dt>Local</dt><dd class="mono">{detail.spec.bucket}/{detail.spec.prefix ?? ''}</dd>
				<dt>Remote</dt><dd class="mono">{remoteOf(detail)}</dd>
				<dt>Access key</dt><dd class="mono">{detail.spec.remote.access_key}</dd>
				<dt>Overwrite</dt><dd>{detail.spec.overwrite}</dd>
				<dt>State</dt><dd><span class="badge {badge(detail.state)}">{detail.state}</span></dd>
				<dt>Progress</dt><dd>{formatNumber(detail.copied)} copied, {formatNumber(detail.skipped)} skipped, {formatNumber(detail.failed)} failed of {formatNumber(detail.listed)} listed · {formatBytes(detail.bytes)}</dd>
				<dt>Started by</dt><dd>{detail.actor}</dd>
				<dt>Started</dt><dd>{formatRelative(detail.started_at)}</dd>
				<dt>Finished</dt><dd>{formatRelative(detail.finished_at)}</dd>
			</dl>
			{#if detail.error}<div class="alert alert-danger" style="margin-top:0.75rem">{detail.error}</div>{/if}
			{#if detail.errors && detail.errors.length > 0}
				<h3 style="margin-top:0.75rem">First failures</h3>
				<ul class="errors mono small">
					{#each detail.errors as e, i (i)}<li>{e}</li>{/each}
				</ul>
			{/if}
			{#snippet footer()}
				<button class="btn" type="button" onclick={() => (detail = null)}>Close</button>
			{/snippet}
		</Dialog>
	{/if}

	<ConfirmDialog
		open={removing !== null}
		title="Remove transfer record"
		message="Remove the record of {removing?.id ?? ''}? Copied objects are not affected."
		confirmLabel="Remove"
		busy={busy !== ''}
		onconfirm={() => {
			if (removing) void cancel(removing);
		}}
	/>
</AdminOnly>

<style>
	.link {
		background: none;
		border: none;
		padding: 0;
		color: var(--accent);
		cursor: pointer;
		font: inherit;
		text-decoration: underline;
	}
	.danger {
		color: var(--danger);
		font-weight: 600;
	}
	.errors {
		margin: 0;
		padding-left: 1.2rem;
		max-height: 12rem;
		overflow: auto;
	}
</style>
