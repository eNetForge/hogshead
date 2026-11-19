import { pushToast } from '$lib/stores/toast';

export async function copyText(text: string, label = 'Copied to clipboard'): Promise<boolean> {
	try {
		await navigator.clipboard.writeText(text);
		pushToast('success', label, 2500);
		return true;
	} catch {
		pushToast('error', 'Clipboard access denied; select and copy manually');
		return false;
	}
}
