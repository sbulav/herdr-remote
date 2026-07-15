# Herdr Remote web client

## Verification

Install the pinned dependencies, then run the complete browser-client verification:

```bash
npm ci
npm run verify
```

`verify` runs protocol generation, strict type checking, all Vitest/RTL/axe tests, the production InjectManifest build, and the Chromium/WebKit Playwright matrix.

The browser matrix drives the real React UI, reducer, protocol validator, and native WebSocket client through read-only and writable protocol lifecycles. It supplies an in-browser control-plane route; starting the future repository-root control-plane integration is intentionally outside this web package.

### NixOS

Playwright's downloaded WebKit build needs an FHS library environment on NixOS, and headless WPE may fail to initialize EGL on systems without a compatible renderer. The checked-in runner builds an FHS environment from the repository's locked nixpkgs input and runs WebKit under Xvfb without changing `flake.nix`:

```bash
npm ci
npm run verify:nix
```

Use `npm run test:e2e:nix` to run only the phone/tablet Chromium and WebKit matrix. The initial Nix build may download browser runtime libraries.

The automated matrix does not verify Web Push delivery on a physical iPhone or iPad. That remains a release-device check.
