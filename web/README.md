# Herdr Remote web client

## Verification

Install the pinned dependencies, then run the complete browser-client verification:

```bash
npm ci
npm run verify
```

`verify` runs protocol generation, strict type checking, all Vitest/RTL/axe tests, the production InjectManifest build, and the Chromium/WebKit Playwright matrix.

The browser matrix drives the React UI, reducer, protocol validator, and browser WebSocket client through read-only and writable protocol lifecycles. It supplies an in-browser protocol route so the UI suite remains independent of service credentials.

### NixOS

Playwright's WebKit build needs an FHS library environment on NixOS, and headless WPE may fail to initialize EGL on systems without a compatible renderer. The checked-in runner builds an FHS environment from the locked nixpkgs input, installs the browser version pinned by `package-lock.json`, and runs WebKit under Xvfb:

```bash
npm ci
npm run verify:nix
```

Use `npm run test:e2e:nix` to run only the phone/tablet Chromium and WebKit matrix. The initial Nix build may download browser runtime libraries.

The automated matrix does not verify Web Push delivery on a physical iPhone or iPad. That remains a release-device check.
