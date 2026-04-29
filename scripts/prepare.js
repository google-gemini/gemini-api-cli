import { execSync } from 'child_process';

function hasBun() {
  try {
    execSync('bun --version', { stdio: 'ignore' });
    return true;
  } catch (e) {
    return false;
  }
}

if (hasBun()) {
  console.log('Bun found. Building with Bun...');
  execSync('bun run build', { stdio: 'inherit' });
} else {
  console.log('Bun not found. Falling back to esbuild...');
  try {
    execSync('npx -y esbuild src/cli.ts --bundle --platform=node --outdir=dist "--banner:js=#!/usr/bin/env node"', { stdio: 'inherit' });
  } catch (e) {
    console.error('Failed to build with esbuild.');
    process.exit(1);
  }
}
