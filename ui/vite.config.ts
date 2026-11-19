import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		// Local development against a running node: proxy API traffic.
		proxy: {
			'/_admin': 'http://127.0.0.1:9000',
			'^/(?!_ui).*': { target: 'http://127.0.0.1:9000', bypass: (req) => (req.url?.startsWith('/_ui') ? req.url : undefined) }
		}
	},
	test: {
		include: ['src/**/*.test.ts'],
		environment: 'node'
	}
});
